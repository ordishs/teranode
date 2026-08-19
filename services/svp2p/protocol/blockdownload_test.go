package protocol

import (
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

// downloadFixture is one genesis-rooted index, the headers-first machine that
// shares it, and the downloader under test. The headers here carry no valid
// proof of work on purpose: nothing in this file reaches OnHeaders, which is
// the only code path that checks it, so the cheap headerindex_test helpers are
// used instead of the ground ones from headersync_test.
type downloadFixture struct {
	idx     *HeaderIndex
	hs      *HeaderSync
	bd      *BlockDownloader
	genesis *wire.BlockHeader
	chain   []*wire.BlockHeader // chain[i] has height i+1
	nonces  *nonceCounter
}

func newDownloadFixture(t *testing.T, height int) *downloadFixture {
	t.Helper()

	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	nonces := &nonceCounter{}
	chain := buildChain(t, idx, nonces, genesis, height)

	// A checkpoint far above the fixture height keeps headers-first mode on
	// once a peer takes the sync slot, which is the state the rotation rules
	// are exercised in.
	cpHash := chainhash.Hash{0xC0}

	hs, err := NewHeaderSync(HeaderSyncConfig{
		Index:  idx,
		Params: syncTestParams([]chaincfg.Checkpoint{{Height: 100000, Hash: &cpHash}}),
	})
	require.NoError(t, err)

	bd, err := NewBlockDownloader(idx, hs)
	require.NoError(t, err)

	return &downloadFixture{idx: idx, hs: hs, bd: bd, genesis: genesis, chain: chain, nonces: nonces}
}

// node returns the indexed header at height h, where 0 is genesis.
func (f *downloadFixture) node(t *testing.T, h int) HeaderNode {
	t.Helper()

	hash := f.genesis.BlockHash()
	if h > 0 {
		hash = f.chain[h-1].BlockHash()
	}

	n, ok := f.idx.Lookup(hash)
	require.True(t, ok)

	return n
}

// peerAt returns a full-node peer whose pindexBestKnownBlock is the fixture
// header at height h.
func (f *downloadFixture) peerAt(t *testing.T, addr string, h int) *SyncPeer {
	t.Helper()

	peer := fullNodePeer(addr)
	best := f.node(t, h)
	peer.State.pindexBestKnownBlock = &best

	return peer
}

// micros converts a duration to the microsecond unit every timestamp in this
// machine uses, the port of SVNode's GetTimeMicros.
// noIngest is the "no block is being ingested from this peer" observation,
// which is what every case in this file exercises: the ingest-aware
// suppression has its own tests.
var noIngest = IngestSnapshot{}

func micros(d time.Duration) int64 { return int64(d / time.Microsecond) }

// testNow is an arbitrary fixed microsecond timestamp. Every clock value in
// this file is derived from it: the machine reads no clock of its own.
const testNow = int64(1_700_000_000_000_000)

func invMsg(t *testing.T, typ wire.InvType, hashes ...chainhash.Hash) *wire.MsgInv {
	t.Helper()

	msg := wire.NewMsgInv()

	for i := range hashes {
		hash := hashes[i]
		require.NoError(t, msg.AddInvVect(wire.NewInvVect(typ, &hash)))
	}

	return msg
}

// TestFindNextBlocksToDownload_PeerWithNothingUseful covers the
// net_processing.cpp "This peer has nothing interesting" returns, including the
// IBD world where every peer except the sync peer carries only
// hashLastUnknownBlock.
func TestFindNextBlocksToDownload_PeerWithNothingUseful(t *testing.T) {
	f := newDownloadFixture(t, 3)
	activeTip := f.node(t, 3)

	tests := []struct {
		name  string
		setup func(peer *SyncPeer)
	}{
		{
			name: "only an unknown announcement parked: pindexBestKnownBlock is still nullptr",
			setup: func(peer *SyncPeer) {
				peer.State.hashLastUnknownBlock = chainhash.Hash{0xAB}
			},
		},
		{
			name:  "nothing announced at all",
			setup: func(_ *SyncPeer) {},
		},
		{
			name: "best known block below our own tip",
			setup: func(peer *SyncPeer) {
				n := f.node(t, 1)
				peer.State.pindexBestKnownBlock = &n
			},
		},
		{
			name: "best known block equal to our own tip",
			setup: func(peer *SyncPeer) {
				n := f.node(t, 3)
				peer.State.pindexBestKnownBlock = &n
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			peer := fullNodePeer("1.2.3.4:8333")
			tc.setup(peer)

			blocks, staller := f.bd.FindNextBlocksToDownload(peer, activeTip, MaxBlocksInTransitPerPeer)

			require.Empty(t, blocks)
			require.Nil(t, staller)
		})
	}
}

func TestFindNextBlocksToDownload_ReturnsSuccessorsInOrder(t *testing.T) {
	f := newDownloadFixture(t, 10)
	activeTip := f.node(t, 4)
	peer := f.peerAt(t, "1.2.3.4:8333", 10)

	t.Run("every missing successor up to the peer's best known block", func(t *testing.T) {
		blocks, staller := f.bd.FindNextBlocksToDownload(peer, activeTip, MaxBlocksInTransitPerPeer)

		require.Nil(t, staller)
		require.Len(t, blocks, 6)

		for i := range blocks {
			require.Equal(t, int32(5+i), blocks[i].Height)
			require.Equal(t, f.node(t, 5+i).Hash, blocks[i].Hash)
		}
	})

	t.Run("count caps the batch", func(t *testing.T) {
		peer.State.pindexLastCommonBlock = nil

		blocks, staller := f.bd.FindNextBlocksToDownload(peer, activeTip, 3)

		require.Nil(t, staller)
		require.Len(t, blocks, 3)
		require.Equal(t, int32(5), blocks[0].Height)
		require.Equal(t, int32(7), blocks[2].Height)
	})
}

func TestFindNextBlocksToDownload_SkipsBlocksAlreadyInFlight(t *testing.T) {
	f := newDownloadFixture(t, 10)
	activeTip := f.node(t, 4)

	holder := f.peerAt(t, "5.6.7.8:8333", 10)
	require.True(t, f.bd.MarkBlockAsInFlight(holder, f.node(t, 6)))

	peer := f.peerAt(t, "1.2.3.4:8333", 10)

	blocks, staller := f.bd.FindNextBlocksToDownload(peer, activeTip, MaxBlocksInTransitPerPeer)

	// The window never ran out, so nobody is named as a staller.
	require.Nil(t, staller)
	require.Len(t, blocks, 5)
	require.Equal(t, int32(5), blocks[0].Height)
	require.Equal(t, int32(7), blocks[1].Height, "the in-flight height 6 must be skipped, not re-requested")
}

// TestFindNextBlocksToDownload_ComparesChainWorkNotHeight pins the
// net_processing.cpp:362-368 gate: "This peer has nothing interesting" fires
// when the peer's best known block carries less work than our own tip, whatever
// the two heights are. The Phase 2 port compared heights here, which scheduled
// a whole low-work branch off a peer that had nothing better than what we hold.
func TestFindNextBlocksToDownload_ComparesChainWorkNotHeight(t *testing.T) {
	// Two branches off the same genesis, weights from the per-header proofs
	// pinned by TestBlockProof:
	//   heavy, 3 headers: 4_295_032_833 + 3 * 8_589_934_591 = 30_064_836_606
	//   light, 5 headers: 6 * 4_295_032_833                 = 25_770_196_998
	// The light branch stands two blocks taller and carries less work.
	const (
		heavyLen = 3
		lightLen = 5
	)

	newFixture := func(t *testing.T) (*BlockDownloader, HeaderNode, HeaderNode) {
		t.Helper()

		genesis := testGenesis()

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		nc := &nonceCounter{}
		heavy := buildChainBits(t, idx, nc, genesis, heavyLen, heavyBits)
		light := buildChainBits(t, idx, nc, genesis, lightLen, difficulty1Bits)

		cpHash := chainhash.Hash{0xC0}

		hs, err := NewHeaderSync(HeaderSyncConfig{
			Index:  idx,
			Params: syncTestParams([]chaincfg.Checkpoint{{Height: 100000, Hash: &cpHash}}),
		})
		require.NoError(t, err)

		bd, err := NewBlockDownloader(idx, hs)
		require.NoError(t, err)

		heavyTip, ok := idx.Lookup(heavy[len(heavy)-1].BlockHash())
		require.True(t, ok)
		require.Equal(t, int32(heavyLen), heavyTip.Height)

		lightTip, ok := idx.Lookup(light[len(light)-1].BlockHash())
		require.True(t, ok)
		require.Equal(t, int32(lightLen), lightTip.Height)

		require.Positive(t, heavyTip.ChainWork.Cmp(lightTip.ChainWork),
			"test setup: the shorter branch must be the heavier one")

		return bd, heavyTip, lightTip
	}

	t.Run("a taller but lighter best known block has nothing interesting", func(t *testing.T) {
		bd, heavyTip, lightTip := newFixture(t)

		peer := fullNodePeer("1.2.3.4:8333")
		peer.State.pindexBestKnownBlock = &lightTip

		blocks, staller := bd.FindNextBlocksToDownload(peer, heavyTip, MaxBlocksInTransitPerPeer)

		require.Empty(t, blocks, "a branch with less work than our tip must not be scheduled")
		require.Nil(t, staller)
	})

	t.Run("a shorter but heavier best known block is scheduled", func(t *testing.T) {
		bd, heavyTip, lightTip := newFixture(t)

		peer := fullNodePeer("1.2.3.4:8333")
		peer.State.pindexBestKnownBlock = &heavyTip

		blocks, staller := bd.FindNextBlocksToDownload(peer, lightTip, MaxBlocksInTransitPerPeer)

		require.Nil(t, staller)
		require.Len(t, blocks, heavyLen, "the whole heavier branch is missing and must be fetched")

		for i := range blocks {
			require.Equal(t, int32(1+i), blocks[i].Height)
		}
	})
}

// TestFindNextBlocksToDownload_FollowsAPeerReorg pins the net_processing.cpp
// "If the peer reorganized, our previous pindexLastCommonBlock may not be an
// ancestor of its current tip anymore" rewind.
func TestFindNextBlocksToDownload_FollowsAPeerReorg(t *testing.T) {
	f := newDownloadFixture(t, 6)
	activeTip := f.node(t, 2)

	// A branch forking off height 2 and reaching height 7.
	side := buildChain(t, f.idx, &nonceCounter{next: 1000}, f.chain[1], 5)

	sideTip, ok := f.idx.Lookup(side[len(side)-1].BlockHash())
	require.True(t, ok)
	require.Equal(t, int32(7), sideTip.Height)

	peer := fullNodePeer("1.2.3.4:8333")
	peer.State.pindexBestKnownBlock = &sideTip

	// A stale last-common from before the peer reorged, on the main branch and
	// above the fork point.
	stale := f.node(t, 5)
	peer.State.pindexLastCommonBlock = &stale

	blocks, staller := f.bd.FindNextBlocksToDownload(peer, activeTip, MaxBlocksInTransitPerPeer)

	require.Nil(t, staller)
	require.Equal(t, int32(2), peer.State.pindexLastCommonBlock.Height, "last common must rewind to the fork point")
	require.Len(t, blocks, 5)

	for i := range blocks {
		require.Equal(t, int32(3+i), blocks[i].Height)
		require.Equal(t, side[i].BlockHash(), blocks[i].Hash, "the peer's branch must be fetched, not ours")
	}
}

// TestFindNextBlocksToDownload_WindowAdvancesOnlyOnContiguousCompletion is the
// moving-window rule. A block completing in the middle of the window frees its
// own slot but must not extend the window: net_processing.cpp advances
// pindexLastCommonBlock only over blocks whose ancestors are all downloaded
// (the GetChainTx() guard), so the window end moves only when the prefix below
// the hole closes.
func TestFindNextBlocksToDownload_WindowAdvancesOnlyOnContiguousCompletion(t *testing.T) {
	const peerHeight = BlockDownloadWindow + 6

	f := newDownloadFixture(t, peerHeight)
	activeTip := f.node(t, 0)
	peer := f.peerAt(t, "1.2.3.4:8333", peerHeight)

	// The first sweep offers exactly the window: heights 1..1024, never the
	// blocks above it, even though the peer has them.
	blocks, staller := f.bd.FindNextBlocksToDownload(peer, activeTip, 4096)
	require.Nil(t, staller)
	require.Len(t, blocks, BlockDownloadWindow)
	require.Equal(t, int32(1), blocks[0].Height)
	require.Equal(t, int32(BlockDownloadWindow), blocks[len(blocks)-1].Height)

	for _, b := range blocks {
		require.True(t, f.bd.MarkBlockAsInFlight(peer, b))
	}

	// Out-of-order completion in the middle of the window.
	require.True(t, f.bd.BlockReceived(peer, f.node(t, 500).Hash, testNow))

	blocks, staller = f.bd.FindNextBlocksToDownload(peer, activeTip, 4096)
	require.Nil(t, staller, "the peer is waiting on its own in-flight blocks, so it is not its own staller")
	require.Empty(t, blocks, "a mid-window completion must not release blocks beyond the window end")
	require.Equal(t, int32(0), peer.State.pindexLastCommonBlock.Height, "the window must not move over a hole")

	// Closing the prefix below the hole moves the window.
	for h := 1; h < 500; h++ {
		require.True(t, f.bd.BlockReceived(peer, f.node(t, h).Hash, testNow))
	}

	// net_processing.cpp computes nWindowEnd once, before the walk that
	// advances pindexLastCommonBlock, so this pass records the advance and the
	// next one gets the wider window.
	blocks, staller = f.bd.FindNextBlocksToDownload(peer, activeTip, 4096)
	require.Nil(t, staller)
	require.Empty(t, blocks)
	require.Equal(t, int32(500), peer.State.pindexLastCommonBlock.Height)

	blocks, staller = f.bd.FindNextBlocksToDownload(peer, activeTip, 4096)
	require.Nil(t, staller)
	require.Len(t, blocks, 6, "the window end moved to 500+1024, exposing the remaining blocks")
	require.Equal(t, int32(BlockDownloadWindow+1), blocks[0].Height)
	require.Equal(t, int32(peerHeight), blocks[len(blocks)-1].Height)
}

// TestSendGetDataBlocks_InTransitCapHoldsAtSixteen pins
// MAX_BLOCKS_IN_TRANSIT_PER_PEER: the gate closes at exactly 16 and reopens by
// exactly one slot per receipt.
func TestSendGetDataBlocks_InTransitCapHoldsAtSixteen(t *testing.T) {
	f := newDownloadFixture(t, 100)
	activeTip := f.node(t, 0)
	peer := f.peerAt(t, "1.2.3.4:8333", 100)

	msgs := f.bd.SendGetDataBlocks(peer, activeTip, testNow)
	require.Len(t, msgs, 1)

	getData, ok := msgs[0].(*wire.MsgGetData)
	require.True(t, ok)
	require.Len(t, getData.InvList, MaxBlocksInTransitPerPeer)
	require.Equal(t, MaxBlocksInTransitPerPeer, peer.State.nBlocksInFlight)

	for i, inv := range getData.InvList {
		require.Equal(t, wire.InvTypeBlock, inv.Type)
		require.Equal(t, f.node(t, i+1).Hash, inv.Hash)
	}

	require.Empty(t, f.bd.SendGetDataBlocks(peer, activeTip, testNow), "the cap must hold at exactly 16")
	require.Equal(t, MaxBlocksInTransitPerPeer, peer.State.nBlocksInFlight)

	require.True(t, f.bd.BlockReceived(peer, getData.InvList[0].Hash, testNow))
	require.Equal(t, MaxBlocksInTransitPerPeer-1, peer.State.nBlocksInFlight)

	msgs = f.bd.SendGetDataBlocks(peer, activeTip, testNow)
	require.Len(t, msgs, 1)

	next, ok := msgs[0].(*wire.MsgGetData)
	require.True(t, ok)
	require.Len(t, next.InvList, 1, "one receipt frees exactly one slot")
	require.Equal(t, f.node(t, MaxBlocksInTransitPerPeer+1).Hash, next.InvList[0].Hash)
	require.Equal(t, MaxBlocksInTransitPerPeer, peer.State.nBlocksInFlight)
}

// TestFindNextBlocksToDownload_PrunesReceivedBlocksBehindTheActiveTip pins the
// haveData watermark. Without it every received block stays recorded for the
// lifetime of the process, because the download walk starts above the last
// common block and the steady-state calls return before ever reaching it.
func TestFindNextBlocksToDownload_PrunesReceivedBlocksBehindTheActiveTip(t *testing.T) {
	f := newDownloadFixture(t, 40)
	peer := f.peerAt(t, "1.2.3.4:8333", 40)

	// Our chain is still at genesis while 20 blocks arrive.
	activeTip := f.node(t, 0)

	blocks, _ := f.bd.FindNextBlocksToDownload(peer, activeTip, 20)
	require.Len(t, blocks, 20)

	for _, b := range blocks {
		require.True(t, f.bd.MarkBlockAsInFlight(peer, b))
		require.True(t, f.bd.BlockReceived(peer, b.Hash, testNow))
	}

	require.Equal(t, 20, len(f.bd.haveData), "every delivered block is recorded until our chain covers it")

	// Our chain connects the first 12 of them.
	f.bd.FindNextBlocksToDownload(peer, f.node(t, 12), MaxBlocksInTransitPerPeer)

	require.Equal(t, 8, len(f.bd.haveData), "everything at or below the active tip must be dropped")

	for h := 13; h <= 20; h++ {
		require.Contains(t, f.bd.haveData, f.node(t, h).Hash, "blocks above the active tip are still ours to remember")
	}

	// The whole run connects.
	f.bd.FindNextBlocksToDownload(peer, f.node(t, 20), MaxBlocksInTransitPerPeer)
	require.Empty(t, f.bd.haveData)

	// A steady-state call that returns early must still prune: this peer has
	// nothing interesting, which is the path that made the leak unbounded.
	idle := fullNodePeer("9.9.9.9:8333")
	require.True(t, f.bd.MarkBlockAsInFlight(peer, f.node(t, 25)))
	require.True(t, f.bd.BlockReceived(peer, f.node(t, 25).Hash, testNow))
	require.Len(t, f.bd.haveData, 1)

	f.bd.FindNextBlocksToDownload(idle, f.node(t, 25), MaxBlocksInTransitPerPeer)
	require.Empty(t, f.bd.haveData, "the prune must run before the nothing-interesting return")
}

// TestFindNextBlocksToDownload_RefusesAnActiveTipOutsideTheIndex pins the
// contract that activeTip must be a header this index holds. Guessing would
// mean treating our own blocks as missing and re-requesting the whole branch.
func TestFindNextBlocksToDownload_RefusesAnActiveTipOutsideTheIndex(t *testing.T) {
	f := newDownloadFixture(t, 10)
	peer := f.peerAt(t, "1.2.3.4:8333", 10)

	stranger := HeaderNode{Hash: chainhash.Hash{0xFE}, Height: 4}

	t.Run("on the bootstrap path", func(t *testing.T) {
		blocks, staller := f.bd.FindNextBlocksToDownload(peer, stranger, MaxBlocksInTransitPerPeer)

		require.Empty(t, blocks)
		require.Nil(t, staller)
	})

	t.Run("on the steady-state path, with last-common already set", func(t *testing.T) {
		known := f.node(t, 2)
		peer.State.pindexLastCommonBlock = &known

		blocks, staller := f.bd.FindNextBlocksToDownload(peer, stranger, MaxBlocksInTransitPerPeer)

		require.Empty(t, blocks, "an unplaceable active tip must not re-request the branch")
		require.Nil(t, staller)
	})
}

// TestSendGetDataBlocks_SkipsAPeerThatCannotServeBlocks pins the C++ !fClient
// gate, which this port reads through isSyncCandidate.
func TestSendGetDataBlocks_SkipsAPeerThatCannotServeBlocks(t *testing.T) {
	f := newDownloadFixture(t, 10)
	activeTip := f.node(t, 0)

	peer := NewSyncPeer("1.2.3.4:8333", 0, newPeerSyncState())
	best := f.node(t, 10)
	peer.State.pindexBestKnownBlock = &best

	require.Empty(t, f.bd.SendGetDataBlocks(peer, activeTip, testNow))
	require.Equal(t, 0, peer.State.nBlocksInFlight)
	require.Equal(t, 0, f.bd.BlocksInFlight())
}

// TestSendGetDataBlocks_NamesTheStaller pins the nodeStaller path: a peer whose
// whole window is held in flight by someone else cannot move, and the peer
// holding it starts its nStallingSince clock.
func TestSendGetDataBlocks_NamesTheStaller(t *testing.T) {
	const peerHeight = BlockDownloadWindow + 6

	f := newDownloadFixture(t, peerHeight)
	activeTip := f.node(t, 0)

	holder := f.peerAt(t, "5.6.7.8:8333", peerHeight)
	for h := 1; h <= BlockDownloadWindow; h++ {
		require.True(t, f.bd.MarkBlockAsInFlight(holder, f.node(t, h)))
	}

	waiter := f.peerAt(t, "1.2.3.4:8333", peerHeight)

	require.Empty(t, f.bd.SendGetDataBlocks(waiter, activeTip, testNow))
	require.Equal(t, testNow, holder.State.nStallingSince, "the peer blocking the window starts stalling")
	require.Equal(t, int64(0), waiter.State.nStallingSince, "the waiting peer is not the staller")

	// net_processing.cpp only starts the clock once.
	require.Empty(t, f.bd.SendGetDataBlocks(waiter, activeTip, testNow+micros(time.Minute)))
	require.Equal(t, testNow, holder.State.nStallingSince)
}

func TestCheckStall_DisconnectsAStallingPeer(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.peerAt(t, "1.2.3.4:8333", 3)

	peer.State.nStallingSince = testNow

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow+micros(BlockStallingTimeout)))
	require.Equal(t, StallActionDisconnect, f.bd.CheckStall(peer, noIngest, testNow+micros(BlockStallingTimeout)+1))
}

// TestCheckStall_DisconnectsAStallerThatHoldsNoSyncSlot pins the ORDER of the
// two clauses in CheckStall. DetectStalling runs before the fSyncStarted early
// return, so a peer a rotation stripped of the sync slot is still judged on the
// blocks it holds. Reverse the two and a rotated-but-connected peer becomes
// ungovernable: every block the scheduler re-hands it is lost until it
// disconnects on its own.
func TestCheckStall_DisconnectsAStallerThatHoldsNoSyncSlot(t *testing.T) {
	f := newDownloadFixture(t, 3)

	peer := f.peerAt(t, "1.2.3.4:8333", 3)
	require.False(t, peer.State.fSyncStarted, "this is the state a rotation leaves behind")

	peer.State.nStallingSince = testNow

	require.Equal(t, StallActionDisconnect, f.bd.CheckStall(peer, noIngest, testNow+micros(BlockStallingTimeout)+1))
}

func TestCheckStall_IgnoresAPeerThatIsNotStalling(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.peerAt(t, "1.2.3.4:8333", 3)

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow+micros(time.Hour)))
}

// TestCheckStall_RotatesTheSyncPeer pins the Teranode rotation carried from
// legacy netsync handleCheckSyncPeer: a sync peer that makes no progress for
// maxLastBlockTime loses the slot. It also covers the headers-first case, where
// no block ever arrives and the SVNode nStallingSince clock never starts.
func TestCheckStall_RotatesTheSyncPeer(t *testing.T) {
	f := newDownloadFixture(t, 3)

	peer := fullNodePeer("1.2.3.4:8333")
	require.Len(t, f.hs.PeerEstablished(peer), 1)
	require.True(t, peer.State.fSyncStarted)
	require.True(t, f.hs.IsHeadersFirstMode())

	best := f.node(t, 3)
	peer.State.pindexBestKnownBlock = &best
	peer.State.pindexLastCommonBlock = &best

	require.True(t, f.bd.MarkBlockAsInFlight(peer, best))

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow))
	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow+micros(MaxLastBlockTime)))

	require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, noIngest, testNow+micros(MaxLastBlockTime)+1))

	require.False(t, peer.State.fSyncStarted, "the sync slot must be released through HeaderSync.SyncPeerTimedOut")
	require.False(t, f.hs.IsHeadersFirstMode(), "the header state must reset with the round")
	require.False(t, f.bd.IsInFlight(best.Hash), "the rotated peer's in-flight blocks must be released")
	require.Equal(t, 0, peer.State.nBlocksInFlight)
	require.Nil(t, peer.State.pindexLastCommonBlock)
}

// syncingPeer returns a peer holding the sync slot with one block in flight,
// which is the state the rotation rules are written against.
func (f *downloadFixture) syncingPeer(t *testing.T, addr string) *SyncPeer {
	t.Helper()

	peer := fullNodePeer(addr)
	require.Len(t, f.hs.PeerEstablished(peer), 1)

	best := f.node(t, len(f.chain))
	peer.State.pindexBestKnownBlock = &best
	peer.State.pindexLastCommonBlock = &best

	require.True(t, f.bd.MarkBlockAsInFlight(peer, best))

	return peer
}

// TestCheckStall_KeepsAPeerStillDeliveringALargeBlock covers the suppression
// carried from legacy netsync manager.go:1052-1068: a block big enough to
// outlast maxLastBlockTime must not cost the peer that is still streaming it
// the sync slot. The rate floor and the wall-clock cap are both exercised.
func TestCheckStall_KeepsAPeerStillDeliveringALargeBlock(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.syncingPeer(t, "1.2.3.4:8333")

	const tick = 5 * time.Second

	// Bytes arriving comfortably above MinBlockDownloadBytesPerSec.
	perTick := uint64(MinBlockDownloadBytesPerSec) * uint64(tick/time.Second) * 2

	ingest := IngestSnapshot{Active: true, StartedMicros: testNow}

	// Seed the first sample, then run past the rotation window while the
	// ingest keeps pulling bytes.
	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, ingest, testNow))

	for elapsed := tick; elapsed <= MaxLastBlockTime+2*tick; elapsed += tick {
		ingest.BytesRead += perTick

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, ingest, testNow+micros(elapsed)),
			"a peer still delivering at %d B/s must keep the sync slot at %s", MinBlockDownloadBytesPerSec, elapsed)
	}

	require.True(t, peer.State.fSyncStarted, "the sync slot must survive a long but healthy download")
}

func TestCheckStall_RotatesAnIngestThatStoppedMoving(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.syncingPeer(t, "1.2.3.4:8333")

	// An ingest that took some bytes and then went quiet: the byte count stops
	// rising, so it is a stalled peer, not a large block.
	ingest := IngestSnapshot{Active: true, StartedMicros: testNow, BytesRead: 1 << 20}

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, ingest, testNow))
	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, ingest, testNow+micros(MaxLastBlockTime)))

	require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, ingest, testNow+micros(MaxLastBlockTime)+1),
		"an ingest whose byte count stopped rising is a stall, whatever it read earlier")
}

func TestCheckStall_RotatesADribblingIngestPastTheDownloadCap(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.syncingPeer(t, "1.2.3.4:8333")

	const tick = time.Minute

	perTick := uint64(MinBlockDownloadBytesPerSec) * uint64(tick/time.Second) * 2

	ingest := IngestSnapshot{Active: true, StartedMicros: testNow}

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, ingest, testNow))

	// Healthy throughput holds the slot right up to the cap...
	for elapsed := tick; elapsed < MaxBlockDownloadTime; elapsed += tick {
		ingest.BytesRead += perTick

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, ingest, testNow+micros(elapsed)))
	}

	// ...and stops holding it there, so a peer cannot dribble bytes for ever
	// to keep the single sync slot.
	ingest.BytesRead += perTick

	require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, ingest, testNow+micros(MaxBlockDownloadTime)),
		"MaxBlockDownloadTime caps the suppression regardless of throughput")
}

func TestCheckStall_NeverRotatesANonSyncPeer(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.peerAt(t, "1.2.3.4:8333", 3)

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow))
	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow+micros(10*MaxLastBlockTime)))
}

// TestCheckStall_ProgressKeepsTheSyncPeer covers both progress sources the
// rotation clock accepts: a delivered block, and a headers-first round that
// pushes the header index tip forward.
func TestCheckStall_ProgressKeepsTheSyncPeer(t *testing.T) {
	t.Run("a delivered block refreshes the clock", func(t *testing.T) {
		f := newDownloadFixture(t, 3)

		peer := fullNodePeer("1.2.3.4:8333")
		require.Len(t, f.hs.PeerEstablished(peer), 1)

		best := f.node(t, 3)
		peer.State.pindexBestKnownBlock = &best
		require.True(t, f.bd.MarkBlockAsInFlight(peer, best))

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow))

		delivered := testNow + micros(100*time.Second)
		require.True(t, f.bd.BlockReceived(peer, best.Hash, delivered))

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, delivered+micros(MaxLastBlockTime)))
		require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, noIngest, delivered+micros(MaxLastBlockTime)+1))
	})

	t.Run("a headers-first round advancing the index tip refreshes the clock", func(t *testing.T) {
		f := newDownloadFixture(t, 3)

		peer := fullNodePeer("1.2.3.4:8333")
		require.Len(t, f.hs.PeerEstablished(peer), 1)
		require.True(t, f.hs.IsHeadersFirstMode())

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow))

		// The round delivers headers: the index tip rises, which is the only
		// progress signal available while no block is being downloaded.
		connected, err := f.idx.AddHeader(childOf(f.chain[2], 9999))
		require.NoError(t, err)
		require.True(t, connected)

		advanced := testNow + micros(MaxLastBlockTime) - 1
		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, advanced))
		require.True(t, peer.State.fSyncStarted)

		// The clock restarted from the observation, not from testNow.
		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, advanced+micros(MaxLastBlockTime)))
		require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, noIngest, advanced+micros(MaxLastBlockTime)+1))
	})

	// The tip is chosen by cumulative work, so a heavier branch can take it
	// while standing shorter than the branch it displaces. The rotation clock
	// must read that as progress: the peer delivered the headers that moved
	// the tip, and a height watermark would have missed it and rotated a peer
	// that was doing exactly what we wanted.
	t.Run("a tip moving to a shorter heavier branch refreshes the clock", func(t *testing.T) {
		f := newDownloadFixture(t, 3)

		peer := fullNodePeer("1.2.3.4:8333")
		require.Len(t, f.hs.PeerEstablished(peer), 1)
		require.True(t, f.hs.IsHeadersFirstMode())

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow))

		// Two heavy headers off genesis outweigh the fixture's three light
		// ones (4_295_032_833 + 2 * 8_589_934_591 = 21_474_902_015 against
		// 4 * 4_295_032_833 = 17_180_131_332) one block shorter.
		heavy := buildChainBits(t, f.idx, &nonceCounter{next: 7000}, f.genesis, 2, heavyBits)

		tipHash, tipHeight := f.idx.Tip()
		require.Equal(t, heavy[len(heavy)-1].BlockHash(), tipHash)
		require.Equal(t, int32(2), tipHeight, "the new tip stands below the branch it displaced")

		advanced := testNow + micros(MaxLastBlockTime) - 1
		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, advanced))
		require.True(t, peer.State.fSyncStarted)

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, advanced+micros(MaxLastBlockTime)))
		require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, noIngest, advanced+micros(MaxLastBlockTime)+1))
	})

	// Header progress may only stand in for block progress when there is no
	// block download to judge instead. A peer that trickles headers while
	// sitting on everything we asked it for is withholding blocks, and must
	// still be rotated.
	t.Run("header progress does not excuse a peer withholding blocks", func(t *testing.T) {
		f := newDownloadFixture(t, MaxBlocksInTransitPerPeer+4)

		peer := fullNodePeer("1.2.3.4:8333")
		require.Len(t, f.hs.PeerEstablished(peer), 1)
		require.True(t, f.hs.IsHeadersFirstMode())

		best := f.node(t, MaxBlocksInTransitPerPeer+4)
		peer.State.pindexBestKnownBlock = &best

		msgs := f.bd.SendGetDataBlocks(peer, f.node(t, 0), testNow)
		require.Len(t, msgs, 1)
		require.Equal(t, MaxBlocksInTransitPerPeer, peer.State.nBlocksInFlight)

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow))

		// The peer keeps the header round moving but delivers no block.
		prev := f.chain[len(f.chain)-1]
		for i := uint32(0); i < 3; i++ {
			header := childOf(prev, 5000+i)

			connected, err := f.idx.AddHeader(header)
			require.NoError(t, err)
			require.True(t, connected)

			prev = header

			require.Equal(t, StallActionNone, f.bd.CheckStall(peer, noIngest, testNow+micros(time.Duration(i+1)*time.Second)))
		}

		require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, noIngest, testNow+micros(MaxLastBlockTime)+1),
			"headers must not hold the sync slot while the peer sits on in-flight blocks")
		require.False(t, peer.State.fSyncStarted)
	})
}

func TestOnInv_UnknownBlockAsksForHeaders(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := fullNodePeer("1.2.3.4:8333")

	unknown := chainhash.Hash{0xAB}

	msgs, err := f.bd.OnInv(peer, invMsg(t, wire.InvTypeBlock, unknown))
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	getHeaders, ok := msgs[0].(*wire.MsgGetHeaders)
	require.True(t, ok)
	require.Equal(t, unknown, getHeaders.HashStop)

	locator := f.idx.Locator()
	require.Len(t, getHeaders.BlockLocatorHashes, len(locator))

	for i := range locator {
		require.Equal(t, locator[i], *getHeaders.BlockLocatorHashes[i])
	}

	require.Equal(t, unknown, peer.State.hashLastUnknownBlock)
	require.Nil(t, peer.State.pindexBestKnownBlock)
}

func TestOnInv_KnownBlockOnlyUpdatesAvailability(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := fullNodePeer("1.2.3.4:8333")

	known := f.node(t, 3)

	msgs, err := f.bd.OnInv(peer, invMsg(t, wire.InvTypeBlock, known.Hash))
	require.NoError(t, err)
	require.Empty(t, msgs, "a block we already have a header for needs no getheaders")

	require.NotNil(t, peer.State.pindexBestKnownBlock)
	require.Equal(t, int32(3), peer.State.pindexBestKnownBlock.Height)
	require.Equal(t, known.Hash, peer.State.pindexBestKnownBlock.Hash)
}

func TestOnInv_DoesNotRequestTheSameBlockTwice(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := fullNodePeer("1.2.3.4:8333")

	unknown := chainhash.Hash{0xAB}

	msgs, err := f.bd.OnInv(peer, invMsg(t, wire.InvTypeBlock, unknown, unknown, unknown))
	require.NoError(t, err)
	require.Len(t, msgs, 1, "a hash repeated inside one inv message draws one getheaders")
}

func TestOnInv_TxInvsAreCountedOnly(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := fullNodePeer("1.2.3.4:8333")

	msgs, err := f.bd.OnInv(peer, invMsg(t, wire.InvTypeTx, chainhash.Hash{0x01}, chainhash.Hash{0x02}))
	require.NoError(t, err)
	require.Empty(t, msgs, "the tx path is Phase 3")
	require.Equal(t, uint64(2), f.bd.TxInvsReceived())

	require.Equal(t, chainhash.Hash{}, peer.State.hashLastUnknownBlock, "a tx inv must not touch block availability")
	require.Nil(t, peer.State.pindexBestKnownBlock)
}

func TestOnInv_UnsupportedInvTypeIsAProtocolViolation(t *testing.T) {
	tests := []struct {
		name string
		typ  wire.InvType
	}{
		{name: "error type", typ: wire.InvTypeError},
		{name: "filtered block", typ: wire.InvTypeFilteredBlock},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newDownloadFixture(t, 3)
			peer := fullNodePeer("1.2.3.4:8333")

			msgs, err := f.bd.OnInv(peer, invMsg(t, tc.typ, chainhash.Hash{0x01}))
			require.Error(t, err)
			require.ErrorIs(t, err, ErrProtocolViolation)
			require.Empty(t, msgs)
		})
	}
}

func TestMarkBlockAsInFlight_IsIdempotent(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.peerAt(t, "1.2.3.4:8333", 3)
	other := f.peerAt(t, "5.6.7.8:8333", 3)

	block := f.node(t, 3)

	require.True(t, f.bd.MarkBlockAsInFlight(peer, block))
	require.Equal(t, 1, peer.State.nBlocksInFlight)

	require.False(t, f.bd.MarkBlockAsInFlight(peer, block), "the same block from the same peer is a no-op")
	require.Equal(t, 1, peer.State.nBlocksInFlight)

	require.False(t, f.bd.MarkBlockAsInFlight(other, block), "Phase 2 fetches each block from one peer only")
	require.Equal(t, 0, other.State.nBlocksInFlight)
}

func TestBlockReceived_ClearsInFlightAndTheStallClock(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.peerAt(t, "1.2.3.4:8333", 3)

	block := f.node(t, 3)
	require.True(t, f.bd.MarkBlockAsInFlight(peer, block))

	peer.State.nStallingSince = testNow

	require.True(t, f.bd.BlockReceived(peer, block.Hash, testNow))
	require.Equal(t, 0, peer.State.nBlocksInFlight)
	require.Equal(t, int64(0), peer.State.nStallingSince)
	require.False(t, f.bd.IsInFlight(block.Hash))

	require.False(t, f.bd.BlockReceived(peer, block.Hash, testNow), "a second receipt is a no-op")
	require.Equal(t, 0, peer.State.nBlocksInFlight)
}

func TestBlockFailed_ReleasesTheBlockForAnotherPeer(t *testing.T) {
	f := newDownloadFixture(t, 10)
	activeTip := f.node(t, 4)

	holder := f.peerAt(t, "5.6.7.8:8333", 10)
	block := f.node(t, 5)
	require.True(t, f.bd.MarkBlockAsInFlight(holder, block))

	require.True(t, f.bd.BlockFailed(holder, block.Hash))
	require.False(t, f.bd.IsInFlight(block.Hash))
	require.Equal(t, 0, holder.State.nBlocksInFlight)

	peer := f.peerAt(t, "1.2.3.4:8333", 10)
	blocks, _ := f.bd.FindNextBlocksToDownload(peer, activeTip, MaxBlocksInTransitPerPeer)

	require.NotEmpty(t, blocks)
	require.Equal(t, block.Hash, blocks[0].Hash, "a failed download must be offered again")
}

func TestPeerDisconnected_ReleasesEverythingThePeerHeld(t *testing.T) {
	f := newDownloadFixture(t, 10)
	activeTip := f.node(t, 4)

	peer := f.peerAt(t, "1.2.3.4:8333", 10)
	other := f.peerAt(t, "5.6.7.8:8333", 10)

	require.True(t, f.bd.MarkBlockAsInFlight(peer, f.node(t, 5)))
	require.True(t, f.bd.MarkBlockAsInFlight(peer, f.node(t, 6)))
	require.True(t, f.bd.MarkBlockAsInFlight(other, f.node(t, 7)))

	peer.State.nStallingSince = testNow
	peer.State.pindexLastCommonBlock = &activeTip

	f.bd.PeerDisconnected(peer)

	require.False(t, f.bd.IsInFlight(f.node(t, 5).Hash))
	require.False(t, f.bd.IsInFlight(f.node(t, 6).Hash))
	require.True(t, f.bd.IsInFlight(f.node(t, 7).Hash), "another peer's downloads must survive")

	require.Equal(t, 0, peer.State.nBlocksInFlight)
	require.Equal(t, int64(0), peer.State.nStallingSince)
	require.Nil(t, peer.State.pindexLastCommonBlock)
	require.Equal(t, 1, other.State.nBlocksInFlight)
	require.Equal(t, 1, f.bd.BlocksInFlight())
}

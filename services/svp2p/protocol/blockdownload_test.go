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

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, testNow+micros(BlockStallingTimeout)))
	require.Equal(t, StallActionDisconnect, f.bd.CheckStall(peer, testNow+micros(BlockStallingTimeout)+1))
}

func TestCheckStall_IgnoresAPeerThatIsNotStalling(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.peerAt(t, "1.2.3.4:8333", 3)

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, testNow+micros(time.Hour)))
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

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, testNow))
	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, testNow+micros(MaxLastBlockTime)))

	require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, testNow+micros(MaxLastBlockTime)+1))

	require.False(t, peer.State.fSyncStarted, "the sync slot must be released through HeaderSync.SyncPeerTimedOut")
	require.False(t, f.hs.IsHeadersFirstMode(), "the header state must reset with the round")
	require.False(t, f.bd.IsInFlight(best.Hash), "the rotated peer's in-flight blocks must be released")
	require.Equal(t, 0, peer.State.nBlocksInFlight)
	require.Nil(t, peer.State.pindexLastCommonBlock)
}

func TestCheckStall_NeverRotatesANonSyncPeer(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.peerAt(t, "1.2.3.4:8333", 3)

	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, testNow))
	require.Equal(t, StallActionNone, f.bd.CheckStall(peer, testNow+micros(10*MaxLastBlockTime)))
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

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, testNow))

		delivered := testNow + micros(100*time.Second)
		require.True(t, f.bd.BlockReceived(peer, best.Hash, delivered))

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, delivered+micros(MaxLastBlockTime)))
		require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, delivered+micros(MaxLastBlockTime)+1))
	})

	t.Run("a headers-first round advancing the index tip refreshes the clock", func(t *testing.T) {
		f := newDownloadFixture(t, 3)

		peer := fullNodePeer("1.2.3.4:8333")
		require.Len(t, f.hs.PeerEstablished(peer), 1)
		require.True(t, f.hs.IsHeadersFirstMode())

		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, testNow))

		// The round delivers headers: the index tip rises, which is the only
		// progress signal available while no block is being downloaded.
		connected, err := f.idx.AddHeader(childOf(f.chain[2], 9999))
		require.NoError(t, err)
		require.True(t, connected)

		advanced := testNow + micros(MaxLastBlockTime) - 1
		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, advanced))
		require.True(t, peer.State.fSyncStarted)

		// The clock restarted from the observation, not from testNow.
		require.Equal(t, StallActionNone, f.bd.CheckStall(peer, advanced+micros(MaxLastBlockTime)))
		require.Equal(t, StallActionRotateSyncPeer, f.bd.CheckStall(peer, advanced+micros(MaxLastBlockTime)+1))
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

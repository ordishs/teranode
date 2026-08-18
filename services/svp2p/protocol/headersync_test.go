package protocol

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// testEasyBits encodes a target of 0x7fffff * 2^232 (just under 2^255), the
// regtest-style "anything hashes" difficulty. Grinding a header nonce against
// it takes a couple of tries.
const testEasyBits uint32 = 0x207fffff

// testHardBits is the mainnet genesis difficulty (target 0xffff * 2^208). No
// header this test file grinds will ever meet it, which is how the PoW-failure
// rows are built.
const testHardBits uint32 = 0x1d00ffff

// testTarget decodes nBits the same way SVNode's arith_uint256::SetCompact
// does, written out here rather than reusing the production helper so the
// tests pin the rule and not the implementation.
func testTarget(bits uint32) *big.Int {
	exponent := bits >> 24
	mantissa := int64(bits & 0x007fffff)

	if exponent <= 3 {
		return big.NewInt(mantissa >> (8 * (3 - exponent)))
	}

	return new(big.Int).Lsh(big.NewInt(mantissa), uint(8*(exponent-3)))
}

// testMeetsTarget reports whether the header hash, read as a big-endian
// number, is at or below the target its own nBits claims.
func testMeetsTarget(h *wire.BlockHeader) bool {
	hash := h.BlockHash()

	be := make([]byte, len(hash))
	for i := range hash {
		be[len(hash)-1-i] = hash[i]
	}

	return new(big.Int).SetBytes(be).Cmp(testTarget(h.Bits)) <= 0
}

// testPowLimit is the highest target the test chain params allow: 2^255 - 1,
// the regtest powLimit. It is above testEasyBits' target, so a ground header
// passes the range check.
func testPowLimit() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1))
}

// syncTestParams returns mainnet params with the regtest powLimit and the
// given checkpoints, so tests can mine headers cheaply while still exercising
// the non-regtest sync-candidate branch.
func syncTestParams(checkpoints []chaincfg.Checkpoint) *chaincfg.Params {
	params := chaincfg.MainNetParams
	params.PowLimit = testPowLimit()
	params.Checkpoints = checkpoints

	return &params
}

func syncGenesis() *wire.BlockHeader {
	zero := chainhash.Hash{}
	h := wire.NewBlockHeader(1, &zero, &zero, testEasyBits, 0)

	for !testMeetsTarget(h) {
		h.Nonce++
	}

	return h
}

// minedChild grinds a child of parent until it meets its own claimed target.
// salt keeps siblings of one parent distinct.
func minedChild(parent *wire.BlockHeader, bits, salt uint32) *wire.BlockHeader {
	prevHash := parent.BlockHash()
	merkle := chainhash.Hash{}
	merkle[0] = byte(salt)
	merkle[1] = byte(salt >> 8)

	h := wire.NewBlockHeader(1, &prevHash, &merkle, bits, 0)

	for !testMeetsTarget(h) {
		h.Nonce++
	}

	return h
}

// failingChild grinds a child of parent until its hash does NOT meet the
// target its nBits claims, giving a deterministic "high-hash" header.
func failingChild(parent *wire.BlockHeader, salt uint32) *wire.BlockHeader {
	prevHash := parent.BlockHash()
	merkle := chainhash.Hash{}
	merkle[0] = byte(salt)

	h := wire.NewBlockHeader(1, &prevHash, &merkle, testHardBits, 0)

	for testMeetsTarget(h) {
		h.Nonce++
	}

	return h
}

// minedRun returns count headers extending from, each with valid PoW, without
// adding them to any index.
func minedRun(from *wire.BlockHeader, count int, salt uint32) []*wire.BlockHeader {
	run := make([]*wire.BlockHeader, 0, count)
	prev := from

	for i := 0; i < count; i++ {
		h := minedChild(prev, testEasyBits, salt+uint32(i)) //nolint:gosec // test-only salt
		run = append(run, h)
		prev = h
	}

	return run
}

// syncFixture is one genesis-rooted index plus the headers already in it.
type syncFixture struct {
	genesis *wire.BlockHeader
	idx     *HeaderIndex
	chain   []*wire.BlockHeader // chain[i] has height i+1
}

// newSyncFixture builds an index whose tip is at localHeight.
func newSyncFixture(t *testing.T, localHeight int) *syncFixture {
	t.Helper()

	genesis := syncGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	chain := minedRun(genesis, localHeight, 1)
	for _, h := range chain {
		connected, addErr := idx.AddHeader(h)
		require.NoError(t, addErr)
		require.True(t, connected)
	}

	return &syncFixture{genesis: genesis, idx: idx, chain: chain}
}

func (f *syncFixture) tip() *wire.BlockHeader {
	if len(f.chain) == 0 {
		return f.genesis
	}

	return f.chain[len(f.chain)-1]
}

func fullNodePeer(addr string) *SyncPeer {
	return NewSyncPeer(addr, wire.SFNodeNetwork, newPeerSyncState())
}

// requireGetHeaders unwraps the single expected getheaders message.
func requireGetHeaders(t *testing.T, msgs []wire.Message) *wire.MsgGetHeaders {
	t.Helper()

	require.Len(t, msgs, 1)

	gh, ok := msgs[0].(*wire.MsgGetHeaders)
	require.True(t, ok, "expected a getheaders message, got %T", msgs[0])

	return gh
}

func locatorValues(gh *wire.MsgGetHeaders) []chainhash.Hash {
	out := make([]chainhash.Hash, 0, len(gh.BlockLocatorHashes))
	for _, h := range gh.BlockLocatorHashes {
		out = append(out, *h)
	}

	return out
}

// TestHeaderSync_InitialGetHeaders pins the initial getheaders that
// net_processing.cpp SendMessages sends when a sync candidate completes the
// handshake, with the legacy checkpoint hashStop layered on top.
func TestHeaderSync_InitialGetHeaders(t *testing.T) {
	t.Run("fresh peer below a checkpoint gets the pprev locator and the checkpoint hashStop", func(t *testing.T) {
		f := newSyncFixture(t, 5)

		// The checkpoint sits above our tip, so headers-first mode applies.
		cpHash := chainhash.Hash{0xC0}
		hs, err := NewHeaderSync(HeaderSyncConfig{
			Index:  f.idx,
			Params: syncTestParams([]chaincfg.Checkpoint{{Height: 1000, Hash: &cpHash}}),
		})
		require.NoError(t, err)

		peer := fullNodePeer("1.2.3.4:8333")

		gh := requireGetHeaders(t, hs.PeerEstablished(peer))

		// net_processing.cpp SendMessages: "If possible, start at the block
		// preceding the currently best known header" — the locator is built
		// from our tip's parent, not the tip.
		parent := f.chain[len(f.chain)-2].BlockHash()
		require.Equal(t, f.idx.LocatorFrom(parent), locatorValues(gh))

		// Legacy manager.go startSync: while headers-first, getheaders carries
		// the next checkpoint hash as hashStop.
		require.Equal(t, cpHash, gh.HashStop)
		require.True(t, hs.IsHeadersFirstMode())
		require.True(t, peer.State.fSyncStarted)
	})

	t.Run("tip past the final checkpoint gets a zero hashStop and no headers-first mode", func(t *testing.T) {
		f := newSyncFixture(t, 5)

		cpHash := f.chain[1].BlockHash() // height 2, below our tip
		hs, err := NewHeaderSync(HeaderSyncConfig{
			Index:  f.idx,
			Params: syncTestParams([]chaincfg.Checkpoint{{Height: 2, Hash: &cpHash}}),
		})
		require.NoError(t, err)

		gh := requireGetHeaders(t, hs.PeerEstablished(fullNodePeer("1.2.3.4:8333")))

		require.Equal(t, chainhash.Hash{}, gh.HashStop)
		require.False(t, hs.IsHeadersFirstMode())
	})

	t.Run("genesis tip has no parent so the locator is the tip locator", func(t *testing.T) {
		f := newSyncFixture(t, 0)

		hs, err := NewHeaderSync(HeaderSyncConfig{Index: f.idx, Params: syncTestParams(nil)})
		require.NoError(t, err)

		gh := requireGetHeaders(t, hs.PeerEstablished(fullNodePeer("1.2.3.4:8333")))

		require.Equal(t, []chainhash.Hash{f.genesis.BlockHash()}, locatorValues(gh))
	})

	t.Run("only one peer starts header sync at a time", func(t *testing.T) {
		f := newSyncFixture(t, 3)

		hs, err := NewHeaderSync(HeaderSyncConfig{Index: f.idx, Params: syncTestParams(nil)})
		require.NoError(t, err)

		first := fullNodePeer("1.2.3.4:8333")
		second := fullNodePeer("5.6.7.8:8333")

		require.Len(t, hs.PeerEstablished(first), 1)

		// net_processing.cpp SendMessages: nSyncStarted == 0 gates the initial
		// getheaders, so the second peer is not asked.
		require.Nil(t, hs.PeerEstablished(second))
		require.False(t, second.State.fSyncStarted)

		// A repeat event for the same peer must not re-ask either
		// (state.fSyncStarted).
		require.Nil(t, hs.PeerEstablished(first))

		// FinalizeNode: losing the sync peer frees the slot.
		hs.PeerDisconnected(first)
		require.False(t, first.State.fSyncStarted)

		require.Len(t, hs.PeerEstablished(second), 1)
		require.True(t, second.State.fSyncStarted)
	})

	t.Run("sync candidate rules", func(t *testing.T) {
		regtestParams := func() *chaincfg.Params {
			params := chaincfg.RegressionNetParams
			params.PowLimit = testPowLimit()
			params.Checkpoints = nil

			return &params
		}

		tests := []struct {
			name          string
			params        *chaincfg.Params
			allowNonLocal bool
			addr          string
			services      wire.ServiceFlag
			wantCandidate bool
		}{
			// legacy manager.go isSyncCandidate: outside regtest the peer must
			// serve SFNodeNetwork.
			{"full node is a candidate", syncTestParams(nil), false, "1.2.3.4:8333", wire.SFNodeNetwork, true},
			{"non-full node is not a candidate", syncTestParams(nil), false, "1.2.3.4:8333", 0, false},
			// legacy manager.go isSyncCandidate: in regtest the service flag is
			// not required, but the peer must be local unless
			// legacy_allowSyncCandidateFromLocalPeers is set.
			{"regtest localhost candidate without the service flag", regtestParams(), false, "127.0.0.1:18444", 0, true},
			{"regtest remote peer rejected", regtestParams(), false, "10.0.0.9:18444", wire.SFNodeNetwork, false},
			{"regtest remote peer allowed by setting", regtestParams(), true, "10.0.0.9:18444", 0, true},
			{"regtest unparsable address rejected", regtestParams(), false, "not-an-address", wire.SFNodeNetwork, false},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				f := newSyncFixture(t, 2)

				hs, err := NewHeaderSync(HeaderSyncConfig{
					Index:                            f.idx,
					Params:                           tc.params,
					AllowSyncCandidateFromLocalPeers: tc.allowNonLocal,
				})
				require.NoError(t, err)

				peer := NewSyncPeer(tc.addr, tc.services, newPeerSyncState())
				msgs := hs.PeerEstablished(peer)

				if tc.wantCandidate {
					require.Len(t, msgs, 1)
					require.True(t, peer.State.fSyncStarted)
				} else {
					require.Nil(t, msgs)
					require.False(t, peer.State.fSyncStarted)
				}
			})
		}
	})

	t.Run("regtest never enters headers-first mode", func(t *testing.T) {
		f := newSyncFixture(t, 2)

		params := chaincfg.RegressionNetParams
		params.PowLimit = testPowLimit()

		cpHash := chainhash.Hash{0xC0}
		params.Checkpoints = []chaincfg.Checkpoint{{Height: 1000, Hash: &cpHash}}

		hs, err := NewHeaderSync(HeaderSyncConfig{Index: f.idx, Params: &params})
		require.NoError(t, err)

		gh := requireGetHeaders(t, hs.PeerEstablished(NewSyncPeer("127.0.0.1:18444", 0, newPeerSyncState())))

		// legacy manager.go startSync: "regression test mode does not support
		// the headers-first approach".
		require.False(t, hs.IsHeadersFirstMode())
		require.Equal(t, chainhash.Hash{}, gh.HashStop)
	})

	t.Run("disabled checkpoints leave headers-first mode off", func(t *testing.T) {
		f := newSyncFixture(t, 2)

		cpHash := chainhash.Hash{0xC0}
		hs, err := NewHeaderSync(HeaderSyncConfig{
			Index:              f.idx,
			Params:             syncTestParams([]chaincfg.Checkpoint{{Height: 1000, Hash: &cpHash}}),
			DisableCheckpoints: true,
		})
		require.NoError(t, err)

		gh := requireGetHeaders(t, hs.PeerEstablished(fullNodePeer("1.2.3.4:8333")))

		require.False(t, hs.IsHeadersFirstMode())
		require.Equal(t, chainhash.Hash{}, gh.HashStop)
	})
}

// startedSync returns a machine that has already sent its initial getheaders
// to peer, so OnHeaders rows start from the state a real batch arrives in.
func startedSync(t *testing.T, f *syncFixture, checkpoints []chaincfg.Checkpoint) (*HeaderSync, *SyncPeer) {
	t.Helper()

	hs, err := NewHeaderSync(HeaderSyncConfig{Index: f.idx, Params: syncTestParams(checkpoints)})
	require.NoError(t, err)

	peer := fullNodePeer("1.2.3.4:8333")
	require.Len(t, hs.PeerEstablished(peer), 1)

	return hs, peer
}

func headersMsg(headers []*wire.BlockHeader) *wire.MsgHeaders {
	return &wire.MsgHeaders{Headers: headers}
}

// TestHeaderSync_OnHeadersBatching pins the net_processing.cpp HEADERS rules:
// the MAX_HEADERS_RESULTS cap, the continuation getheaders after a full batch,
// and the silence after a short one.
func TestHeaderSync_OnHeadersBatching(t *testing.T) {
	t.Run("a full batch triggers a continuation getheaders from the new tip locator", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		cpHash := chainhash.Hash{0xC0}
		hs, peer := startedSync(t, f, []chaincfg.Checkpoint{{Height: 100000, Hash: &cpHash}})

		batch := minedRun(f.tip(), MaxHeadersResults, 100)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, misbehavior)

		gh := requireGetHeaders(t, msgs)

		last := batch[len(batch)-1].BlockHash()
		require.Equal(t, f.idx.LocatorFrom(last), locatorValues(gh))
		require.Equal(t, cpHash, gh.HashStop)

		// Every header in the batch must have landed in the index.
		tipHash, tipHeight := f.idx.Tip()
		require.Equal(t, last, tipHash)
		require.Equal(t, int32(1+MaxHeadersResults), tipHeight)

		// net_processing.cpp HEADERS: UpdateBlockAvailability with the last
		// header of the batch.
		require.NotNil(t, peer.State.pindexBestKnownBlock)
		require.Equal(t, last, peer.State.pindexBestKnownBlock.Hash)
	})

	t.Run("a short batch inside the headers-first round still asks for the next one", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		cpHash := chainhash.Hash{0xC0}
		hs, peer := startedSync(t, f, []chaincfg.Checkpoint{{Height: 100000, Hash: &cpHash}})

		batch := minedRun(f.tip(), 3, 200)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, misbehavior)

		// legacy manager.go handleHeadersMsg re-requests unconditionally while
		// the checkpoint is not reached — no batch-length gate. Without the
		// request the round would stall with nothing outstanding.
		gh := requireGetHeaders(t, msgs)
		require.Equal(t, f.idx.LocatorFrom(batch[len(batch)-1].BlockHash()), locatorValues(gh))
		require.Equal(t, cpHash, gh.HashStop)
		require.True(t, hs.IsHeadersFirstMode())
	})

	t.Run("a repeated batch of known headers ends the round instead of looping", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		cpHash := chainhash.Hash{0xC0}
		hs, peer := startedSync(t, f, []chaincfg.Checkpoint{{Height: 100000, Hash: &cpHash}})

		batch := minedRun(f.tip(), 3, 220)

		// The first delivery is real progress and earns the next request.
		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, misbehavior)
		require.Len(t, msgs, 1)

		// Replaying it changes nothing: AddHeader reports headers we already
		// hold as connected, so without the anchor the machine would answer
		// with the same getheaders for ever. legacy manager.go catches the
		// replay on its header-list anchor and disconnects the peer.
		msgs, misbehavior, err = hs.OnHeaders(peer, headersMsg(batch))
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrHeadersNoProgress))
		require.False(t, errors.Is(err, ErrCheckpointMismatch))
		require.Nil(t, msgs)
		require.Zero(t, misbehavior)

		// The message must name the height it stalled at and the height the
		// request went out from.
		require.Contains(t, err.Error(), "height 4")
	})

	t.Run("a peer at our own height makes the only progress it can", func(t *testing.T) {
		f := newSyncFixture(t, 3)

		cpHash := chainhash.Hash{0xC0}
		hs, peer := startedSync(t, f, []chaincfg.Checkpoint{{Height: 100000, Hash: &cpHash}})

		// The initial getheaders is built from our tip's parent, so a peer at
		// our height answers with exactly one header: our own tip. That is not
		// a replay and must not end the round.
		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg([]*wire.BlockHeader{f.tip()}))
		require.NoError(t, err)
		require.Zero(t, misbehavior)
		require.Len(t, msgs, 1)

		// The follow-up request runs from our tip, so the peer now has nothing
		// to send and the exchange ends on the empty batch.
		msgs, misbehavior, err = hs.OnHeaders(peer, headersMsg(nil))
		require.NoError(t, err)
		require.Zero(t, misbehavior)
		require.Nil(t, msgs)
	})

	t.Run("a short batch outside the headers-first round asks for nothing more", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		// No checkpoints, so the machine never enters headers-first mode and
		// only the net_processing.cpp full-batch rule applies.
		hs, peer := startedSync(t, f, nil)
		require.False(t, hs.IsHeadersFirstMode())

		batch := minedRun(f.tip(), 3, 210)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, misbehavior)
		require.Nil(t, msgs)
	})

	t.Run("an empty batch is ignored", func(t *testing.T) {
		f := newSyncFixture(t, 1)
		hs, peer := startedSync(t, f, nil)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(nil))
		require.NoError(t, err)
		require.Zero(t, misbehavior)
		require.Nil(t, msgs)
	})

	t.Run("more than MAX_HEADERS_RESULTS scores too-many-headers", func(t *testing.T) {
		f := newSyncFixture(t, 1)
		hs, peer := startedSync(t, f, nil)

		batch := minedRun(f.tip(), MaxHeadersResults+1, 300)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))
		require.NoError(t, err)
		require.Nil(t, msgs)

		// net_processing.cpp HEADERS: Misbehaving(pfrom, 20, "too-many-headers").
		// The wire decoder refuses more than MaxBlockHeadersPerMsg headers, so
		// a real peer cannot reach this rule over the network; the batch here
		// is built in-process. The rule is kept because it belongs with the
		// rest of the HEADERS handling, not because the decoder might miss it.
		require.Equal(t, 20, misbehavior)

		// Nothing from an over-long batch may reach the index.
		_, tipHeight := f.idx.Tip()
		require.Equal(t, int32(1), tipHeight)
	})
}

// TestHeaderSync_SyncSlotRelease pins FinalizeNode plus resetHeaderState: the
// sync slot and the header state must both come back when the sync peer goes
// away or stops answering.
func TestHeaderSync_SyncSlotRelease(t *testing.T) {
	release := []struct {
		name string
		call func(hs *HeaderSync, peer *SyncPeer)
	}{
		{"disconnect", func(hs *HeaderSync, peer *SyncPeer) { hs.PeerDisconnected(peer) }},
		{"timeout", func(hs *HeaderSync, peer *SyncPeer) { hs.SyncPeerTimedOut(peer) }},
	}

	for _, tc := range release {
		t.Run(tc.name+" resets header state and frees the slot", func(t *testing.T) {
			f := newSyncFixture(t, 1)

			cpHash := chainhash.Hash{0xC0}
			hs, peer := startedSync(t, f, []chaincfg.Checkpoint{{Height: 100000, Hash: &cpHash}})
			require.True(t, hs.IsHeadersFirstMode())

			tc.call(hs, peer)

			// legacy manager.go resetHeaderState: the mode goes off with the
			// peer that was driving it, so IsHeadersFirstMode cannot stay true
			// with nobody syncing.
			require.False(t, hs.IsHeadersFirstMode())
			require.False(t, peer.State.fSyncStarted)

			// The freed slot must be usable by the next candidate, and the
			// checkpoint re-seeds for the fresh round.
			next := fullNodePeer("5.6.7.8:8333")
			gh := requireGetHeaders(t, hs.PeerEstablished(next))
			require.Equal(t, cpHash, gh.HashStop)
			require.True(t, hs.IsHeadersFirstMode())
		})
	}

	t.Run("releasing a peer that never held the slot changes nothing", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		cpHash := chainhash.Hash{0xC0}
		hs, holder := startedSync(t, f, []chaincfg.Checkpoint{{Height: 100000, Hash: &cpHash}})

		bystander := fullNodePeer("9.9.9.9:8333")
		hs.PeerDisconnected(bystander)
		hs.SyncPeerTimedOut(bystander)

		require.True(t, hs.IsHeadersFirstMode())
		require.True(t, holder.State.fSyncStarted)

		// The slot is still taken, so a new candidate is not asked.
		require.Nil(t, hs.PeerEstablished(fullNodePeer("5.6.7.8:8333")))
	})
}

// TestHeaderSync_OnHeadersMisbehavior pins the misbehavior scores
// net_processing.cpp assigns to malformed header batches.
func TestHeaderSync_OnHeadersMisbehavior(t *testing.T) {
	t.Run("a non-continuous batch scores 20 and is dropped whole", func(t *testing.T) {
		f := newSyncFixture(t, 2)
		hs, peer := startedSync(t, f, nil)

		good := minedRun(f.tip(), 1, 400)
		// A header that connects to our tip instead of to good[0]: the batch
		// is not a continuous sequence.
		stray := minedChild(f.tip(), testEasyBits, 999)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg([]*wire.BlockHeader{good[0], stray}))
		require.NoError(t, err)
		require.Nil(t, msgs)

		// net_processing.cpp HEADERS: Misbehaving(pfrom, 20, "disconnected
		// headers") → error("non-continuous headers sequence").
		require.Equal(t, 20, misbehavior)

		// The scan runs before any header is accepted, so even the valid
		// leading header must not be in the index.
		_, ok := f.idx.Lookup(good[0].BlockHash())
		require.False(t, ok)
	})

	t.Run("an unconnecting batch re-asks for headers and scores every MAX_UNCONNECTING_HEADERS", func(t *testing.T) {
		f := newSyncFixture(t, 2)
		hs, peer := startedSync(t, f, nil)

		// A chain rooted at a header we have never seen.
		orphanRoot := syncGenesis()
		orphanRoot.Nonce += 7

		for !testMeetsTarget(orphanRoot) {
			orphanRoot.Nonce++
		}

		orphans := minedRun(orphanRoot, 2, 500)

		for i := 1; i <= MaxUnconnectingHeaders; i++ {
			msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(orphans))
			require.NoError(t, err)

			// net_processing.cpp HEADERS: on a missing prev block, send
			// getheaders from our best header and count the event.
			gh := requireGetHeaders(t, msgs)
			require.Equal(t, f.idx.Locator(), locatorValues(gh))

			if i == MaxUnconnectingHeaders {
				// Misbehaving(pfrom, 20, "too-many-unconnected-headers").
				require.Equal(t, 20, misbehavior)
			} else {
				require.Zero(t, misbehavior)
			}
		}

		_, tipHeight := f.idx.Tip()
		require.Equal(t, int32(2), tipHeight)

		// net_processing.cpp HEADERS: UpdateBlockAvailability runs on this path
		// too, with the last header of the batch. It is unknown to us, so it
		// lands in hashLastUnknownBlock.
		require.Equal(t, orphans[len(orphans)-1].BlockHash(), peer.State.hashLastUnknownBlock)
	})

	t.Run("a bulk unconnecting batch scores prev-blk-not-found instead of gap-filling", func(t *testing.T) {
		f := newSyncFixture(t, 2)
		hs, peer := startedSync(t, f, nil)

		orphanRoot := syncGenesis()
		orphanRoot.Nonce += 11

		for !testMeetsTarget(orphanRoot) {
			orphanRoot.Nonce++
		}

		// MAX_BLOCKS_TO_ANNOUNCE headers is one too many for the announcement
		// path, so net_processing.cpp lets AcceptBlockHeader fail it with
		// DoS(10, "prev-blk-not-found") instead of asking for the gap.
		orphans := minedRun(orphanRoot, MaxBlocksToAnnounce, 550)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(orphans))
		require.NoError(t, err)
		require.Nil(t, msgs)
		require.Equal(t, 10, misbehavior)

		// The unconnecting counter belongs to the announcement path only.
		require.Zero(t, peer.nUnconnectingHeaders)

		_, tipHeight := f.idx.Tip()
		require.Equal(t, int32(2), tipHeight)
	})

	t.Run("headers that connect reset the unconnecting counter", func(t *testing.T) {
		f := newSyncFixture(t, 2)
		hs, peer := startedSync(t, f, nil)

		orphanRoot := syncGenesis()
		orphanRoot.Nonce += 13

		for !testMeetsTarget(orphanRoot) {
			orphanRoot.Nonce++
		}

		orphans := minedRun(orphanRoot, 2, 600)

		// One short of the score threshold.
		for i := 1; i < MaxUnconnectingHeaders; i++ {
			_, misbehavior, err := hs.OnHeaders(peer, headersMsg(orphans))
			require.NoError(t, err)
			require.Zero(t, misbehavior)
		}

		// net_processing.cpp HEADERS: "resetting nUnconnectingHeaders (%d ->
		// 0)" once a batch connects.
		good := minedRun(f.tip(), 1, 650)

		_, misbehavior, err := hs.OnHeaders(peer, headersMsg(good))
		require.NoError(t, err)
		require.Zero(t, misbehavior)
		require.Zero(t, peer.nUnconnectingHeaders)

		// The next unconnecting batch is the first of a new run, not the tenth
		// of the old one, so it must not score.
		_, misbehavior, err = hs.OnHeaders(peer, headersMsg(orphans))
		require.NoError(t, err)
		require.Zero(t, misbehavior)
	})

	t.Run("a header failing proof of work scores 50 and is not accepted", func(t *testing.T) {
		f := newSyncFixture(t, 2)
		hs, peer := startedSync(t, f, nil)

		bad := failingChild(f.tip(), 600)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg([]*wire.BlockHeader{bad}))
		require.NoError(t, err)
		require.Nil(t, msgs)

		// validation.cpp CheckBlockHeader: CheckProofOfWork failure is
		// DoS(50, ..., "high-hash"), surfaced by net_processing.cpp as
		// Misbehaving(pfrom, nDoS, "invalid header received").
		require.Equal(t, 50, misbehavior)

		_, ok := f.idx.Lookup(bad.BlockHash())
		require.False(t, ok)
	})

	t.Run("a header whose target exceeds powLimit scores 50", func(t *testing.T) {
		f := newSyncFixture(t, 2)

		// Mainnet powLimit (2^224 - 1) with a header claiming the regtest-easy
		// target: the hash meets its claimed target, but the target itself is
		// out of range. pow.cpp CheckProofOfWork rejects on
		// bnTarget > powLimit before it ever compares the hash.
		mainnet := chaincfg.MainNetParams

		hs, err := NewHeaderSync(HeaderSyncConfig{Index: f.idx, Params: &mainnet})
		require.NoError(t, err)

		peer := fullNodePeer("1.2.3.4:8333")
		require.Len(t, hs.PeerEstablished(peer), 1)

		tooEasy := minedChild(f.tip(), testEasyBits, 700)
		require.True(t, testMeetsTarget(tooEasy))

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg([]*wire.BlockHeader{tooEasy}))
		require.NoError(t, err)
		require.Nil(t, msgs)
		require.Equal(t, 50, misbehavior)
	})

	t.Run("a header with a zero target scores 50", func(t *testing.T) {
		f := newSyncFixture(t, 2)
		hs, peer := startedSync(t, f, nil)

		prevHash := f.tip().BlockHash()
		zero := chainhash.Hash{}
		// nBits == 0 decodes to target 0, which SetCompact rejects as out of
		// range regardless of the hash.
		malformed := wire.NewBlockHeader(1, &prevHash, &zero, 0, 1)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg([]*wire.BlockHeader{malformed}))
		require.NoError(t, err)
		require.Nil(t, msgs)
		require.Equal(t, 50, misbehavior)
	})
}

// TestCheckBlockHeaderPoW_MainnetVector runs the PoW check against a real
// mainnet header rather than a ground test header, so the byte order of both
// the hash and the compact target is pinned against the live chain and not
// against this file's own helpers.
func TestCheckBlockHeaderPoW_MainnetVector(t *testing.T) {
	// The mainnet genesis header, 80 bytes on the wire: version, null prev
	// hash, merkle root, timestamp, nBits 0x1d00ffff, nonce.
	const genesisHex = "01000000" +
		"0000000000000000000000000000000000000000000000000000000000000000" +
		"3ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a" +
		"29ab5f49" + "ffff001d" + "1dac2b7c"

	raw, err := hex.DecodeString(genesisHex)
	require.NoError(t, err)

	var header wire.BlockHeader
	require.NoError(t, header.Deserialize(bytes.NewReader(raw)))

	// If the vector were wrong in any byte, this would not be the hash the
	// chain params carry.
	require.Equal(t, *chaincfg.MainNetParams.GenesisHash, header.BlockHash())
	require.Equal(t, testHardBits, header.Bits)

	idx, err := NewHeaderIndex(&header)
	require.NoError(t, err)

	mainnet := chaincfg.MainNetParams

	hs, err := NewHeaderSync(HeaderSyncConfig{Index: idx, Params: &mainnet})
	require.NoError(t, err)

	require.True(t, hs.checkBlockHeaderPoW(&header))

	// Same header, different nonce: the work is gone and the check must say so.
	// Grinding to a definite failure keeps the row deterministic.
	broken := header
	broken.Nonce++

	for testMeetsTarget(&broken) {
		broken.Nonce++
	}

	require.False(t, hs.checkBlockHeaderPoW(&broken))
}

// TestHeaderSync_Checkpoints pins the headers-first-to-next-checkpoint scheme
// carried from legacy netsync manager.go.
func TestHeaderSync_Checkpoints(t *testing.T) {
	t.Run("a wrong hash at the checkpoint height disconnects the peer", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		// The checkpoint claims some other hash at height 3.
		wrong := chainhash.Hash{0xDE, 0xAD}
		hs, peer := startedSync(t, f, []chaincfg.Checkpoint{{Height: 3, Hash: &wrong}})
		require.True(t, hs.IsHeadersFirstMode())

		batch := minedRun(f.tip(), 3, 800)

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))

		// legacy manager.go handleHeadersMsg: "Block header at height %d/hash
		// %s does NOT match expected checkpoint hash" → DisconnectWithWarning.
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrCheckpointMismatch))
		require.Nil(t, msgs)
		require.Zero(t, misbehavior)

		// errors.Is matches by code alone, so pin the rendered message too: it
		// must name the offending height, the hash we got, and the checkpoint
		// hash we wanted.
		require.Contains(t, err.Error(), "height 3")
		require.Contains(t, err.Error(), batch[1].BlockHash().String())
		require.Contains(t, err.Error(), wrong.String())
	})

	t.Run("matching a checkpoint asks for headers up to the next one", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		// Pre-mine the headers the peer will send so the first checkpoint can
		// name the real hash at height 3.
		batch := minedRun(f.tip(), 3, 900)
		first := batch[len(batch)-1].BlockHash()
		second := chainhash.Hash{0xBE, 0xEF}

		hs, peer := startedSync(t, f, []chaincfg.Checkpoint{
			{Height: 4, Hash: &first},
			{Height: 5000, Hash: &second},
		})

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, misbehavior)

		gh := requireGetHeaders(t, msgs)

		// legacy manager.go: the next round of headers runs from the reached
		// checkpoint up to the checkpoint after it.
		require.Equal(t, f.idx.LocatorFrom(first), locatorValues(gh))
		require.Equal(t, second, gh.HashStop)
		require.True(t, hs.IsHeadersFirstMode())
	})

	t.Run("a short batch reaching the final checkpoint ends headers-first mode", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		batch := minedRun(f.tip(), 3, 1000)
		final := batch[len(batch)-1].BlockHash()

		hs, peer := startedSync(t, f, []chaincfg.Checkpoint{{Height: 4, Hash: &final}})
		require.True(t, hs.IsHeadersFirstMode())

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, misbehavior)

		// legacy manager.go: "Reached the final checkpoint -- switching to
		// normal mode". The batch is short, so net_processing.cpp asks for
		// nothing more either.
		require.False(t, hs.IsHeadersFirstMode())
		require.Nil(t, msgs)
	})

	t.Run("headers stop at the checkpoint header inside a full batch", func(t *testing.T) {
		f := newSyncFixture(t, 0)

		batch := minedRun(f.genesis, MaxHeadersResults, 1100)
		cpHash := batch[1].BlockHash() // height 2, well inside the batch

		hs, peer := startedSync(t, f, []chaincfg.Checkpoint{{Height: 2, Hash: &cpHash}})

		msgs, misbehavior, err := hs.OnHeaders(peer, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, misbehavior)

		// legacy manager.go handleHeadersMsg breaks out of the header loop at
		// the checkpoint, so the rest of the batch is not accepted.
		_, tipHeight := f.idx.Tip()
		require.Equal(t, int32(2), tipHeight)

		_, ok := f.idx.Lookup(batch[2].BlockHash())
		require.False(t, ok)

		// No checkpoint is left, so the continuation runs with a zero hashStop.
		gh := requireGetHeaders(t, msgs)
		require.Equal(t, chainhash.Hash{}, gh.HashStop)
		require.False(t, hs.IsHeadersFirstMode())
	})

	t.Run("during a round a peer without the sync slot cannot index headers", func(t *testing.T) {
		f := newSyncFixture(t, 1)

		batch := minedRun(f.tip(), 3, 1200)
		wrong := chainhash.Hash{0xDE, 0xAD}

		// The sync slot is held by another peer, so this one is outside the
		// headers-first round: legacy scopes that round to its sync peer and
		// returns early for everyone else.
		hs, holder := startedSync(t, f, []chaincfg.Checkpoint{{Height: 3, Hash: &wrong}})
		require.True(t, hs.IsHeadersFirstMode())

		bystander := fullNodePeer("5.6.7.8:8333")
		require.Nil(t, hs.PeerEstablished(bystander))
		require.False(t, bystander.State.fSyncStarted)

		msgs, misbehavior, err := hs.OnHeaders(bystander, headersMsg(batch))

		// The same batch would have disconnected the sync peer at height 3.
		require.NoError(t, err)
		require.Zero(t, misbehavior)
		require.Nil(t, msgs)

		// Nothing was indexed, so the bystander can neither race the round nor
		// push our tip past the checkpoint the round is working toward.
		_, tipHeight := f.idx.Tip()
		require.Equal(t, int32(1), tipHeight)

		for _, h := range batch {
			_, ok := f.idx.Lookup(h.BlockHash())
			require.False(t, ok)
		}

		// The round and its checkpoint are untouched.
		require.True(t, hs.IsHeadersFirstMode())
		require.True(t, holder.State.fSyncStarted)

		// The announcement still counts: the peer is known to have that block
		// once we learn of it, so block download can use it later.
		require.Equal(t, batch[len(batch)-1].BlockHash(), bystander.State.hashLastUnknownBlock)

		// Once the round ends, the same peer's headers index normally — the
		// restriction is round-scoped, not permanent.
		hs.PeerDisconnected(holder)
		require.False(t, hs.IsHeadersFirstMode())

		msgs, misbehavior, err = hs.OnHeaders(bystander, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, misbehavior)
		require.Nil(t, msgs)

		_, tipHeight = f.idx.Tip()
		require.Equal(t, int32(4), tipHeight)
	})

	t.Run("findNextHeaderCheckpoint picks the first checkpoint above the height", func(t *testing.T) {
		f := newSyncFixture(t, 0)

		h1 := chainhash.Hash{0x01}
		h2 := chainhash.Hash{0x02}
		h3 := chainhash.Hash{0x03}

		hs, err := NewHeaderSync(HeaderSyncConfig{
			Index: f.idx,
			Params: syncTestParams([]chaincfg.Checkpoint{
				{Height: 100, Hash: &h1},
				{Height: 200, Hash: &h2},
				{Height: 300, Hash: &h3},
			}),
		})
		require.NoError(t, err)

		tests := []struct {
			height int32
			want   *chainhash.Hash
		}{
			{0, &h1},
			{99, &h1},
			{100, &h2},
			{199, &h2},
			{200, &h3},
			{299, &h3},
			{300, nil},
			{4000, nil},
		}

		for _, tc := range tests {
			got := hs.findNextHeaderCheckpoint(tc.height)

			if tc.want == nil {
				require.Nil(t, got, "height %d", tc.height)
				continue
			}

			require.NotNil(t, got, "height %d", tc.height)
			require.Equal(t, *tc.want, *got.Hash, "height %d", tc.height)
		}
	})
}

func TestNewHeaderSync_Validation(t *testing.T) {
	f := newSyncFixture(t, 0)

	_, err := NewHeaderSync(HeaderSyncConfig{Index: nil, Params: syncTestParams(nil)})
	require.Error(t, err)

	_, err = NewHeaderSync(HeaderSyncConfig{Index: f.idx, Params: nil})
	require.Error(t, err)

	// Params without a powLimit would let the header PoW check accept any
	// target a peer claims.
	noLimit := chaincfg.MainNetParams
	noLimit.PowLimit = nil

	_, err = NewHeaderSync(HeaderSyncConfig{Index: f.idx, Params: &noLimit})
	require.Error(t, err)
}

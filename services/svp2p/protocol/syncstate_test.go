package protocol

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/stretchr/testify/require"
)

func TestUpdateBlockAvailability_KnownHash(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	chain := buildChain(t, idx, nc, genesis, 6) // heights 1..6

	atHeight := func(h int) chainhash.Hash { return chain[h-1].BlockHash() }

	// A side chain forking off height 2 gives a second, distinct hash at
	// height 4 (2 -> 3' -> 4'), for a tie-break test that isn't degenerately
	// comparing a hash against itself.
	side := buildChain(t, idx, nc, chain[1], 2)
	sideHeight4 := side[len(side)-1].BlockHash()
	require.NotEqual(t, atHeight(4), sideHeight4)

	tests := []struct {
		name           string
		initialBest    *HeaderNode // nil means pindexBestKnownBlock starts as nullptr
		announce       chainhash.Hash
		wantBestHeight int32
		wantBestHash   chainhash.Hash
	}{
		{
			name:           "no prior best known block: known hash sets it",
			initialBest:    nil,
			announce:       atHeight(3),
			wantBestHeight: 3,
			wantBestHash:   atHeight(3),
		},
		{
			name:           "higher known hash raises pindexBestKnownBlock",
			initialBest:    &HeaderNode{Hash: atHeight(2), Height: 2},
			announce:       atHeight(5),
			wantBestHeight: 5,
			wantBestHash:   atHeight(5),
		},
		{
			name:           "lower known hash does not lower pindexBestKnownBlock",
			initialBest:    &HeaderNode{Hash: atHeight(5), Height: 5},
			announce:       atHeight(2),
			wantBestHeight: 5,
			wantBestHash:   atHeight(5),
		},
		{
			name:           "equal-height known hash still replaces pindexBestKnownBlock, mirroring the C++ >= compare",
			initialBest:    &HeaderNode{Hash: atHeight(4), Height: 4},
			announce:       sideHeight4,
			wantBestHeight: 4,
			wantBestHash:   sideHeight4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := newPeerSyncState()
			state.pindexBestKnownBlock = tc.initialBest

			state.updateBlockAvailability(idx, tc.announce)

			require.NotNil(t, state.pindexBestKnownBlock)
			require.Equal(t, tc.wantBestHeight, state.pindexBestKnownBlock.Height)
			require.Equal(t, tc.wantBestHash, state.pindexBestKnownBlock.Hash)
			require.Equal(t, chainhash.Hash{}, state.hashLastUnknownBlock)
		})
	}
}

func TestUpdateBlockAvailability_UnknownHashParks(t *testing.T) {
	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	unknown := chainhash.Hash{0xAB}

	state := newPeerSyncState()
	state.updateBlockAvailability(idx, unknown)

	require.Nil(t, state.pindexBestKnownBlock)
	require.Equal(t, unknown, state.hashLastUnknownBlock)
}

func TestUpdateBlockAvailability_ResolvesPendingBeforeHandlingNewAnnouncement(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	chain := buildChain(t, idx, nc, genesis, 3) // heights 1..3
	nowKnown := chain[2].BlockHash()            // height 3, was unknown when announced

	state := newPeerSyncState()
	state.hashLastUnknownBlock = nowKnown // parked earlier, before AddHeader connected it
	state.pindexBestKnownBlock = nil

	stillUnknown := chainhash.Hash{0xCD}

	// net_processing.cpp UpdateBlockAvailability calls ProcessBlockAvailability
	// FIRST, resolving the pending hashLastUnknownBlock, before evaluating the
	// newly announced hash. If the order were reversed, the newly announced
	// (still unknown) hash would overwrite hashLastUnknownBlock before the
	// pending one got a chance to resolve, and pindexBestKnownBlock would
	// wrongly stay nil.
	state.updateBlockAvailability(idx, stillUnknown)

	require.NotNil(t, state.pindexBestKnownBlock)
	require.Equal(t, int32(3), state.pindexBestKnownBlock.Height)
	require.Equal(t, nowKnown, state.pindexBestKnownBlock.Hash)
	require.Equal(t, stillUnknown, state.hashLastUnknownBlock)
}

func TestProcessBlockAvailability_PromotesWhenLaterKnown(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	// Build a header but don't feed it to the index yet: announce its hash
	// while the peer is still ahead of what we've indexed.
	futureHeader := childOf(genesis, nc.take())
	futureHash := futureHeader.BlockHash()

	state := newPeerSyncState()
	state.updateBlockAvailability(idx, futureHash)

	require.Nil(t, state.pindexBestKnownBlock)
	require.Equal(t, futureHash, state.hashLastUnknownBlock)

	// A later getheaders round-trip connects that same header into the index.
	connected, err := idx.AddHeader(futureHeader)
	require.NoError(t, err)
	require.True(t, connected)

	state.processBlockAvailability(idx)

	require.NotNil(t, state.pindexBestKnownBlock)
	require.Equal(t, int32(1), state.pindexBestKnownBlock.Height)
	require.Equal(t, futureHash, state.pindexBestKnownBlock.Hash)
	require.Equal(t, chainhash.Hash{}, state.hashLastUnknownBlock)
}

func TestProcessBlockAvailability_NoOpWhenStillUnknown(t *testing.T) {
	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	stillUnknown := chainhash.Hash{0x42}

	state := newPeerSyncState()
	state.hashLastUnknownBlock = stillUnknown

	state.processBlockAvailability(idx)

	require.Nil(t, state.pindexBestKnownBlock)
	require.Equal(t, stillUnknown, state.hashLastUnknownBlock)
}

func TestProcessBlockAvailability_NoOpWhenNoPending(t *testing.T) {
	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	state := newPeerSyncState()
	state.pindexBestKnownBlock = &HeaderNode{Hash: genesis.BlockHash(), Height: 0}

	state.processBlockAvailability(idx)

	require.Equal(t, chainhash.Hash{}, state.hashLastUnknownBlock)
	require.Equal(t, int32(0), state.pindexBestKnownBlock.Height)
}

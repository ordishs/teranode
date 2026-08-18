package protocol

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

// nonceCounter hands out ever-increasing nonces so headers built within one
// test never collide on hash, regardless of how many chains/forks a test
// builds from the same genesis.
type nonceCounter struct{ next uint32 }

func (c *nonceCounter) take() uint32 {
	c.next++
	return c.next
}

func testGenesis() *wire.BlockHeader {
	zero := chainhash.Hash{}
	return wire.NewBlockHeader(1, &zero, &zero, 0x1d00ffff, 0)
}

// childOf returns a header extending parent, with a unique nonce so its
// hash never collides with a sibling built from the same parent.
func childOf(parent *wire.BlockHeader, nonce uint32) *wire.BlockHeader {
	prevHash := parent.BlockHash()
	zero := chainhash.Hash{}

	return wire.NewBlockHeader(1, &prevHash, &zero, 0x1d00ffff, nonce)
}

// buildChain extends the index from "from" for "count" headers, requiring
// every AddHeader to connect. It returns the built headers in order.
func buildChain(t *testing.T, idx *HeaderIndex, nc *nonceCounter, from *wire.BlockHeader, count int) []*wire.BlockHeader {
	t.Helper()

	headers := make([]*wire.BlockHeader, 0, count)
	prev := from

	for i := 0; i < count; i++ {
		h := childOf(prev, nc.take())

		connected, err := idx.AddHeader(h)
		require.NoError(t, err)
		require.True(t, connected)

		headers = append(headers, h)
		prev = h
	}

	return headers
}

func TestNewHeaderIndex(t *testing.T) {
	t.Run("seeds tip at genesis", func(t *testing.T) {
		genesis := testGenesis()

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		hash, height := idx.Tip()
		require.Equal(t, genesis.BlockHash(), hash)
		require.Equal(t, int32(0), height)

		n, ok := idx.Lookup(hash)
		require.True(t, ok)
		require.Equal(t, int32(0), n.height)
		require.Nil(t, n.prev)
	})

	t.Run("rejects a nil genesis header", func(t *testing.T) {
		idx, err := NewHeaderIndex(nil)
		require.Error(t, err)
		require.Nil(t, idx)
	})
}

func TestAddHeader_UnknownParent(t *testing.T) {
	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	// A header whose PrevBlock is not in the index anywhere: connected must
	// come back false, the tip must stay exactly where it was, and the
	// orphan must not be reachable via Lookup.
	orphanParent := chainhash.Hash{0xAA}
	zero := chainhash.Hash{}
	orphan := wire.NewBlockHeader(1, &orphanParent, &zero, 0x1d00ffff, 1)

	connected, err := idx.AddHeader(orphan)
	require.NoError(t, err)
	require.False(t, connected)

	hash, height := idx.Tip()
	require.Equal(t, genesis.BlockHash(), hash)
	require.Equal(t, int32(0), height)

	_, ok := idx.Lookup(orphan.BlockHash())
	require.False(t, ok)
}

func TestAddHeader_NilHeader(t *testing.T) {
	idx, err := NewHeaderIndex(testGenesis())
	require.NoError(t, err)

	connected, err := idx.AddHeader(nil)
	require.Error(t, err)
	require.False(t, connected)
}

func TestAddHeader_DuplicateIsIdempotent(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	headers := buildChain(t, idx, nc, genesis, 1)

	beforeHash, beforeHeight := idx.Tip()

	// SVNode's mapBlockIndex lookup finds the existing node for a header it
	// has already accepted; re-submitting it must report connected without
	// mutating anything.
	connected, err := idx.AddHeader(headers[0])
	require.NoError(t, err)
	require.True(t, connected)

	afterHash, afterHeight := idx.Tip()
	require.Equal(t, beforeHash, afterHash)
	require.Equal(t, beforeHeight, afterHeight)
}

func TestAddHeader_SideChainDoesNotMoveTipUnlessLonger(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	// Main chain: genesis -> 1 -> 2 -> 3 -> 4 -> 5.
	main := buildChain(t, idx, nc, genesis, 5)
	mainTip := main[len(main)-1]

	tipHash, tipHeight := idx.Tip()
	require.Equal(t, mainTip.BlockHash(), tipHash)
	require.Equal(t, int32(5), tipHeight)

	// Side chain forks off height 2 and only reaches height 4 (2 -> 3' -> 4'):
	// shorter than the main tip, so it must extend the tree without moving
	// the tip.
	forkPoint := main[1] // height 2
	side := buildChain(t, idx, nc, forkPoint, 2)
	sideTip := side[len(side)-1]

	n, ok := idx.Lookup(sideTip.BlockHash())
	require.True(t, ok)
	require.Equal(t, int32(4), n.height)

	tipHash, tipHeight = idx.Tip()
	require.Equal(t, mainTip.BlockHash(), tipHash)
	require.Equal(t, int32(5), tipHeight)

	// Extend the side chain past the main tip (4' -> 5' -> 6'): the tip must
	// now switch, since Phase 2 selects by height.
	longerSide := buildChain(t, idx, nc, sideTip, 2)
	newSideTip := longerSide[len(longerSide)-1]

	tipHash, tipHeight = idx.Tip()
	require.Equal(t, newSideTip.BlockHash(), tipHash)
	require.Equal(t, int32(6), tipHeight)
}

func TestLocator_HeightSequences(t *testing.T) {
	// Expected sequences derived directly from chain.cpp CChain::GetLocator:
	// push the current height, stop at height 0 (genesis is always last),
	// step back by nStep each time, nStep doubles once vHave holds more
	// than 10 entries.
	tests := []struct {
		name           string
		tipHeight      int32
		expectedHeight []int32
	}{
		{"height 0 is genesis only", 0, []int32{0}},
		{"height 5 all linear", 5, []int32{5, 4, 3, 2, 1, 0}},
		{"height 10 all linear", 10, []int32{10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0}},
		{"height 11 starts doubling on the 12th entry", 11, []int32{11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0}},
		{
			"height 100 exponential tail", 100,
			[]int32{100, 99, 98, 97, 96, 95, 94, 93, 92, 91, 90, 89, 87, 83, 75, 59, 27, 0},
		},
		{
			"height 10000 exponential tail", 10000,
			[]int32{
				10000, 9999, 9998, 9997, 9996, 9995, 9994, 9993, 9992, 9991, 9990, 9989,
				9987, 9983, 9975, 9959, 9927, 9863, 9735, 9479, 8967, 7943, 5895, 1799, 0,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			genesis := testGenesis()
			nc := &nonceCounter{}

			idx, err := NewHeaderIndex(genesis)
			require.NoError(t, err)

			var tipHeader *wire.BlockHeader = genesis
			if tc.tipHeight > 0 {
				chain := buildChain(t, idx, nc, genesis, int(tc.tipHeight))
				tipHeader = chain[len(chain)-1]
			}

			locator := idx.Locator()

			gotHeights := heightsOf(t, idx, locator)
			require.Equal(t, tc.expectedHeight, gotHeights)
			require.Equal(t, genesis.BlockHash(), locator[len(locator)-1])
			require.Equal(t, tipHeader.BlockHash(), locator[0])

			// LocatorFrom(tip hash) must agree with Locator() exactly.
			fromTip := idx.LocatorFrom(tipHeader.BlockHash())
			require.Equal(t, locator, fromTip)
		})
	}
}

// heightsOf maps each locator hash back to its node height, for asserting
// exact locator content rather than just its length.
func heightsOf(t *testing.T, idx *HeaderIndex, hashes []chainhash.Hash) []int32 {
	t.Helper()

	heights := make([]int32, len(hashes))

	for i, h := range hashes {
		n, ok := idx.Lookup(h)
		require.True(t, ok)
		heights[i] = n.height
	}

	return heights
}

func TestLocatorFrom_UnknownHashReturnsNil(t *testing.T) {
	idx, err := NewHeaderIndex(testGenesis())
	require.NoError(t, err)

	unknown := chainhash.Hash{0xBB}
	require.Nil(t, idx.LocatorFrom(unknown))
}

func TestAncestor(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	chain := buildChain(t, idx, nc, genesis, 5)
	tip := chain[len(chain)-1]

	t.Run("finds an ancestor at a lower height", func(t *testing.T) {
		n, ok := idx.Ancestor(tip.BlockHash(), 2)
		require.True(t, ok)
		require.Equal(t, chain[1].BlockHash(), n.hash) // height 2 is chain[1]
	})

	t.Run("returns itself at its own height", func(t *testing.T) {
		n, ok := idx.Ancestor(tip.BlockHash(), 5)
		require.True(t, ok)
		require.Equal(t, tip.BlockHash(), n.hash)
	})

	t.Run("rejects a height above the node", func(t *testing.T) {
		_, ok := idx.Ancestor(chain[1].BlockHash(), 5)
		require.False(t, ok)
	})

	t.Run("rejects a negative height", func(t *testing.T) {
		_, ok := idx.Ancestor(tip.BlockHash(), -1)
		require.False(t, ok)
	})

	t.Run("rejects an unknown hash", func(t *testing.T) {
		_, ok := idx.Ancestor(chainhash.Hash{0xCC}, 0)
		require.False(t, ok)
	})
}

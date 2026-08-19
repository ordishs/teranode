package protocol

import (
	"fmt"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
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
	return childOfBits(parent, nonce, difficulty1Bits)
}

// childOfBits is childOf with the difficulty target chosen by the caller, so
// a test can build two branches of unequal work.
func childOfBits(parent *wire.BlockHeader, nonce uint32, bits uint32) *wire.BlockHeader {
	prevHash := parent.BlockHash()
	zero := chainhash.Hash{}

	return wire.NewBlockHeader(1, &prevHash, &zero, bits, nonce)
}

// The two targets every work-based test in this package builds branches from.
// Their hand-derived GetBlockProof values are pinned by TestBlockProof below.
const (
	// difficulty1Bits is the difficulty-1 target, worth 4_295_032_833 per
	// header.
	difficulty1Bits = uint32(0x1d00ffff)

	// heavyBits is a target worth 8_589_934_591 per header, just under twice
	// difficulty1Bits.
	heavyBits = uint32(0x1d008000)
)

const (
	difficulty1Work = int64(4_295_032_833)
	heavyWork       = int64(8_589_934_591)
)

// buildChainBits is buildChain with the branch's difficulty target chosen by
// the caller.
func buildChainBits(t *testing.T, idx *HeaderIndex, nc *nonceCounter, from *wire.BlockHeader, count int, bits uint32) []*wire.BlockHeader {
	t.Helper()

	headers := make([]*wire.BlockHeader, 0, count)
	prev := from

	for i := 0; i < count; i++ {
		h := childOfBits(prev, nc.take(), bits)

		connected, err := idx.AddHeader(h)
		require.NoError(t, err)
		require.True(t, connected)

		headers = append(headers, h)
		prev = h
	}

	return headers
}

// TestBlockProof pins the block_index.cpp GetBlockProof port
// (block_index.cpp:114-125) against values derived by hand from the compact
// target, independent of any big.Int code this package calls.
func TestBlockProof(t *testing.T) {
	tests := []struct {
		name string
		bits uint32
		want string
	}{
		{
			// 0x1d00ffff: exponent 0x1d = 29, mantissa 0x00ffff = 65535, so
			// target = 65535 * 256^(29-3) = (2^16 - 1) * 2^208.
			// GetBlockProof is floor(2^256 / (target + 1)). Take
			// q = 4295032833; then q * 65535 = 2^48 - 1, so
			//   q * (target + 1) = (2^48 - 1) * 2^208 + q = 2^256 - 2^208 + q,
			// which is below 2^256 because 2^208 > q, while
			//   (q + 1) * (target + 1) = 2^256 + 65534 * 2^208 + q + 1
			// is above it. The floor is therefore exactly q.
			name: "difficulty-1 target",
			bits: 0x1d00ffff,
			want: "4295032833",
		},
		{
			// 0x1d008000: exponent 29, mantissa 0x008000 = 2^15, so
			// target = 2^15 * 2^208 = 2^223.
			// (2^33) * (2^223 + 1) = 2^256 + 2^33 is above 2^256, and
			// (2^33 - 1) * (2^223 + 1) = 2^256 + 2^33 - 2^223 - 1 is below it
			// because 2^223 > 2^33. The floor is 2^33 - 1 = 8589934591.
			name: "heavier target",
			bits: 0x1d008000,
			want: "8589934591",
		},
		{
			// 0x207fffff (the regtest power limit): exponent 0x20 = 32,
			// mantissa 0x7fffff = 2^23 - 1, so
			// target = (2^23 - 1) * 2^232 = 2^255 - 2^232.
			// 2 * (target + 1) = 2^256 - 2^233 + 2 is below 2^256 and
			// 3 * (target + 1) is above it, since 3 * 2^255 alone exceeds
			// 2^256. The floor is 2.
			name: "regtest power limit",
			bits: 0x207fffff,
			want: "2",
		},
		{
			// arith_uint256::SetCompact reports fNegative when the sign bit
			// (0x00800000) is set on a non-zero mantissa, and GetBlockProof
			// returns 0 for it.
			name: "negative-encoded target contributes no work",
			bits: 0x1d80ffff,
			want: "0",
		},
		{
			// exponent 0x22 = 34 with mantissa 0x010000 puts the target past
			// 2^256, which is arith_uint256::SetCompact's fOverflow: 0 work.
			name: "overflowing target contributes no work",
			bits: 0x22010000,
			want: "0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want, ok := new(big.Int).SetString(tc.want, 10)
			require.True(t, ok)

			require.Equal(t, 0, want.Cmp(blockProof(tc.bits)),
				"want %s, got %s", want, blockProof(tc.bits))
		})
	}
}

// TestAddHeader_AccumulatesChainWork pins block_index.h SetChainWork:
// nChainWork = (pprev ? pprev->nChainWork : 0) + GetBlockProof(*this). Genesis
// carries its own proof and nothing else; every child adds its own on top of
// its parent's total.
func TestAddHeader_AccumulatesChainWork(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	root, ok := idx.Lookup(genesis.BlockHash())
	require.True(t, ok)
	require.Equal(t, big.NewInt(difficulty1Work), root.ChainWork,
		"genesis has no parent, so its chain work is its own proof")

	// Three difficulty-1 headers, then two heavy ones: the running total is
	// the hand-derived per-header proof summed along the chain.
	light := buildChain(t, idx, nc, genesis, 3)
	heavy := buildChainBits(t, idx, nc, light[2], 2, heavyBits)

	wants := []struct {
		header *wire.BlockHeader
		want   int64
	}{
		{light[0], 2 * difficulty1Work},
		{light[1], 3 * difficulty1Work},
		{light[2], 4 * difficulty1Work},
		{heavy[0], 4*difficulty1Work + heavyWork},
		{heavy[1], 4*difficulty1Work + 2*heavyWork},
	}

	for i, w := range wants {
		n, found := idx.Lookup(w.header.BlockHash())
		require.True(t, found)
		require.Equal(t, big.NewInt(w.want), n.ChainWork, "chain work at index %d", i)
	}
}

// TestAddHeader_TipFollowsMostWork pins the block_index_store.h SetBestHeader
// rule and its CBlockIndexWorkComparator (block_index.h:1225-1260): "First
// sort by most total work", with the sequence-id tail leaving the first-seen
// branch in place on a tie. It replaces the Phase 2 height rule, and with it
// the reorg case Phase 2 Task 3 deferred: a shorter branch that carries more
// work must take the tip.
func TestAddHeader_TipFollowsMostWork(t *testing.T) {
	// Branch weights, all derived from the pinned per-header proofs above:
	//   3 light headers above genesis: 3 * 4_295_032_833 = 12_885_098_499
	//   2 heavy headers above genesis: 2 * 8_589_934_591 = 17_179_869_182
	// The heavy branch is one block shorter than the light branch and still
	// outweighs it.
	const (
		lightLen = 3
		heavyLen = 2
	)

	t.Run("a taller but lighter branch does not take the tip", func(t *testing.T) {
		genesis := testGenesis()
		nc := &nonceCounter{}

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		heavy := buildChainBits(t, idx, nc, genesis, heavyLen, heavyBits)
		heavyTip := heavy[len(heavy)-1]

		hash, height := idx.Tip()
		require.Equal(t, heavyTip.BlockHash(), hash)
		require.Equal(t, int32(heavyLen), height)

		light := buildChainBits(t, idx, nc, genesis, lightLen, difficulty1Bits)
		lightTip := light[len(light)-1]

		lightNode, ok := idx.Lookup(lightTip.BlockHash())
		require.True(t, ok)
		require.Equal(t, int32(lightLen), lightNode.Height,
			"the lighter branch is taller than the heavy one")

		hash, height = idx.Tip()
		require.Equal(t, heavyTip.BlockHash(), hash, "the taller branch carries less work")
		require.Equal(t, int32(heavyLen), height)
	})

	t.Run("a shorter but heavier branch takes the tip", func(t *testing.T) {
		genesis := testGenesis()
		nc := &nonceCounter{}

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		light := buildChainBits(t, idx, nc, genesis, lightLen, difficulty1Bits)
		lightTip := light[len(light)-1]

		hash, height := idx.Tip()
		require.Equal(t, lightTip.BlockHash(), hash)
		require.Equal(t, int32(lightLen), height)

		heavy := buildChainBits(t, idx, nc, genesis, heavyLen, heavyBits)
		heavyTip := heavy[len(heavy)-1]

		hash, height = idx.Tip()
		require.Equal(t, heavyTip.BlockHash(), hash, "the heavier branch takes the tip although it is shorter")
		require.Equal(t, int32(heavyLen), height)
	})

	t.Run("equal work leaves the first-seen branch on the tip", func(t *testing.T) {
		genesis := testGenesis()
		nc := &nonceCounter{}

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		first := buildChainBits(t, idx, nc, genesis, lightLen, difficulty1Bits)
		firstTip := first[len(first)-1]

		second := buildChainBits(t, idx, nc, genesis, lightLen, difficulty1Bits)
		secondTip := second[len(second)-1]
		require.NotEqual(t, firstTip.BlockHash(), secondTip.BlockHash())

		firstNode, ok := idx.Lookup(firstTip.BlockHash())
		require.True(t, ok)

		secondNode, ok := idx.Lookup(secondTip.BlockHash())
		require.True(t, ok)
		require.Equal(t, firstNode.ChainWork, secondNode.ChainWork, "both branches carry the same work")

		hash, _ := idx.Tip()
		require.Equal(t, firstTip.BlockHash(), hash, "a tie keeps the branch that arrived first")
	})
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
		require.Equal(t, int32(0), n.Height)
		require.Equal(t, chainhash.Hash{}, n.ParentHash)
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

// TestAddHeader_SideChainDoesNotMoveTipUnlessLonger builds every branch at the
// same difficulty target, where height and cumulative work rank identically, so
// it pins the same outcomes under the work-based tip rule as it did under the
// Phase 2 height rule. The cases where the two rules disagree are
// TestAddHeader_TipFollowsMostWork.
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
	require.Equal(t, int32(4), n.Height)

	tipHash, tipHeight = idx.Tip()
	require.Equal(t, mainTip.BlockHash(), tipHash)
	require.Equal(t, int32(5), tipHeight)

	// Extend the side chain past the main tip (4' -> 5' -> 6'): at equal
	// difficulty the taller branch also carries the most work, so the tip
	// must now switch.
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
		heights[i] = n.Height
	}

	return heights
}

func TestLookup_ParentHashWalksOneStepAtATime(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	// chain[0] is height 1, chain[1] is height 2; chain[0]'s parent is genesis.
	chain := buildChain(t, idx, nc, genesis, 2)

	// A consumer in another package can walk toward genesis using only the
	// exported HeaderNode: look up a hash, then look up its ParentHash.
	n, ok := idx.Lookup(chain[1].BlockHash())
	require.True(t, ok)
	require.Equal(t, chain[0].BlockHash(), n.ParentHash)

	parent, ok := idx.Lookup(n.ParentHash)
	require.True(t, ok)
	require.Equal(t, genesis.BlockHash(), parent.ParentHash)

	grandparent, ok := idx.Lookup(parent.ParentHash)
	require.True(t, ok)
	require.Equal(t, genesis.BlockHash(), grandparent.Hash)
	require.Equal(t, chainhash.Hash{}, grandparent.ParentHash) // genesis has no parent
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
		require.Equal(t, chain[1].BlockHash(), n.Hash) // height 2 is chain[1]
	})

	t.Run("returns itself at its own height", func(t *testing.T) {
		n, ok := idx.Ancestor(tip.BlockHash(), 5)
		require.True(t, ok)
		require.Equal(t, tip.BlockHash(), n.Hash)
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

// naivePrevWalk walks pprev links directly from n, the pre-skiplist algorithm
// this task replaces. It is the test's independent authority for what
// Ancestor and the pskip pointers should compute, so it must not call
// ancestorLocked or share any logic with it.
func naivePrevWalk(n *node, height int32) (*node, bool) {
	if height < 0 || height > n.height {
		return nil, false
	}

	for n.height > height {
		if n.prev == nil {
			return nil, false
		}

		n = n.prev
	}

	return n, true
}

// sampleHeightsFor returns a deterministic set of heights to probe against a
// chain of length maxHeight: the boundaries (0, 1, height-1, height), every
// power of two up to maxHeight, and several heights spread across the range
// by dividing maxHeight by small primes.
func sampleHeightsFor(maxHeight int32) []int32 {
	set := map[int32]struct{}{
		0: {}, 1: {}, maxHeight - 1: {}, maxHeight: {},
	}

	for p := int32(1); p <= maxHeight; p *= 2 {
		set[p] = struct{}{}
	}

	for _, prime := range []int32{3, 5, 7, 11, 13, 17, 19, 23, 29, 31} {
		set[maxHeight/prime] = struct{}{}
	}

	heights := make([]int32, 0, len(set))

	for h := range set {
		if h >= 0 && h <= maxHeight {
			heights = append(heights, h)
		}
	}

	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })

	return heights
}

// TestAncestor_DeepChainMatchesNaiveWalk builds a 100_000-header chain (the
// depth this task's brief measured at testnet, where the pprev-only walk
// costs hours of pure pointer-chasing over a full IBD) and cross-checks
// idx.Ancestor, which is now skiplist-accelerated, against naivePrevWalk at
// many heights: both must agree at every sampled height, and both must
// reject out-of-range heights identically.
func TestAncestor_DeepChainMatchesNaiveWalk(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	const chainLen = 100_000

	chain := buildChain(t, idx, nc, genesis, chainLen)
	tip := chain[len(chain)-1]

	idx.mu.Lock()
	tipNode := idx.nodes[tip.BlockHash()]
	idx.mu.Unlock()
	require.Equal(t, int32(chainLen), tipNode.height)

	for _, h := range sampleHeightsFor(chainLen) {
		h := h

		t.Run(fmt.Sprintf("height=%d", h), func(t *testing.T) {
			want, wantOK := naivePrevWalk(tipNode, h)

			got, gotOK := idx.Ancestor(tip.BlockHash(), h)

			require.Equal(t, wantOK, gotOK)

			if wantOK {
				require.Equal(t, want.hash, got.Hash)
				require.Equal(t, want.height, got.Height)
			}
		})
	}

	_, ok := idx.Ancestor(tip.BlockHash(), chainLen+1)
	require.False(t, ok, "height above the tip must be rejected")

	_, ok = idx.Ancestor(tip.BlockHash(), -1)
	require.False(t, ok, "a negative height must be rejected")
}

// TestSkipPointer_MatchesGetSkipHeightRecurrence checks every node's pskip
// against the block_index.cpp BuildSkipNL recurrence directly: pskip must be
// exactly the node at height getSkipHeight(n.height), reached by walking
// pprev from n.prev — the same computation BuildSkipNL performs at insert
// time via pprev->GetAncestor(GetSkipHeight(nHeight)).
func TestSkipPointer_MatchesGetSkipHeightRecurrence(t *testing.T) {
	genesis := testGenesis()
	nc := &nonceCounter{}

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	const chainLen = 5_000

	chain := buildChain(t, idx, nc, genesis, chainLen)

	idx.mu.Lock()
	defer idx.mu.Unlock()

	genesisNode := idx.nodes[genesis.BlockHash()]
	require.Nil(t, genesisNode.skip, "genesis has no pprev, so BuildSkipNL leaves pskip nil")

	for _, h := range chain {
		n := idx.nodes[h.BlockHash()]

		wantHeight := getSkipHeight(n.height)

		want, ok := naivePrevWalk(n.prev, wantHeight)
		require.True(t, ok, "height %d must be reachable from the parent of height %d", wantHeight, n.height)

		require.NotNil(t, n.skip, "node at height %d must have a skip pointer", n.height)
		require.Equal(t, want.hash, n.skip.hash)
		require.Equal(t, wantHeight, n.skip.height)
	}
}

// TestGetSkipHeight checks the recurrence itself against block_index.cpp
// GetSkipHeight (block_index.cpp:22-33) for known inputs, independent of any
// tree structure.
func TestGetSkipHeight(t *testing.T) {
	tests := []struct {
		height int32
		want   int32
	}{
		{0, 0},
		{1, 0},
		{2, 0},
		{3, 1},
		{4, 0},
		{5, 1},
		{6, 4},
		{7, 1},
		{8, 0},
		{9, 1},
		{10, 8},
		{1023, 1017},
		{1024, 0},
		{1025, 1},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("height=%d", tc.height), func(t *testing.T) {
			require.Equal(t, tc.want, getSkipHeight(tc.height))
		})
	}
}

// TestHeaderIndex_ConcurrentReadsDuringWrites is the documented exception to
// the project's default of avoiding t.Parallel()/goroutines in tests: it
// specifically exercises the "safe for concurrent reads under one mutex"
// contract by running several readers against a concurrent writer under
// -race.
//
// testify's require.* calls t.FailNow, which the testing package forbids
// calling from any goroutine but the test's own; so the writer and readers
// below record outcomes into plain values instead of asserting inline, and
// every require.* call happens back on the test's own goroutine after
// wg.Wait() has joined all of them.
func TestHeaderIndex_ConcurrentReadsDuringWrites(t *testing.T) {
	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	const chainLen = 500

	// Precompute the chain so the writer goroutine's loop body is just the
	// AddHeader call under test, not hashing.
	nc := &nonceCounter{}
	headers := make([]*wire.BlockHeader, 0, chainLen)
	prev := genesis

	for i := 0; i < chainLen; i++ {
		h := childOf(prev, nc.take())
		headers = append(headers, h)
		prev = h
	}

	var wg sync.WaitGroup

	done := make(chan struct{})
	addErrs := make([]error, chainLen)
	addConnected := make([]bool, chainLen)

	wg.Add(1)

	go func() {
		defer wg.Done()
		defer close(done)

		for i, h := range headers {
			connected, addErr := idx.AddHeader(h)
			addErrs[i] = addErr
			addConnected[i] = connected
		}
	}()

	// Readers hammer every read method concurrently with the writer. Tip's
	// returned hash must always resolve via Lookup and Ancestor at its own
	// height, at every point in the walk: AddHeader only ever appends nodes,
	// so this invariant holds under any interleaving.
	var readerInconsistent atomic.Bool

	const readerCount = 8

	for i := 0; i < readerCount; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for {
				select {
				case <-done:
					return
				default:
				}

				hash, height := idx.Tip()

				if _, ok := idx.Lookup(hash); !ok {
					readerInconsistent.Store(true)
				}

				if _, ok := idx.Ancestor(hash, height); !ok {
					readerInconsistent.Store(true)
				}

				_ = idx.Locator()
			}
		}()
	}

	wg.Wait()

	for i := range addErrs {
		require.NoError(t, addErrs[i])
		require.True(t, addConnected[i])
	}

	require.False(t, readerInconsistent.Load(), "a concurrent reader observed an inconsistent index")

	hash, height := idx.Tip()
	require.Equal(t, headers[len(headers)-1].BlockHash(), hash)
	require.Equal(t, int32(chainLen), height)
}

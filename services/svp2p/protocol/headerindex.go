package protocol

import (
	"sync"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// node mirrors block_index.h CBlockIndex: pprev links to the parent, pskip
// is the skiplist shortcut set once at insert time, and nHeight is the
// height above genesis. Teranode's blockchain service stays the
// authoritative header store (spec §11); this tree is a rebuildable,
// in-memory cache scoped to protocol decisions (locators, best-known-block,
// download scheduling) that must not sit on a per-message gRPC call.
type node struct {
	hash   chainhash.Hash
	header wire.BlockHeader
	prev   *node // CBlockIndex::pprev
	skip   *node // CBlockIndex::pskip
	height int32 // CBlockIndex::nHeight
}

// invertLowestOne mirrors block_index.cpp InvertLowestOne (block_index.cpp:18-20):
// turn the lowest '1' bit in the binary representation of n into a '0'.
func invertLowestOne(n int32) int32 {
	return n & (n - 1)
}

// getSkipHeight mirrors block_index.cpp GetSkipHeight (block_index.cpp:22-33):
// compute what height to jump back to with the node's skip pointer
// (CBlockIndex::pskip). Any number strictly lower than height is acceptable,
// but this expression performs well in simulations (max 110 steps to go back
// up to 2**18 blocks).
func getSkipHeight(height int32) int32 {
	if height < 2 {
		return 0
	}

	if height&1 != 0 {
		return invertLowestOne(invertLowestOne(height-1)) + 1
	}

	return invertLowestOne(height)
}

// HeaderNode is the exported, immutable snapshot of a node returned by
// Lookup and Ancestor. It carries the CBlockIndex fields callers in other
// packages need: the hash, its height, and its parent's hash for walking
// the tree one step at a time via another Lookup call. ParentHash is the
// zero chainhash.Hash at genesis, which has no parent.
type HeaderNode struct {
	Hash       chainhash.Hash
	ParentHash chainhash.Hash
	Height     int32
}

func exportNode(n *node) HeaderNode {
	var parentHash chainhash.Hash
	if n.prev != nil {
		parentHash = n.prev.hash
	}

	return HeaderNode{Hash: n.hash, ParentHash: parentHash, Height: n.height}
}

// HeaderIndex is the net_processing.cpp mapBlockIndex counterpart: a small
// header tree hydrated from the blockchain client at startup and kept
// current from its subscription. It is safe for concurrent use under one
// mutex and holds no reference to any Teranode client.
//
// The tree is genesis-rooted: NewHeaderIndex seeds a single height-0 node
// with no parent, and AddHeader only ever attaches a new node to a parent
// already in the tree, so every node's prev chain terminates at that
// height-0 node. Locator and Ancestor walk that chain and assume the
// height-0 terminus holds; there is no API to seed a partial, non-genesis
// tree.
type HeaderIndex struct {
	mu    sync.Mutex
	nodes map[chainhash.Hash]*node
	tip   *node
}

// NewHeaderIndex seeds the tree with the chain's genesis header at height 0.
// Callers hydrate the rest by feeding headers fetched from the blockchain
// client into AddHeader, in order, before serving any peer.
func NewHeaderIndex(genesis *wire.BlockHeader) (*HeaderIndex, error) {
	if genesis == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: genesis header is nil")
	}

	root := &node{
		hash:   genesis.BlockHash(),
		header: *genesis,
		prev:   nil,
		height: 0,
	}

	return &HeaderIndex{
		nodes: map[chainhash.Hash]*node{root.hash: root},
		tip:   root,
	}, nil
}

// AddHeader links header onto its parent, mirroring only the mapBlockIndex
// insert portion of net_processing.cpp AcceptBlockHeader. It performs no
// header validation: CheckBlockHeader's proof-of-work check and any
// contextual checks are deliberately the caller's responsibility (the
// headers-first sync layer), not this passive index's. connected is false
// when the parent is not yet known (an orphan header the caller must fill
// in with more getheaders before this one can attach); the tip is left
// unmutated in that case. A header already present returns
// connected == true without mutating anything, matching a mapBlockIndex
// lookup that finds the existing entry.
func (idx *HeaderIndex) AddHeader(header *wire.BlockHeader) (connected bool, err error) {
	if header == nil {
		return false, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: header is nil")
	}

	hash := header.BlockHash()

	idx.mu.Lock()
	defer idx.mu.Unlock()

	if _, ok := idx.nodes[hash]; ok {
		return true, nil
	}

	parent, ok := idx.nodes[header.PrevBlock]
	if !ok {
		return false, nil
	}

	n := &node{
		hash:   hash,
		header: *header,
		prev:   parent,
		height: parent.height + 1,
	}
	n.skip = buildSkipLocked(n)

	idx.nodes[hash] = n

	// Phase 2 simplification: the tip is the tallest known header, not the
	// one with the most cumulative chain work. This is testnet-sufficient;
	// a work-based tip is a Phase 3 hardening candidate if the parity
	// harness diverges (spec §6, header index).
	if n.height > idx.tip.height {
		idx.tip = n
	}

	return true, nil
}

// Tip returns the current best-known header.
func (idx *HeaderIndex) Tip() (hash chainhash.Hash, height int32) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	return idx.tip.hash, idx.tip.height
}

// Lookup returns the node for hash, mirroring a mapBlockIndex find.
func (idx *HeaderIndex) Lookup(hash chainhash.Hash) (HeaderNode, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	n, ok := idx.nodes[hash]
	if !ok {
		return HeaderNode{}, false
	}

	return exportNode(n), true
}

// Ancestor returns the predecessor of hash at height, mirroring
// CBlockIndex::GetAncestor (block_index.cpp:81-105) via the skiplist descent
// in ancestorLocked.
func (idx *HeaderIndex) Ancestor(hash chainhash.Hash, height int32) (HeaderNode, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	n, ok := idx.nodes[hash]
	if !ok {
		return HeaderNode{}, false
	}

	anc, ok := ancestorLocked(n, height)
	if !ok {
		return HeaderNode{}, false
	}

	return exportNode(anc), true
}

// ancestorLocked mirrors block_index.cpp CBlockIndex::GetAncestor
// (block_index.cpp:81-105): descend via the skip pointer whenever it lands
// at-or-after the target height and isn't worse than falling back to pprev,
// otherwise step pprev once. This is the same decision order and heuristic
// condition as the source, in the same order. The nil guard on walk.prev
// replaces the source's `assert(pindexWalk->pprev)`: given the
// genesis-rooted invariant documented on HeaderIndex, every node's chain
// reaches the height-0 node before prev ever goes nil, so this should be
// unreachable. It exists so a violated invariant fails the call (ok ==
// false) instead of panicking on a nil dereference.
func ancestorLocked(n *node, height int32) (*node, bool) {
	if height < 0 || height > n.height {
		return nil, false
	}

	walk := n
	heightWalk := n.height

	for heightWalk > height {
		heightSkip := getSkipHeight(heightWalk)
		heightSkipPrev := getSkipHeight(heightWalk - 1)

		if walk.skip != nil &&
			(heightSkip == height ||
				(heightSkip > height &&
					!(heightSkipPrev < heightSkip-2 && heightSkipPrev >= height))) {
			// Only follow pskip if pprev->pskip isn't better than pskip->pprev.
			walk = walk.skip
			heightWalk = heightSkip
		} else {
			if walk.prev == nil {
				return nil, false
			}

			walk = walk.prev
			heightWalk--
		}
	}

	return walk, true
}

// buildSkipLocked mirrors block_index.cpp CBlockIndex::BuildSkipNL
// (block_index.cpp:107-112): pskip = pprev->GetAncestor(GetSkipHeight(nHeight)).
// Called once at insert time, under idx.mu, after n.prev and n.height are
// set and before n is published into idx.nodes; genesis has no prev, so it
// keeps pskip nil, matching the source's `if (pprev)` guard.
func buildSkipLocked(n *node) *node {
	if n.prev == nil {
		return nil
	}

	skip, ok := ancestorLocked(n.prev, getSkipHeight(n.height))
	if !ok {
		return nil
	}

	return skip
}

// Locator builds a block locator for the current tip.
func (idx *HeaderIndex) Locator() []chainhash.Hash {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	return locatorFromLocked(idx.tip)
}

// LocatorFrom builds a block locator for hash instead of the tip, used when
// answering a peer whose best-known-block is behind ours. It returns nil if
// hash is not in the index.
func (idx *HeaderIndex) LocatorFrom(hash chainhash.Hash) []chainhash.Hash {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	n, ok := idx.nodes[hash]
	if !ok {
		return nil
	}

	return locatorFromLocked(n)
}

// locatorFromLocked mirrors chain.cpp CChain::GetLocator: walk back from n,
// appending each visited hash. The back-step starts at 1 and doubles again
// every time more than 10 hashes have already been collected — the literal
// `if (vHave.size() > 10) nStep *= 2` check, re-evaluated after each append,
// so it keeps doubling as the walk continues. Genesis (height 0) is always
// the final entry appended, and ends the walk. Called with idx.mu held.
func locatorFromLocked(n *node) []chainhash.Hash {
	have := make([]chainhash.Hash, 0, 32)
	step := int32(1)

	for {
		have = append(have, n.hash)

		if n.height == 0 {
			break
		}

		nextHeight := n.height - step
		if nextHeight < 0 {
			nextHeight = 0
		}

		next, ok := ancestorLocked(n, nextHeight)
		if !ok {
			// Defensive: unreachable given the genesis-rooted invariant
			// documented on HeaderIndex (every chain terminates at height
			// 0). Stop rather than continue with a nil node.
			break
		}

		n = next

		if len(have) > 10 {
			step *= 2
		}
	}

	return have
}

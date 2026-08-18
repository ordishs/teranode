package protocol

import (
	"sync"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// node mirrors chain.h CBlockIndex: pprev links to the parent and nHeight is
// the height above genesis. Teranode's blockchain service stays the
// authoritative header store (spec §11); this tree is a rebuildable,
// in-memory cache scoped to protocol decisions (locators, best-known-block,
// download scheduling) that must not sit on a per-message gRPC call.
type node struct {
	hash   chainhash.Hash
	header wire.BlockHeader
	prev   *node // CBlockIndex::pprev
	height int32 // CBlockIndex::nHeight
}

// HeaderIndex is the net_processing.cpp mapBlockIndex counterpart: a small
// header tree hydrated from the blockchain client at startup and kept
// current from its subscription. It is safe for concurrent use under one
// mutex and holds no reference to any Teranode client.
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

// AddHeader links header onto its parent, mirroring net_processing.cpp
// AcceptBlockHeader's mapBlockIndex insert. connected is false when the
// parent is not yet known (an orphan header the caller must fill in with
// more getheaders before this one can attach); the tip is left unmutated in
// that case. A header already present returns connected == true without
// mutating anything, matching a mapBlockIndex lookup that finds the
// existing entry.
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
func (idx *HeaderIndex) Lookup(hash chainhash.Hash) (*node, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	n, ok := idx.nodes[hash]

	return n, ok
}

// Ancestor returns the predecessor of hash at height, mirroring
// CBlockIndex::GetAncestor. Phase 2 walks pprev links directly rather than
// porting SVNode's skiplist; the tree is small enough that this is not a
// hot path.
func (idx *HeaderIndex) Ancestor(hash chainhash.Hash, height int32) (*node, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	n, ok := idx.nodes[hash]
	if !ok {
		return nil, false
	}

	return ancestorLocked(n, height)
}

func ancestorLocked(n *node, height int32) (*node, bool) {
	if height < 0 || height > n.height {
		return nil, false
	}

	for n.height > height {
		n = n.prev
	}

	return n, true
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
// appending each visited hash. The step is 1 for the first 10 entries, then
// doubles on every subsequent entry; genesis (height 0) is always the last
// entry appended, and ends the walk. Called with idx.mu held.
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

		n, _ = ancestorLocked(n, nextHeight)

		if len(have) > 10 {
			step *= 2
		}
	}

	return have
}

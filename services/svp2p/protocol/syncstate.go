package protocol

import (
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
)

// peerSyncState is the net_processing.cpp CNodeState port: per-peer sync
// bookkeeping consumed by headers-first sync (Task 5) and block download
// scheduling (Task 6). Phase 2 keeps only the fields those two consumers
// need; the rest of CNodeState (pindexLastCommonBlock,
// pindexBestHeaderSent, nUnconnectingHeaders, and the block-download
// bookkeeping beyond the in-flight count) is not carried here and is a
// later-phase candidate if a consumer needs it.
//
// Locking: peerSyncState carries no lock of its own. SVNode guards the
// equivalent mapNodeState entries with cs_main; this port's plan (spec §11,
// Task 11) instead gives PeerManager one shared sync-state mutex covering
// every peer's state, so every method here assumes the caller already holds
// that mutex. This mirrors cs_main's role faithfully: one coarse lock
// serializing sync-state mutation, just scoped to this subsystem instead of
// the whole node.
type peerSyncState struct {
	// pindexBestKnownBlock mirrors CNodeState::pindexBestKnownBlock: the best
	// known block we know this peer has announced. nil mirrors the C++
	// nullptr — we don't have a known best block for this peer yet. SVNode
	// compares by nChainWork; the header index (spec §6, Phase 2
	// simplification) tracks height instead, so this ports the same
	// nullptr-or-not-lower comparison against Height.
	pindexBestKnownBlock *HeaderNode

	// hashLastUnknownBlock mirrors CNodeState::hashLastUnknownBlock: the hash
	// of the last unknown block this peer has announced. The zero
	// chainhash.Hash mirrors the C++ uint256().SetNull() sentinel used by
	// IsNull().
	hashLastUnknownBlock chainhash.Hash

	// nBlocksInFlight mirrors CNodeState::nBlocksInFlight: the number of
	// blocks currently being downloaded from this peer. Populated and
	// consumed by block download scheduling (Task 6); not touched here.
	nBlocksInFlight int

	// fSyncStarted mirrors CNodeState::fSyncStarted: whether we've started
	// headers synchronization with this peer. Populated and consumed by
	// headers-first sync (Task 5); not touched here.
	fSyncStarted bool

	// nStallingSince mirrors CNodeState::nStallingSince: since when we're
	// stalling block download progress, in microseconds since the Unix
	// epoch, or 0 when not stalling. Populated and consumed by block download
	// scheduling (Task 6); not touched here.
	nStallingSince int64
}

// newPeerSyncState returns a zero-value peerSyncState: no best known block,
// no pending unknown announcement, sync not started, nothing in flight or
// stalling. This mirrors a freshly default-constructed CNodeState entry.
func newPeerSyncState() *peerSyncState {
	return &peerSyncState{}
}

// updateBlockAvailability mirrors net_processing.cpp UpdateBlockAvailability.
// Requires the caller to hold PeerManager's shared sync-state mutex (see the
// locking note on peerSyncState), the port of SVNode's cs_main requirement.
func (s *peerSyncState) updateBlockAvailability(idx *HeaderIndex, hash chainhash.Hash) {
	s.processBlockAvailability(idx)

	node, ok := idx.Lookup(hash)
	if ok {
		// An actually better block was announced.
		if s.pindexBestKnownBlock == nil || node.Height >= s.pindexBestKnownBlock.Height {
			s.pindexBestKnownBlock = &node
		}
	} else {
		// An unknown block was announced; just assume that the latest one is
		// the best one.
		s.hashLastUnknownBlock = hash
	}
}

// processBlockAvailability mirrors net_processing.cpp ProcessBlockAvailability.
// Requires the caller to hold PeerManager's shared sync-state mutex (see the
// locking note on peerSyncState), the port of SVNode's cs_main requirement.
func (s *peerSyncState) processBlockAvailability(idx *HeaderIndex) {
	if s.hashLastUnknownBlock == (chainhash.Hash{}) {
		return
	}

	node, ok := idx.Lookup(s.hashLastUnknownBlock)
	if !ok {
		return
	}

	if s.pindexBestKnownBlock == nil || node.Height >= s.pindexBestKnownBlock.Height {
		s.pindexBestKnownBlock = &node
	}

	s.hashLastUnknownBlock = chainhash.Hash{}
}

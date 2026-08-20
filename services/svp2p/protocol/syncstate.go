package protocol

import (
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
)

// peerSyncState is the net_processing.cpp CNodeState port: per-peer sync
// bookkeeping consumed by headers-first sync (Task 5) and block download
// scheduling (Task 6). Phase 2 keeps only the fields those two consumers
// need; the rest of CNodeState (pindexBestHeaderSent, nUnconnectingHeaders,
// and the block-download bookkeeping beyond the in-flight count) is not
// carried here and is a later-phase candidate if a consumer needs it.
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
	// nullptr — we don't have a known best block for this peer yet. The
	// comparison is SVNode's own nullptr-or-not-lower test against
	// nChainWork; Phase 2 compared Height instead because the header index
	// carried no work, and Phase 3 Task 1 restored the work compare with the
	// index field. HeaderNode is a value snapshot returned by
	// HeaderIndex.Lookup at the moment it was read, not a live pointer into
	// the index's tree: a later Lookup of the same hash returns an
	// equal-by-value HeaderNode, never the same address, so pointer equality
	// against a fresh Lookup result is meaningless here.
	pindexBestKnownBlock *HeaderNode

	// hashLastUnknownBlock mirrors CNodeState::hashLastUnknownBlock: the hash
	// of the last unknown block this peer has announced. The zero
	// chainhash.Hash mirrors the C++ uint256().SetNull() sentinel used by
	// IsNull().
	hashLastUnknownBlock chainhash.Hash

	// pindexLastCommonBlock mirrors CNodeState::pindexLastCommonBlock: the last
	// block on this peer's branch that we already have, which is where block
	// download resumes and where the download window starts. nil mirrors the
	// C++ nullptr and makes FindNextBlocksToDownload bootstrap it from our own
	// chain. Like pindexBestKnownBlock this is a HeaderNode value snapshot, not
	// a live pointer into the index tree, so pointer equality against a fresh
	// Lookup result is meaningless; compare Hash. Populated and consumed by
	// block download scheduling (Task 6); not touched here.
	pindexLastCommonBlock *HeaderNode

	// nBlocksInFlight mirrors CNodeState::nBlocksInFlight: the number of
	// blocks currently being downloaded from this peer. Populated and
	// consumed by block download scheduling (Task 6); not touched here.
	nBlocksInFlight int

	// vBlocksInFlight mirrors CNodeState::vBlocksInFlight (node_state.h:80)
	// reduced to what the timeout needs: the hashes this peer owes us, in the
	// order they were requested. Only the FRONT entry is ever read —
	// DetectStalling names it in the timeout log line and nDownloadingSince
	// measures it — but the whole order has to be kept, because which entry is
	// the front changes as blocks arrive out of order. The C++ list carries a
	// QueuedBlock per entry (the CBlockIndex pointer and the partial-block
	// buffer for compact blocks); neither has a consumer in this port.
	//
	// It is a slice rather than a linked list: removals are O(n) over a list
	// capped at MaxBlocksInTransitPerPeer, which is 16.
	vBlocksInFlight []chainhash.Hash

	// nDownloadingSince mirrors CNodeState::nDownloadingSince
	// (node_state.h:81-83): "When the first entry in vBlocksInFlight started
	// downloading. Don't care when vBlocksInFlight is empty." In microseconds
	// since the Unix epoch. It is armed when the peer's in-flight queue goes
	// from empty to one entry, and re-armed when the front entry leaves the
	// queue, so it always measures the block at the head.
	nDownloadingSince int64

	// fSyncStarted mirrors CNodeState::fSyncStarted: whether we've started
	// headers synchronization with this peer. Populated and consumed by
	// headers-first sync (Task 5); not touched here.
	fSyncStarted bool

	// nStallingSince mirrors CNodeState::nStallingSince: since when we're
	// stalling block download progress, in microseconds since the Unix
	// epoch, or 0 when not stalling. Populated and consumed by block download
	// scheduling (Task 6); not touched here.
	nStallingSince int64

	// nIngestBytesLastSample and nIngestSampleMicros have no CNodeState
	// counterpart either. They carry legacy netsync manager.go
	// syncPeerState.assocReadBytesLastTick and the tick it was taken on: the
	// previous sample of how far the block this peer is ingesting has got,
	// which is what lets CheckStall compute a byte rate and keep a peer that
	// is still delivering a large block. Both are zero when no ingest is
	// running. Populated and consumed by block download scheduling (Task 6).
	nIngestBytesLastSample uint64
	nIngestSampleMicros    int64

	// nLastProgressTime has no CNodeState counterpart. It carries legacy
	// netsync manager.go syncPeerState.lastBlockTime, the clock the Teranode
	// sync-peer rotation measures against maxLastBlockTime (PR 1067), in
	// microseconds since the Unix epoch. 0 means no progress has been observed
	// yet; the first stall check seeds it. Populated and consumed by block
	// download scheduling (Task 6); not touched here.
	nLastProgressTime int64
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

	// C++ additionally guards on pindex->nChainWork > 0, which detects a
	// header accepted into mapBlockIndex but not yet linked to the
	// genesis-rooted chain. No counterpart is needed here, but it takes two
	// guarantees rather than one:
	//
	//   - HeaderIndex.AddHeader only ever attaches a node to a parent already
	//     in the tree, so every Lookup hit is genesis-rooted, unlike SVNode's
	//     mapBlockIndex which can hold disconnected entries;
	//   - every header reaching AddHeader has a valid target, so the proof
	//     summed along that chain is non-zero.
	//
	// The second guarantee is not enforced by AddHeader, and genesis-rootedness
	// alone does not imply it: a chain of headers whose nBits all encode an
	// invalid target would accumulate zero work. It is enforced upstream, on
	// both paths that write to the index. Peer headers pass
	// HeaderSync.checkBlockHeaderPoW (headersync.go), whose target.Sign() <= 0
	// test rejects exactly the negative and overflowing encodings that make
	// GetBlockProof zero, before OnHeaders calls AddHeader. Headers from the
	// blockchain subscription come from Teranode's own validated store. A
	// future caller that writes to the index past both of those breaks this
	// invariant and would need the guard restored.
	node, ok := idx.Lookup(hash)
	if ok {
		// An actually better block was announced.
		if s.pindexBestKnownBlock == nil ||
			chainWorkOf(node).Cmp(chainWorkOf(*s.pindexBestKnownBlock)) >= 0 {
			s.pindexBestKnownBlock = &node
		}
	} else {
		// An unknown block was announced; just assume that the latest one is
		// the best one.
		s.hashLastUnknownBlock = hash
	}
}

// processBlockAvailability mirrors net_processing.cpp ProcessBlockAvailability:
// "Check whether the last unknown block a peer advertised is not yet known."
// Requires the caller to hold PeerManager's shared sync-state mutex (see the
// locking note on peerSyncState), the port of SVNode's cs_main requirement.
func (s *peerSyncState) processBlockAvailability(idx *HeaderIndex) {
	if s.hashLastUnknownBlock == (chainhash.Hash{}) {
		return
	}

	// C++ additionally guards on pindex->nChainWork > 0, which detects a
	// header accepted into mapBlockIndex but not yet linked to the
	// genesis-rooted chain. No counterpart is needed here, but it takes two
	// guarantees rather than one:
	//
	//   - HeaderIndex.AddHeader only ever attaches a node to a parent already
	//     in the tree, so every Lookup hit is genesis-rooted, unlike SVNode's
	//     mapBlockIndex which can hold disconnected entries;
	//   - every header reaching AddHeader has a valid target, so the proof
	//     summed along that chain is non-zero.
	//
	// The second guarantee is not enforced by AddHeader, and genesis-rootedness
	// alone does not imply it: a chain of headers whose nBits all encode an
	// invalid target would accumulate zero work. It is enforced upstream, on
	// both paths that write to the index. Peer headers pass
	// HeaderSync.checkBlockHeaderPoW (headersync.go), whose target.Sign() <= 0
	// test rejects exactly the negative and overflowing encodings that make
	// GetBlockProof zero, before OnHeaders calls AddHeader. Headers from the
	// blockchain subscription come from Teranode's own validated store. A
	// future caller that writes to the index past both of those breaks this
	// invariant and would need the guard restored.
	node, ok := idx.Lookup(s.hashLastUnknownBlock)
	if !ok {
		return
	}

	// The clear below is unconditional once the hash resolves, independent
	// of whether it actually raises pindexBestKnownBlock: net_processing.cpp
	// clears hashLastUnknownBlock as soon as the pending hash is found,
	// even when it turns out to be no better than what's already known.
	if s.pindexBestKnownBlock == nil ||
		chainWorkOf(node).Cmp(chainWorkOf(*s.pindexBestKnownBlock)) >= 0 {
		s.pindexBestKnownBlock = &node
	}

	s.hashLastUnknownBlock = chainhash.Hash{}
}

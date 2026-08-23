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

	// nIngestBytesPerSec and nIngestRateMicros have no CNodeState counterpart
	// either. They are this port's stand-in for what SVNode reads off the
	// association: CAssociation::GetAverageBandwidth(BLOCK), the per-peer block
	// delivery rate IsBlockDownloadStallingFromPeer tests
	// (net_processing.cpp:105-109). Two consumers need that rate at moments when
	// no IngestSnapshot is to hand — the parallel-fetch branch asks it of every
	// holder of a block, not just the peer being walked — so CheckStall computes
	// it once per peer per tick and leaves it here.
	//
	// nIngestRateMicros is when it was computed, which is what lets a reader tell
	// a rate of zero from a rate nobody has measured lately. Both are zero when
	// no ingest has been sampled.
	nIngestBytesPerSec uint64
	nIngestRateMicros  int64

	// nLastProgressTime has no CNodeState counterpart. It carries legacy
	// netsync manager.go syncPeerState.lastBlockTime, the clock the Teranode
	// sync-peer rotation measures against maxLastBlockTime (PR 1067), in
	// microseconds since the Unix epoch. 0 means no progress has been observed
	// yet; the first stall check seeds it. Populated and consumed by block
	// download scheduling (Task 6); not touched here.
	nLastProgressTime int64

	// pindexBestHeaderSent mirrors CNodeState::pindexBestHeaderSent
	// (node_state.h): the last header we told this peer about, via a
	// getheaders reply OR a relay headers announcement. Serving.OnGetHeaders's
	// doc comment named this field as its own missing counterpart, deferred
	// to "the relay path (Task 12)" — this is that task. nil mirrors the C++
	// nullptr: neither has happened yet for this peer.
	//
	// Written in two places, matching the two places net_processing.cpp
	// writes it:
	//
	//   - Serving.OnGetHeaders, ProcessGetHeadersMessage's unconditional reset
	//     (net_processing.cpp:3044, bitcoin-sv@879fc8b42): "It is important
	//     that we simply reset the BestHeaderSent value here, and not
	//     max(BestHeaderSent, newHeaderSent)" — every getheaders reply
	//     overwrites it, never merges with what was there.
	//   - PeerManager.RelayBlock, SendBlockHeaders's own write right after it
	//     pushes the plain HEADERS message (net_processing.cpp:5372-5373 —
	//     verified by grepping every pindexBestHeaderSent site in the file
	//     rather than trusting a remembered line number; :5357 is the
	//     sibling compact-block branch this port does not take, Phase 4).
	//     A relay announcement is exactly as much "telling the peer about a
	//     header" as a getheaders reply is. This write is its OWN defect,
	//     not part of I1/I2's root cause: I1 was the dropped
	//     pindexBestKnownBlock branch and I2 was the absent parent test,
	//     both read-side; this write is what I2's parent test needs in
	//     order to see a PRIOR relay's headers send when it decides the
	//     NEXT block's hasParent (fix round 2, review Minor 2 correction).
	//
	// Read by the block announcement relay (relay.go peerHasHeader), which is
	// half of net_processing.cpp PeerHasHeader — see that function's doc
	// comment for the other half, pindexBestKnownBlock. This is its only
	// consumer; peerSyncState carries no field without one (see the file doc
	// comment above).
	pindexBestHeaderSent *HeaderNode

	// knownBlocks is this peer's known-block set: legacy peer.go's
	// knownInventory (mruInventoryMap, AddKnownInventory/QueueInventory.Exists)
	// reduced to blocks only, since this port relays no other inventory type
	// yet. It covers exactly what neither pindexBestKnownBlock nor
	// pindexBestHeaderSent can see: a block WE already relayed to this peer
	// by plain INV, which touches neither field (see relay.go relayCandidate's
	// doc comment on hasBlock). Combined with those two fields by the relay
	// (relay.go, manager.go RelayBlock), it is what makes "never announce to
	// the originating peer" and "never announce the same block twice" the
	// same rule: whichever peer told us about a hash first (an inv, or the
	// block body itself) has that hash marked here before the relay for it
	// ever runs (manager.go Inv, BlockDone), and the relay marks it for
	// whoever it sends the announcement to.
	//
	// Sized for the STEADY-STATE case only — see knownBlockCap's own doc
	// comment for why a single large inv batch can consume the whole cap in
	// one call, which is a real limitation of a count-based bound, not a
	// time-based one.
	knownBlocks *knownBlockSet
}

// newPeerSyncState returns a zero-value peerSyncState: no best known block,
// no pending unknown announcement, sync not started, nothing in flight or
// stalling. This mirrors a freshly default-constructed CNodeState entry.
func newPeerSyncState() *peerSyncState {
	return &peerSyncState{knownBlocks: newKnownBlockSet()}
}

// knownBlockCap bounds knownBlockSet. Legacy's own cap (peer.go
// maxKnownInventory = wire.MaxInvPerMsg, 50000) sizes a set shared between
// every relayed transaction and every relayed block; this port's set holds
// blocks only, at roughly one every ten minutes on mainnet (the target
// spacing chaincfg.MainNetParams.TargetTimePerBlock encodes), so that cap
// would cover the better part of a year. 288 is sized against that
// STEADY-STATE rate: 48 hours of blocks at the target spacing, with what
// looks like headroom for a catch-up burst.
//
// It is NOT a time-based bound, and the "48 hours" framing above is an
// average, not a floor: the cap is consumed per mark() call, and Inv (in
// manager.go) marks every InvTypeBlock entry in a single inbound MsgInv —
// up to wire.MaxInvPerMsg (50,000) entries in ONE message. A single large
// inv batch, exactly the "multi-block catch-up burst after a reconnect"
// case this comment used to cite as the reason for the headroom, can evict
// most or all of the set in one call, so the real worst-case protection
// window is "until the next big inv batch", which can be much shorter than
// 48 hours. The consequence stays bounded to a wasted small message either
// way — see relay.go's doc comments on hasBlock for why eviction is
// wasteful rather than harmful — so the number is kept, with this correction
// to what it actually guarantees.
//
// Each entry costs roughly 75-80 bytes, not the 32 bytes of a bare
// chainhash.Hash (fix round 2, review Minor 4 correction: knownBlockSet
// stores every hash TWICE — once as a map key, with the map's own
// bucket/entry overhead, and once again in the order slice that makes
// eviction FIFO): call it 25 KB per peer at the cap, and roughly 23 MB in
// aggregate across 1000 connected peers. Still small against this
// service's other per-peer state (negligible next to Task 10's
// maxPendingGetData, whose cost analysis this mirrors), just not as small
// as a bare-hash estimate would suggest.
const knownBlockCap = 288

// knownBlockSet is a bounded, insertion-ordered set of block hashes: the
// backing store for peerSyncState.knownBlocks (see its doc comment for what
// it means and who writes it). Bounded FIFO eviction rather than legacy's
// most-recently-used replacement (peer.go mruInventoryMap) — the two are
// equivalent for this set's actual access pattern, since every hash here is
// inserted exactly once, checked for existence any number of times, and
// never re-inserted after it is marked; there is no "most recently used"
// distinct from "most recently inserted" to preserve.
//
// Not safe for concurrent use on its own: every peerSyncState field is
// guarded by PeerManager.syncMu, and this is no exception.
type knownBlockSet struct {
	set   map[chainhash.Hash]struct{}
	order []chainhash.Hash
}

func newKnownBlockSet() *knownBlockSet {
	return &knownBlockSet{set: make(map[chainhash.Hash]struct{})}
}

// has reports whether hash is already known. A nil receiver (a peerSyncState
// built by a zero-value literal instead of newPeerSyncState, as some test
// doubles do) answers false rather than panicking: "not yet known" is the
// correct answer for a set that was never given anything to know.
func (k *knownBlockSet) has(hash chainhash.Hash) bool {
	if k == nil {
		return false
	}

	_, ok := k.set[hash]

	return ok
}

// mark records hash as known, evicting the oldest entry first if the set is
// already at knownBlockCap. A hash already in the set is left exactly where
// it is: this is a "known or not" set, not an LRU, so re-marking does not
// refresh an eviction order that does not exist (see the type's doc
// comment).
func (k *knownBlockSet) mark(hash chainhash.Hash) {
	if k == nil {
		return
	}

	if _, ok := k.set[hash]; ok {
		return
	}

	if len(k.order) >= knownBlockCap {
		oldest := k.order[0]
		k.order = k.order[1:]
		delete(k.set, oldest)
	}

	k.set[hash] = struct{}{}
	k.order = append(k.order, hash)
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

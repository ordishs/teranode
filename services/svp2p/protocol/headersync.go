package protocol

import (
	"encoding/binary"
	"math/big"
	"net"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
)

// MaxHeadersResults mirrors net_processing.h MAX_HEADERS_RESULTS (2000): the
// number of headers a peer may put in one headers message.
const MaxHeadersResults = 2000

// MaxUnconnectingHeaders mirrors net_processing.cpp MAX_UNCONNECTING_HEADERS
// (10): how many headers messages with an unknown parent a peer may send
// before it scores misbehavior.
const MaxUnconnectingHeaders = 10

// MaxFutureBlockTime mirrors block_index.h MAX_FUTURE_BLOCK_TIME
// (block_index.h:31): 2 * 60 * 60 seconds. It is how far past the adjusted
// time a header's timestamp may sit before ContextualCheckBlockHeader refuses
// it as "time-too-new".
const MaxFutureBlockTime int64 = 2 * 60 * 60

// MaxBlocksToAnnounce mirrors net_processing.cpp MAX_BLOCKS_TO_ANNOUNCE (8):
// the size limit that separates a short "we are announcing a block you have
// not seen" headers message, which earns the gap-filling getheaders, from a
// bulk batch, which does not.
const MaxBlocksToAnnounce = 8

// Misbehavior scores for headers processing, from net_processing.cpp.
const (
	scoreTooManyHeaders            = 20 // Misbehaving(pfrom, 20, "too-many-headers")
	scoreNonContinuousHeaders      = 20 // Misbehaving(pfrom, 20, "disconnected headers")
	scoreTooManyUnconnectedHeaders = 20 // Misbehaving(pfrom, 20, "too-many-unconnected-headers")

	// AcceptBlockHeader fails a header whose parent is not in mapBlockIndex
	// with state.DoS(10, ..., "prev-blk-not-found"), which net_processing.cpp
	// applies as Misbehaving(pfrom, nDoS, "invalid header received"). Only a
	// bulk batch reaches it: a batch shorter than MAX_BLOCKS_TO_ANNOUNCE takes
	// the gap-filling getheaders path instead.
	scorePrevBlkNotFound = 10

	// CheckBlockHeader fails a bad proof of work with
	// state.DoS(50, ..., "high-hash"); net_processing.cpp then applies that
	// nDoS as Misbehaving(pfrom, nDoS, "invalid header received").
	scoreInvalidHeader = 50

	// ContextualCheckBlockHeader fails a header whose nBits is not the value
	// GetNextWorkRequired demands with
	// state.DoS(100, ..., REJECT_INVALID, "bad-diffbits"), which
	// net_processing.cpp applies as Misbehaving(pfrom, nDoS, "invalid header
	// received"). One hundred is the ban threshold, so a single such header
	// ends the peer.
	scoreBadDiffBits = 100
)

// ErrCheckpointMismatch reports a header at a checkpoint height whose hash is
// not the checkpoint hash. Legacy netsync manager.go answers this with
// DisconnectWithWarning, so the caller must drop the peer rather than score
// it. It shares ERR_NETWORK_PEER_MALICIOUS with ErrProtocolViolation on
// purpose: the teranode errors package matches errors.Is by code, and both
// sentinels mean the same thing to a caller — disconnect this peer.
var ErrCheckpointMismatch = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: header does not match checkpoint")

// ErrCheckpointFork reports a header that would fork the chain below the last
// checkpoint the index holds — validation.cpp CheckIndexAgainstCheckpoint's
// "bad-fork-prior-to-checkpoint". SVNode scores that DoS(100), one point above
// the ban threshold, so the peer is gone either way; this port answers with a
// disconnect error instead, the legacy DisconnectWithWarning shape the
// checkpoint-mismatch path above already uses, so both checkpoint refusals
// reach the caller the same way.
//
// It shares ERR_NETWORK_PEER_MALICIOUS with ErrCheckpointMismatch for the
// reason documented there: the teranode errors package matches errors.Is by
// code, and both sentinels mean "disconnect this peer". A caller that must
// tell them apart reads the message.
var ErrCheckpointFork = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: header forks below the last checkpoint")

// ErrHeadersNoProgress reports a headers batch that did not carry the
// headers-first round past the point the last getheaders was issued from — a
// peer replaying headers we already have. Legacy netsync catches the same
// peer with its header-list anchor ("Received block header that does not
// properly connect to the chain") and answers with DisconnectWithWarning, so
// the caller must drop the peer. It carries ERR_NETWORK_INVALID_RESPONSE to
// stay distinguishable from ErrCheckpointMismatch, since errors.Is matches by
// code; it therefore aliases the handshake's ErrObsoleteVersion, which no
// caller ever compares against a headers error.
var ErrHeadersNoProgress = errors.New(errors.ERR_NETWORK_INVALID_RESPONSE, "svp2p: headers batch made no sync progress")

// HeaderSyncConfig carries the immutable inputs of the headers-first machine.
type HeaderSyncConfig struct {
	// Index is the mapBlockIndex counterpart the machine reads and extends.
	Index *HeaderIndex

	// Params is the active chain configuration. It supplies the checkpoints
	// (the same source legacy netsync uses: chainParams.Checkpoints), the
	// powLimit the header PoW range check needs, and the network magic that
	// selects the regtest branches.
	Params *chaincfg.Params

	// DisableCheckpoints mirrors legacy netsync Config.DisableCheckpoints
	// (the -nocheckpoints flag): no checkpoint is ever selected and the
	// machine never enters headers-first mode.
	DisableCheckpoints bool

	// AllowSyncCandidateFromLocalPeers mirrors
	// settings.Legacy.AllowSyncCandidateFromLocalPeers
	// (legacy_allowSyncCandidateFromLocalPeers): in regtest it lifts the
	// localhost restriction on sync candidates.
	AllowSyncCandidateFromLocalPeers bool

	// AdjustedTime supplies the nAdjustedTime argument
	// ContextualCheckBlockHeader takes, in Unix seconds. It is the only clock
	// this machine consults, and it is injected rather than read, which is
	// what keeps every decision path in this package testable without a wall
	// clock. NewHeaderSync fills it with the system clock when it is nil, so
	// the default is chosen at construction rather than inside a check.
	//
	// SVNode passes GetAdjustedTime(), which is the local clock plus the
	// median time offset of its peers (timedata.cpp, clamped to +/-70
	// minutes). This port has no peer time-offset machinery, so it passes the
	// local clock alone. The divergence only matters for a node whose own
	// clock is wrong by more than MAX_FUTURE_BLOCK_TIME minus the network's
	// real spread — two hours — and a clock that far out breaks far more of
	// this node than header acceptance.
	AdjustedTime func() int64
}

// SyncPeer is the per-peer handle the machine needs for one event: the
// address and services the sync-candidate rules read, the peer's CNodeState
// entry, and the CNodeState::nUnconnectingHeaders counter this machine owns.
// PeerManager keeps one SyncPeer per connected peer for the peer's lifetime.
type SyncPeer struct {
	// Addr is the peer's remote address in host:port form, read by the
	// regtest localhost rule in isSyncCandidate.
	Addr string

	// Services is the service flag set the peer advertised in its version
	// message.
	Services wire.ServiceFlag

	// State is the peer's CNodeState entry, shared with block download
	// scheduling.
	State *peerSyncState

	// nUnconnectingHeaders mirrors CNodeState::nUnconnectingHeaders: how many
	// headers messages this peer has sent whose first header has no known
	// parent.
	nUnconnectingHeaders int
}

func NewSyncPeer(addr string, services wire.ServiceFlag, state *peerSyncState) *SyncPeer {
	return &SyncPeer{Addr: addr, Services: services, State: state}
}

// HeaderSync is the headers-first sync state machine: the net_processing.cpp
// header path (SendMessages' initial getheaders and the HEADERS handler) with
// the checkpointed getheaders scheme legacy netsync manager.go carries from
// bsvd. It performs no I/O: every event returns the messages the caller must
// send, a misbehavior delta to apply to the peer, and an error only when the
// peer must be disconnected. It mutates neither Conn nor Peer.
//
// Locking: HeaderSync carries no lock of its own. Like peerSyncState, every
// method assumes the caller already holds PeerManager's shared sync-state
// mutex — this package's port of cs_main. Lock order in this package is
// peer lock, then manager lock.
type HeaderSync struct {
	cfg         HeaderSyncConfig
	checkpoints []chaincfg.Checkpoint

	// nextCheckpoint mirrors SyncManager.nextCheckpoint: the checkpoint the
	// current headers-first round runs up to, or nil past the final one.
	nextCheckpoint *chaincfg.Checkpoint

	// headersFirstMode mirrors SyncManager.headersFirstMode.
	headersFirstMode bool

	// roundAnchorHeight is the height of the header the current round's last
	// getheaders was issued from. It is this port's replacement for legacy's
	// header-list anchor: a batch must reach past it, or the round is not
	// advancing and the peer is replaying headers we already hold.
	roundAnchorHeight int32

	// nSyncStarted mirrors the net_processing.cpp file-scope nSyncStarted:
	// how many peers we have started headers synchronization with.
	nSyncStarted int
}

func NewHeaderSync(cfg HeaderSyncConfig) (*HeaderSync, error) {
	if cfg.Index == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: header index is nil")
	}

	if cfg.Params == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: chain params are nil")
	}

	// Without a powLimit the header PoW check would accept any target the peer
	// claims, so refuse to build a machine that cannot enforce it.
	if cfg.Params.PowLimit == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: chain params carry no powLimit")
	}

	// Both findNextHeaderCheckpoint and lastCheckpointInIndex read the list as
	// ascending by height — the first walks it backwards to find the next
	// checkpoint above a height, the second to find the highest one we hold.
	// SVNode gets that ordering for free from std::map<int32_t, uint256>; a Go
	// slice does not, and lastCheckpointInIndex now gates a disconnect, so an
	// unordered list is refused here rather than silently fencing at the wrong
	// height. Every network go-chaincfg ships is already ascending.
	for i := 1; i < len(cfg.Params.Checkpoints); i++ {
		if cfg.Params.Checkpoints[i].Height <= cfg.Params.Checkpoints[i-1].Height {
			return nil, errors.New(errors.ERR_INVALID_ARGUMENT,
				"svp2p: chain params checkpoints are not ascending by height (%d after %d at index %d)",
				cfg.Params.Checkpoints[i].Height, cfg.Params.Checkpoints[i-1].Height, i)
		}
	}

	// The machine consults no wall clock of its own; the default is bound here
	// so every check downstream can assume a non-nil function.
	if cfg.AdjustedTime == nil {
		cfg.AdjustedTime = func() int64 { return time.Now().Unix() }
	}

	hs := &HeaderSync{cfg: cfg}

	// legacy netsync manager.go New: the checkpoint list comes from the chain
	// params, and stays empty when checkpoints are disabled.
	if !cfg.DisableCheckpoints {
		hs.checkpoints = cfg.Params.Checkpoints
	}

	// legacy netsync manager.go New: seed nextCheckpoint from our current
	// height via resetHeaderState, which leaves headers-first mode off until a
	// sync actually starts.
	hs.resetHeaderState()

	return hs, nil
}

// IsHeadersFirstMode reports whether the node is downloading headers up to a
// checkpoint. Phase 3 reads it to refuse serving getheaders during catch-up,
// the same use legacy netsync makes of it.
func (hs *HeaderSync) IsHeadersFirstMode() bool { return hs.headersFirstMode }

// PeerEstablished is the SendMessages event: a peer finished the handshake, so
// consider starting headers synchronization with it. It returns the initial
// getheaders, or nothing when this peer must not drive the sync.
//
// Peer choice follows net_processing.cpp, which starts with the first eligible
// candidate that reaches SendMessages. Legacy netsync startSync instead ranks
// the candidates and picks the one with the greatest advertised height. Ranking
// needs the whole peer set, which this per-peer machine does not see; if the
// parity harness shows the first-eligible rule picking poor sync peers,
// PeerManager can rank candidates before it calls this.
func (hs *HeaderSync) PeerEstablished(peer *SyncPeer) []wire.Message {
	if peer == nil || peer.State == nil {
		return nil
	}

	// net_processing.cpp SendMessages gates the initial getheaders on
	// !state.fSyncStarted and on fFetch, the "this peer can serve us blocks"
	// predicate. isSyncCandidate carries the legacy reading of fFetch.
	if peer.State.fSyncStarted || !hs.isSyncCandidate(peer) {
		return nil
	}

	// net_processing.cpp SendMessages: "Only actively request headers from a
	// single peer". The C++ relaxes this when pindexBestHeader is within 24
	// hours of now, which only ever adds parallel header syncs; Phase 2 keeps
	// the single-peer rule alone, because HeaderIndex exposes no header
	// timestamp to test the relaxation against. Legacy netsync has the same
	// single-sync-peer behavior.
	if hs.nSyncStarted > 0 {
		return nil
	}

	peer.State.fSyncStarted = true
	hs.nSyncStarted++

	_, tipHeight := hs.cfg.Index.Tip()
	hs.nextCheckpoint = hs.findNextHeaderCheckpoint(tipHeight)

	// legacy netsync manager.go startSync: run headers-first only while a
	// checkpoint is still ahead of us, and never in regression test mode.
	hs.headersFirstMode = hs.nextCheckpoint != nil &&
		tipHeight < hs.nextCheckpoint.Height &&
		!hs.isRegtest()

	locator, anchor := hs.syncStartLocator()
	hs.roundAnchorHeight = anchor

	return []wire.Message{hs.getHeaders(locator)}
}

// PeerDisconnected mirrors net_processing.cpp FinalizeNode: a peer that was
// driving header sync releases the single sync slot when it goes away, and the
// header state resets so the next candidate starts a clean round.
func (hs *HeaderSync) PeerDisconnected(peer *SyncPeer) {
	hs.releaseSyncPeer(peer)
}

// SyncPeerTimedOut releases a sync peer that is still connected but has stopped
// answering, mirroring legacy netsync's sync-peer timeout, which calls
// resetHeaderState and lets startSync choose another peer. The machine reads no
// clock: the caller owns the timeout and calls this when it expires — see
// BlockDownloader.CheckStall, which is that caller. There is no SVNode rule
// behind it. Bitcoin Core times a headers round out against
// HEADERS_DOWNLOAD_TIMEOUT_BASE, but this SVNode fork never took that constant;
// it relies on the peer being disconnected or its in-flight blocks timing out,
// neither of which fires during a silent headers-first round. The timeout is
// therefore the Teranode rotation carried from legacy netsync. The peer stays
// connected; only the sync slot and the header state are released.
func (hs *HeaderSync) SyncPeerTimedOut(peer *SyncPeer) {
	hs.releaseSyncPeer(peer)
}

// releaseSyncPeer frees the single sync slot and resets the header state, the
// FinalizeNode plus resetHeaderState pair. Without the reset,
// IsHeadersFirstMode stays true with nobody syncing, and nextCheckpoint keeps
// pointing at the round that died.
func (hs *HeaderSync) releaseSyncPeer(peer *SyncPeer) {
	if peer == nil || peer.State == nil || !peer.State.fSyncStarted {
		return
	}

	peer.State.fSyncStarted = false

	if hs.nSyncStarted > 0 {
		hs.nSyncStarted--
	}

	hs.resetHeaderState()
}

// resetHeaderState is legacy netsync manager.go resetHeaderState: leave
// headers-first mode and re-seed the checkpoint from where our chain now
// stands. The legacy header list has no counterpart here — HeaderIndex holds
// the headers already accepted, and they stay valid.
func (hs *HeaderSync) resetHeaderState() {
	hs.headersFirstMode = false
	hs.roundAnchorHeight = 0

	_, tipHeight := hs.cfg.Index.Tip()
	hs.nextCheckpoint = hs.findNextHeaderCheckpoint(tipHeight)
}

// OnHeaders is the net_processing.cpp NetMsgType::HEADERS event. It returns
// the messages to send back, the misbehavior score to add to this peer, and an
// error only when the peer must be disconnected outright.
//
// Caller contract on that error, which is either ErrCheckpointMismatch or
// ErrHeadersNoProgress: the machine does not release the round itself. The
// offending peer still holds the sync slot and headers-first mode is still on
// when the error returns. The caller must disconnect the peer and then call
// PeerDisconnected, which frees the slot and resets the header state. This is
// the legacy shape, where DisconnectWithWarning leads to resetHeaderState and a
// fresh startSync; splitting it this way keeps the machine free of any
// knowledge of how a peer is torn down.
func (hs *HeaderSync) OnHeaders(peer *SyncPeer, msg *wire.MsgHeaders) ([]wire.Message, int, error) {
	if peer == nil || peer.State == nil || msg == nil {
		return nil, 0, nil
	}

	headers := msg.Headers

	// net_processing.cpp HEADERS: "headers message size = %u" →
	// Misbehaving(pfrom, 20, "too-many-headers"). The wire decoder already
	// refuses to decode more than MaxBlockHeadersPerMsg, so this only fires
	// for a batch built in-process; it is the rule, kept where the rule lives.
	if len(headers) > MaxHeadersResults {
		return nil, scoreTooManyHeaders, nil
	}

	// net_processing.cpp HEADERS: "Nothing interesting. Stop asking this
	// peer for more headers."
	if len(headers) == 0 {
		return nil, 0, nil
	}

	// Deliberate divergence from both parents, and stricter than either.
	// net_processing.cpp indexes headers from every peer at all times. Legacy
	// netsync manager.go handleHeadersMsg tests the MODE, not the peer — it
	// disconnects anyone who sends headers while headers-first is off — but the
	// round it protects is one it drives with a single sync peer. This port
	// keeps the round single-peer without borrowing the disconnect: while the
	// round runs, a peer that does not hold the sync slot has its batch
	// ignored, so it can neither race the round nor push the tip past the
	// checkpoint the round is working toward. Its announcement still counts for
	// block availability, so it stays usable for download afterwards. Outside
	// the round every peer's headers are indexed, as net_processing.cpp does.
	//
	// The ignored batch is deliberately unscored: junk headers cost a peer
	// nothing but our wire decode here, and any disconnect policy for
	// unrequested headers belongs to the manager, which knows what it asked
	// each peer for.
	//
	// Task 11 deliberately did NOT add that policy, and this is where it would
	// go. The peer loop gained the equivalent gate for unrequested BLOCKS,
	// because those consume the shared admission budget and starve the sync
	// peer; unrequested headers cost only a decode and are already bounded by
	// MAX_HEADERS_RESULTS, so a disconnect policy for them is Phase 3 work,
	// alongside the header-index hardening this file's PoW note describes.
	if hs.headersFirstMode && !peer.State.fSyncStarted {
		peer.State.updateBlockAvailability(hs.cfg.Index, headers[len(headers)-1].BlockHash())

		return nil, 0, nil
	}

	// net_processing.cpp HEADERS: when the first header's parent is unknown and
	// the batch is shorter than MAX_BLOCKS_TO_ANNOUNCE, treat it as a block
	// announcement that outran us — ask for the headers that fill the gap
	// instead of accepting an orphan, and count the event. A bulk batch with an
	// unknown parent falls through to AcceptBlockHeader instead, which fails it
	// with prev-blk-not-found (see acceptHeaders).
	if _, ok := hs.cfg.Index.Lookup(headers[0].PrevBlock); !ok && len(headers) < MaxBlocksToAnnounce {
		peer.nUnconnectingHeaders++

		score := 0
		if peer.nUnconnectingHeaders%MaxUnconnectingHeaders == 0 {
			score = scoreTooManyUnconnectedHeaders
		}

		// net_processing.cpp HEADERS: UpdateBlockAvailability with the last
		// header of the batch even here, so the peer counts as having that
		// block once the gap fills.
		peer.State.updateBlockAvailability(hs.cfg.Index, headers[len(headers)-1].BlockHash())

		return []wire.Message{hs.getHeaders(hs.cfg.Index.Locator())}, score, nil
	}

	// net_processing.cpp HEADERS: the whole batch must be one continuous
	// sequence, checked before any header is accepted →
	// Misbehaving(pfrom, 20, "disconnected headers"),
	// error("non-continuous headers sequence").
	var hashLastBlock chainhash.Hash

	for _, header := range headers {
		if hashLastBlock != (chainhash.Hash{}) && header.PrevBlock != hashLastBlock {
			return nil, scoreNonContinuousHeaders, nil
		}

		hashLastBlock = header.BlockHash()
	}

	return hs.acceptHeaders(peer, headers)
}

// acceptHeaders is the ProcessNewBlockHeaders half of the HEADERS handler: it
// validates and inserts each header, enforces the checkpoint, and decides what
// to ask for next.
func (hs *HeaderSync) acceptHeaders(peer *SyncPeer, headers []*wire.BlockHeader) ([]wire.Message, int, error) {
	var (
		lastAccepted       chainhash.Hash
		lastAcceptedHeight int32
		accepted           int
		sawNewHeader       bool
		reachedCheckpoint  bool
	)

	// The peer holding the sync slot is the one driving the headers-first
	// round; only it can trip the checkpoint or advance the node-global
	// checkpoint state. Outside a round, roundOwner is false for everyone and
	// this machine follows net_processing.cpp instead.
	roundOwner := hs.headersFirstMode && peer.State.fSyncStarted
	checkpointRound := roundOwner && hs.nextCheckpoint != nil

	for _, header := range headers {
		hash := header.BlockHash()

		// AcceptBlockHeader (validation.cpp:6104-6117) checks for a duplicate
		// FIRST and returns early: a header already in mapBlockIndex is
		// answered without running CheckBlockHeader, the checkpoint fence or
		// ContextualCheckBlockHeader. That order is load-bearing here, not an
		// optimisation. Our own history is re-served to us constantly — the
		// start-at-the-parent locator asks for it, and any peer may echo it —
		// and the fence below refuses every NEW header below the last
		// checkpoint. Re-checking a header we already hold would turn that
		// echo into a disconnect.
		//
		// Whether the header is new is also what the round's progress rule at
		// the bottom of this function needs, since AddHeader reports a header
		// already in the index as connected.
		node, known := hs.cfg.Index.Lookup(hash)

		if !known {
			inserted, score, ok, err := hs.acceptHeader(header)
			if err != nil {
				return nil, 0, err
			}

			// A refused header stops the batch where AcceptBlockHeader fails
			// it. The headers already inserted stay, as they do in the source,
			// and the handler returns before the availability update and before
			// any continuation getheaders — net_processing.cpp's early return
			// on a failed ProcessNewBlockHeaders.
			//
			// The score is whatever the failed check declares. It is non-zero
			// for the DoS refusals, which net_processing.cpp applies as
			// Misbehaving(pfrom, nDoS, "invalid header received"), and zero for
			// a state.Invalid refusal such as time-too-new. Reading `ok` rather
			// than testing the score is what keeps those two apart.
			if !ok {
				return nil, score, nil
			}

			node = inserted
			sawNewHeader = true
		}

		lastAccepted = node.Hash
		lastAcceptedHeight = node.Height
		accepted++

		// legacy netsync manager.go handleHeadersMsg: "Verify the header at
		// the next checkpoint height matches", and disconnect the peer when it
		// does not. The loop stops at the checkpoint either way.
		if checkpointRound && node.Height == hs.nextCheckpoint.Height {
			if !node.Hash.IsEqual(hs.nextCheckpoint.Hash) {
				return nil, 0, errors.New(errors.ERR_NETWORK_PEER_MALICIOUS,
					"svp2p: block header at height %d/hash %s does not match expected checkpoint hash %s",
					node.Height, node.Hash, hs.nextCheckpoint.Hash, ErrCheckpointMismatch)
			}

			reachedCheckpoint = true

			break
		}
	}

	if accepted == 0 {
		return nil, 0, nil
	}

	// net_processing.cpp HEADERS: headers that connect clear the unconnecting
	// counter ("resetting nUnconnectingHeaders (%d -> 0)"), so a peer that was
	// briefly behind is not scored for it later.
	peer.nUnconnectingHeaders = 0

	// net_processing.cpp HEADERS: UpdateBlockAvailability with the last header
	// of the batch.
	peer.State.updateBlockAvailability(hs.cfg.Index, lastAccepted)

	if reachedCheckpoint {
		// legacy netsync manager.go: on reaching a checkpoint, run the next
		// round of headers up to the checkpoint after it; when none is left,
		// "Reached the final checkpoint -- switching to normal mode".
		hs.nextCheckpoint = hs.findNextHeaderCheckpoint(hs.nextCheckpoint.Height)
		if hs.nextCheckpoint == nil {
			hs.headersFirstMode = false
		}
	}

	if roundOwner && hs.headersFirstMode {
		// The round must move forward. AddHeader reports a header we already
		// hold as connected, so a peer replaying an old batch would otherwise
		// draw the same getheaders from us for ever, unscored. Legacy netsync
		// catches that peer on its header-list anchor — a replayed header does
		// not connect to the expected parent — and disconnects it.
		//
		// A batch counts as progress if it carried any header the index did not
		// already hold, or if it ended above the height the last request went
		// out from. Both clauses are needed. Height alone rejects an honest
		// fork reply: our tip may sit on a taller branch, so a peer serving its
		// real chain answers below that height with headers that are all new.
		// Novelty alone rejects the honest echo of our own tip that the
		// start-at-the-parent locator asks for. Only a replay — nothing new and
		// no higher — fails both.
		if !sawNewHeader && lastAcceptedHeight <= hs.roundAnchorHeight {
			return nil, 0, errors.New(errors.ERR_NETWORK_INVALID_RESPONSE,
				"svp2p: headers batch ended at height %d with nothing new, not past the height %d it was requested from",
				lastAcceptedHeight, hs.roundAnchorHeight, ErrHeadersNoProgress)
		}

		// legacy netsync manager.go handleHeadersMsg: while the headers-first
		// round runs and the checkpoint is not reached yet, "request the next
		// batch of headers starting from the latest known header and ending
		// with the next checkpoint" — unconditionally, with no batch-length
		// gate. Without this, a peer that answers with a short batch below the
		// checkpoint would leave the round with no outstanding request and no
		// way to resume.
		//
		// The anchor only ever rises. A batch that carried something new but
		// ended below the anchor — an honest fork reply, which the progress
		// test above deliberately admits — must not lower the bar the next
		// batch is measured against, or a peer could walk the anchor down one
		// novel-but-lower batch at a time and then replay everything above it.
		if lastAcceptedHeight > hs.roundAnchorHeight {
			hs.roundAnchorHeight = lastAcceptedHeight
		}

		return []wire.Message{hs.getHeaders(hs.locatorFrom(lastAccepted))}, 0, nil
	}

	// net_processing.cpp HEADERS: "Headers message had its maximum size; the
	// peer may have more headers" → getheaders from the last header received.
	// A shorter batch means the peer has nothing more, so we ask for nothing.
	if len(headers) == MaxHeadersResults {
		return []wire.Message{hs.getHeaders(hs.locatorFrom(lastAccepted))}, 0, nil
	}

	return nil, 0, nil
}

// acceptHeader is the body of validation.cpp AcceptBlockHeader
// (validation.cpp:6087-6154) for a header the index does not already hold:
// CheckBlockHeader, then the previous-block lookup, then the checkpoint fence,
// then ContextualCheckBlockHeader's difficulty rule, then the mapBlockIndex
// insert. The order is the source's, and it matters — the fence and the
// difficulty check both read the parent, and neither may run before the cheap
// proof-of-work check has made the header cost something to produce.
//
// accepted reports whether the header was inserted. When it is false the
// header is refused and the batch stops, and score carries the DoS value the
// failed check declares — which is legitimately ZERO for a state.Invalid
// refusal such as time-too-new, where SVNode refuses the header but neither
// scores nor disconnects the peer. err is set only for a refusal that must
// disconnect the peer.
func (hs *HeaderSync) acceptHeader(header *wire.BlockHeader) (node HeaderNode, score int, accepted bool, err error) {
	// AcceptBlockHeader calls CheckBlockHeader before the mapBlockIndex
	// insert; HeaderIndex.AddHeader deliberately carries only the insert, so
	// the PoW check belongs here.
	if !hs.checkBlockHeaderPoW(header) {
		return HeaderNode{}, scoreInvalidHeader, false, nil
	}

	// FindPreviousBlockIndex. AcceptBlockHeader answers a missing parent with
	// "prev-blk-not-found" → DoS(10). Reachable only for the first header of a
	// bulk batch whose parent we do not have; the continuity scan in OnHeaders
	// guarantees the rest of the batch attaches.
	parent, ok := hs.cfg.Index.Lookup(header.PrevBlock)
	if !ok {
		return HeaderNode{}, scorePrevBlkNotFound, false, nil
	}

	// fCheckpointsEnabled && !CheckIndexAgainstCheckpoint(...).
	fence := hs.lastCheckpointInIndex()
	if !checkIndexAgainstCheckpoint(parent, fence) {
		return HeaderNode{}, 0, false, errors.New(errors.ERR_NETWORK_PEER_MALICIOUS,
			"svp2p: header at height %d forks below the last checkpoint we hold at height %d",
			parent.Height+1, fence.Height, ErrCheckpointFork)
	}

	// ContextualCheckBlockHeader (validation.cpp:5896-5901): "Check proof of
	// work" — the header's nBits must be exactly what GetNextWorkRequired
	// demands for this parent → state.DoS(100, ..., "bad-diffbits").
	//
	// validated is false where this port has no answer: a parent in a historic
	// difficulty era, or one whose DAA window is short. Both are documented on
	// GetNextWorkRequired, and both mean the header is accepted on its own
	// claimed target alone — see the scope note at the top of difficulty.go
	// before assuming otherwise.
	headerTime := header.Timestamp.Unix()

	if want, validated := GetNextWorkRequired(hs.cfg.Index, hs.cfg.Params, parent, headerTime); validated &&
		header.Bits != want {
		return HeaderNode{}, scoreBadDiffBits, false, nil
	}

	// ContextualCheckBlockHeader's "Check timestamp against prev" —
	// time-too-old, block.GetBlockTime() <= pindexPrev->GetMedianTimePast()
	// (validation.cpp:5904-5907) — SITS HERE IN THE SOURCE AND IS NOT PORTED.
	// It needs median-time-past over the header index, which is a piece of
	// machinery this index does not have (the 11-block median CBlockIndex
	// caches at insert). Booked as a named Phase 3 follow-up: "port
	// median-time-past to HeaderIndex and add the time-too-old rule". Until it
	// lands, a header may claim any timestamp at or before its parent's, so
	// the timestamp sequence of an indexed branch is not monotonic.
	//
	// ContextualCheckBlockHeader "Check timestamp"
	// (validation.cpp:5909-5913): a header more than MAX_FUTURE_BLOCK_TIME
	// past the adjusted time is refused with
	// state.Invalid(false, REJECT_INVALID, "time-too-new", ...).
	//
	// state.Invalid is DoS(0), and net_processing.cpp calls Misbehaving only
	// when nDoS > 0, so this refusal neither scores nor disconnects the peer —
	// deliberately, because it is the ONLY refusal in this function that a
	// well-behaved peer can trigger and that time alone will undo. The header
	// may be perfectly valid a few minutes from now, and the peer re-announces
	// it; scoring an honest peer for our own clock would be wrong.
	if headerTime > hs.cfg.AdjustedTime()+MaxFutureBlockTime {
		return HeaderNode{}, 0, false, nil
	}

	// AddToBlockIndex.
	connected, err := hs.cfg.Index.AddHeader(header)
	if err != nil {
		return HeaderNode{}, 0, false, err
	}

	// Defensive: the parent lookup above already proved the header attaches,
	// and nothing writes to the index between the two calls, so this cannot
	// fire. It scores the source's value rather than inserting nothing and
	// carrying on.
	if !connected {
		return HeaderNode{}, scorePrevBlkNotFound, false, nil
	}

	node, ok = hs.cfg.Index.Lookup(header.BlockHash())
	if !ok {
		return HeaderNode{}, 0, false, errors.New(errors.ERR_SERVICE_ERROR,
			"svp2p: header %s vanished from the index directly after insert", header.BlockHash())
	}

	return node, 0, true, nil
}

// checkIndexAgainstCheckpoint is validation.cpp CheckIndexAgainstCheckpoint
// (validation.cpp:5856-5884), second half: "Don't accept any forks from the
// main chain prior to last checkpoint." A new header whose own height falls
// below the last checkpoint we hold is refused, because our chain already
// reaches that checkpoint and anything branching in below it is a fork we can
// never adopt.
//
// The source's first half — Checkpoints::CheckBlock, the hash-must-match rule
// at a checkpoint height — is not repeated here. This port already carries it
// in acceptHeaders, from legacy netsync manager.go, where it is scoped to the
// headers-first round and answers with ErrCheckpointMismatch. Running it twice
// would give one rule two different refusal shapes.
//
// WHAT THAT COSTS. The two rules cover different sets, and the omission leaves
// a real gap rather than a redundancy. Take a checkpoint height ABOVE the
// highest checkpoint currently in the index — one the sync has not reached yet:
//
//   - this fence does not refuse a wrong-hash header there, because the header's
//     height is above the fence height, which is all this rule tests;
//   - the round's check does not see it either, unless the header arrives from
//     the peer holding the sync slot while headers-first mode is running.
//
// So a NON-SYNC peer's header claiming a wrong hash at an unreached checkpoint
// height is checked by neither rule, and is indexed if it satisfies the
// difficulty and timestamp gates. It cannot mislead the sync round, which
// re-checks the checkpoint when it gets there, and it cannot take the tip
// without out-working the honest chain. It does cost an index entry. Closing it
// means porting CheckBlock here and accepting two refusal shapes for one rule,
// or unifying both onto this one — a deliberate design change, not an edit.
//
// It runs for every new header, in a round or outside one, which is what
// bounds HeaderIndex growth below the final checkpoint: a peer cannot spend a
// map entry on a header at a height our chain has already settled.
//
// The source resolves the fence itself with Checkpoints::GetLastCheckpoint;
// here the caller resolves it through lastCheckpointInIndex and passes it in,
// so the rule stays a pure function of the two values it compares and the
// index is read once per header. A nil checkpoint is both "we have reached
// none of them" and the source's fCheckpointsEnabled being off, which
// DisableCheckpoints expresses by leaving hs.checkpoints empty.
func checkIndexAgainstCheckpoint(parent HeaderNode, checkpoint *chaincfg.Checkpoint) bool {
	if checkpoint == nil {
		return true
	}

	// int32_t nHeight = pindexPrev->GetHeight() + 1;
	return parent.Height+1 >= checkpoint.Height
}

// lastCheckpointInIndex is checkpoints.cpp Checkpoints::GetLastCheckpoint
// (checkpoints.cpp:24-36): the highest checkpoint whose hash the index holds,
// or nil when we have reached none of them. It walks the list in reverse, as
// the source does with boost::adaptors::reverse over the ordered map.
//
// The fence therefore tightens as the chain grows: it does nothing on a fresh
// node, and settles at the final checkpoint once we are past it. That is the
// source's behavior, and it is why the fence cannot lock out a node that is
// still syncing toward its first checkpoint.
//
// The reverse walk assumes hs.checkpoints is ASCENDING by height — the source
// gets that from std::map<int32_t, uint256>, a Go slice does not. NewHeaderSync
// refuses to build a machine from an unordered list, so the assumption is
// enforced at construction rather than trusted here; without it this function
// could answer a checkpoint that is not the highest one held, and fence at the
// wrong height on a rule that disconnects.
func (hs *HeaderSync) lastCheckpointInIndex() *chaincfg.Checkpoint {
	for i := len(hs.checkpoints) - 1; i >= 0; i-- {
		checkpoint := &hs.checkpoints[i]
		if checkpoint.Hash == nil {
			continue
		}

		if _, ok := hs.cfg.Index.Lookup(*checkpoint.Hash); ok {
			return checkpoint
		}
	}

	return nil
}

// checkBlockHeaderPoW is pow.cpp CheckProofOfWork, the cheap per-header check
// validation.cpp CheckBlockHeader runs: the target the header's own nBits
// claims must be in range, and the header hash must be at or below it. It
// needs no ancestor context.
//
// Phase 2 left the contextual difficulty check unported, which let a peer feed
// difficulty-1 headers above the final checkpoint at no cost and grow
// HeaderIndex by one unevictable map entry each. Phase 3 Task 2 narrowed that:
// acceptHeader now runs checkIndexAgainstCheckpoint, GetNextWorkRequired and
// the time-too-new cap on every new header from every peer, in a round or
// outside one.
//
// The honest scope of that, because "closed" would overstate it:
//
//   - mainnet past the final checkpoint (945000) — its steady state — is fully
//     bounded: every header there is post-DAA and costs real difficulty.
//   - mainnet and STN during IBD are bounded below the highest checkpoint the
//     index holds and above daaHeight, with a transient unpriced band between
//     the two that closes as the sync passes each checkpoint. A node holding no
//     checkpoint yet has no lower bound.
//   - testnet, teratestnet and tstn are NOT bounded by difficulty at all: their
//     min-difficulty rule makes cheap headers legitimate. time-too-new caps how
//     DEEP a cheap chain can run (about six headers) but not how WIDE, so a peer
//     may still put many cheap siblings on one parent. SVNode has the same
//     exposure on the same networks.
//
// Still unported, deliberately: the historic difficulty eras (see difficulty.go)
// and the time-too-old rule (see acceptHeader).
func (hs *HeaderSync) checkBlockHeaderPoW(header *wire.BlockHeader) bool {
	var bits model.NBit

	binary.LittleEndian.PutUint32(bits[:], header.Bits)

	// NBit.CalculateTarget already carries arith_uint256::SetCompact's
	// negative and overflow rejection, returning a zero target for both.
	target := bits.CalculateTarget()
	if target.Sign() <= 0 {
		return false
	}

	// pow.cpp: bnTarget > powLimit is out of range.
	if hs.cfg.Params.PowLimit != nil && target.Cmp(hs.cfg.Params.PowLimit) > 0 {
		return false
	}

	hash := header.BlockHash()

	bigEndian := make([]byte, len(hash))
	for i := range hash {
		bigEndian[len(hash)-1-i] = hash[i]
	}

	return new(big.Int).SetBytes(bigEndian).Cmp(target) <= 0
}

// findNextHeaderCheckpoint returns the next checkpoint after height, or nil
// when height is already at or past the final one, or when there are no
// checkpoints. Ported from legacy netsync manager.go.
func (hs *HeaderSync) findNextHeaderCheckpoint(height int32) *chaincfg.Checkpoint {
	if len(hs.checkpoints) == 0 {
		return nil
	}

	finalCheckpoint := &hs.checkpoints[len(hs.checkpoints)-1]
	if height >= finalCheckpoint.Height {
		return nil
	}

	nextCheckpoint := finalCheckpoint

	for i := len(hs.checkpoints) - 2; i >= 0; i-- {
		if height >= hs.checkpoints[i].Height {
			break
		}

		nextCheckpoint = &hs.checkpoints[i]
	}

	return nextCheckpoint
}

// isSyncCandidate reports whether we may sync from this peer, ported from
// legacy netsync manager.go isSyncCandidate.
func (hs *HeaderSync) isSyncCandidate(peer *SyncPeer) bool {
	// Regression test is special: the regression tool is not a full node and
	// still needs to be a sync candidate, so the service flag is not required
	// and a localhost restriction takes its place.
	if hs.isRegtest() {
		if hs.cfg.AllowSyncCandidateFromLocalPeers {
			return true
		}

		host, _, err := net.SplitHostPort(peer.Addr)
		if err != nil {
			return false
		}

		return host == "127.0.0.1" || host == "localhost"
	}

	// Outside regtest, a peer that is not a full node cannot serve us blocks.
	return peer.Services&wire.SFNodeNetwork == wire.SFNodeNetwork
}

// isRegtest reports whether the active params are the regression network. The
// legacy port compares the params pointer with &chaincfg.RegressionNetParams;
// this machine compares the network magic instead, so a copied Params value is
// still recognized. The legacy pointer comparison is an artifact of its test
// harness, not a protocol rule.
func (hs *HeaderSync) isRegtest() bool {
	return hs.cfg.Params.Net == wire.RegTestNet
}

// syncStartLocator builds the locator for the initial getheaders, and returns
// the height it was built from for the round anchor.
// net_processing.cpp SendMessages: "If possible, start at the block preceding
// the currently best known header. This ensures that we always get a
// non-empty list of headers back as long as the peer is up-to-date." That
// non-empty list starts with our own tip, so the anchor is the parent's
// height: a peer at our height answers with one header we already have and is
// still making the only progress it can.
func (hs *HeaderSync) syncStartLocator() (locator []chainhash.Hash, anchorHeight int32) {
	tipHash, tipHeight := hs.cfg.Index.Tip()

	if node, ok := hs.cfg.Index.Lookup(tipHash); ok && node.ParentHash != (chainhash.Hash{}) {
		if locator := hs.cfg.Index.LocatorFrom(node.ParentHash); locator != nil {
			return locator, node.Height - 1
		}
	}

	return hs.cfg.Index.Locator(), tipHeight
}

// locatorFrom builds a locator for hash, falling back to the tip locator if
// the hash left the index between the accept and this call.
func (hs *HeaderSync) locatorFrom(hash chainhash.Hash) []chainhash.Hash {
	if locator := hs.cfg.Index.LocatorFrom(hash); locator != nil {
		return locator
	}

	return hs.cfg.Index.Locator()
}

// getHeaders builds the getheaders message for a locator. While headers-first
// mode runs, hashStop is the next checkpoint hash — the legacy netsync scheme
// that asks a peer for exactly the headers up to the next checkpoint.
// Otherwise hashStop is the zero hash net_processing.cpp always sends.
func (hs *HeaderSync) getHeaders(locator []chainhash.Hash) *wire.MsgGetHeaders {
	msg := wire.NewMsgGetHeaders()
	msg.ProtocolVersion = wire.ProtocolVersion

	// A locator holds one hash per height step with an exponential tail, so it
	// stays far below MaxBlockLocatorsPerMsg (500) at any reachable height.
	for i := range locator {
		hash := locator[i]
		msg.BlockLocatorHashes = append(msg.BlockLocatorHashes, &hash)
	}

	if hs.headersFirstMode && hs.nextCheckpoint != nil {
		msg.HashStop = *hs.nextCheckpoint.Hash
	}

	return msg
}

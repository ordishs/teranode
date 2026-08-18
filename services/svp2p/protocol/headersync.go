package protocol

import (
	"encoding/binary"
	"math/big"
	"net"

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
)

// ErrCheckpointMismatch reports a header at a checkpoint height whose hash is
// not the checkpoint hash. Legacy netsync manager.go answers this with
// DisconnectWithWarning, so the caller must drop the peer rather than score
// it. It shares ERR_NETWORK_PEER_MALICIOUS with ErrProtocolViolation on
// purpose: the teranode errors package matches errors.Is by code, and both
// sentinels mean the same thing to a caller — disconnect this peer.
var ErrCheckpointMismatch = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: header does not match checkpoint")

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
// clock: the caller owns the timeout (SVNode measures it in SendMessages
// against HEADERS_DOWNLOAD_TIMEOUT_BASE), and calls this when it expires. The
// peer stays connected; only the sync slot and the header state are released.
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

	// legacy netsync manager.go handleHeadersMsg returns early for any headers
	// that arrive outside the headers-first round it drives with its single
	// sync peer. Carry that scope: while the round runs, a peer that does not
	// hold the sync slot cannot put headers into the index at all, so it can
	// neither race the round nor push the tip past the checkpoint the round is
	// working toward. Its announcement still counts for block availability, so
	// it stays usable for download afterwards. Outside the round every peer's
	// headers are indexed, as net_processing.cpp does.
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
		reachedCheckpoint  bool
	)

	// The peer holding the sync slot is the one driving the headers-first
	// round; only it can trip the checkpoint or advance the node-global
	// checkpoint state. Outside a round, roundOwner is false for everyone and
	// this machine follows net_processing.cpp instead.
	roundOwner := hs.headersFirstMode && peer.State.fSyncStarted
	checkpointRound := roundOwner && hs.nextCheckpoint != nil

	for _, header := range headers {
		// AcceptBlockHeader calls CheckBlockHeader before the mapBlockIndex
		// insert; HeaderIndex.AddHeader deliberately carries only the insert,
		// so the PoW check belongs here.
		if !hs.checkBlockHeaderPoW(header) {
			return nil, scoreInvalidHeader, nil
		}

		connected, err := hs.cfg.Index.AddHeader(header)
		if err != nil {
			return nil, 0, err
		}

		// AcceptBlockHeader: "prev-blk-not-found" → DoS(10). Reachable only for
		// the first header of a bulk batch whose parent we do not have; the
		// continuity scan guarantees the rest of the batch attaches.
		if !connected {
			return nil, scorePrevBlkNotFound, nil
		}

		node, ok := hs.cfg.Index.Lookup(header.BlockHash())
		if !ok {
			break
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
		// not connect to the expected parent — and disconnects it; the anchor
		// height is this port's equivalent of that expectation.
		if lastAcceptedHeight <= hs.roundAnchorHeight {
			return nil, 0, errors.New(errors.ERR_NETWORK_INVALID_RESPONSE,
				"svp2p: headers batch ended at height %d, not past the height %d it was requested from",
				lastAcceptedHeight, hs.roundAnchorHeight, ErrHeadersNoProgress)
		}

		// legacy netsync manager.go handleHeadersMsg: while the headers-first
		// round runs and the checkpoint is not reached yet, "request the next
		// batch of headers starting from the latest known header and ending
		// with the next checkpoint" — unconditionally, with no batch-length
		// gate. Without this, a peer that answers with a short batch below the
		// checkpoint would leave the round with no outstanding request and no
		// way to resume.
		hs.roundAnchorHeight = lastAcceptedHeight

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

// checkBlockHeaderPoW is pow.cpp CheckProofOfWork, the cheap per-header check
// validation.cpp CheckBlockHeader runs: the target the header's own nBits
// claims must be in range, and the header hash must be at or below it. It
// needs no ancestor context.
//
// Phase 2 does not carry the contextual difficulty check
// (ContextualCheckBlockHeader / GetNextWorkRequired), and the bound on that gap
// is narrower than it looks. Checkpoint enforcement covers only the
// headers-first round: it applies to the sync peer, up to the next checkpoint.
// The mainnet checkpoint list in chaincfg ends at height 868500, so above that
// height the only gate left on a received header is this check: a peer can feed
// difficulty-1 headers that cost nothing to grind, and HeaderIndex grows one
// unbounded map entry per header with no eviction. Restricting the round to the
// sync peer bounds who can do it only while a round runs, by design — outside a
// round every peer's headers are indexed, as net_processing.cpp does. Phase 3
// must close it with one of: the contextual GetNextWorkRequired check, a
// CheckIndexAgainstCheckpoint port that refuses headers forking below the last
// checkpoint, or a height/size cap on the index. This is spec §6 header-index
// hardening, and it is tracked in the Task 5 report.
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

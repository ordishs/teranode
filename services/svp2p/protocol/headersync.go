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

// Misbehavior scores for headers processing, from net_processing.cpp.
const (
	scoreTooManyHeaders            = 20 // Misbehaving(pfrom, 20, "too-many-headers")
	scoreNonContinuousHeaders      = 20 // Misbehaving(pfrom, 20, "disconnected headers")
	scoreTooManyUnconnectedHeaders = 20 // Misbehaving(pfrom, 20, "too-many-unconnected-headers")

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

	hs := &HeaderSync{cfg: cfg}

	// legacy netsync manager.go New: the checkpoint list comes from the chain
	// params, and stays empty when checkpoints are disabled.
	if !cfg.DisableCheckpoints {
		hs.checkpoints = cfg.Params.Checkpoints
	}

	// legacy netsync manager.go New: seed nextCheckpoint from our current
	// height, then resetHeaderState, which leaves headers-first mode off
	// until a sync actually starts.
	_, tipHeight := cfg.Index.Tip()
	hs.nextCheckpoint = hs.findNextHeaderCheckpoint(tipHeight)

	return hs, nil
}

// IsHeadersFirstMode reports whether the node is downloading headers up to a
// checkpoint. Phase 3 reads it to refuse serving getheaders during catch-up,
// the same use legacy netsync makes of it.
func (hs *HeaderSync) IsHeadersFirstMode() bool { return hs.headersFirstMode }

// PeerEstablished is the SendMessages event: a peer finished the handshake, so
// consider starting headers synchronization with it. It returns the initial
// getheaders, or nothing when this peer must not drive the sync.
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

	return []wire.Message{hs.getHeaders(hs.syncStartLocator())}
}

// PeerDisconnected mirrors net_processing.cpp FinalizeNode: a peer that was
// driving header sync releases the single sync slot when it goes away.
func (hs *HeaderSync) PeerDisconnected(peer *SyncPeer) {
	if peer == nil || peer.State == nil || !peer.State.fSyncStarted {
		return
	}

	peer.State.fSyncStarted = false

	if hs.nSyncStarted > 0 {
		hs.nSyncStarted--
	}
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

	// net_processing.cpp HEADERS: when the first header's parent is unknown,
	// ask for the headers that fill the gap instead of accepting an orphan,
	// and count the event.
	if _, ok := hs.cfg.Index.Lookup(headers[0].PrevBlock); !ok {
		peer.nUnconnectingHeaders++

		score := 0
		if peer.nUnconnectingHeaders%MaxUnconnectingHeaders == 0 {
			score = scoreTooManyUnconnectedHeaders
		}

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
		lastAccepted      chainhash.Hash
		accepted          int
		reachedCheckpoint bool
	)

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

		// Unreachable: the parent of headers[0] was found above and the batch
		// is continuous, so every header attaches. Stop rather than carry on
		// with a header that is not in the index.
		if !connected {
			break
		}

		node, ok := hs.cfg.Index.Lookup(header.BlockHash())
		if !ok {
			break
		}

		lastAccepted = node.Hash
		accepted++

		// legacy netsync manager.go handleHeadersMsg: "Verify the header at
		// the next checkpoint height matches", and disconnect the peer when it
		// does not. The loop stops at the checkpoint either way.
		if hs.headersFirstMode && hs.nextCheckpoint != nil && node.Height == hs.nextCheckpoint.Height {
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

	// net_processing.cpp HEADERS: UpdateBlockAvailability with the last header
	// of the batch.
	peer.State.updateBlockAvailability(hs.cfg.Index, lastAccepted)

	if reachedCheckpoint {
		// legacy netsync manager.go: on reaching a checkpoint, run the next
		// round of headers up to the checkpoint after it; when none is left,
		// "Reached the final checkpoint -- switching to normal mode".
		hs.nextCheckpoint = hs.findNextHeaderCheckpoint(hs.nextCheckpoint.Height)
		if hs.nextCheckpoint != nil {
			return []wire.Message{hs.getHeaders(hs.locatorFrom(lastAccepted))}, 0, nil
		}

		hs.headersFirstMode = false
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
// needs no ancestor context. The contextual difficulty-adjustment check
// (ContextualCheckBlockHeader / GetNextWorkRequired) is not carried in Phase
// 2; while headers-first mode runs, the checkpoint bounds what a peer can feed
// us, and Phase 3's block validation re-checks every header it accepts.
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

// syncStartLocator builds the locator for the initial getheaders.
// net_processing.cpp SendMessages: "If possible, start at the block preceding
// the currently best known header. This ensures that we always get a
// non-empty list of headers back as long as the peer is up-to-date."
func (hs *HeaderSync) syncStartLocator() []chainhash.Hash {
	tipHash, _ := hs.cfg.Index.Tip()

	if node, ok := hs.cfg.Index.Lookup(tipHash); ok && node.ParentHash != (chainhash.Hash{}) {
		if locator := hs.cfg.Index.LocatorFrom(node.ParentHash); locator != nil {
			return locator
		}
	}

	return hs.cfg.Index.Locator()
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

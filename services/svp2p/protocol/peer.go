package protocol

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/svp2p/transport"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// BlockIngestor is this package's whole view of Teranode-side block
// ingestion. Spec §4.4 forbids protocol from importing the bridge or any
// Teranode client, so the interface is declared here, where the peer loop
// consumes it, and implemented by a thin adapter in the svp2p package over
// bridge.Bridge and bridge.Admission.
type BlockIngestor interface {
	// WatchProgress wraps a block's transaction stream so the peer loop can
	// tell a slow-but-live ingest from a peer that went silent mid-payload,
	// while Ingest runs on another goroutine. The adapter returns
	// bridge.NewProgressReader.
	WatchProgress(r io.ReadCloser) IngestProgress

	// Ingest admits one block against the bridge's admission gate and runs it
	// through the ingestion pipeline. It must release req.TxReader on every
	// exit path: the transport read loop stays parked on this connection
	// until the stream closes.
	Ingest(ctx context.Context, req BlockIngestRequest) IngestOutcome
}

// IngestProgress is the observable half of a block's transaction stream,
// satisfied by bridge.ProgressReader.
type IngestProgress interface {
	io.ReadCloser

	// BytesRead reports the payload bytes the ingest has taken off the stream.
	BytesRead() uint64

	// LastProgress reports when bytes last moved, or the construction time
	// when none have yet. It is never the zero time.
	LastProgress() time.Time
}

// BlockIngestRequest is one streamed block handed to the ingestor.
type BlockIngestRequest struct {
	// Header is the 80 byte header the transport already decoded.
	Header *wire.BlockHeader

	// TxCount is the transaction count the peer declared.
	TxCount uint64

	// SizeBytes is the payload length the peer declared. It is the only
	// honest weight for the admission budget, and only the transport has it.
	SizeBytes uint64

	// TxReader is the progress-wrapped transaction stream.
	TxReader io.ReadCloser

	// PeerAddr identifies the sending peer, for logging only.
	PeerAddr string

	// Quit is closed when the peer goes away, so a parked admission wait is
	// released on teardown as well as on ctx cancellation.
	Quit <-chan struct{}
}

// IngestOutcome is what the ingestor reports back. The flags separate the
// cases the peer loop and the scheduler must treat differently; Err carries
// the underlying failure for logging in all of them.
type IngestOutcome struct {
	// Err is nil only when the block was ingested.
	Err error

	// Duplicate reports bridge.ErrDuplicateBlockInFlight: another copy of the
	// same hash owns the admission slot and the download record, and will
	// report on its own completion. Benign; the peer is not at fault.
	Duplicate bool

	// Rotate reports a pre-admission deadline (bridge.PreAdmitTimedOut): a
	// wedged local round-trip stranded a requested block, so the sync peer
	// must rotate. The peer stays connected, and a rotation NEVER disconnects
	// — the rotation has already released the slot and the peer's downloads,
	// so disconnecting would run that release a second time.
	Rotate bool

	// PeerFault reports that the peer is to blame: the pipeline rejected what
	// it sent. It is the only INGEST OUTCOME that disconnects a peer — Peer.Run
	// documents the other six ways a peer is dropped — and it is set explicitly
	// rather than derived from the absence of another flag, so a fault nobody
	// classified cannot silently become a disconnect.
	PeerFault bool

	// ParentMissing reports that the block's parent is not in our chain yet, so
	// the pre-admission check refused it (bridge PreAdmit). It is a local fault
	// like TransientLocal, which is also set, and it carries the extra fact the
	// scheduler needs: this block is worth having, and worth asking for again,
	// but not until its parent is here. Without the distinction the block goes
	// straight back on offer and is fetched again every tick until the parent
	// lands — see ParentMissingRetryDelay.
	ParentMissing bool

	// TransientLocal reports a local storage or service fault, including the
	// admission backoff skip. Admission.SkipForBackoff's caller contract: the
	// delivering peer did its job, so refresh its stall clock and never
	// rotate or penalize it for our own fault.
	TransientLocal bool
}

// TxIngestor is this package's whole view of Teranode-side transaction
// ingestion, the TxIngestor counterpart to BlockIngestor above. Spec §4.4
// forbids protocol from importing the bridge or any Teranode client, so this
// interface is declared here, where the peer loop consumes it, and
// implemented by a thin adapter in the svp2p package over bridge.Bridge
// (services/svp2p/ingest.go).
type TxIngestor interface {
	// Ingest runs one inbound transaction through the validator. It must
	// never be called from the peer's Run goroutine directly: validation is
	// slow, and Run has to keep servicing pings, the idle timer and shutdown
	// while it happens. See txIngestLoop for where this actually runs and
	// why that keeps Run unblocked.
	Ingest(ctx context.Context, msg *wire.MsgTx, peerAddr string) TxIngestOutcome

	// Rejected reports whether hash is in the ingest-side rejected-tx set
	// (bridge.Bridge.TxRejected). This is Task 16's seam: the inv handler's
	// "already rejected, skip the getdata" suppression
	// (services/legacy/netsync/manager.go:2400) is made reachable through
	// this method but is not called from anywhere in this package today —
	// BlockDownloader.OnInv's InvTypeTx case stops at Decision 1 (Phase 2)
	// and Task 16 is who extends it.
	Rejected(hash chainhash.Hash) bool
}

// TxIngestOutcome is what one Ingest call reports. Unlike IngestOutcome
// (blocks), no outcome here ever disconnects the peer: legacy's own
// handleTxMsg neither scores nor disconnects a peer for an invalid or
// unsolicited transaction (services/legacy/netsync/manager.go:1208-1215, the
// BitcoinJ-interop comment — carried here, not reinvented; Task 11's own
// review turned on the same principle, that a false-positive ban is worse
// than a miss).
type TxIngestOutcome struct {
	// Err is a local/processing failure (e.g. malformed tx bytes). Logged
	// only; never treated as the peer's fault.
	Err error

	// Accepted reports the transaction validated and fed the tx
	// announcement relay (Task 13's txAnnouncer.put seam, composed in the
	// svp2p-package adapter that implements this interface).
	Accepted bool

	// Orphan reports a missing-parent/locked classification. Task 15 owns
	// the orphan transaction pool; nothing further happens here.
	Orphan bool

	// Reject is the wire.MsgReject to send to the peer, or nil when nothing
	// should be sent: accepted, orphan, or an already-rejected tx that
	// legacy deliberately does not re-reject
	// (services/legacy/netsync/manager.go:1218-1224).
	Reject *wire.MsgReject
}

// syncDispatcher is the peer loop's view of the manager-owned sync machines
// (HeaderSync and BlockDownloader, both behind PeerManager's shared
// sync-state mutex). PeerManager implements it. Every method takes that mutex
// itself, so the peer loop must call them WITHOUT holding the peer lock: the
// package lock order is peer lock, then manager lock.
//
// Three of the methods below can end the connection by returning an error, and
// none of them tears anything down itself — the caller disconnects. Peer.Run
// carries the full list of what disconnects a peer, including the causes that
// never pass through this interface.
type syncDispatcher interface {
	// Established is the SendMessages event for a finished handshake.
	Established(sp *SyncPeer, services wire.ServiceFlag) []wire.Message

	// Headers dispatches a headers message; the int is a misbehavior delta
	// and a non-nil error means this peer must be disconnected.
	Headers(sp *SyncPeer, msg *wire.MsgHeaders) ([]wire.Message, int, error)

	// Inv dispatches an inv message; a non-nil error means this peer must be
	// disconnected.
	Inv(sp *SyncPeer, msg *wire.MsgInv) ([]wire.Message, error)

	// GetHeaders and GetBlocks dispatch the two chain-query messages. Neither
	// can end the connection: SVNode answers both from the block index and
	// scores nothing, and the one refusal either of them has (headers-first
	// catch-up) is a decision about US, not about the peer.
	GetHeaders(sp *SyncPeer, msg *wire.MsgGetHeaders) []wire.Message
	GetBlocks(sp *SyncPeer, msg *wire.MsgGetBlocks) []wire.Message

	// GetData classifies one getdata request. Like the two above it cannot end
	// the connection: a getdata only REQUESTS data, so an entry we cannot
	// answer is a gap on our side (serving.go OnGetData).
	GetData(sp *SyncPeer, msg *wire.MsgGetData) []getDataItem

	// ContinueInv is the getdata continuation: after a served block, an inv of
	// our tip when that block was the one that closed a full getblocks reply.
	ContinueInv(sp *SyncPeer, hash chainhash.Hash) []wire.Message

	// BlockExpected reports whether hash is in flight from this peer, which is
	// what makes an inbound block solicited.
	BlockExpected(sp *SyncPeer, hash chainhash.Hash) bool

	// BlockDone reports one block's ingest outcome to the scheduler. A non-nil
	// error means this peer must be disconnected.
	BlockDone(sp *SyncPeer, hash chainhash.Hash, outcome IngestOutcome) error
}

type PeerConfig struct {
	Handshake    HandshakeConfig
	Conn         *transport.Conn
	Logger       ulogger.Logger
	IdleTimeout  time.Duration
	PingInterval time.Duration
	BanThreshold int

	// Sync is the manager's sync dispatch, or nil when block sync is not
	// wired (the Phase 1 shape: handshake and ping only).
	Sync syncDispatcher

	// SyncPeer is this peer's CNodeState entry, owned by PeerManager and
	// mutated only under its sync-state mutex.
	SyncPeer *SyncPeer

	// Ingestor is the Teranode-side block ingestion path, or nil when it is
	// not wired.
	Ingestor BlockIngestor

	// Fetcher is the Teranode-side read path a getdata is answered from, or
	// nil when it is not wired. Without it a getdata is ignored: see
	// queueGetData.
	Fetcher BlockTxFetcher

	// TxIngestor is the Teranode-side transaction ingestion path, or nil when
	// it is not wired. Without it an inbound tx is dropped before it is even
	// queued (queueTx).
	TxIngestor TxIngestor

	// MarkTxKnown records that this peer is the origin of an inbound tx, so
	// the tx announcement relay (PeerManager.RelayTxs) never re-announces it
	// back to the peer that sent it — legacy's own AddKnownInventory-before-
	// queueing rule (services/legacy/peer_server.go:906-908), the tx-side
	// counterpart of PeerManager.Inv's unconditional knownBlocks.mark for
	// blocks. nil when sync is not wired; dispatchTx no-ops in that case.
	MarkTxKnown func(sp *SyncPeer, hash chainhash.Hash)
}

type PeerSnapshot struct {
	Addr             string
	Inbound          bool
	UserAgent        string
	ProtocolVersion  uint32
	StartingHeight   int32
	BytesSent        uint64
	BytesReceived    uint64
	ConnectedAt      time.Time
	LastRecv         time.Time
	MisbehaviorScore int

	// TxDropped counts inbound txs this peer had refused entry to txMsgCh
	// because it was already full (queueTx) — sustained loss that would
	// otherwise be invisible in production (review round 1, Minor 5). Not
	// yet surfaced over the peer_api gRPC surface (GetPeers); that is a
	// natural follow-up, not done here, disclosed rather than silent.
	TxDropped uint64
}

// Peer owns one connection's runtime: it feeds inbound messages through the
// handshake state machine, sends the machine's replies, keeps the SVNode
// ping cadence, and enforces the idle timeout and ban threshold.
// The handshake machine is mutated only from the Run goroutine; Info reads
// it under mu, and Run takes mu around every mutation.
type Peer struct {
	cfg         PeerConfig
	hs          *Handshake
	established chan struct{}
	estOnce     sync.Once
	mu          sync.Mutex
	lastRecv    time.Time
	connectedAt time.Time
	discErr     error
	discOnce    sync.Once

	// gone is closed when Run returns, releasing an ingest parked on the
	// admission budget.
	gone     chan struct{}
	goneOnce sync.Once

	// ingestCh carries each finished ingest back to the Run goroutine.
	ingestCh chan ingestReport

	// getData is this peer's pending inventory requests, the port of
	// CNode::vRecvGetData: a deque the Run goroutine appends classified entries
	// to and the serve goroutine drains one pass at a time, so a request the
	// pass could not finish is retained rather than dropped (getdata.go).
	// getDataWake signals the serve goroutine that entries are waiting.
	getDataMu   sync.Mutex
	getData     []getDataItem
	getDataWake chan struct{}

	// ingest, ingestStarted, ingestTxBytes and ingestActive are written by the
	// Run goroutine and read by it plus the manager's stall ticker through
	// IngestSnapshot, so they are guarded by mu like the rest of the peer's
	// shared state. ingest is the newest running ingest's progress handle;
	// ingestTxBytes is how many transaction bytes that ingest's stream yields
	// in total; ingestActive counts the ingests still to report.
	ingest        IngestProgress
	ingestStarted time.Time
	ingestTxBytes uint64
	ingestActive  int

	// txMsgCh carries inbound tx messages to txIngestLoop, the single
	// per-peer worker goroutine that actually calls TxIngestor.Ingest. See
	// txIngestQueueDepth for the bound and its rationale.
	txMsgCh chan *wire.MsgTx

	// txDropped counts queueTx's own drops (txMsgCh full). Surfaced through
	// PeerSnapshot.TxDropped; see that field's own doc comment.
	txDropped atomic.Uint64
}

// txIngestQueueDepth bounds how many not-yet-validated txs one peer can have
// queued before a further tx from it is dropped rather than piling up
// unboundedly in memory (queueTx). Legacy has no true per-peer analogue:
// its whole SyncManager is served by ONE global goroutine draining one
// shared channel across every peer and every message type
// (services/legacy/netsync/manager.go:130, maxMsgQueueSize = 10_000). This
// package instead gives each peer its own worker (txIngestLoop) so one
// peer's slow validation cannot starve another's, at the cost of a bound
// chosen per peer rather than once for the whole node.
//
// The bound is on message COUNT, not bytes (review round 1, Important 3):
// each of the 100 slots retains a whole *wire.MsgTx, decoded, not a fixed-
// size record, so "100 pointers, negligible" — this comment's own earlier,
// incorrect claim — understated the real cost by ignoring what each
// pointer keeps alive. The honest per-entry ceiling is whatever go-wire
// currently allows one MsgTx to declare: MsgTx.MaxPayloadLength returns
// MaxBlockPayload() (go-wire msg_tx.go), i.e. a tx has NO size limit of its
// own distinct from a block's. That ceiling tracks the excessive-block-size
// value passed to wire.SetLimits, which for every node running this
// package is 4,000,000,000 (Server.Init, unconditional) — NOT go-wire's
// own pre-SetLimits default of 32,000,000 (giving a 64 MiB maxMessagePayload
// pre-check and a ~30.5 MiB per-tx mpl at that default; neither is the
// figure that applies here). So the true worst case for THIS node is
// ~3.73 GiB (4,000,000,000 bytes) per queued entry, and
// ~373 GiB (100 * 3.73 GiB) per peer at this bound — a ceiling that
// predates this task (it governs every non-block message type, not
// something Task 14 introduced) but whose blast radius this queue widens:
// before, one such tx was decoded, validated-or-rejected, and released by a
// single synchronous call; now up to 100 can be retained per peer at once.
// In practice bandwidth and time to deliver that many maximal-size messages
// bound this far below the theoretical figure, and the transport's own
// Conn.Inbound() channel (128 deep, same byte-unboundedness) sits upstream
// of this one with an identical property already — but the theoretical
// number is the one a later reader should be able to trust, so it is
// stated plainly here rather than rounded down to "negligible."
//
// Aggregate cost across N connected peers is N*txIngestQueueDepth entries
// (not bytes, per above) — at a few hundred peers, still a smaller entry
// count than legacy's own single 10,000-deep queue, and unlike that queue
// this one only ever holds tx messages, not headers/blocks/invs as well.
const txIngestQueueDepth = 100

// ingestReport is one finished ingest, delivered to the Run goroutine so the
// scheduler update and the idle-timer reset happen on the loop that owns them.
type ingestReport struct {
	hash    chainhash.Hash
	outcome IngestOutcome
}

// ingestReportQueue bounds the reports the Run goroutine has yet to drain.
// Only one block is ever on the wire at a time — the transport read loop
// parks until the stream closes — but an ingest that releases the stream
// before it reports lets the next one start, so the queue holds a few.
const ingestReportQueue = 4

func NewPeer(cfg PeerConfig) *Peer {
	return &Peer{
		cfg:         cfg,
		hs:          NewHandshake(cfg.Handshake),
		established: make(chan struct{}),
		connectedAt: time.Now(),
		gone:        make(chan struct{}),
		ingestCh:    make(chan ingestReport, ingestReportQueue),
		// Capacity one: it is an edge signal, not a queue. The queue is
		// p.getData, and the serve loop drains it until it is empty.
		getDataWake: make(chan struct{}, 1),
		txMsgCh:     make(chan *wire.MsgTx, txIngestQueueDepth),
	}
}

// Run drives one connection until it ends. Every error it returns closes the
// connection, so this is the one place the whole disconnect contract is
// visible. Seven judgements drop a peer, from four different sources:
//
//   - An unsolicited block, or any block when no ingestor is wired. startIngest
//     refuses both (see the note there on why neither drains the stream).
//   - An ingest that came back with IngestOutcome.PeerFault. The peer loop never
//     reads that flag itself: syncDispatcher.BlockDone turns it into the error
//     returned here, so the classification stays with the scheduler.
//   - The idle timeout, when no running ingest excuses it (see ingestAlive).
//   - StallActionDisconnect, the net_processing.cpp DetectStalling rule, decided
//     by the manager's stall ticker (blockdownload.go CheckStall). It arrives as
//     Disconnect from outside this loop, not as a returned error.
//   - The ban threshold, reached by the handshake's own scoring or by a
//     misbehavior delta a sync machine returned (checkBanThreshold).
//   - HeaderSync's two round-ending errors, ErrCheckpointMismatch and
//     ErrHeadersNoProgress, returned through syncDispatcher.Headers. The machine
//     performs no teardown itself; the caller disconnects, and PeerDisconnected
//     is what releases the round.
//   - An unsupported inv type, returned through syncDispatcher.Inv
//     (blockdownload.go).
//
// Two more exits are not judgements about the peer: the transport closing its
// inbound channels (the socket failed) and ctx cancellation (we are shutting
// down).
//
// Just as important is what does NOT disconnect. A sync-peer rotation
// (IngestOutcome.Rotate, StallActionRotateSyncPeer) has already released the
// slot and the peer's downloads, so disconnecting would run that release a
// second time. IngestOutcome.TransientLocal and IngestOutcome.Duplicate are our
// fault or nobody's. A send that could not be queued is dropped with a warning,
// because the getdata pass and the rotation both run again.
//
// Run's deferred cancel is part of this contract: a disconnect cancels the
// context every running ingest holds, so anything that drops a peer mid-ingest
// aborts that ingest as well.
func (p *Peer) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer p.goneOnce.Do(func() { close(p.gone) })

	p.cfg.Conn.Start(ctx)

	// The getdata answerer runs off this loop: one block send takes minutes,
	// and Run has to keep the idle timer honest while it happens. It stops on
	// ctx cancellation or on p.gone, both of which Run closes on its way out.
	if p.cfg.Sync != nil && p.cfg.Fetcher != nil {
		go p.serveLoop(ctx)
	}

	// The tx ingest worker runs off this loop for the same reason serveLoop
	// does: validating a transaction is slow, and Run still has to service
	// pings, the idle timer and shutdown while it happens. It stops on ctx
	// cancellation or p.gone, both of which Run closes on its way out.
	if p.cfg.TxIngestor != nil {
		go p.txIngestLoop(ctx)
	}

	for _, msg := range p.hs.Initial() {
		if err := p.cfg.Conn.SendPriority(msg); err != nil {
			return p.disconnect(err)
		}
	}

	idle := time.NewTimer(p.cfg.IdleTimeout)
	defer idle.Stop()

	ping := time.NewTicker(p.cfg.PingInterval)
	defer ping.Stop()

	resetIdle := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}

		idle.Reset(p.cfg.IdleTimeout)
	}

	for {
		select {
		case msg, open := <-p.cfg.Conn.Inbound():
			if !open {
				return p.disconnect(p.cfg.Conn.Err())
			}

			resetIdle()

			if err := p.handleMessage(msg); err != nil {
				return p.disconnect(err)
			}

		case stream, open := <-p.cfg.Conn.InboundBlocks():
			if !open {
				return p.disconnect(p.cfg.Conn.Err())
			}

			// The peer delivered a block: it is alive, and it stays alive for
			// as long as the ingest keeps taking bytes (see ingestAlive).
			resetIdle()

			if err := p.startIngest(ctx, stream); err != nil {
				return p.disconnect(err)
			}

		case report := <-p.ingestCh:
			p.ingestFinished()

			// An ingest that finishes is fresh evidence of a live peer, and
			// the idle window must start from the end of it: the peer has been
			// waiting on us, unable to send anything else.
			resetIdle()

			if p.cfg.Sync != nil {
				// A block the pipeline rejected is the peer's fault, and the
				// dispatcher says so by returning an error.
				if err := p.cfg.Sync.BlockDone(p.cfg.SyncPeer, report.hash, report.outcome); err != nil {
					return p.disconnect(err)
				}
			}

		case <-idle.C:
			// A block being ingested holds the peer's whole connection, so the
			// idle timer must judge the ingest instead of the silent socket.
			if p.ingestAlive() {
				idle.Reset(p.cfg.IdleTimeout)
				continue
			}

			return p.disconnect(errors.New(errors.ERR_NETWORK_TIMEOUT, "svp2p: peer idle timeout"))

		case <-ping.C:
			// net_processing.cpp SendMessages: ping on the PING_INTERVAL cadence.
			if p.hs.Established() {
				if err := p.cfg.Conn.Send(wire.NewMsgPing(randNonce())); err != nil {
					p.cfg.Logger.Debugf("[svp2p] ping enqueue failed for %s: %v", p.cfg.Conn.RemoteAddr(), err)
				}
			}

		case <-ctx.Done():
			return p.disconnect(ctx.Err())
		}
	}
}

func (p *Peer) handleMessage(msg wire.Message) error {
	p.mu.Lock()
	p.lastRecv = time.Now()
	replies, err := p.hs.OnMessage(msg)
	score := p.hs.MisbehaviorScore()
	est := p.hs.Established()
	info := p.hs.PeerInfo()
	p.mu.Unlock()

	if err != nil {
		return err
	}

	for _, r := range replies {
		if err := p.cfg.Conn.SendPriority(r); err != nil {
			return err
		}
	}

	firstEstablished := false

	if est {
		p.estOnce.Do(func() {
			close(p.established)

			firstEstablished = true
		})
	}

	// net_processing.cpp Misbehaving: disconnect at the ban threshold.
	if err := p.checkBanThreshold(score); err != nil {
		return err
	}

	// The sync machines are dispatched with the peer lock released: they take
	// PeerManager's sync-state mutex, and the package lock order is peer lock
	// then manager lock.
	if err := p.dispatchSync(msg, est, firstEstablished, info.Services); err != nil {
		return err
	}

	// Tx ingestion is independent of the sync machines (see dispatchTx's own
	// doc comment) and never returns an error that disconnects the peer.
	p.dispatchTx(msg, est)

	return nil
}

// dispatchSync feeds one post-handshake message to the manager-owned sync
// machines and sends what they return. The machines perform no I/O; this is
// the only place their output reaches the wire (spec §4.3).
func (p *Peer) dispatchSync(msg wire.Message, established, firstEstablished bool, services wire.ServiceFlag) error {
	if p.cfg.Sync == nil {
		return nil
	}

	if firstEstablished {
		p.send(p.cfg.Sync.Established(p.cfg.SyncPeer, services))
	}

	// net_processing.cpp ProcessMessage drops everything that arrives before
	// the handshake completes ("Must have a version message before anything
	// else"), so nothing pre-handshake may reach the sync machines. The
	// handshake already scores those messages; this only stops them from
	// mutating sync state.
	if !established {
		return nil
	}

	switch m := msg.(type) {
	case *wire.MsgHeaders:
		out, delta, err := p.cfg.Sync.Headers(p.cfg.SyncPeer, m)

		// A disconnect error still leaves the round held by this peer: the
		// caller drops it, and PeerManager.runPeer drives PeerDisconnected,
		// which is what releases the sync slot (headersync.go OnHeaders).
		if err != nil {
			return err
		}

		if err := p.scoreMisbehavior(delta); err != nil {
			return err
		}

		p.send(out)

	case *wire.MsgInv:
		out, err := p.cfg.Sync.Inv(p.cfg.SyncPeer, m)
		if err != nil {
			return err
		}

		p.send(out)

	case *wire.MsgGetHeaders:
		p.send(p.cfg.Sync.GetHeaders(p.cfg.SyncPeer, m))

	case *wire.MsgGetBlocks:
		p.send(p.cfg.Sync.GetBlocks(p.cfg.SyncPeer, m))

	case *wire.MsgGetData:
		// Handed to the serve goroutine rather than answered here: see
		// queueGetData.
		p.queueGetData(m)
	}

	return nil
}

// dispatchTx routes a post-handshake *wire.MsgTx to the tx ingest worker.
// Kept separate from dispatchSync deliberately: tx ingestion is not part of
// the block-sync machine graph (HeaderSync/BlockDownloader) dispatchSync
// dispatches to, and unlike that method this must still run when cfg.Sync is
// nil. The correct reason is PeerManager.RelayTxs itself, not the block
// ingestor's construction path (an earlier version of this comment cited
// that, incorrectly — review round 1, Minor 3): RelayTxs iterates m.peers
// directly and has no dependency on m.headerSync/SyncEnabled(), so a peer
// whose txMsgCh this feeds must reach the ingestor regardless of whether
// block sync itself is configured. handleMessage calls this after
// dispatchSync, under the same "established" gate net_processing.cpp
// ProcessMessage applies to every post-handshake message.
//
// legacy's OnTx (services/legacy/peer_server.go:892, check at :898-900)
// refuses every inbound tx up front when cfg.BlocksOnly is set. That is a
// legacy command-line flag with no settings key in this port, and F5
// forbids adding one — deliberately not carried here, disclosed rather
// than silently dropped (review round 1, Minor 10).
func (p *Peer) dispatchTx(msg wire.Message, established bool) {
	if !established || p.cfg.TxIngestor == nil {
		return
	}

	m, ok := msg.(*wire.MsgTx)
	if !ok {
		return
	}

	// Computed once, unconditionally, and reused below by both MarkTxKnown
	// and queueTx's own drop log (see queueTx's doc comment on why that
	// reuse matters): there is no cheaper way to get either of their inputs.
	hash := m.TxHash()

	// Marked BEFORE queueing, matching legacy's own ordering exactly
	// (services/legacy/peer_server.go:906-908, AddKnownInventory ahead of
	// QueueTx) — see MarkTxKnown's own doc comment (PeerConfig) for why.
	// This runs synchronously, on the Run goroutine, ahead of queueTx's
	// non-blocking handoff to the async worker — so by the time
	// TxIngestor.Ingest is ever called for this tx, the mark has already
	// happened (review round 1, Important 2).
	if p.cfg.MarkTxKnown != nil {
		p.cfg.MarkTxKnown(p.cfg.SyncPeer, hash)
	}

	p.queueTx(m, hash)
}

// send puts machine output on the droppable lane. A full send queue is not
// worth a disconnect: the getdata pass runs again on the manager's next tick,
// and a dropped getheaders is caught by the sync-peer rotation.
func (p *Peer) send(msgs []wire.Message) {
	for _, msg := range msgs {
		if err := p.cfg.Conn.Send(msg); err != nil {
			p.cfg.Logger.Warnf("[svp2p] dropped %s to %s: %v", msg.Command(), p.cfg.Conn.RemoteAddr(), err)
		}
	}
}

// scoreMisbehavior applies a machine's misbehavior delta to the same counter
// the handshake scores against, so both feed one ban threshold.
func (p *Peer) scoreMisbehavior(delta int) error {
	if delta <= 0 {
		return nil
	}

	p.mu.Lock()
	p.hs.AddMisbehavior(delta)
	score := p.hs.MisbehaviorScore()
	p.mu.Unlock()

	return p.checkBanThreshold(score)
}

func (p *Peer) checkBanThreshold(score int) error {
	if p.cfg.BanThreshold > 0 && score >= p.cfg.BanThreshold {
		return errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: misbehavior threshold reached (score %d)", score)
	}

	return nil
}

// startIngest hands one inbound block to the ingestor on its own goroutine.
// It must not run on the Run goroutine: an ingest takes minutes on a fat
// block, and the loop still has to keep the idle timer honest, answer pings,
// and observe shutdown while it runs.
func (p *Peer) startIngest(ctx context.Context, stream *transport.BlockStream) error {
	header := stream.Header()
	hash := header.BlockHash()

	// Neither refusal below drains the stream, and that is deliberate. A drain
	// runs io.Copy over up to MaxBlockPayload bytes ON THIS GOROUTINE, which
	// is the one servicing the idle timer and ctx cancellation — a peer that
	// declares a huge payload and then dribbles would hold the loop for as
	// long as it liked. Both refusals end in a disconnect instead, and
	// Conn.Close releases the parked read loop through sockClosed without
	// reading a byte. Byte alignment does not matter on a connection we are
	// closing.
	if p.cfg.Ingestor == nil {
		// Nothing can ingest this block, and nothing asked for it either:
		// without an ingestor this node never requests a block.
		return errors.New(errors.ERR_NETWORK_PEER_MALICIOUS,
			"svp2p: unexpected block %s from a peer we cannot ingest for", hash, ErrProtocolViolation)
	}

	// Reject an unsolicited block before it can consume admission budget
	// (services/legacy/peer_server.go OnBlock, PR 1190: "Reject unrequested
	// blocks before they can consume prefetch budget"). Without this gate a
	// peer can push blocks nobody asked for straight into the shared byte
	// budget and starve the real sync peer.
	//
	// A sync-peer rotation releases the rotated peer's in-flight records while
	// its blocks may still be on the wire, so an honest peer's next block can
	// land here and cost it the connection. Legacy behaves the same way
	// (clearRequestedState followed by handleBlockMsg's unrequested-block
	// disconnect), and the cost is one reconnect of a peer we just stopped
	// syncing from.
	// Without a dispatcher there is no in-flight record to test against, which
	// is only ever the shape of a test peer driving the ingest path directly.
	if p.cfg.Sync != nil && !p.cfg.Sync.BlockExpected(p.cfg.SyncPeer, hash) {
		return errors.New(errors.ERR_NETWORK_PEER_MALICIOUS,
			"svp2p: unrequested block %s", hash, ErrProtocolViolation)
	}

	progress := p.cfg.Ingestor.WatchProgress(stream.TxReader())

	req := BlockIngestRequest{
		Header:    &header,
		TxCount:   stream.TxCount(),
		SizeBytes: stream.Length(),
		TxReader:  progress,
		PeerAddr:  p.cfg.Conn.RemoteAddr().String(),
		Quit:      p.gone,
	}

	p.mu.Lock()
	p.ingest = progress
	p.ingestStarted = time.Now()
	p.ingestTxBytes = txPayloadBytes(req.SizeBytes, req.TxCount)
	p.ingestActive++
	p.mu.Unlock()

	go func() {
		outcome := p.cfg.Ingestor.Ingest(ctx, req)

		// Close is idempotent, and the transport read loop stays parked until
		// the stream is released, so the release is never left to the
		// ingestor's error paths alone.
		if err := progress.Close(); err != nil {
			p.cfg.Logger.Debugf("[svp2p] block stream %s from %s closed with: %v", hash, req.PeerAddr, err)
		}

		select {
		case p.ingestCh <- ingestReport{hash: hash, outcome: outcome}:
		case <-p.gone:
		}
	}()

	return nil
}

// queueTx hands one inbound tx to txIngestLoop instead of validating it on
// the Run goroutine: bridge validation is slow, and Run has to keep
// servicing pings, the idle timer and shutdown while it happens — the same
// reasoning startIngest documents for blocks. Unlike a block, a tx arrives on
// the ordinary message channel (Conn.Inbound()), interleaved with every
// other post-handshake message, so nothing upstream serializes it the way
// the one-block-at-a-time transport does for InboundBlocks(); txMsgCh's
// bound (txIngestQueueDepth) is this method's own backpressure instead.
//
// A full queue means this peer is delivering txs faster than they can be
// validated. The newest one is dropped rather than blocking — a deliberate
// DEPARTURE from legacy's own policy, disclosed here per spec §4.3 (review
// round 1, Important 3) rather than silently ported as a smaller version of
// it. Legacy's OnTx does the opposite on purpose, and says so:
// "intentionally block further receives until the transaction is fully
// processed and known good or bad. This helps prevent a malicious peer from
// queuing up a bunch of bad transactions before disconnecting (or being
// disconnected) and wasting memory" (OnTx, services/legacy/peer_server.go:
// 910-914, the QueueTx call itself at :915 — verified directly against the
// file, not against a prior citation of this same quote, which was wrong
// twice over: this port's own review round 1 fix cited :900-905, and it
// propagated from a citation in the controller's own round-1 text; the
// blocking send QueueTx makes is manager.go:2668, sm.msgChan<- into the
// single global 10,000-deep queue). Legacy can afford to block there
// because OnTx runs on that peer's own dedicated callback goroutine,
// separate from the one that services pings — blocking it stalls only that
// peer's further reads. This package has no such separation: queueTx runs
// on dispatchTx <- handleMessage <- Run's own select loop, the SAME
// goroutine that also owns the idle timer, pings and shutdown (exactly why
// block ingestion was moved off it — startIngest's own doc comment). A
// blocking send here would stall Run itself, not just this peer's tx
// processing, reintroducing the defect Task 10 already fixed for blocks.
// So: dropped, not blocked. The consequence, honestly stated: a dropped
// unsolicited tx has no ack and no retry, so it is genuinely LOST for this
// delivery attempt, not merely delayed the way legacy's blocking peer would
// be — and it does not self-heal until Task 16 wires the inv->getdata round
// trip, at which point another peer's inv of the same hash can still recover
// it. Unsolicited transactions are never punished either way (F3), so a
// drop here stays a capacity decision, not a fault judgement.
//
// Logged at Warn, not Debug (review round 1, Important 3/Minor 5): a silent
// Debug-level drop of peer data is invisible in a normal production
// deployment, and this file's own convention already logs every other drop
// at Warn (see Peer.send). hash is dispatchTx's caller-computed
// msg.TxHash(), passed in rather than recomputed here — a stale version of
// this comment claimed the hash was "deliberately NOT computed" to avoid a
// double-SHA256 on the drop path, which stopped being true the moment
// dispatchTx started computing it unconditionally for MarkTxKnown (review
// round 1, Important 2): the cost moved, it was never removed, and legacy
// pays a double cost itself: OnTx's own tracing call hashes
// (peer_server.go:895, msg.TxHash(), a Go function argument evaluated
// whether or not debug logging is enabled) and then hashes again three
// lines later (:907, tx.Hash() — a fresh bsvutil.Tx, so nothing is cached
// from the first hash to reuse). Reusing dispatchTx's already-paid-for
// value here costs nothing further and makes the drop log useful (which
// tx, not just that one dropped). txDropped
// (PeerSnapshot.TxDropped) is the counter for the aggregate signal; see its
// own doc comment for what it does and does not yet reach.
func (p *Peer) queueTx(msg *wire.MsgTx, hash chainhash.Hash) {
	select {
	case p.txMsgCh <- msg:
	default:
		p.txDropped.Add(1)
		p.cfg.Logger.Warnf("[svp2p] tx ingest queue full for %s, dropping %s (%d dropped so far)",
			p.cfg.Conn.RemoteAddr(), hash, p.txDropped.Load())
	}
}

// txIngestLoop is the one goroutine that calls TxIngestor.Ingest for this
// peer. Serializing here — one worker per peer, rather than one goroutine
// per inbound tx — bounds how many concurrent validations a single peer can
// force, without needing a new setting (F5): the bound is txMsgCh's fixed
// capacity. It stops on ctx cancellation or p.gone, the same two shutdown
// signals serveLoop uses.
func (p *Peer) txIngestLoop(ctx context.Context) {
	for {
		select {
		case msg := <-p.txMsgCh:
			p.ingestTx(ctx, msg)
		case <-p.gone:
			return
		case <-ctx.Done():
			return
		}
	}
}

// ingestTx runs one queued tx through the ingestor and sends its reject
// message, if any. Conn.Send is safe to call from any goroutine — it only
// enqueues onto a channel guarded by its own byte budget — so this sends
// directly rather than funneling the outcome back through the Run loop the
// way a finished block ingest does (ingestCh). That indirection exists for
// blocks because BlockDone can return a disconnect error and only Run may
// disconnect a peer; no TxIngestOutcome ever disconnects (see its own doc
// comment), so there is nothing here that has to run on Run itself.
func (p *Peer) ingestTx(ctx context.Context, msg *wire.MsgTx) {
	peerAddr := p.cfg.Conn.RemoteAddr().String()
	outcome := p.cfg.TxIngestor.Ingest(ctx, msg, peerAddr)

	if outcome.Err != nil {
		p.cfg.Logger.Debugf("[svp2p] tx ingest error for %s from %s: %v", msg.TxHash(), peerAddr, outcome.Err)
	}

	if outcome.Reject != nil {
		if err := p.cfg.Conn.Send(outcome.Reject); err != nil {
			p.cfg.Logger.Warnf("[svp2p] dropped tx reject to %s: %v", peerAddr, err)
		}
	}
}

func (p *Peer) ingestFinished() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.ingestActive--

	if p.ingestActive == 0 {
		p.ingest = nil
	}
}

// IngestSnapshot reports what the peer's current block ingest has achieved, so
// the manager's stall ticker can tell a large block still streaming in from a
// peer that went quiet. It takes only the peer lock, and the package lock
// order requires the caller to hold no manager lock when it calls this.
func (p *Peer) IngestSnapshot() IngestSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ingest == nil {
		return IngestSnapshot{}
	}

	return IngestSnapshot{
		Active:        true,
		StartedMicros: p.ingestStarted.UnixMicro(),
		BytesRead:     p.ingest.BytesRead(),
	}
}

// txPayloadBytes is how many transaction bytes a block stream yields in total:
// the payload length the peer declared, less the prefix the transport already
// took off it before handing the stream over — the 80 byte header and the
// transaction-count varint. It is the count BytesRead converges on, so
// comparing BytesRead against the declared payload length directly would never
// match. A declared length no longer than its own prefix yields nothing, so
// the count floors at zero rather than wrapping.
func txPayloadBytes(sizeBytes, txCount uint64) uint64 {
	prefix := uint64(wire.MaxBlockHeaderPayload) + uint64(wire.VarIntSerializeSize(txCount)) //nolint:gosec // both are small positive constants

	if sizeBytes <= prefix {
		return 0
	}

	return sizeBytes - prefix
}

// ingestAlive reports whether a running ingest excuses the idle timer. An
// ingest is in one of three states, and each needs a different judgement.
//
// The stream is fully consumed. The peer has delivered every byte it declared
// and owes us nothing more, so ProgressReader's stamp is frozen for good. What
// runs now is OUR post-stream pipeline tail — extendTransactions, createUtxos,
// createSubtrees, ProcessBlock — which outlasts the idle window on a fat
// block. Judging that tail on byte silence disconnects the peer mid-validation,
// and Run's deferred cancel then aborts the ingest, so the block comes back
// and stalls exactly the same way for ever. The tail is bounded by
// MaxBlockDownloadTime instead: the same ceiling CheckStall puts on one block's
// hold over the sync slot, so an ingest wedged for good still ends the peer.
//
// No byte has moved yet. bridge.ProgressReader's contract, stated on the type:
// LastProgress is seeded at construction, and BytesRead stays at 0 through
// IngestBlock's LOCAL pre-read waits (WaitForBlockAssemblyReady,
// waitForPreviousBlockMined). A peer must never be dropped for our own
// slowness, so this counts as alive.
//
// Bytes moved but the stream is short. This is the one state the peer is
// answerable for: it started delivering and went quiet mid-payload. The stamp
// has to keep moving inside the idle window.
func (p *Peer) ingestAlive() bool {
	p.mu.Lock()
	ingest := p.ingest
	txBytes := p.ingestTxBytes
	started := p.ingestStarted
	p.mu.Unlock()

	if ingest == nil {
		return false
	}

	read := ingest.BytesRead()

	if read >= txBytes {
		return time.Since(started) < MaxBlockDownloadTime
	}

	if read == 0 {
		return true
	}

	return time.Since(ingest.LastProgress()) < p.cfg.IdleTimeout
}

func (p *Peer) Established() <-chan struct{} { return p.established }

func (p *Peer) Disconnect(reason string) {
	_ = p.disconnect(errors.New(errors.ERR_ERROR, "svp2p: disconnected: %s", reason))
}

func (p *Peer) Info() PeerSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	info := p.hs.PeerInfo()

	return PeerSnapshot{
		Addr:             p.cfg.Conn.RemoteAddr().String(),
		Inbound:          p.cfg.Handshake.Inbound,
		UserAgent:        info.UserAgent,
		ProtocolVersion:  info.NegotiatedVersion,
		StartingHeight:   info.StartingHeight,
		BytesSent:        p.cfg.Conn.BytesSent(),
		BytesReceived:    p.cfg.Conn.BytesReceived(),
		ConnectedAt:      p.connectedAt,
		LastRecv:         p.lastRecv,
		MisbehaviorScore: p.hs.MisbehaviorScore(),
		TxDropped:        p.txDropped.Load(),
	}
}

// WantsHeaders reports whether this peer negotiated sendheaders
// (PeerInfo.WantsHeaders, the fPreferHeaders equivalent), which is what the
// block announcement relay (relay.go selectRelayTargets) reads to choose a
// headers message over a plain inv for this peer.
//
// Takes the peer lock like every other read of handshake state (Info); the
// package's lock order forbids calling this while a manager lock is held —
// PeerManager.RelayBlock reads it before taking syncMu, not after.
func (p *Peer) WantsHeaders() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.hs.PeerInfo().WantsHeaders
}

// RelayTxDisabled reports whether this peer negotiated fRelayTxes=false
// (PeerInfo.DisableRelayTx, wire.MsgVersion.DisableRelayTx), the tx
// announcement relay's (relay.go selectTxRelayTargets) per-peer opt-out —
// legacy's own relayTxDisabled gate at the equivalent site
// (services/legacy/peer_server.go handleRelayInvMsg, "Don't relay the
// transaction to the peer when it has transaction relaying disabled").
//
// Takes the peer lock like WantsHeaders, for the same reason: the package's
// lock order forbids calling this while a manager lock is held, so
// PeerManager.RelayTxs reads it before taking syncMu, not after.
func (p *Peer) RelayTxDisabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.hs.PeerInfo().DisableRelayTx
}

// FeeFilter reports this peer's last announced minimum relay fee rate, in
// satoshis/kB (PeerInfo.FeeFilter, net_processing.cpp FEEFILTER, set by
// handshake.go's *wire.MsgFeeFilter case). Zero means the peer has never
// sent one, which selectTxRelayTargets treats as "no filter" — the same
// `feeFilter > 0` gate legacy's own handleRelayTxMsg uses
// (services/legacy/peer_server.go:2566).
//
// Takes the peer lock like WantsHeaders and RelayTxDisabled, for the same
// lock-order reason.
func (p *Peer) FeeFilter() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.hs.PeerInfo().FeeFilter
}

func (p *Peer) disconnect(err error) error {
	p.discOnce.Do(func() { p.discErr = err })

	_ = p.cfg.Conn.Close()

	return p.discErr
}

func randNonce() uint64 {
	var b [8]byte

	_, _ = rand.Read(b[:])

	return binary.LittleEndian.Uint64(b[:])
}

package protocol

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"sync"
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
}

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
	return p.dispatchSync(msg, est, firstEstablished, info.Services)
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
	}

	return nil
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
	}
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

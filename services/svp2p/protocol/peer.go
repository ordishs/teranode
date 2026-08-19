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
	// must rotate. The peer stays connected.
	Rotate bool

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
type syncDispatcher interface {
	// Established is the SendMessages event for a finished handshake.
	Established(sp *SyncPeer, services wire.ServiceFlag) []wire.Message

	// Headers dispatches a headers message; the int is a misbehavior delta
	// and a non-nil error means this peer must be disconnected.
	Headers(sp *SyncPeer, msg *wire.MsgHeaders) ([]wire.Message, int, error)

	// Inv dispatches an inv message; a non-nil error means this peer must be
	// disconnected.
	Inv(sp *SyncPeer, msg *wire.MsgInv) ([]wire.Message, error)

	// BlockDone reports one block's ingest outcome to the scheduler.
	BlockDone(sp *SyncPeer, hash chainhash.Hash, outcome IngestOutcome)
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

	// ingest and ingestActive are read and written by the Run goroutine only.
	// ingest is the newest running ingest's progress handle, which the idle
	// timer watches; ingestActive counts the ingests still to report.
	ingest       IngestProgress
	ingestActive int
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

			p.startIngest(ctx, stream)

		case report := <-p.ingestCh:
			p.ingestActive--
			if p.ingestActive == 0 {
				p.ingest = nil
			}

			// An ingest that finishes is fresh evidence of a live peer, and
			// the idle window must start from the end of it: the peer has been
			// waiting on us, unable to send anything else.
			resetIdle()

			if p.cfg.Sync != nil {
				p.cfg.Sync.BlockDone(p.cfg.SyncPeer, report.hash, report.outcome)
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
	return p.dispatchSync(msg, firstEstablished, info.Services)
}

// dispatchSync feeds one post-handshake message to the manager-owned sync
// machines and sends what they return. The machines perform no I/O; this is
// the only place their output reaches the wire (spec §4.3).
func (p *Peer) dispatchSync(msg wire.Message, firstEstablished bool, services wire.ServiceFlag) error {
	if p.cfg.Sync == nil {
		return nil
	}

	if firstEstablished {
		p.send(p.cfg.Sync.Established(p.cfg.SyncPeer, services))
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
func (p *Peer) startIngest(ctx context.Context, stream *transport.BlockStream) {
	header := stream.Header()
	hash := header.BlockHash()

	if p.cfg.Ingestor == nil {
		// Nothing can ingest this block. Closing drains the payload so the
		// connection stays aligned and the parked read loop is released.
		if err := stream.Close(); err != nil {
			p.cfg.Logger.Warnf("[svp2p] failed to drain unhandled block %s from %s: %v", hash, p.cfg.Conn.RemoteAddr(), err)
		}

		return
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

	p.ingest = progress
	p.ingestActive++

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
}

// ingestAlive reports whether a running ingest excuses the idle timer.
//
// bridge.ProgressReader's contract, stated on the type: LastProgress is
// seeded at construction, and BytesRead stays at 0 through IngestBlock's LOCAL
// pre-read waits (WaitForBlockAssemblyReady, waitForPreviousBlockMined). A
// peer must never be dropped for our own slowness, so zero bytes read counts
// as alive; once bytes have moved, the stamp has to keep moving too.
func (p *Peer) ingestAlive() bool {
	if p.ingest == nil {
		return false
	}

	if p.ingest.BytesRead() == 0 {
		return true
	}

	return time.Since(p.ingest.LastProgress()) < p.cfg.IdleTimeout
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

package protocol

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/svp2p/transport"
	"github.com/bsv-blockchain/teranode/ulogger"
)

type PeerConfig struct {
	Handshake    HandshakeConfig
	Conn         *transport.Conn
	Logger       ulogger.Logger
	IdleTimeout  time.Duration
	PingInterval time.Duration
	BanThreshold int
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
}

func NewPeer(cfg PeerConfig) *Peer {
	return &Peer{
		cfg:         cfg,
		hs:          NewHandshake(cfg.Handshake),
		established: make(chan struct{}),
		connectedAt: time.Now(),
	}
}

func (p *Peer) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	for {
		select {
		case msg, open := <-p.cfg.Conn.Inbound():
			if !open {
				return p.disconnect(p.cfg.Conn.Err())
			}

			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(p.cfg.IdleTimeout)

			if err := p.handleMessage(msg); err != nil {
				return p.disconnect(err)
			}

		case <-idle.C:
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
	p.mu.Unlock()

	if err != nil {
		return err
	}

	for _, r := range replies {
		if err := p.cfg.Conn.SendPriority(r); err != nil {
			return err
		}
	}

	if est {
		p.estOnce.Do(func() { close(p.established) })
	}

	// net_processing.cpp Misbehaving: disconnect at the ban threshold.
	if p.cfg.BanThreshold > 0 && score >= p.cfg.BanThreshold {
		return errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: misbehavior threshold reached (score %d)", score)
	}

	return nil
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

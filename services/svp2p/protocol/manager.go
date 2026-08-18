package protocol

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/svp2p/transport"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
)

const (
	// UserAgent identifies this node on the wire.
	UserAgent = "/teranode-svp2p:0.1.0/"

	// pingInterval mirrors SVNode PING_INTERVAL (net.h: 2 * 60).
	pingInterval = 2 * time.Minute

	// banScoreThreshold mirrors SVNode DEFAULT_BANSCORE_THRESHOLD (100).
	banScoreThreshold = 100

	// dialRetryBase and dialRetryMax bound the outbound reconnect backoff,
	// matching the retry semantics the bsvd connmgr gave the old service
	// (DefaultRetryDuration 5s, capped).
	dialRetryBase = 5 * time.Second
	dialRetryMax  = 5 * time.Minute

	// maxSentNonces bounds the self-connection nonce registry, mirroring
	// bsvd's sentNonces mruNonceMap size (50).
	maxSentNonces = 50

	sendBudgetBytes = 10 * 1024 * 1024
	recvQueueLen    = 128
	writeTimeout    = 30 * time.Second
)

// PeerManager owns listeners, the outbound dialer, and the peer registry.
// It is the net.cpp CConnman counterpart for Phase 1.
type PeerManager struct {
	logger    ulogger.Logger
	tSettings *settings.Settings
	banList   *BanList

	mu        sync.Mutex
	peers     map[*Peer]struct{}
	listeners []net.Listener
	nonces    []uint64
	started   bool

	quit chan struct{}
	wg   sync.WaitGroup
}

func NewPeerManager(logger ulogger.Logger, tSettings *settings.Settings, banList *BanList) *PeerManager {
	return &PeerManager{
		logger:    logger,
		tSettings: tSettings,
		banList:   banList,
		peers:     make(map[*Peer]struct{}),
		quit:      make(chan struct{}),
	}
}

func (m *PeerManager) Start(ctx context.Context, listenAddresses []string) error {
	m.mu.Lock()

	if m.started {
		m.mu.Unlock()
		return errors.New(errors.ERR_SERVICE_ERROR, "svp2p: peer manager already started")
	}

	m.started = true

	for _, addr := range listenAddresses {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			m.mu.Unlock()
			_ = m.Stop()

			return errors.New(errors.ERR_SERVICE_ERROR, "svp2p: cannot listen on %s", addr, err)
		}

		m.listeners = append(m.listeners, ln)
	}

	listeners := append([]net.Listener(nil), m.listeners...)
	m.mu.Unlock()

	for _, ln := range listeners {
		m.wg.Add(1)

		go func(ln net.Listener) {
			defer m.wg.Done()
			m.acceptLoop(ctx, ln)
		}(ln)
	}

	for _, addr := range m.tSettings.Legacy.ConnectPeers {
		m.wg.Add(1)

		go func(addr string) {
			defer m.wg.Done()
			m.dialLoop(ctx, addr)
		}(addr)
	}

	return nil
}

func (m *PeerManager) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			select {
			case <-m.quit:
			case <-ctx.Done():
			default:
				m.logger.Errorf("[svp2p] accept failed on %s: %v", ln.Addr(), err)
			}

			return
		}

		// net.cpp CConnman::AcceptConnection: drop banned peers before any
		// protocol traffic.
		if m.banList.IsBanned(nc.RemoteAddr().String()) {
			m.logger.Infof("[svp2p] rejected banned inbound peer %s", nc.RemoteAddr())

			_ = nc.Close()

			continue
		}

		m.wg.Add(1)

		go func() {
			defer m.wg.Done()

			_ = m.runPeer(ctx, nc, true)
		}()
	}
}

func (m *PeerManager) dialLoop(ctx context.Context, addr string) {
	delay := dialRetryBase

	for {
		if m.banList.IsBanned(addr) {
			m.logger.Infof("[svp2p] not dialing banned peer %s", addr)
			return
		}

		nc, err := net.DialTimeout("tcp", addr, 30*time.Second)
		if err == nil {
			runErr := m.runPeer(ctx, nc, false)

			// net.cpp: never redial a peer that proved to be ourselves.
			if errors.Is(runErr, ErrSelfConnection) {
				m.logger.Warnf("[svp2p] %s is ourselves, not redialing", addr)
				return
			}

			// bsvd connmgr semantics: a completed connection resets backoff.
			delay = dialRetryBase
		} else {
			m.logger.Debugf("[svp2p] dial %s failed: %v", addr, err)
		}

		select {
		case <-time.After(delay):
		case <-m.quit:
			return
		case <-ctx.Done():
			return
		}

		delay *= 2
		if delay > dialRetryMax {
			delay = dialRetryMax
		}
	}
}

func (m *PeerManager) runPeer(ctx context.Context, nc net.Conn, inbound bool) error {
	conn := transport.New(nc, transport.Config{
		Net:             m.tSettings.ChainCfgParams.Net,
		ProtocolVersion: wire.ProtocolVersion,
		SendBudgetBytes: sendBudgetBytes,
		RecvQueueLen:    recvQueueLen,
		WriteTimeout:    writeTimeout,
	})

	peer := NewPeer(PeerConfig{
		Handshake: HandshakeConfig{
			Inbound:              inbound,
			Nonce:                m.newNonce(),
			UserAgent:            UserAgent,
			StartingHeight:       0, // the header index arrives in Phase 2
			MaxRecvPayloadLength: wire.DefaultMaxRecvPayloadLength,
			AllowBlockPriority:   m.tSettings.Legacy.AllowBlockPriority,
			LocalAddr:            netAddressOf(nc.LocalAddr()),
			RemoteAddr:           netAddressOf(nc.RemoteAddr()),
			CheckIncomingNonce:   m.hasSentNonce,
		},
		Conn:         conn,
		Logger:       m.logger,
		IdleTimeout:  m.tSettings.Legacy.PeerIdleTimeout,
		PingInterval: pingInterval,
		BanThreshold: banScoreThreshold,
	})

	m.mu.Lock()
	m.peers[peer] = struct{}{}
	m.mu.Unlock()

	err := peer.Run(ctx)

	m.mu.Lock()
	delete(m.peers, peer)
	m.mu.Unlock()

	m.logger.Infof("[svp2p] peer %s done: %v", nc.RemoteAddr(), err)

	return err
}

func (m *PeerManager) newNonce() uint64 {
	nonce := randNonce()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.nonces = append(m.nonces, nonce)
	if len(m.nonces) > maxSentNonces {
		m.nonces = m.nonces[len(m.nonces)-maxSentNonces:]
	}

	return nonce
}

// hasSentNonce mirrors net.cpp CConnman::CheckIncomingNonce: true if this
// node itself generated the given nonce for one of its own connections
// (inbound or outbound), meaning an incoming VERSION carrying it proves a
// self-connect.
func (m *PeerManager) hasSentNonce(nonce uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, n := range m.nonces {
		if n == nonce {
			return true
		}
	}

	return false
}

func (m *PeerManager) ListenAddrs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	addrs := make([]string, 0, len(m.listeners))
	for _, ln := range m.listeners {
		addrs = append(addrs, ln.Addr().String())
	}

	return addrs
}

func (m *PeerManager) ConnectedCount() int32 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return int32(len(m.peers)) //nolint:gosec // peer count is small
}

func (m *PeerManager) Snapshots() []PeerSnapshot {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))

	for p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()

	snaps := make([]PeerSnapshot, 0, len(peers))
	for _, p := range peers {
		snaps = append(snaps, p.Info())
	}

	return snaps
}

func (m *PeerManager) DisconnectHost(host string) {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))

	for p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()

	for _, p := range peers {
		peerHost, _, err := net.SplitHostPort(p.Info().Addr)
		if err != nil {
			peerHost = p.Info().Addr
		}

		if peerHost == host {
			p.Disconnect("banned by operator")
		}
	}
}

func (m *PeerManager) Stop() error {
	m.mu.Lock()

	select {
	case <-m.quit:
	default:
		close(m.quit)
	}

	listeners := m.listeners
	m.listeners = nil

	peers := make([]*Peer, 0, len(m.peers))
	for p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()

	for _, ln := range listeners {
		_ = ln.Close()
	}

	for _, p := range peers {
		p.Disconnect("shutting down")
	}

	m.wg.Wait()

	return nil
}

func netAddressOf(addr net.Addr) *wire.NetAddress {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return wire.NewNetAddressIPPort(nil, 0, 0)
	}

	return wire.NewNetAddress(tcpAddr, 0)
}

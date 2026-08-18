package protocol

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

func managerSettings() *settings.Settings {
	return &settings.Settings{
		ChainCfgParams: &chaincfg.MainNetParams,
		Legacy: settings.LegacySettings{
			PeerIdleTimeout:    125 * time.Second,
			AllowBlockPriority: true,
		},
	}
}

func newTestManager(t *testing.T, banList *BanList) *PeerManager {
	t.Helper()

	if banList == nil {
		var err error

		banList, err = NewBanList("")
		require.NoError(t, err)
	}

	return NewPeerManager(ulogger.TestLogger{}, managerSettings(), banList)
}

// dialScripted connects to addr and acts as the outbound side of the
// handshake: it sends version first, then completes the exchange.
func dialScripted(t *testing.T, addr string) *scriptedPeer {
	t.Helper()

	nc, err := net.Dial("tcp", addr)
	require.NoError(t, err)

	return &scriptedPeer{nc: nc}
}

func (s *scriptedPeer) completeOutboundHandshake(t *testing.T) {
	t.Helper()

	s.write(t, remoteVersion(4321))

	sawVerack := false
	for !sawVerack {
		switch s.read(t).(type) {
		case *wire.MsgVerAck:
			sawVerack = true
		case *wire.MsgVersion, *wire.MsgProtoconf:
		}
	}

	s.write(t, wire.NewMsgVerAck())
}

func TestManagerAcceptsInboundHandshake(t *testing.T) {
	m := newTestManager(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	addrs := m.ListenAddrs()
	require.Len(t, addrs, 1)

	far := dialScripted(t, addrs[0])
	far.completeOutboundHandshake(t)

	require.Eventually(t, func() bool { return m.ConnectedCount() == 1 }, 5*time.Second, 50*time.Millisecond)

	snaps := m.Snapshots()
	require.Len(t, snaps, 1)
	require.True(t, snaps[0].Inbound)
	require.Equal(t, "/sv:1.1.0/", snaps[0].UserAgent)

	require.NoError(t, far.nc.Close())
	require.Eventually(t, func() bool { return m.ConnectedCount() == 0 }, 5*time.Second, 50*time.Millisecond)
}

func TestManagerRejectsBannedInbound(t *testing.T) {
	banList, err := NewBanList("")
	require.NoError(t, err)
	require.NoError(t, banList.Add("127.0.0.1", time.Now().Add(time.Hour)))

	m := newTestManager(t, banList)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	nc, err := net.Dial("tcp", m.ListenAddrs()[0])
	require.NoError(t, err)

	defer func() { _ = nc.Close() }()

	// net.cpp CConnman::AcceptConnection: banned peers are dropped before
	// any protocol traffic. Expect the socket to close without data.
	require.NoError(t, nc.SetReadDeadline(time.Now().Add(5*time.Second)))

	buf := make([]byte, 1)
	_, err = nc.Read(buf)
	require.Error(t, err)
	require.Equal(t, int32(0), m.ConnectedCount())
}

func TestManagerDialsConfiguredPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer func() { _ = ln.Close() }()

	tSettings := managerSettings()
	tSettings.Legacy.ConnectPeers = []string{ln.Addr().String()}

	banList, err := NewBanList("")
	require.NoError(t, err)

	m := NewPeerManager(ulogger.TestLogger{}, tSettings, banList)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, nil))

	defer func() { require.NoError(t, m.Stop()) }()

	nc, err := ln.Accept()
	require.NoError(t, err)

	far := &scriptedPeer{nc: nc}
	require.IsType(t, &wire.MsgVersion{}, far.read(t)) // outbound dialer sends version first

	require.NoError(t, nc.Close())
}

// captureLogger records Infof lines so a test can assert on disconnect
// reasons without any new production surface: PeerManager already logs
// each peer's terminal error via Infof (manager.go runPeer).
type captureLogger struct {
	ulogger.TestLogger
	mu   sync.Mutex
	logs []string
}

func (l *captureLogger) Infof(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.logs = append(l.logs, fmt.Sprintf(format, args...))
}

func (l *captureLogger) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, line := range l.logs {
		if strings.Contains(line, substr) {
			return true
		}
	}

	return false
}

// establishedCount counts peers whose handshake reached Established,
// using PeerManager's existing private peer registry and Peer's existing
// public Established() channel — no new production surface.
func establishedCount(m *PeerManager) int {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))

	for p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()

	n := 0

	for _, p := range peers {
		select {
		case <-p.Established():
			n++
		default:
		}
	}

	return n
}

// Ledger ruling 2026-08-18: the plan's "dialer must not redial" assertion
// was a defect, superseded by SVNode fidelity (spec §4.3). net.cpp
// CConnman::CheckIncomingNonce / net_processing.cpp ProcessVersionMessage
// detect a self-connect only on the accepting (inbound) side, and that
// side disconnects immediately without ever pushing its own version
// reply — so the outbound (-connect) dialer only ever sees a plain
// socket close, never ErrSelfConnection, and keeps redialing on its
// normal backoff. That matches real SVNode/bitcoind behavior for a
// self-pointed -connect misconfiguration and is not a defect this task
// fixes. What this task guarantees: a self-connection never reaches
// Established, on either end, no matter how many times the dialer retries.
func TestManagerDetectsSelfConnection(t *testing.T) {
	// Reserve a free port, then reuse the same address as both this
	// manager's listener and its configured outbound peer, so the dialer
	// connects back to itself: a real self-connect, where the outbound and
	// inbound ends of the loopback carry different per-connection nonces.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	tSettings := managerSettings()
	tSettings.Legacy.ConnectPeers = []string{addr}

	banList, err := NewBanList("")
	require.NoError(t, err)

	logger := &captureLogger{}
	m := NewPeerManager(logger, tSettings, banList)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{addr}))
	defer func() { require.NoError(t, m.Stop()) }()

	// The inbound side's registry check kills the session inside onVersion,
	// before any reply — confirms detection actually fired rather than the
	// dialer simply never having reached the listener.
	require.Eventually(t, func() bool { return logger.contains("svp2p: connected to self") },
		5*time.Second, 50*time.Millisecond, "expected the inbound side to log an ErrSelfConnection disconnect")

	// The behavioral contract: no self-connection ever reaches Established,
	// on either end, across at least two full redial windows — including
	// the redials the dialer keeps making per the ruling above.
	require.Never(t, func() bool { return establishedCount(m) != 0 }, 2*dialRetryBase, 100*time.Millisecond)
}

func TestManagerStopClosesEverything(t *testing.T) {
	m := newTestManager(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	addr := m.ListenAddrs()[0]

	far := dialScripted(t, addr)
	far.completeOutboundHandshake(t)
	require.Eventually(t, func() bool { return m.ConnectedCount() == 1 }, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, m.Stop())
	require.Equal(t, int32(0), m.ConnectedCount())

	_, err := net.DialTimeout("tcp", addr, time.Second)
	require.Error(t, err)
}

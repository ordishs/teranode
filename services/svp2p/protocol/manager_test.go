package protocol

import (
	"context"
	"net"
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

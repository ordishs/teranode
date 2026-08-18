package protocol

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/services/svp2p/transport"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

type scriptedPeer struct {
	nc net.Conn
}

func (s *scriptedPeer) read(t *testing.T) wire.Message {
	t.Helper()

	require.NoError(t, s.nc.SetReadDeadline(time.Now().Add(5*time.Second)))

	_, msg, _, err := wire.ReadMessageWithEncodingN(s.nc, wire.ProtocolVersion, wire.MainNet, wire.BaseEncoding)
	require.NoError(t, err)

	return msg
}

func (s *scriptedPeer) write(t *testing.T, msg wire.Message) {
	t.Helper()

	_, err := wire.WriteMessageWithEncodingN(s.nc, msg, wire.ProtocolVersion, wire.MainNet, wire.BaseEncoding)
	require.NoError(t, err)
}

func newTestPeer(t *testing.T, idle, ping time.Duration) (*Peer, *scriptedPeer) {
	t.Helper()

	a, b := net.Pipe()
	conn := transport.New(a, transport.Config{
		Net: wire.MainNet, ProtocolVersion: wire.ProtocolVersion,
		SendBudgetBytes: 1 << 20, RecvQueueLen: 32, WriteTimeout: 5 * time.Second,
	})

	cfg := PeerConfig{
		Handshake: HandshakeConfig{
			Inbound: false, Nonce: 7777, UserAgent: "/teranode-svp2p:0.1.0/",
			StartingHeight: 0, MaxRecvPayloadLength: wire.DefaultMaxRecvPayloadLength,
			AllowBlockPriority: true,
			LocalAddr:          wire.NewNetAddressIPPort(nil, 8333, 0),
			RemoteAddr:         wire.NewNetAddressIPPort(nil, 8333, 0),
		},
		Conn: conn, Logger: ulogger.TestLogger{},
		IdleTimeout: idle, PingInterval: ping, BanThreshold: 100,
	}

	return NewPeer(cfg), &scriptedPeer{nc: b}
}

func completeHandshake(t *testing.T, far *scriptedPeer) {
	t.Helper()

	require.IsType(t, &wire.MsgVersion{}, far.read(t))
	far.write(t, remoteVersion(1234))
	require.IsType(t, &wire.MsgVerAck{}, far.read(t))
	require.IsType(t, &wire.MsgProtoconf{}, far.read(t))
	far.write(t, wire.NewMsgVerAck())
	require.IsType(t, &wire.MsgSendHeaders{}, far.read(t))
}

func TestPeerCompletesHandshake(t *testing.T) {
	p, far := newTestPeer(t, 30*time.Second, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)

	go func() { errCh <- p.Run(ctx) }()

	completeHandshake(t, far)

	select {
	case <-p.Established():
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not complete")
	}

	snap := p.Info()
	require.Equal(t, "/sv:1.1.0/", snap.UserAgent)
	require.False(t, snap.Inbound)
	require.Equal(t, int32(850000), snap.StartingHeight)
	require.Positive(t, snap.BytesSent)
	require.Positive(t, snap.BytesReceived)
}

func TestPeerIdleTimeoutDisconnects(t *testing.T) {
	p, far := newTestPeer(t, 200*time.Millisecond, time.Hour)

	errCh := make(chan error, 1)

	go func() { errCh <- p.Run(context.Background()) }()

	require.IsType(t, &wire.MsgVersion{}, far.read(t)) // drain, then go silent

	select {
	case err := <-errCh:
		require.Error(t, err)
		require.Contains(t, err.Error(), "idle")
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not disconnect on idle timeout")
	}
}

func TestPeerSendsPings(t *testing.T) {
	p, far := newTestPeer(t, time.Hour, 200*time.Millisecond)

	go func() { _ = p.Run(context.Background()) }()

	completeHandshake(t, far)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := far.read(t).(*wire.MsgPing); ok {
			return
		}
	}

	t.Fatal("no ping observed")
}

func TestPeerSelfConnectionTerminatesRun(t *testing.T) {
	p, far := newTestPeer(t, time.Hour, time.Hour)

	errCh := make(chan error, 1)

	go func() { errCh <- p.Run(context.Background()) }()

	require.IsType(t, &wire.MsgVersion{}, far.read(t))
	far.write(t, remoteVersion(7777)) // our own nonce

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, ErrSelfConnection)
	case <-time.After(5 * time.Second):
		t.Fatal("self-connection not detected")
	}
}

func TestPeerDisconnectStopsRun(t *testing.T) {
	p, far := newTestPeer(t, time.Hour, time.Hour)

	errCh := make(chan error, 1)

	go func() { errCh <- p.Run(context.Background()) }()

	require.IsType(t, &wire.MsgVersion{}, far.read(t))

	p.Disconnect("test teardown")

	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect did not stop Run")
	}
}

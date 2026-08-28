package protocol

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// startedManager builds a manager listening on a dual-stack ephemeral port and
// starts it. The listener is dual-stack so a test can reach the same node over
// two different remote IPs (127.0.0.1 and ::1) without an interface alias.
func startedManager(t *testing.T) *PeerManager {
	t.Helper()

	return startedManagerWith(t, nil, nil)
}

func startedManagerWith(t *testing.T, tweakSettings func(*settings.Settings), tweakManager func(*PeerManager)) *PeerManager {
	t.Helper()

	tSettings := managerSettings()
	if tweakSettings != nil {
		tweakSettings(tSettings)
	}

	banList, err := NewBanList("")
	require.NoError(t, err)

	m := NewPeerManager(ulogger.TestLogger{}, tSettings, banList)

	if tweakManager != nil {
		tweakManager(m)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, m.Start(ctx, []string{":0"}))
	t.Cleanup(func() { require.NoError(t, m.Stop()) })

	return m
}

// nodeAddr rebuilds the manager's listen address with an explicit host, so a
// test can choose which local address it reaches the node from.
func nodeAddr(t *testing.T, m *PeerManager, host string) string {
	t.Helper()

	addrs := m.ListenAddrs()
	require.Len(t, addrs, 1)

	_, port, err := net.SplitHostPort(addrs[0])
	require.NoError(t, err)

	return net.JoinHostPort(host, port)
}

func dialRaw(t *testing.T, addr string) net.Conn {
	t.Helper()

	nc, err := net.Dial("tcp", addr)
	require.NoError(t, err)

	t.Cleanup(func() { _ = nc.Close() })

	return nc
}

func writeMsg(t *testing.T, nc net.Conn, msg wire.Message) {
	t.Helper()

	require.NoError(t, nc.SetWriteDeadline(time.Now().Add(5*time.Second)))

	_, err := wire.WriteMessageWithEncodingN(nc, msg, wire.ProtocolVersion, wire.MainNet, wire.BaseEncoding)
	require.NoError(t, err)
}

func readMsg(t *testing.T, nc net.Conn, timeout time.Duration) wire.Message {
	t.Helper()

	require.NoError(t, nc.SetReadDeadline(time.Now().Add(timeout)))

	_, msg, _, err := wire.ReadMessageWithEncodingN(nc, wire.ProtocolVersion, wire.MainNet, wire.BaseEncoding)
	require.NoError(t, err)

	return msg
}

// requireEOF asserts the node ended the connection.
func requireEOF(t *testing.T, nc net.Conn, timeout time.Duration) {
	t.Helper()

	require.NoError(t, nc.SetReadDeadline(time.Now().Add(timeout)))

	buf := make([]byte, 1)
	_, err := nc.Read(buf)
	require.Error(t, err)
	require.False(t, isTimeout(err), "the node did not close the connection: %v", err)
}

func isTimeout(err error) bool {
	var ne net.Error

	return errors.As(err, &ne) && ne.Timeout()
}

// completeOutboundHandshakeWithAssociationID is completeOutboundHandshake with
// an association ID in the peer's version message, which is what an inbound
// peer uses to name the association its later streams join
// (net_processing.cpp:1775 SetAssociationID).
func (s *scriptedPeer) completeOutboundHandshakeWithAssociationID(t *testing.T, id []byte) {
	t.Helper()

	version := remoteVersion(4321)
	version.AssociationID = id

	s.completeOutboundHandshakeAs(t, version)
}

// establishAssociation connects a scripted peer that names id, and waits for
// the manager to register the association under it.
func establishAssociation(t *testing.T, m *PeerManager, id []byte) *scriptedPeer {
	t.Helper()

	far := dialScripted(t, nodeAddr(t, m, "127.0.0.1"))

	// The cleanup closure is also what keeps far reachable for the whole
	// test: nothing else holds the connection, and the runtime finalizer on
	// an unreachable socket would close it and end the association early.
	t.Cleanup(func() { _ = far.nc.Close() })

	far.completeOutboundHandshakeWithAssociationID(t, id)

	require.Eventually(t, func() bool { return m.associationByID(id) != nil }, 5*time.Second, 20*time.Millisecond)

	return far
}

// net_processing.cpp:1514-1590 ProcessCreateStreamMessage + net.cpp:3188
// MoveStream: a createstream naming a live association from the SAME IP
// attaches and is acked ON THE NEW STREAM; the husk runs no Peer.
func TestInbound_CreateStreamAttachesAndAcksOnNewStream(t *testing.T) {
	m := startedManager(t)

	id := []byte{0x00, 9, 9, 9, 9}
	_ = establishAssociation(t, m, id)

	raw := dialRaw(t, nodeAddr(t, m, "127.0.0.1"))
	writeMsg(t, raw, &wire.MsgCreateStream{
		AssociationID:    id,
		StreamType:       wire.StreamTypeData1,
		StreamPolicyName: wire.BlockPriorityStreamPolicy,
	})

	ack, ok := readMsg(t, raw, 5*time.Second).(*wire.MsgStreamAck)
	require.True(t, ok, "the node must answer createstream with streamack")
	require.Equal(t, id, ack.AssociationID)
	require.Equal(t, wire.StreamTypeData1, ack.StreamType)

	a := m.associationByID(id)
	require.NotNil(t, a)
	require.True(t, a.HasStream(wire.StreamTypeData1))
	require.Equal(t, wire.BlockPriorityStreamPolicy, a.Policy().Name())
	require.Equal(t, 1, len(m.peerHandles()), "the husk connection must not become a Peer")
}

// net.cpp:3220-3230: a createstream from a different IP is a DoS attempt →
// ban + disconnect.
func TestInbound_CreateStreamFromOtherIPIsBanned(t *testing.T) {
	m := startedManager(t)

	id := []byte{0x00, 8, 8, 8, 8}
	_ = establishAssociation(t, m, id)

	otherHost, raw := dialFromOtherIP(t, m)

	writeMsg(t, raw, &wire.MsgCreateStream{
		AssociationID:    id,
		StreamType:       wire.StreamTypeData1,
		StreamPolicyName: wire.BlockPriorityStreamPolicy,
	})

	rej, ok := readMsg(t, raw, 5*time.Second).(*wire.MsgReject)
	require.True(t, ok, "a refused createstream must be rejected before the disconnect")
	require.Equal(t, rejectStreamSetup, rej.Code)

	requireEOF(t, raw, 5*time.Second)

	require.True(t, m.banList.IsBanned(otherHost), "the misbehaving IP must be banned")
	require.False(t, m.associationByID(id).HasStream(wire.StreamTypeData1))
}

// dialFromOtherIP reaches the node from a local address that is not the
// 127.0.0.1 the association was established from. It prefers the brief's
// 127.0.0.2 source address and falls back to the ::1 side of the dual-stack
// listener when the OS refuses to bind it (macOS without an lo0 alias).
func dialFromOtherIP(t *testing.T, m *PeerManager) (string, net.Conn) {
	t.Helper()

	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}

	nc, err := dialer.Dial("tcp", nodeAddr(t, m, "127.0.0.1"))
	if err == nil {
		t.Cleanup(func() { _ = nc.Close() })

		return "127.0.0.2", nc
	}

	t.Logf("127.0.0.2 source address unavailable, falling back to ::1: %v", err)

	nc, err = net.Dial("tcp", nodeAddr(t, m, "::1"))
	if err != nil {
		t.Skipf("no second local address available for the different-IP test: %v", err)
	}

	t.Cleanup(func() { _ = nc.Close() })

	return "::1", nc
}

// Unknown ID, duplicate type, unknown policy and an out-of-range stream type
// all fail stream setup: REJECT_STREAM_SETUP (0x60) then disconnect
// (net_processing.cpp:1577-1584).
func TestInbound_CreateStreamSetupFailures(t *testing.T) {
	m := startedManager(t)

	id := []byte{0x00, 7, 7, 7, 7}
	_ = establishAssociation(t, m, id)

	// The duplicate case needs a DATA1 stream already in place, so attach one
	// first and keep it open for the rest of the test.
	good := dialRaw(t, nodeAddr(t, m, "127.0.0.1"))
	writeMsg(t, good, &wire.MsgCreateStream{
		AssociationID:    id,
		StreamType:       wire.StreamTypeData1,
		StreamPolicyName: wire.BlockPriorityStreamPolicy,
	})
	require.IsType(t, &wire.MsgStreamAck{}, readMsg(t, good, 5*time.Second))

	tests := []struct {
		name   string
		msg    *wire.MsgCreateStream
		reason string
	}{
		{
			name:   "unknown association id",
			msg:    &wire.MsgCreateStream{AssociationID: []byte{0x00, 1, 2, 3}, StreamType: wire.StreamTypeData1, StreamPolicyName: wire.BlockPriorityStreamPolicy},
			reason: "No node found with association ID",
		},
		{
			name:   "duplicate data1",
			msg:    &wire.MsgCreateStream{AssociationID: id, StreamType: wire.StreamTypeData1, StreamPolicyName: wire.BlockPriorityStreamPolicy},
			reason: "Attempt to overwrite existing stream in move",
		},
		{
			name:   "unknown policy",
			msg:    &wire.MsgCreateStream{AssociationID: id, StreamType: wire.StreamTypeData1, StreamPolicyName: "Nope"},
			reason: "unknown stream policy",
		},
		{
			name:   "unknown stream type",
			msg:    &wire.MsgCreateStream{AssociationID: id, StreamType: wire.StreamTypeUnknown, StreamPolicyName: wire.BlockPriorityStreamPolicy},
			reason: "StreamType out of range",
		},
		{
			name:   "stream type out of range",
			msg:    &wire.MsgCreateStream{AssociationID: id, StreamType: wire.StreamType(9), StreamPolicyName: wire.BlockPriorityStreamPolicy},
			reason: "StreamType out of range",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := dialRaw(t, nodeAddr(t, m, "127.0.0.1"))
			writeMsg(t, raw, test.msg)

			rej, ok := readMsg(t, raw, 5*time.Second).(*wire.MsgReject)
			require.True(t, ok, "stream setup failure must answer with reject")
			require.Equal(t, rejectStreamSetup, rej.Code)
			require.Equal(t, wire.CmdCreateStream, rej.Cmd)
			require.True(t, strings.Contains(rej.Reason, test.reason), "reject reason %q must mention %q", rej.Reason, test.reason)

			requireEOF(t, raw, 5*time.Second)
		})
	}

	require.Equal(t, 1, len(m.peerHandles()), "no husk may become a Peer")
}

// net_processing.cpp:1598-1604: streamack with nothing pending → reject +
// disconnect.
func TestInbound_UnsolicitedStreamAckIsRejected(t *testing.T) {
	m := startedManager(t)

	raw := dialRaw(t, nodeAddr(t, m, "127.0.0.1"))
	writeMsg(t, raw, &wire.MsgStreamAck{AssociationID: []byte{0x00, 5, 5, 5}, StreamType: wire.StreamTypeData1})

	rej, ok := readMsg(t, raw, 5*time.Second).(*wire.MsgReject)
	require.True(t, ok, "an unsolicited streamack must be rejected")
	require.Equal(t, wire.RejectNonstandard, rej.Code)
	require.Equal(t, wire.CmdStreamAck, rej.Cmd)

	requireEOF(t, raw, 5*time.Second)
	require.Equal(t, 0, len(m.peerHandles()))
}

// net_processing.cpp:4709: the first message must be version or createstream;
// anything else disconnects.
func TestInbound_OtherFirstMessageDisconnects(t *testing.T) {
	m := startedManager(t)

	raw := dialRaw(t, nodeAddr(t, m, "127.0.0.1"))
	writeMsg(t, raw, wire.NewMsgPing(42))

	requireEOF(t, raw, 5*time.Second)
	require.Equal(t, 0, len(m.peerHandles()))
}

// With multistreams off a createstream is refused with REJECT_STREAM_SETUP.
func TestInbound_CreateStreamRefusedWhenBlockPriorityOff(t *testing.T) {
	m := startedManagerWith(t, func(s *settings.Settings) { s.Legacy.AllowBlockPriority = false }, nil)

	id := []byte{0x00, 6, 6, 6, 6}
	_ = establishAssociation(t, m, id)

	raw := dialRaw(t, nodeAddr(t, m, "127.0.0.1"))
	writeMsg(t, raw, &wire.MsgCreateStream{
		AssociationID:    id,
		StreamType:       wire.StreamTypeData1,
		StreamPolicyName: wire.BlockPriorityStreamPolicy,
	})

	rej, ok := readMsg(t, raw, 5*time.Second).(*wire.MsgReject)
	require.True(t, ok, "createstream must be rejected when multistreams are off")
	require.Equal(t, rejectStreamSetup, rej.Code)
	require.True(t, strings.Contains(rej.Reason, "multistreams disabled"), "reject reason %q", rej.Reason)

	requireEOF(t, raw, 5*time.Second)
	require.False(t, m.associationByID(id).HasStream(wire.StreamTypeData1))
}

// A husk that sends nothing is dropped at the first-message timeout.
func TestInbound_SilentConnectionTimesOut(t *testing.T) {
	m := startedManagerWith(t, nil, func(m *PeerManager) { m.firstMessageTimeout = 200 * time.Millisecond })

	raw := dialRaw(t, nodeAddr(t, m, "127.0.0.1"))

	requireEOF(t, raw, 5*time.Second)
	require.Equal(t, 0, len(m.peerHandles()))
}

// An established peer's association leaves the registry when it disconnects,
// so a later createstream naming it cannot attach to a dead association.
func TestInbound_AssociationUnregistersOnDisconnect(t *testing.T) {
	m := startedManager(t)

	id := []byte{0x00, 4, 4, 4, 4}
	far := establishAssociation(t, m, id)

	require.NoError(t, far.nc.Close())

	require.Eventually(t, func() bool { return m.associationByID(id) == nil }, 5*time.Second, 20*time.Millisecond)
}

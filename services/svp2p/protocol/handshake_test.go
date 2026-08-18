package protocol

import (
	"testing"

	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

func remoteVersion(nonce uint64) *wire.MsgVersion {
	msg := wire.NewMsgVersion(
		wire.NewNetAddressIPPort(nil, 8333, 0),
		wire.NewNetAddressIPPort(nil, 8333, 0),
		nonce, 850000,
	)
	msg.UserAgent = "/sv:1.1.0/"

	return msg
}

func outboundConfig() HandshakeConfig {
	return HandshakeConfig{
		Inbound: false, Nonce: 7777, UserAgent: "/teranode-svp2p:0.1.0/",
		StartingHeight: 900000, MaxRecvPayloadLength: wire.DefaultMaxRecvPayloadLength,
		AllowBlockPriority: true,
		LocalAddr:          wire.NewNetAddressIPPort(nil, 8333, 0),
		RemoteAddr:         wire.NewNetAddressIPPort(nil, 8333, 0),
	}
}

func TestOutboundHandshakeHappyPath(t *testing.T) {
	h := NewHandshake(outboundConfig())

	initial := h.Initial()
	require.Len(t, initial, 1)
	require.IsType(t, &wire.MsgVersion{}, initial[0])

	// Their version arrives: we owe verack + protoconf
	// (net_processing.cpp ProcessVersionMessage: PushMessage(VERACK) then
	// PushProtoconf immediately after).
	reply, err := h.OnMessage(remoteVersion(1234))
	require.NoError(t, err)
	require.Len(t, reply, 2)
	require.IsType(t, &wire.MsgVerAck{}, reply[0])
	require.IsType(t, &wire.MsgProtoconf{}, reply[1])
	require.False(t, h.Established())

	// Their verack arrives: established; we send sendheaders
	// (net_processing.cpp ProcessVerAckMessage).
	reply, err = h.OnMessage(wire.NewMsgVerAck())
	require.NoError(t, err)
	require.True(t, h.Established())
	require.Len(t, reply, 1)
	require.IsType(t, &wire.MsgSendHeaders{}, reply[0])

	info := h.PeerInfo()
	require.Equal(t, "/sv:1.1.0/", info.UserAgent)
	require.Equal(t, int32(850000), info.StartingHeight)
	require.Equal(t, wire.ProtocolVersion, info.NegotiatedVersion)
}

func TestInboundHandshakeSendsVersionReply(t *testing.T) {
	cfg := outboundConfig()
	cfg.Inbound = true
	h := NewHandshake(cfg)

	require.Empty(t, h.Initial()) // net_processing.cpp: "Be shy and don't send version until we hear"

	reply, err := h.OnMessage(remoteVersion(1234))
	require.NoError(t, err)
	require.Len(t, reply, 3)
	require.IsType(t, &wire.MsgVersion{}, reply[0]) // PushNodeVersion on inbound VERSION
	require.IsType(t, &wire.MsgVerAck{}, reply[1])
	require.IsType(t, &wire.MsgProtoconf{}, reply[2])
}

func TestSelfConnectionDetected(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()

	_, err := h.OnMessage(remoteVersion(7777)) // our own nonce echoed back
	require.ErrorIs(t, err, ErrSelfConnection)
}

func TestObsoleteVersionRejected(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()

	old := remoteVersion(1)
	old.ProtocolVersion = 300

	_, err := h.OnMessage(old)
	require.ErrorIs(t, err, ErrObsoleteVersion)
}

func TestDuplicateVersionScoresMisbehavior(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()

	_, err := h.OnMessage(remoteVersion(1))
	require.NoError(t, err)

	// net_processing.cpp: Misbehaving(pfrom, 1, "multiple-version")
	reply, err := h.OnMessage(remoteVersion(2))
	require.NoError(t, err)
	require.Empty(t, reply)
	require.Equal(t, 1, h.MisbehaviorScore())
}

func TestMessageBeforeVersionScoresMisbehavior(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()

	// net_processing.cpp: Misbehaving(pfrom, 1, "missing-version")
	_, err := h.OnMessage(wire.NewMsgPing(1))
	require.NoError(t, err)
	require.Equal(t, 1, h.MisbehaviorScore())

	_, err = h.OnMessage(wire.NewMsgVerAck())
	require.NoError(t, err)
	require.Equal(t, 2, h.MisbehaviorScore())
}

func TestPingAfterEstablishedGetsPong(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()
	_, _ = h.OnMessage(remoteVersion(1))
	_, _ = h.OnMessage(wire.NewMsgVerAck())

	reply, err := h.OnMessage(wire.NewMsgPing(99))
	require.NoError(t, err)
	require.Len(t, reply, 1)
	pong, ok := reply[0].(*wire.MsgPong)
	require.True(t, ok)
	require.Equal(t, uint64(99), pong.Nonce)
}

func TestProtoconfRecorded(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()
	_, _ = h.OnMessage(remoteVersion(1))
	_, _ = h.OnMessage(wire.NewMsgVerAck())

	_, err := h.OnMessage(wire.NewMsgProtoconf(1<<21, true))
	require.NoError(t, err)
	require.Equal(t, uint32(1<<21), h.PeerInfo().TheirMaxRecvPayloadLength)
}

func TestDuplicateProtoconfDisconnects(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()
	_, _ = h.OnMessage(remoteVersion(1))
	_, _ = h.OnMessage(wire.NewMsgVerAck())
	_, _ = h.OnMessage(wire.NewMsgProtoconf(1<<21, true))

	// net_processing.cpp ProcessProtoconfMessage: protoconfReceived → fDisconnect
	_, err := h.OnMessage(wire.NewMsgProtoconf(1<<21, true))
	require.ErrorIs(t, err, ErrProtocolViolation)
}

func TestUndersizedProtoconfDisconnects(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()
	_, _ = h.OnMessage(remoteVersion(1))
	_, _ = h.OnMessage(wire.NewMsgVerAck())

	// net_processing.cpp ProcessProtoconfMessage: maxRecvPayloadLength below
	// LEGACY_MAX_PROTOCOL_PAYLOAD_LENGTH (1 MiB) → fDisconnect
	small := &wire.MsgProtoconf{NumberOfFields: 1, MaxRecvPayloadLength: 512 * 1024}

	_, err := h.OnMessage(small)
	require.ErrorIs(t, err, ErrProtocolViolation)
}

func TestSendHeadersAndFeeFilterRecorded(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()
	_, _ = h.OnMessage(remoteVersion(1))
	_, _ = h.OnMessage(wire.NewMsgVerAck())

	_, err := h.OnMessage(wire.NewMsgSendHeaders())
	require.NoError(t, err)
	require.True(t, h.PeerInfo().WantsHeaders)

	_, err = h.OnMessage(wire.NewMsgFeeFilter(250))
	require.NoError(t, err)
	require.Equal(t, int64(250), h.PeerInfo().FeeFilter)
}

func TestNoSendHeadersForOldPeer(t *testing.T) {
	h := NewHandshake(outboundConfig())
	_ = h.Initial()

	old := remoteVersion(1)
	old.ProtocolVersion = 70011 // below SENDHEADERS_VERSION

	_, err := h.OnMessage(old)
	require.NoError(t, err)

	// net_processing.cpp ProcessVerAckMessage: sendheaders only when
	// pfrom->nVersion >= SENDHEADERS_VERSION.
	reply, err := h.OnMessage(wire.NewMsgVerAck())
	require.NoError(t, err)
	require.Empty(t, reply)
	require.True(t, h.Established())
}

package protocol

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/svp2p/transport"
)

// rejectStreamSetup is net_types.h:26 REJECT_STREAM_SETUP, the reject code
// every stream-setup failure carries (net_processing.cpp:1580). go-wire has no
// constant for it.
const rejectStreamSetup = wire.RejectCode(0x60)

// defaultFirstMessageTimeout bounds how long a fresh inbound connection may
// stay silent before its first message. It is SVNode's handshake bound, not
// its inactivity bound: net.h:88 DEFAULT_P2P_HANDSHAKE_TIMEOUT_INTERVAL is
// 1 * 60, and net.cpp:1058-1064 CNode::ServiceSockets disconnects a connection
// that has still received nothing (nLastRecv == 0) once that window passes.
//
// This is a DIFFERENT constant from net.h:86
// DEFAULT_P2P_TIMEOUT_INTERVAL (20 * 60), the idle window cited by
// effectivePingInterval further down manager.go. A connection that has said
// nothing at all gets 60 s; one that is talking gets 20 min of quiet.
const defaultFirstMessageTimeout = 60 * time.Second

// extLengthMarker is protocol.cpp:220-237's extended-header length marker. A
// first message is never a block, so an extended frame here is never legal.
const extLengthMarker = uint32(0xffffffff)

// misbehavingBanDuration is the bantime SVNode gives BanReasonNodeMisbehaving
// (net.cpp:3225 Ban(fromAddr, BanReasonNodeMisbehaving), DEFAULT_MISBEHAVING_BANTIME).
const misbehavingBanDuration = 24 * time.Hour

var (
	// ErrFirstMessageExtended reports an extended message header as the first
	// message on a connection.
	ErrFirstMessageExtended = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: extended header as the first message")

	// ErrFirstMessageTooLong reports a first message that declares more
	// payload than we ever advertise we will receive.
	ErrFirstMessageTooLong = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: first message payload too long")
)

// The reject reason a stream-setup failure puts ON THE WIRE. SVNode sends
// what its own exception carried (net_processing.cpp:1580 e.what()), so the
// first four are its strings verbatim. The last three name cases SVNode has no
// string for: its own message for the address mismatch (net.cpp:3227-3229)
// names both endpoints, which would hand an attacker the victim's IP.
//
// Every one of them is a fixed constant well inside
// MAX_REJECT_MESSAGE_LENGTH (validation.h:155, 111 bytes), and none carries
// this node's internal error text. The detail belongs in the log line.
const (
	reasonStreamTypeRange = "StreamType out of range"
	reasonNoSuchNode      = "No node found with association ID"
	reasonStreamExists    = "Attempt to overwrite existing stream in move"
	reasonBadlyFormatted  = "Badly formatted message"
	reasonMultistreamsOff = "Multistreams disabled"
	reasonUnknownPolicy   = "Unknown stream policy"
	reasonOtherAddress    = "Stream setup from a different address"
)

// streamSetupError splits what a stream-setup failure says on the wire from
// what it says in the log. Reason is the plain constant the reject carries;
// Error is the detail — IPs, IDs, codes — which stays local.
type streamSetupError struct {
	reason string
	err    error
}

func (e *streamSetupError) Error() string { return e.err.Error() }

func (e *streamSetupError) Unwrap() error { return e.err }

func (e *streamSetupError) Reason() string { return e.reason }

func streamSetupFailure(reason string, err error) error {
	return &streamSetupError{reason: reason, err: err}
}

// rejectReason is the wire string for a stream-setup failure. Anything that
// did not name its own reason is reported as a malformed message rather than
// leaking the text of an error this node did not intend to publish.
func rejectReason(err error) string {
	var setup *streamSetupError

	if errors.As(err, &setup) {
		return setup.Reason()
	}

	return reasonBadlyFormatted
}

// classifyInbound decides what a fresh inbound connection is from its FIRST
// message. net_processing.cpp:4708-4715 takes version, createstream and
// streamack before anything else, and treats every other command on a
// connection with no version as "missing-version".
//
// It owns nc until it hands it on: a version starts a Peer over it, a
// successful createstream moves it into an existing association as a further
// stream, and every other outcome closes it.
func (m *PeerManager) classifyInbound(ctx context.Context, nc net.Conn) {
	magic := m.tSettings.ChainCfgParams.Net

	// The read below is the only thing standing between a silent connection
	// and Stop's wg.Wait, so shutdown has to be able to break it.
	read := make(chan struct{})

	go func() {
		select {
		case <-read:
		case <-m.quit:
			_ = nc.SetReadDeadline(time.Now())
		case <-ctx.Done():
			_ = nc.SetReadDeadline(time.Now())
		}
	}()

	msg, raw, err := readFirstMessage(nc, magic, m.firstMessageTimeout)

	close(read)

	if err != nil {
		m.logger.Debugf("[svp2p] inbound %s sent no usable first message: %v", nc.RemoteAddr(), err)

		_ = nc.Close()

		return
	}

	switch first := msg.(type) {
	case *wire.MsgVersion:
		_ = m.runPeer(ctx, nc, true, raw)

	case *wire.MsgCreateStream:
		if moveErr := m.moveStream(nc, first); moveErr != nil {
			m.logger.Infof("[svp2p] peer %s failed to setup new stream (%v); disconnecting", nc.RemoteAddr(), moveErr)

			m.writeAndClose(nc, wire.NewMsgReject(wire.CmdCreateStream, rejectStreamSetup, rejectReason(moveErr)))
		}

	case *wire.MsgStreamAck:
		// net_processing.cpp:1598-1604: a streamack answers a createstream WE
		// sent, and this connection has none outstanding.
		m.logger.Infof("[svp2p] peer %s sent an unsolicited streamack; disconnecting", nc.RemoteAddr())

		m.writeAndClose(nc, wire.NewMsgReject(wire.CmdStreamAck, wire.RejectNonstandard, "Invalid streamack"))

	default:
		// net_processing.cpp:4708-4711 "Must have a version or createstream
		// message before anything else".
		m.logger.Infof("[svp2p] peer %s sent %s before version; disconnecting", nc.RemoteAddr(), msg.Command())

		_ = nc.Close()
	}
}

// writeAndClose sends one last message on a raw socket that has no
// transport.Conn behind it, then ends the connection.
func (m *PeerManager) writeAndClose(nc net.Conn, msg wire.Message) {
	_ = nc.SetWriteDeadline(time.Now().Add(writeTimeout))

	if err := wire.WriteMessage(nc, msg, wire.ProtocolVersion, m.tSettings.ChainCfgParams.Net); err != nil {
		m.logger.Debugf("[svp2p] could not send %s to %s: %v", msg.Command(), nc.RemoteAddr(), err)
	}

	_ = nc.Close()
}

// readFirstMessage takes exactly one message off the socket and returns it
// together with the raw header and payload bytes, so a connection that turns
// out to be a peer can replay them into its transport.
func readFirstMessage(nc net.Conn, magic wire.BitcoinNet, timeout time.Duration) (wire.Message, []byte, error) {
	if err := nc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, nil, errors.New(errors.ERR_NETWORK_ERROR, "svp2p: cannot set first message deadline", err)
	}

	raw := make([]byte, wire.MessageHeaderSize)

	if _, err := io.ReadFull(nc, raw); err != nil {
		return nil, nil, errors.New(errors.ERR_NETWORK_ERROR, "svp2p: cannot read first message header", err)
	}

	length := binary.LittleEndian.Uint32(raw[16:20])

	if length == extLengthMarker {
		return nil, nil, ErrFirstMessageExtended
	}

	if length > wire.DefaultMaxRecvPayloadLength {
		return nil, nil, ErrFirstMessageTooLong
	}

	if length > 0 {
		payload := make([]byte, length)

		if _, err := io.ReadFull(nc, payload); err != nil {
			return nil, nil, errors.New(errors.ERR_NETWORK_ERROR, "svp2p: cannot read first message payload", err)
		}

		raw = append(raw, payload...)
	}

	if err := nc.SetReadDeadline(time.Time{}); err != nil {
		return nil, nil, errors.New(errors.ERR_NETWORK_ERROR, "svp2p: cannot clear first message deadline", err)
	}

	_, msg, _, err := wire.ReadMessageWithEncodingN(bytes.NewReader(raw), wire.ProtocolVersion, magic, wire.BaseEncoding)
	if err != nil {
		return nil, nil, errors.New(errors.ERR_NETWORK_INVALID_RESPONSE, "svp2p: cannot decode first message", err)
	}

	return msg, raw, nil
}

// moveStream is net.cpp:3188-3240 CConnman::MoveStream, driven by
// net_processing.cpp:1514-1590 ProcessCreateStreamMessage. On success nc stops
// being a connection of its own and becomes a further stream of the named
// association; the caller must not close it.
func (m *PeerManager) moveStream(nc net.Conn, cs *wire.MsgCreateStream) error {
	if !m.tSettings.Legacy.AllowBlockPriority {
		return streamSetupFailure(reasonMultistreamsOff,
			errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: multistreams disabled"))
	}

	// net_processing.cpp:1547-1551: a raw stream type at or above
	// MAX_STREAM_TYPE is out of range. UNKNOWN names no stream either.
	if cs.StreamType == wire.StreamTypeUnknown || cs.StreamType > wire.StreamTypeData4 {
		return streamSetupFailure(reasonStreamTypeRange,
			errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: stream type %d out of range", cs.StreamType))
	}

	// This port carries GENERAL plus DATA1 only (association.cpp:43); GENERAL
	// is the stream the association was built on and can never be moved in.
	if cs.StreamType != wire.StreamTypeData1 {
		return streamSetupFailure(reasonStreamTypeRange,
			errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: unsupported stream type %d", cs.StreamType))
	}

	// net.cpp:3233-3236: an empty policy name leaves the association's
	// current policy alone; a named one must exist.
	var policy transport.StreamPolicy

	if cs.StreamPolicyName != "" {
		found, ok := transport.PolicyForName(cs.StreamPolicyName)
		if !ok {
			return streamSetupFailure(reasonUnknownPolicy,
				errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: unknown stream policy %q", cs.StreamPolicyName))
		}

		policy = found
	}

	a := m.associationByID(cs.AssociationID)
	if a == nil {
		return streamSetupFailure(reasonNoSuchNode,
			errors.New(errors.ERR_NOT_FOUND, "svp2p: no node found with association id %x", cs.AssociationID))
	}

	// net.cpp:3220-3230: moving a stream between two different endpoints is a
	// DoS attempt, so the source is banned as well as refused.
	from := hostOf(nc.RemoteAddr())

	if !sameHost(nc.RemoteAddr(), a.RemoteAddr()) {
		to := hostOf(a.RemoteAddr())

		if err := m.banList.Add(from, time.Now().Add(misbehavingBanDuration)); err != nil {
			m.logger.Errorf("[svp2p] cannot ban %s: %v", from, err)
		}

		return streamSetupFailure(reasonOtherAddress,
			errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: attempt to move stream between peers with different IPs: %s != %s", from, to))
	}

	// association.cpp:150-153, checked before the socket becomes a stream:
	// Attach closes the Conn it refuses, which would take the socket the
	// reject below still has to travel over.
	if a.HasStream(cs.StreamType) {
		return streamSetupFailure(reasonStreamExists,
			errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: stream type %d already attached to association %x", cs.StreamType, cs.AssociationID))
	}

	stream := transport.New(nc, transport.Config{
		Net:             m.tSettings.ChainCfgParams.Net,
		ProtocolVersion: wire.ProtocolVersion,
		SendBudgetBytes: sendBudgetBytes,
		RecvQueueLen:    recvQueueLen,
		WriteTimeout:    writeTimeout,
		StreamType:      cs.StreamType,
	})

	// Attach BEFORE SetPolicy: a refused Attach closes the stream it refused,
	// and the association must not be left carrying a policy for a stream that
	// never joined it.
	if err := a.Attach(stream); err != nil {
		return streamSetupFailure(attachReason(err), err)
	}

	if policy != nil {
		a.SetPolicy(policy)
	}

	m.logger.Infof("[svp2p] stream %d for association %x moved from %s into its association", cs.StreamType, cs.AssociationID, nc.RemoteAddr())

	// net_processing.cpp:1569-1571: the ack goes out ON THE NEW STREAM, not
	// on the association's general stream. The socket now belongs to that
	// stream, so a failure here is the stream's to report and must NOT come
	// back as a setup failure — the reject would be written over a socket this
	// function no longer owns.
	if err := stream.SendPriority(wire.NewMsgStreamAck(cs.AssociationID, cs.StreamType)); err != nil {
		m.logger.Warnf("[svp2p] cannot ack stream %d for association %x: %v", cs.StreamType, cs.AssociationID, err)
	}

	return nil
}

// attachReason maps an Attach refusal to its wire reason. Attach has taken the
// socket by the time it refuses, so these travel over a connection that is
// already closing.
func attachReason(err error) string {
	if errors.Is(err, transport.ErrStreamExists) {
		return reasonStreamExists
	}

	return reasonNoSuchNode
}

// sameHost is net.cpp:3218-3222's fromAddr != toAddr: a CNetAddr compare, so
// it is the IP alone and not the port. It compares the parsed addresses rather
// than their text, so the same host reached once over IPv4 and once as an
// IPv4-mapped IPv6 address still counts as one host.
func sameHost(a, b net.Addr) bool {
	hostA, hostB := hostOf(a), hostOf(b)

	ipA, ipB := net.ParseIP(hostA), net.ParseIP(hostB)
	if ipA == nil || ipB == nil {
		return hostA == hostB
	}

	return ipA.Equal(ipB)
}

// hostOf is the IP half of an address.
func hostOf(addr net.Addr) string {
	if addr == nil {
		return ""
	}

	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}

	return host
}

// registerAssociation makes an established peer's association reachable by ID,
// which is what a later createstream looks it up in (net.cpp:3192-3202 walks
// vNodes for the same reason).
func (m *PeerManager) registerAssociation(a *transport.Association) {
	id := a.ID()
	if len(id) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// net.cpp:3192-3202 breaks at the FIRST node carrying the ID, so a later
	// peer that claims an ID already in use does not take it over.
	if held, taken := m.associations[string(id)]; taken && held != a {
		m.logger.Warnf("[svp2p] peer %s claims association ID %x, which is already in use", a.RemoteAddr(), id)

		return
	}

	m.associations[string(id)] = a
}

// unregisterAssociation removes every key holding a, which is the whole of a
// association's registry presence however it was keyed.
func (m *PeerManager) unregisterAssociation(a *transport.Association) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, held := range m.associations {
		if held == a {
			delete(m.associations, key)
		}
	}
}

func (m *PeerManager) associationByID(id []byte) *transport.Association {
	if len(id) == 0 {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.associations[string(id)]
}

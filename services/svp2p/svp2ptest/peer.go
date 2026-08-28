package svp2ptest

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// Direction says which way a transcript entry travelled, from the peer's point
// of view.
type Direction int

const (
	// In is a message the peer received from the node.
	In Direction = iota
	// Out is a message the peer sent to the node.
	Out
)

// Entry is one message in a Transcript. Conn is the connection it travelled
// on, so a multi-stream scenario can tell the association's GENERAL stream
// from its DATA1 stream.
type Entry struct {
	At   time.Time
	Dir  Direction
	Cmd  string
	Msg  wire.Message
	Conn net.Conn
}

// Transcript records every message a ScriptedPeer exchanged and who ended the
// connection. It is the harness's evidence.
type Transcript struct {
	mu       sync.Mutex
	entries  []Entry
	closedBy string
}

func (tr *Transcript) add(conn net.Conn, dir Direction, msg wire.Message) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	tr.entries = append(tr.entries, Entry{At: time.Now(), Dir: dir, Cmd: msg.Command(), Msg: msg, Conn: conn})
}

// Count returns how many entries travelled in dir with the given command.
func (tr *Transcript) Count(dir Direction, cmd string) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	n := 0

	for _, e := range tr.entries {
		if e.Dir == dir && e.Cmd == cmd {
			n++
		}
	}

	return n
}

// CountOn is Count restricted to one connection.
func (tr *Transcript) CountOn(conn net.Conn, dir Direction, cmd string) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	n := 0

	for _, e := range tr.entries {
		if e.Conn == conn && e.Dir == dir && e.Cmd == cmd {
			n++
		}
	}

	return n
}

// FirstOn returns the first entry travelling in dir with the given command,
// and whether there was one.
func (tr *Transcript) FirstOn(dir Direction, cmd string) (Entry, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	for _, e := range tr.entries {
		if e.Dir == dir && e.Cmd == cmd {
			return e, true
		}
	}

	return Entry{}, false
}

// Snapshot copies the entries recorded so far.
func (tr *Transcript) Snapshot() []Entry {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	out := make([]Entry, len(tr.entries))
	copy(out, tr.entries)

	return out
}

// ClosedBy is "node" when the node ended the connection, "peer" when the
// script or Close did, and "" while a connection is still open or none was made.
func (tr *Transcript) ClosedBy() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return tr.closedBy
}

func (tr *Transcript) setClosedBy(who string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	if tr.closedBy == "" {
		tr.closedBy = who
	}
}

// Script is the set of reaction rules a ScriptedPeer follows. A nil hook means
// the honest default: serve the fixture chain.
type Script struct {
	// OnGetHeaders answers a getheaders. The default serves from the locator.
	OnGetHeaders func(p *ScriptedPeer, m *wire.MsgGetHeaders) []wire.Message
	// OnGetData answers a getdata. The default serves every known block.
	// Returning nil sends nothing, which is how a peer withholds blocks.
	OnGetData func(p *ScriptedPeer, m *wire.MsgGetData) []wire.Message
	// OnGetBlocks answers a getblocks. The default sends the inv of blocks
	// after the locator, which is how legacy netsync syncs a chain without
	// checkpoints (regtest).
	OnGetBlocks func(p *ScriptedPeer, m *wire.MsgGetBlocks) []wire.Message
	// OnGetAddr answers a getaddr. The default sends nothing.
	OnGetAddr func(p *ScriptedPeer, m *wire.MsgGetAddr) []wire.Message
	// WriteDelay is waited before each outbound message; nil means none.
	WriteDelay func(msg wire.Message, size int) time.Duration
	// Version may mutate the peer's version message before it is sent.
	Version func(v *wire.MsgVersion)
	// OnConnect is sent right after the peer's verack.
	OnConnect []wire.Message
	// BeforeVersion is sent when the node's version arrives, BEFORE the peer's
	// own version — the "missing-version" offence.
	BeforeVersion []wire.Message
	// StreamPolicies is what the peer's protoconf advertises. Nil means the
	// honest default of a node that allows block priority.
	StreamPolicies []string
	// OnCreateStream answers the node's createstream on a second connection.
	// The default acks it on the same connection, which is what
	// net_processing.cpp:1569-1571 does.
	OnCreateStream func(p *ScriptedPeer, conn net.Conn, m *wire.MsgCreateStream) []wire.Message
}

// Raw is a wire message with an arbitrary command and payload, for sending
// what go-wire's typed messages refuse to build (an addr above MaxAddrPerMsg,
// a headers batch above MaxBlockHeadersPerMsg).
type Raw struct {
	Cmd     string
	Payload []byte
}

// Bsvdecode is unused; Raw is only ever sent.
func (r *Raw) Bsvdecode(_ io.Reader, _ uint32, _ wire.MessageEncoding) error {
	return errors.NewProcessingError("svp2ptest.Raw is write-only")
}

// BsvEncode writes the payload verbatim.
func (r *Raw) BsvEncode(w io.Writer, _ uint32, _ wire.MessageEncoding) error {
	_, err := w.Write(r.Payload)

	return err
}

// Command is the wire command name.
func (r *Raw) Command() string { return r.Cmd }

// MaxPayloadLength is the payload's own length.
func (r *Raw) MaxPayloadLength(_ uint32) uint64 { return uint64(len(r.Payload)) }

// ServeLimit is the script of a peer that serves the first n blocks requested
// of it and withholds the rest. n == 0 withholds everything.
func ServeLimit(n int) Script {
	var (
		mu     sync.Mutex
		served int
	)

	return Script{OnGetData: func(p *ScriptedPeer, m *wire.MsgGetData) []wire.Message {
		var out []wire.Message

		for _, inv := range m.InvList {
			if inv == nil || inv.Type != wire.InvTypeBlock {
				continue
			}

			block, known := p.Chain.Blocks[inv.Hash]
			if !known {
				continue
			}

			mu.Lock()
			allowed := served < n
			if allowed {
				served++
			}
			mu.Unlock()

			if allowed {
				out = append(out, block)
			}
		}

		return out
	}}
}

// ScriptedPeer is a wire-level peer that listens on loopback, completes the
// handshake, and answers the node according to its Script while recording a
// Transcript. It stands in for a real node in integration and parity tests.
type ScriptedPeer struct {
	Addr       string
	Chain      *FixtureChain
	Net        wire.BitcoinNet
	Script     Script
	Transcript *Transcript

	t *testing.T

	mu        sync.Mutex
	ln        net.Listener
	conns     []net.Conn
	closed    bool
	served    int
	conns_    int
	dialled   map[net.Conn]bool
	requested map[chainhash.Hash]int
}

// Connections is how many times the node connected to this peer. With
// legacy_connect_peers a dropped peer is redialed, so a scenario that bounds
// what a peer was handed must bound it per connection.
func (p *ScriptedPeer) Connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.conns_
}

// NewScriptedPeer reserves a loopback address for the peer. With listen true it
// accepts connections at once; otherwise call Listen when the scenario wants
// the peer to appear.
func NewScriptedPeer(t *testing.T, chain *FixtureChain, netMagic wire.BitcoinNet, script Script, listen bool) *ScriptedPeer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	p := &ScriptedPeer{
		Addr:       ln.Addr().String(),
		Chain:      chain,
		Net:        netMagic,
		Script:     script,
		Transcript: &Transcript{},
		t:          t,
		dialled:    make(map[net.Conn]bool),
		requested:  make(map[chainhash.Hash]int),
	}

	require.NoError(t, ln.Close())

	if listen {
		p.Listen()
	}

	t.Cleanup(p.Close)

	return p
}

// Listen starts accepting connections on the reserved address.
func (p *ScriptedPeer) Listen() {
	p.t.Helper()

	ln, err := net.Listen("tcp", p.Addr)
	require.NoError(p.t, err)

	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()

	go p.acceptLoop(ln)
}

func (p *ScriptedPeer) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = conn.Close()

			return
		}

		p.conns = append(p.conns, conn)
		p.conns_++
		p.mu.Unlock()

		go p.serve(conn)
	}
}

// Dial connects to the node as an INBOUND peer (from the node's point of view)
// and serves the connection with the same script. The peer keeps its
// reserved Addr for reporting; the node sees an ephemeral source port.
func (p *ScriptedPeer) Dial(nodeAddr string) error {
	conn, err := net.Dial("tcp", nodeAddr)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.conns = append(p.conns, conn)
	p.conns_++
	p.dialled[conn] = true
	p.mu.Unlock()

	// An inbound peer speaks first.
	local := wire.NewNetAddressIPPort(net.ParseIP("127.0.0.1"), 0, wire.SFNodeNetwork)
	version := wire.NewMsgVersion(local, local, uint64(time.Now().UnixNano()), int32(len(p.Chain.Headers))) //nolint:gosec // fixture height is small
	version.UserAgent = "/Bitcoin SV:1.0.16/"
	version.Services = wire.SFNodeNetwork

	if p.Script.Version != nil {
		p.Script.Version(version)
	}

	if err := p.write(conn, version); err != nil {
		return err
	}

	go p.serve(conn)

	return nil
}

// DialRaw opens a bare TCP connection to the node and sends nothing. The
// connection is tracked so Close ends it, but no serve loop runs on it: the
// caller drives it message by message with Write and ReadOne.
func (p *ScriptedPeer) DialRaw(nodeAddr string) (net.Conn, error) {
	conn, err := net.Dial("tcp", nodeAddr)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.conns = append(p.conns, conn)
	p.mu.Unlock()

	return conn, nil
}

// Write sends one message on conn and records it in the Transcript.
func (p *ScriptedPeer) Write(conn net.Conn, msg wire.Message) error {
	return p.write(conn, msg)
}

// ReadOne reads a single message off conn within timeout.
func ReadOne(t *testing.T, conn net.Conn, netMagic wire.BitcoinNet, timeout time.Duration) wire.Message {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))

	_, msg, _, err := wire.ReadMessageWithEncodingN(conn, wire.ProtocolVersion, netMagic, wire.BaseEncoding)
	require.NoError(t, err)

	return msg
}

// Conns copies the connections this peer holds, in the order they appeared.
func (p *ScriptedPeer) Conns() []net.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]net.Conn(nil), p.conns...)
}

// protoconf is the peer's protocol configuration announcement. The stream
// policies it carries are the ones the node picks its preferred policy from
// (net.cpp:904-921 SetSupportedStreamPolicies).
func (p *ScriptedPeer) protoconf() *wire.MsgProtoconf {
	policies := p.Script.StreamPolicies
	if policies == nil {
		policies = []string{wire.BlockPriorityStreamPolicy, wire.DefaultStreamPolicy}
	}

	return &wire.MsgProtoconf{
		NumberOfFields:       2,
		MaxRecvPayloadLength: wire.DefaultMaxRecvPayloadLength,
		StreamPolicies:       policies,
	}
}

// Close ends every connection and stops listening.
func (p *ScriptedPeer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	p.closed = true
	p.Transcript.setClosedBy("peer")

	if p.ln != nil {
		_ = p.ln.Close()
	}

	for _, c := range p.conns {
		_ = c.Close()
	}
}

// Send writes msg to every live connection.
func (p *ScriptedPeer) Send(msg wire.Message) {
	p.mu.Lock()
	conns := append([]net.Conn(nil), p.conns...)
	p.mu.Unlock()

	for _, c := range conns {
		_ = p.write(c, msg)
	}
}

// Requested reports whether the node ever asked this peer for the block.
func (p *ScriptedPeer) Requested(hash chainhash.Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.requested[hash] > 0
}

// RequestedCount is the number of block getdata items the node sent this peer.
func (p *ScriptedPeer) RequestedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0
	for _, c := range p.requested {
		n += c
	}

	return n
}

// ServedBlocks is the number of block messages this peer sent.
func (p *ScriptedPeer) ServedBlocks() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.served
}

func (p *ScriptedPeer) recordRequest(hash chainhash.Hash) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requested[hash]++
}

func (p *ScriptedPeer) write(conn net.Conn, msg wire.Message) error {
	if p.Script.WriteDelay != nil {
		if d := p.Script.WriteDelay(msg, int(msg.MaxPayloadLength(wire.ProtocolVersion))); d > 0 { //nolint:gosec // bounded by the wire limit
			time.Sleep(d)
		}
	}

	if err := wire.WriteMessage(conn, msg, wire.ProtocolVersion, p.Net); err != nil {
		return err
	}

	p.Transcript.add(conn, Out, msg)

	if msg.Command() == "block" {
		p.mu.Lock()
		p.served++
		p.mu.Unlock()
	}

	return nil
}

func (p *ScriptedPeer) writeAll(conn net.Conn, msgs []wire.Message) bool {
	for _, m := range msgs {
		if p.write(conn, m) != nil {
			return false
		}
	}

	return true
}

func (p *ScriptedPeer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	local := wire.NewNetAddressIPPort(net.ParseIP("127.0.0.1"), 0, wire.SFNodeNetwork)
	remote := wire.NewNetAddressIPPort(net.ParseIP("127.0.0.1"), 0, wire.SFNodeNetwork)

	for {
		_, msg, _, err := wire.ReadMessageWithEncodingN(conn, wire.ProtocolVersion, p.Net, wire.BaseEncoding)
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()

			if !closed {
				p.Transcript.setClosedBy("node")
			}

			return
		}

		p.Transcript.add(conn, In, msg)

		switch m := msg.(type) {
		case *wire.MsgVersion:
			p.mu.Lock()
			weDialled := p.dialled[conn]
			p.mu.Unlock()

			if weDialled {
				// We dialled this connection and already sent our version:
				// answer the node's version with a verack and carry on.
				if !p.writeAll(conn, append([]wire.Message{wire.NewMsgVerAck()}, p.Script.OnConnect...)) {
					return
				}

				continue
			}

			version := wire.NewMsgVersion(local, remote, uint64(time.Now().UnixNano()), int32(len(p.Chain.Headers))) //nolint:gosec // fixture height is small
			version.UserAgent = "/Bitcoin SV:1.0.16/"
			version.Services = wire.SFNodeNetwork

			if p.Script.Version != nil {
				p.Script.Version(version)
			}

			if !p.writeAll(conn, p.Script.BeforeVersion) {
				return
			}

			if !p.writeAll(conn, []wire.Message{version, wire.NewMsgVerAck(), p.protoconf()}) {
				return
			}

			if !p.writeAll(conn, p.Script.OnConnect) {
				return
			}

		case *wire.MsgPing:
			if p.write(conn, wire.NewMsgPong(m.Nonce)) != nil {
				return
			}

		case *wire.MsgGetHeaders:
			var out []wire.Message
			if p.Script.OnGetHeaders != nil {
				out = p.Script.OnGetHeaders(p, m)
			} else {
				out = []wire.Message{p.HeadersFor(m)}
			}

			if !p.writeAll(conn, out) {
				return
			}

		case *wire.MsgGetData:
			for _, inv := range m.InvList {
				if inv != nil && inv.Type == wire.InvTypeBlock {
					p.recordRequest(inv.Hash)
				}
			}

			var out []wire.Message
			if p.Script.OnGetData != nil {
				out = p.Script.OnGetData(p, m)
			} else {
				out = p.blocksFor(m)
			}

			if !p.writeAll(conn, out) {
				return
			}

		case *wire.MsgGetBlocks:
			var out []wire.Message
			if p.Script.OnGetBlocks != nil {
				out = p.Script.OnGetBlocks(p, m)
			} else {
				out = []wire.Message{p.InvFor(m)}
			}

			if !p.writeAll(conn, out) {
				return
			}

		case *wire.MsgCreateStream:
			var out []wire.Message
			if p.Script.OnCreateStream != nil {
				out = p.Script.OnCreateStream(p, conn, m)
			} else {
				out = []wire.Message{wire.NewMsgStreamAck(m.AssociationID, m.StreamType)}
			}

			if !p.writeAll(conn, out) {
				return
			}

		case *wire.MsgGetAddr:
			if p.Script.OnGetAddr != nil && !p.writeAll(conn, p.Script.OnGetAddr(p, m)) {
				return
			}
		}
	}
}

func (p *ScriptedPeer) blocksFor(m *wire.MsgGetData) []wire.Message {
	var out []wire.Message

	for _, inv := range m.InvList {
		if inv == nil || inv.Type != wire.InvTypeBlock {
			continue
		}

		if block, known := p.Chain.Blocks[inv.Hash]; known {
			out = append(out, block)
		}
	}

	return out
}

// HeadersFor is the honest getheaders answer: up to MaxBlockHeadersPerMsg
// headers after the first locator hash the chain knows, stopping at HashStop.
func (p *ScriptedPeer) HeadersFor(msg *wire.MsgGetHeaders) *wire.MsgHeaders {
	start := int32(0)

	for _, hash := range msg.BlockLocatorHashes {
		if hash == nil {
			continue
		}

		if height, known := p.Chain.Heights[*hash]; known {
			start = height
			break
		}
	}

	headers := wire.NewMsgHeaders()

	for i := start; i < int32(len(p.Chain.Headers)); i++ { //nolint:gosec // fixture height is small
		if len(headers.Headers) == wire.MaxBlockHeadersPerMsg {
			break
		}

		header := p.Chain.Headers[i]

		_ = headers.AddBlockHeader(header)

		if msg.HashStop != (chainhash.Hash{}) && header.BlockHash() == msg.HashStop {
			break
		}
	}

	return headers
}

// InvFor is the honest getblocks answer: the inventory of up to MaxBlocksPerMsg
// blocks after the first locator hash the chain knows, stopping at HashStop.
func (p *ScriptedPeer) InvFor(msg *wire.MsgGetBlocks) *wire.MsgInv {
	start := int32(0)

	for _, hash := range msg.BlockLocatorHashes {
		if hash == nil {
			continue
		}

		if height, known := p.Chain.Heights[*hash]; known {
			start = height
			break
		}
	}

	inv := wire.NewMsgInv()

	for i := start; i < int32(len(p.Chain.Headers)); i++ { //nolint:gosec // fixture height is small
		if len(inv.InvList) == wire.MaxBlocksPerMsg {
			break
		}

		hash := p.Chain.Headers[i].BlockHash()

		_ = inv.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, &hash))

		if msg.HashStop != (chainhash.Hash{}) && hash == msg.HashStop {
			break
		}
	}

	return inv
}

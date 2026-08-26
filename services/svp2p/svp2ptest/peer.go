package svp2ptest

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
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

// Entry is one message in a Transcript.
type Entry struct {
	At  time.Time
	Dir Direction
	Cmd string
	Msg wire.Message
}

// Transcript records every message a ScriptedPeer exchanged and who ended the
// connection. It is the harness's evidence.
type Transcript struct {
	mu       sync.Mutex
	entries  []Entry
	closedBy string
}

func (tr *Transcript) add(dir Direction, msg wire.Message) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	tr.entries = append(tr.entries, Entry{At: time.Now(), Dir: dir, Cmd: msg.Command(), Msg: msg})
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
	// OnGetAddr answers a getaddr. The default sends nothing.
	OnGetAddr func(p *ScriptedPeer, m *wire.MsgGetAddr) []wire.Message
	// WriteDelay is waited before each outbound message; nil means none.
	WriteDelay func(msg wire.Message, size int) time.Duration
	// Version may mutate the peer's version message before it is sent.
	Version func(v *wire.MsgVersion)
	// OnConnect is sent right after the peer's verack.
	OnConnect []wire.Message
}

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
	requested map[chainhash.Hash]int
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
		p.mu.Unlock()

		go p.serve(conn)
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

	p.Transcript.add(Out, msg)

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

		p.Transcript.add(In, msg)

		switch m := msg.(type) {
		case *wire.MsgVersion:
			version := wire.NewMsgVersion(local, remote, uint64(time.Now().UnixNano()), int32(len(p.Chain.Headers))) //nolint:gosec // fixture height is small
			version.UserAgent = "/svp2p-scripted-peer:1.0/"
			version.Services = wire.SFNodeNetwork

			if p.Script.Version != nil {
				p.Script.Version(version)
			}

			if !p.writeAll(conn, []wire.Message{version, wire.NewMsgVerAck(), wire.NewMsgProtoconf(wire.DefaultMaxRecvPayloadLength, true)}) {
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

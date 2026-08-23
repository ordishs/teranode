// Package transport is the net.cpp port: it owns sockets, framing, and
// per-connection flow control. It knows nothing about protocol decisions.
package transport

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// ErrSendQueueFull carries its own code because the teranode errors package
// matches errors.Is by code, and callers distinguish this condition.
var ErrSendQueueFull = errors.New(errors.ERR_THRESHOLD_EXCEEDED, "svp2p: send queue full")

type Config struct {
	Net             wire.BitcoinNet
	ProtocolVersion uint32
	SendBudgetBytes int
	RecvQueueLen    int
	WriteTimeout    time.Duration
}

// queuedMsg is one entry on a send lane. Exactly one of msg and block is set:
// msg takes go-wire's framing path, block takes the streamed path (see
// SendBlock). Both share one lane so that a getdata answered with a mix of tx
// and block messages reaches the socket in request order — two lanes would
// leave the order to the writer's select.
type queuedMsg struct {
	msg   wire.Message
	size  int
	block *blockSend
}

// Conn wraps one net.Conn with a reader goroutine and a writer goroutine.
// Receive flow control: the reader blocks when inbound is full (the
// net.cpp fPauseRecv contract expressed as a bounded channel).
// Send flow control: Send is the droppable lane bounded by a byte budget
// (net.cpp nSendSize vs GetSendBufferSize); SendPriority is the reserved
// lane for mandatory protocol replies (pong, verack); SendBlock is the
// streamed lane, which is bounded by the socket itself rather than by the
// byte budget (see SendBlock).
type Conn struct {
	nc            net.Conn
	cfg           Config
	pver          atomic.Uint32
	inbound       chan wire.Message
	inboundBlocks chan *BlockStream
	sendCh        chan queuedMsg
	priCh         chan queuedMsg
	pending       atomic.Int64
	sent          atomic.Uint64
	received      atomic.Uint64
	quit          chan struct{}
	done          chan struct{}
	sockClosed    chan struct{}
	errMu         sync.Mutex
	err           error
	quitOnce      sync.Once
	closeOnce     sync.Once
}

func New(nc net.Conn, cfg Config) *Conn {
	c := &Conn{
		nc:      nc,
		cfg:     cfg,
		inbound: make(chan wire.Message, cfg.RecvQueueLen),
		// Unbuffered: the read loop hands the socket to one consumer at a
		// time and waits for it. See InboundBlocks.
		inboundBlocks: make(chan *BlockStream),
		sendCh:        make(chan queuedMsg, 64),
		priCh:         make(chan queuedMsg, 16),
		quit:          make(chan struct{}),
		done:          make(chan struct{}),
		sockClosed:    make(chan struct{}),
	}
	c.pver.Store(cfg.ProtocolVersion)

	return c
}

func (c *Conn) Start(ctx context.Context) {
	var wg sync.WaitGroup

	wg.Add(2)

	go func() { defer wg.Done(); c.readLoop() }()
	go func() { defer wg.Done(); c.writeLoop() }()

	go func() {
		wg.Wait()
		close(c.done)
	}()

	go func() {
		select {
		case <-ctx.Done():
			c.fail(ctx.Err())
		case <-c.quit:
		}
	}()
}

func (c *Conn) readLoop() {
	defer close(c.inbound)
	defer close(c.inboundBlocks)

	for {
		n, hdr, err := readWireHeader(c.nc)

		c.received.Add(uint64(n)) //nolint:gosec // byte count is never negative

		if err != nil {
			c.fail(err)
			return
		}

		// Detect "block" before any payload byte is materialized, and hand
		// the socket to the consumer as a stream. An extmsg-wrapped block
		// carries the outer "extmsg" command, so it takes the buffered path
		// below; extended headers are deferred per the phase plan.
		if hdr.command == wire.CmdBlock {
			if err := c.readBlock(hdr); err != nil {
				c.fail(err)
				return
			}

			continue
		}

		// Every other command keeps go-wire's framing path, checksum
		// verification included: replay the header bytes we already took.
		r := io.MultiReader(bytes.NewReader(hdr.raw[:]), c.nc)

		total, msg, _, err := wire.ReadMessageWithEncodingN(r, c.pver.Load(), c.cfg.Net, wire.BaseEncoding)

		if total > wire.MessageHeaderSize {
			c.received.Add(uint64(total - wire.MessageHeaderSize)) //nolint:gosec // header bytes are already counted
		}

		if err != nil {
			c.fail(err)
			return
		}

		select {
		case c.inbound <- msg: // blocks when full: receive backpressure
		case <-c.quit:
			return
		}
	}
}

// readBlock owns the socket for the whole declared block payload. It delivers
// the stream, then blocks until the consumer closes it. An error returned here
// means the connection is no longer byte aligned, or the peer framing is
// broken, so the caller fails the connection.
func (c *Conn) readBlock(hdr wireHeader) error {
	// readWireHeader bypasses the checks wire.ReadMessageWithEncodingN makes
	// on the buffered path, so repeat the two that matter for a block.
	if hdr.magic != c.cfg.Net {
		return errors.New(errors.ERR_NETWORK_INVALID_RESPONSE, "svp2p: block message from other network [%v]", hdr.magic)
	}

	length := uint64(hdr.length)
	if length > wire.MaxBlockPayload() {
		return errors.New(errors.ERR_NETWORK_INVALID_RESPONSE,
			"svp2p: block payload of %d bytes exceeds the %d byte maximum", length, wire.MaxBlockPayload())
	}

	bs, err := newBlockStream(c.nc, length, c.pver.Load())

	// Payload bytes are charged as they leave the socket, in two steps: the
	// part the read loop decoded itself, then whatever the consumer read plus
	// whatever the drain discarded. A stream the consumer closes is always
	// drained to the boundary, so a completed block charges the full declared
	// length, as the buffered path does. The paths that charge less are the
	// ones that fail the connection.
	counted := bs.consumed()
	c.received.Add(counted)

	if err != nil {
		return err
	}

	defer func() { c.received.Add(bs.consumed() - counted) }()

	select {
	case c.inboundBlocks <- bs:
	case <-c.quit:
		return errors.New(errors.ERR_ERROR, "svp2p: connection closed before the block stream was delivered")
	case <-c.sockClosed:
		return errors.New(errors.ERR_ERROR, "svp2p: socket closed before the block stream was delivered")
	}

	select {
	case <-bs.done:
	case <-c.quit:
		return errors.New(errors.ERR_ERROR, "svp2p: connection closed with a block stream still open")
	case <-c.sockClosed:
		return errors.New(errors.ERR_ERROR, "svp2p: socket closed with a block stream still open")
	}

	// Close is idempotent; this call reports whether the drain reached the
	// payload boundary. A short stream leaves the socket misaligned.
	return bs.Close()
}

func (c *Conn) writeLoop() {
	for {
		var q queuedMsg

		select {
		case q = <-c.priCh:
		default:
			select {
			case q = <-c.priCh:
			case q = <-c.sendCh:
				c.pending.Add(int64(-q.size))
			case <-c.quit:
				return
			}
		}

		// A streamed block is written HERE, on the single writer, for the
		// whole header and body. See SendBlock.
		if q.block != nil {
			// Deliberately vague about WHICH fault, because writeBlock has
			// three falsy returns and they do not share a symptom: a partial
			// header write, a body that ends early — both of which misalign
			// the socket — and a payload that changed between the two passes,
			// where the socket is perfectly aligned and only the checksum is
			// wrong. All three are unrecoverable and all three end the
			// connection; the precise cause already reached the caller through
			// bs.done, so naming any one of them here would mislead an
			// operator reading the log.
			if !c.writeBlock(q.block) {
				c.fail(errors.New(errors.ERR_ERROR, "svp2p: block send failed mid-frame, the connection cannot be reused"))
				return
			}

			continue
		}

		n, err := wire.WriteMessageWithEncodingN(c.nc, q.msg, c.pver.Load(), c.cfg.Net, wire.BaseEncoding)

		c.sent.Add(uint64(n)) //nolint:gosec // byte count is never negative

		if err != nil {
			c.fail(err)
			return
		}
	}
}

// invVectPayloadBytes is one serialized inventory vector: a uint32 type plus a
// 32 byte hash. It mirrors maxInvVectPayload (go-wire invvect.go:20, v1.2.10),
// which is
// not exported.
const invVectPayloadBytes = 4 + chainhash.HashSize

// sendCost is what one queued message is charged against SendBudgetBytes.
//
// go-wire's MaxPayloadLength is a WORST CASE, and for the variable-length
// messages it is a CONSTANT that ignores the receiver entirely: MsgInv,
// MsgNotFound and MsgGetData all report
// MaxVarIntPayload + MaxInvPerMsg*maxInvVectPayload = 1,800,009 bytes
// (go-wire MaxPayloadLength, msg_inv.go:116-119, v1.2.10), and MsgHeaders reports
// MaxVarIntPayload + 81*MaxBlockHeadersPerMsg = 162,009, no matter what any of
// them hold. A one-entry notfound of about
// 40 bytes was therefore charged 1.8 MB, five of them filled the whole 10 MB
// budget, and the sixth was refused — and a dropped notfound leaves a peer
// waiting out its own request timeout for something we already knew we did not
// have. Those messages are exactly what the serving path emits per getdata
// pass (protocol/getdata.go servePass), which is what made the defect reachable
// and is why it was fixed here.
//
// This IS the replacement Send's own comment used to promise for "Phase 2"; the
// promise is discharged and that wording is gone, so nobody should go looking
// for it as outstanding work. What remains outstanding is smaller: a message
// type not listed below whose MaxPayloadLength is a loose bound is still
// overcharged, harmlessly so far because nothing queues those in volume.
//
// Deliberately NOT claiming that covers everything this service sends, because
// it does not: wire.MsgGetHeaders is constructed on the sync path
// (protocol/headersync.go, protocol/blockdownload.go) and goes out on this same
// budgeted lane, and its bound is 16,045 bytes against a real encoded size near
// 69 for a one-locator request (4 + varint(1) + 32 + 32). Nothing is broken by
// that — 653 of them fit the budget and the sync path sends one at a time — so
// it is left alone rather
// than fixed speculatively. What IS listed is every type the SERVING path
// queues per pass, which is where the volume is.
//
// Every other type keeps the bound, which for the fixed-size messages IS the
// encoded size. The clamp applies to both paths, so a message larger than the
// whole budget is still admitted alone rather than refused for ever.
func sendCost(msg wire.Message, pver uint32, budget int) int {
	size, ok := exactPayloadSize(msg)
	if !ok {
		size = int(msg.MaxPayloadLength(pver)) //nolint:gosec // clamped below
	}

	if size > budget || size < 0 {
		size = budget
	}

	return size
}

// headerEntryPayloadBytes is one entry of a headers message: the 80 byte block
// header plus the transaction-count varint that always follows it, always zero
// and so always one byte. It is go-wire's own `MaxBlockHeaderPayload + 1`
// (go-wire MsgHeaders.MaxPayloadLength, msg_headers.go:131-136, v1.2.10), and
// BsvEncode writes exactly that —
// writeBlockHeader then WriteVarInt(0) per header.
const headerEntryPayloadBytes = wire.MaxBlockHeaderPayload + 1

// exactPayloadSize is the encoded payload size of the message types whose
// MaxPayloadLength is a worst case far above their real size. It is O(1) from a
// slice length — never a trial encode.
func exactPayloadSize(msg wire.Message) (int, bool) {
	switch m := msg.(type) {
	// The three inventory-carrying messages: count varint plus one 36 byte
	// vector per entry. All three share the same 1,800,009 byte bound.
	case *wire.MsgInv:
		return invListSize(len(m.InvList)), true
	case *wire.MsgNotFound:
		return invListSize(len(m.InvList)), true
	case *wire.MsgGetData:
		return invListSize(len(m.InvList)), true

	// headers is bounded at MaxVarIntPayload + 81*MaxBlockHeadersPerMsg =
	// 162,009 bytes whatever it holds, so an EMPTY headers reply was charged
	// 162 KB. That is 64 of them against the 10 MB budget rather than the one
	// the inventory types allowed, so it never starved a connection the way
	// they did — but it is the same defect in the same function, and a
	// getheaders reply is common enough during serving that the collateral
	// refusal would have been silent.
	case *wire.MsgHeaders:
		return wire.VarIntSerializeSize(uint64(len(m.Headers))) + len(m.Headers)*headerEntryPayloadBytes, true //nolint:gosec // a slice length is never negative
	}

	return 0, false
}

// invListSize is the encoded size of an inventory list: the count varint plus
// one vector per entry.
func invListSize(n int) int {
	return wire.VarIntSerializeSize(uint64(n)) + n*invVectPayloadBytes //nolint:gosec // a list length is never negative
}

// Send enqueues on the droppable lane, charged against the byte budget by
// sendCost. The budget is only ONE of two bounds here: sendCh's depth is the
// other, and they fail differently — an exhausted byte budget refuses with
// ErrSendQueueFull, while a full channel BLOCKS this call until the writer
// drains one.
func (c *Conn) Send(msg wire.Message) error {
	size := sendCost(msg, c.pver.Load(), c.cfg.SendBudgetBytes)

	if c.pending.Add(int64(size)) > int64(c.cfg.SendBudgetBytes) {
		c.pending.Add(int64(-size))
		return ErrSendQueueFull
	}

	select {
	case c.sendCh <- queuedMsg{msg: msg, size: size}:
		return nil
	case <-c.quit:
		c.pending.Add(int64(-size))
		return c.Err()
	}
}

func (c *Conn) SendPriority(msg wire.Message) error {
	t := time.NewTimer(c.cfg.WriteTimeout)
	defer t.Stop()

	select {
	case c.priCh <- queuedMsg{msg: msg}:
		return nil
	case <-c.quit:
		return c.Err()
	case <-t.C:
		return errors.New(errors.ERR_ERROR, "svp2p: priority send timed out")
	}
}

func (c *Conn) SetProtocolVersion(v uint32) { c.pver.Store(v) }

func (c *Conn) Inbound() <-chan wire.Message { return c.inbound }

// InboundBlocks carries inbound "block" messages as streams: the payload stays
// on the socket instead of arriving as a decoded wire.MsgBlock on Inbound.
//
// The read loop BLOCKS on the connection until the consumer calls Close on the
// stream. Nothing else — no ping, no inv — is read from that peer meanwhile.
// This is the synchronous backpressure baseline; a prefetch budget is the only
// thing that relaxes it. A consumer that abandons a stream stalls the peer
// until the connection closes.
//
// Because a consumer may hold a stream for minutes while it ingests a fat
// block, the transport sets no socket read deadline. The liveness check is the
// idle timer in services/svp2p/protocol, which the caller must hold off while
// a stream is open.
func (c *Conn) InboundBlocks() <-chan *BlockStream { return c.inboundBlocks }

func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()

	return c.err
}

func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

func (c *Conn) BytesSent() uint64 { return c.sent.Load() }

func (c *Conn) BytesReceived() uint64 { return c.received.Load() }

func (c *Conn) fail(err error) {
	c.quitOnce.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()

		close(c.quit)
	})

	_ = c.closeNC()
}

// Close shuts the socket; the reader's resulting error cascades through
// fail, which closes quit and releases the writer.
func (c *Conn) Close() error { return c.closeNC() }

func (c *Conn) closeNC() error {
	var err error

	c.closeOnce.Do(func() {
		err = c.nc.Close()

		// A read loop parked on an open block stream is not waiting on the
		// socket, so it needs the close signalled explicitly.
		close(c.sockClosed)
	})

	return err
}

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

type queuedMsg struct {
	msg  wire.Message
	size int
}

// Conn wraps one net.Conn with a reader goroutine and a writer goroutine.
// Receive flow control: the reader blocks when inbound is full (the
// net.cpp fPauseRecv contract expressed as a bounded channel).
// Send flow control: Send is the droppable lane bounded by a byte budget
// (net.cpp nSendSize vs GetSendBufferSize); SendPriority is the reserved
// lane for mandatory protocol replies (pong, verack).
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

		n, err := wire.WriteMessageWithEncodingN(c.nc, q.msg, c.pver.Load(), c.cfg.Net, wire.BaseEncoding)

		c.sent.Add(uint64(n)) //nolint:gosec // byte count is never negative

		if err != nil {
			c.fail(err)
			return
		}
	}
}

// Send enqueues on the droppable lane. The byte budget uses
// MaxPayloadLength as a cost estimate, which overcharges small messages of
// variable-size types; Phase 2 replaces the estimate with the encoded size
// when the relay lane starts carrying real volume.
func (c *Conn) Send(msg wire.Message) error {
	size := int(msg.MaxPayloadLength(c.pver.Load())) //nolint:gosec // clamped below
	if size > c.cfg.SendBudgetBytes || size < 0 {
		size = c.cfg.SendBudgetBytes
	}

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

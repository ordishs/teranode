// Package transport is the net.cpp port: it owns sockets, framing, and
// per-connection flow control. It knows nothing about protocol decisions.
package transport

import (
	"context"
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
	nc        net.Conn
	cfg       Config
	pver      atomic.Uint32
	inbound   chan wire.Message
	sendCh    chan queuedMsg
	priCh     chan queuedMsg
	pending   atomic.Int64
	sent      atomic.Uint64
	received  atomic.Uint64
	quit      chan struct{}
	done      chan struct{}
	errMu     sync.Mutex
	err       error
	quitOnce  sync.Once
	closeOnce sync.Once
}

func New(nc net.Conn, cfg Config) *Conn {
	c := &Conn{
		nc:      nc,
		cfg:     cfg,
		inbound: make(chan wire.Message, cfg.RecvQueueLen),
		sendCh:  make(chan queuedMsg, 64),
		priCh:   make(chan queuedMsg, 16),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
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

	for {
		n, msg, _, err := wire.ReadMessageWithEncodingN(c.nc, c.pver.Load(), c.cfg.Net, wire.BaseEncoding)

		c.received.Add(uint64(n)) //nolint:gosec // byte count is never negative

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

	c.closeOnce.Do(func() { err = c.nc.Close() })

	return err
}

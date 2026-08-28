package transport

import (
	"context"
	"net"
	"sync"
	"sync/atomic"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// ErrAssociationClosed reports an operation on an association that has already
// torn down. Attach returns it rather than adopting a stream nothing will ever
// service.
var ErrAssociationClosed = errors.New(errors.ERR_ERROR, "svp2p: association closed")

// ErrStreamExists reports a second stream of a type the association already
// holds. association.cpp:137-160 MoveStream throws in the same case: one
// stream per type is the invariant, and a peer that asks twice is misbehaving.
var ErrStreamExists = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: stream type already attached")

// Association is the multi-stream form of PeerConn: one Conn per StreamType,
// GENERAL always present and DATA1 optional (association.cpp:43). It merges
// every stream's inbound traffic into one channel pair, routes outbound
// traffic by the negotiated StreamPolicy, and holds the teardown invariant
// that any one stream failing closes them all, exactly once.
//
// # Locking
//
// mu is a LEAF lock. It guards the stream map, the policy and the closed flag,
// and nothing that blocks may run while it is held: never a Conn send, never a
// channel close, never a wg.Wait. Every method that needs a stream resolves it
// under mu, releases, and only then calls into the Conn. The one thing that
// must happen under mu is wg.Add for a stream's forwarders, because fail
// snapshots the streams under the same lock before it waits — see fail.
type Association struct {
	id      []byte
	mu      sync.Mutex
	streams map[wire.StreamType]*Conn
	policy  StreamPolicy
	closed  bool
	started bool

	pver atomic.Uint32

	inbound       chan wire.Message
	inboundBlocks chan *BlockStream

	quit chan struct{}
	done chan struct{}

	errMu sync.Mutex
	err   error

	closeOnce sync.Once
	wg        sync.WaitGroup

	ctx context.Context
}

// NewAssociation builds an association around its GENERAL stream. The policy
// starts as Default (stream_policy.cpp:98-103) and stays there until
// negotiation completes and SetPolicy replaces it.
func NewAssociation(general *Conn, id []byte) *Association {
	policy, _ := PolicyForName(wire.DefaultStreamPolicy)

	a := &Association{
		id:      id,
		streams: map[wire.StreamType]*Conn{wire.StreamTypeGeneral: general},
		policy:  policy,
		// The merged channel inherits GENERAL's depth, so the association
		// applies the same receive backpressure a single Conn does.
		inbound: make(chan wire.Message, general.cfg.RecvQueueLen),
		// Unbuffered, like Conn.inboundBlocks: the reader that produced the
		// stream is parked on the socket until the consumer closes it.
		inboundBlocks: make(chan *BlockStream),
		quit:          make(chan struct{}),
		done:          make(chan struct{}),
		ctx:           context.Background(),
	}
	a.pver.Store(general.pver.Load())

	return a
}

func (a *Association) ID() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.id
}

// SetID records the association ID an inbound peer named in its version
// message (net_processing.cpp:1775 SetAssociationID). An outbound association
// is built with the ID we generated and never reaches this.
func (a *Association) SetID(id []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.id = id
}

func (a *Association) SetPolicy(p StreamPolicy) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.policy = p
}

func (a *Association) Policy() StreamPolicy {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.policy
}

func (a *Association) Streams() []wire.StreamType {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]wire.StreamType, 0, len(a.streams))
	for t := range a.streams {
		out = append(out, t)
	}

	return out
}

// MaxBlockPayload reports the block payload ceiling the named stream enforces,
// and whether the association holds that stream at all. Every stream of one
// association is built from the same node configuration, so the numbers agree;
// they are reported per stream because each stream is a separate socket with
// its own Config.
func (a *Association) MaxBlockPayload(t wire.StreamType) (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	c, ok := a.streams[t]
	if !ok {
		return 0, false
	}

	return c.MaxBlockPayload(), true
}

func (a *Association) HasStream(t wire.StreamType) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, ok := a.streams[t]

	return ok
}

// Start runs every stream the association holds, GENERAL and any stream
// attached before this call, all of them on the caller's context. A stream
// attached AFTER this call is started by Attach instead. Nothing runs on a
// context the caller did not supply, so cancellation always reaches every
// stream.
func (a *Association) Start(ctx context.Context) {
	a.mu.Lock()

	if a.started || a.closed {
		a.mu.Unlock()
		return
	}

	a.started = true
	a.ctx = ctx

	pending := make([]*Conn, 0, len(a.streams))
	for _, c := range a.streams {
		pending = append(pending, c)
	}

	a.wg.Add(2 * len(pending))

	a.mu.Unlock()

	pver := a.pver.Load()

	for _, c := range pending {
		c.SetProtocolVersion(pver)
		c.Start(ctx)
		a.forward(c)

		go a.watch(c)
	}
}

// Attach adopts a further stream. The stream must carry a type the association
// does not already hold (association.cpp:137-160); anything else is refused
// and the rejected Conn is closed here, because the caller has handed over
// ownership of the socket by calling this.
//
// Attaching BEFORE Start is allowed and is how a stream negotiated during the
// handshake is registered. Such a stream is stored but NOT started here: Start
// starts it with the caller's context, because starting it now would bind it
// to a context the caller never chose and cancellation would never reach it.
func (a *Association) Attach(c *Conn) error {
	a.mu.Lock()

	if a.closed {
		a.mu.Unlock()
		_ = c.Close()

		return ErrAssociationClosed
	}

	t := c.StreamType()

	if _, exists := a.streams[t]; exists {
		a.mu.Unlock()
		_ = c.Close()

		return ErrStreamExists
	}

	a.streams[t] = c

	if !a.started {
		a.mu.Unlock()

		return nil
	}

	ctx := a.ctx

	// Under mu, so that fail cannot snapshot this stream and then reach
	// wg.Wait before these forwarders are counted.
	a.wg.Add(2)

	a.mu.Unlock()

	// The version the association negotiated, applied before the stream reads
	// or writes a byte: an extended block frame on DATA1 is refused unless
	// this stream also knows the peer is at ExtendedPayloadVersion.
	c.SetProtocolVersion(a.pver.Load())

	c.Start(ctx)
	a.forward(c)

	go a.watch(c)

	return nil
}

// forward merges one stream's two inbound channels into the association's.
// The caller has already added 2 to wg under mu.
func (a *Association) forward(c *Conn) {
	go func() {
		defer a.wg.Done()

		for {
			select {
			case msg, ok := <-c.Inbound():
				if !ok {
					return
				}

				select {
				case a.inbound <- msg:
				case <-a.quit:
					return
				}
			case <-a.quit:
				return
			}
		}
	}()

	go func() {
		defer a.wg.Done()

		for {
			select {
			case bs, ok := <-c.InboundBlocks():
				if !ok {
					return
				}

				select {
				case a.inboundBlocks <- bs:
				case <-a.quit:
					return
				}
			case <-a.quit:
				return
			}
		}
	}()
}

// watch turns one stream's death into the association's. association.cpp:93-109
// Shutdown closes every stream of the association together, so a peer never
// half-survives on the streams that are still healthy.
func (a *Association) watch(c *Conn) {
	select {
	case <-c.Done():
		a.fail(c.Err())
	case <-a.quit:
	}
}

// fail is the single teardown path, taken at most once. The order matters:
// mark closed and snapshot the streams under mu, release mu, close quit so
// every forwarder can leave, close the streams, wait for the forwarders, and
// only then close the merged channels. Closing them before the wait would race
// a forwarder into a send on a closed channel; waiting before closing quit
// would deadlock on a forwarder parked on a full merged channel.
func (a *Association) fail(cause error) {
	a.closeOnce.Do(func() {
		if cause == nil {
			cause = ErrAssociationClosed
		}

		a.errMu.Lock()
		a.err = cause
		a.errMu.Unlock()

		a.mu.Lock()
		a.closed = true

		streams := make([]*Conn, 0, len(a.streams))
		for _, c := range a.streams {
			streams = append(streams, c)
		}

		a.mu.Unlock()

		close(a.quit)

		for _, c := range streams {
			_ = c.Close()
		}

		a.wg.Wait()

		close(a.inbound)
		close(a.inboundBlocks)
		close(a.done)
	})
}

func (a *Association) Inbound() <-chan wire.Message { return a.inbound }

func (a *Association) InboundBlocks() <-chan *BlockStream { return a.inboundBlocks }

// streamFor resolves the stream a message routes to. association.cpp:205-210:
// when the policy names a type the association does not hold, the send falls
// back to GENERAL, which is always present.
func (a *Association) streamFor(msg wire.Message) *Conn {
	a.mu.Lock()
	defer a.mu.Unlock()

	if c, ok := a.streams[a.policy.StreamFor(msg)]; ok {
		return c
	}

	return a.streams[wire.StreamTypeGeneral]
}

func (a *Association) general() *Conn {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.streams[wire.StreamTypeGeneral]
}

func (a *Association) Send(msg wire.Message) error {
	return a.streamFor(msg).Send(msg)
}

// SendPriority always takes GENERAL. The priority lane carries the mandatory
// protocol replies, and GENERAL is the one stream every association has for
// the whole of its life.
func (a *Association) SendPriority(msg wire.Message) error {
	return a.general().SendPriority(msg)
}

// SendBlock routes on a block probe, so a BlockPriority association with DATA1
// attached streams the block down DATA1 and a Default one keeps it on GENERAL
// (stream_policy.cpp:187-195).
func (a *Association) SendBlock(ctx context.Context, req BlockSendRequest) error {
	return a.streamFor(&wire.MsgBlock{}).SendBlock(ctx, req)
}

// SetProtocolVersion reaches every stream the association holds now, and the
// stored value reaches every stream Attach adopts later.
func (a *Association) SetProtocolVersion(v uint32) {
	a.pver.Store(v)

	a.mu.Lock()

	streams := make([]*Conn, 0, len(a.streams))
	for _, c := range a.streams {
		streams = append(streams, c)
	}

	a.mu.Unlock()

	for _, c := range streams {
		c.SetProtocolVersion(v)
	}
}

func (a *Association) Close() error {
	a.fail(nil)

	return nil
}

func (a *Association) Err() error {
	a.errMu.Lock()
	defer a.errMu.Unlock()

	return a.err
}

func (a *Association) Done() <-chan struct{} { return a.done }

func (a *Association) RemoteAddr() net.Addr { return a.general().RemoteAddr() }

// BytesSent is the sum over the streams (association.cpp:370-375 sums the
// per-stream counters the same way).
func (a *Association) BytesSent() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	var total uint64
	for _, c := range a.streams {
		total += c.BytesSent()
	}

	return total
}

func (a *Association) BytesReceived() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	var total uint64
	for _, c := range a.streams {
		total += c.BytesReceived()
	}

	return total
}

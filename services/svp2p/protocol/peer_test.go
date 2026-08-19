package protocol

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
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

// writeAsync writes on its own goroutine and ignores the outcome. A block
// message is bigger than net.Pipe's zero buffer, so the write only completes
// once the consumer has taken the whole payload — which is exactly what a
// stalled-ingest test is holding up.
func (s *scriptedPeer) writeAsync(msg wire.Message) {
	go func() {
		_, _ = wire.WriteMessageWithEncodingN(s.nc, msg, wire.ProtocolVersion, wire.MainNet, wire.BaseEncoding)
	}()
}

// writeStalledBlockFrame writes a "block" message that declares `declared`
// payload bytes but carries only what the streaming transport reads before it
// hands the stream to its consumer: the 80 byte block header and the
// transaction count. The rest never arrives, so anything that tries to read
// the remaining payload blocks for ever.
func (s *scriptedPeer) writeStalledBlockFrame(t *testing.T, header *wire.BlockHeader, declared uint32) {
	t.Helper()

	var payload bytes.Buffer

	require.NoError(t, header.Serialize(&payload))
	require.NoError(t, wire.WriteVarInt(&payload, wire.ProtocolVersion, 1))

	frame := make([]byte, wire.MessageHeaderSize)
	binary.LittleEndian.PutUint32(frame[0:4], uint32(wire.MainNet))
	copy(frame[4:4+wire.CommandSize], wire.CmdBlock)
	binary.LittleEndian.PutUint32(frame[16:20], declared)
	// Bytes 20:24 are the payload checksum, which the streaming path does not
	// verify (see the note on transport.BlockStream).

	_, err := s.nc.Write(frame)
	require.NoError(t, err)

	_, err = s.nc.Write(payload.Bytes())
	require.NoError(t, err)
}

// readUntil reads messages until one carries the wanted command, so a test
// can assert on a sync message without scripting every ping and sendheaders
// that shares the lane with it.
func (s *scriptedPeer) readUntil(t *testing.T, want string) wire.Message {
	t.Helper()

	for i := 0; i < 64; i++ {
		msg := s.read(t)
		if msg.Command() == want {
			return msg
		}
	}

	t.Fatalf("no %s message received", want)

	return nil
}

func newTestPeer(t *testing.T, idle, ping time.Duration) (*Peer, *scriptedPeer) {
	t.Helper()

	return newIngestingTestPeer(t, idle, ping, nil)
}

func newIngestingTestPeer(t *testing.T, idle, ping time.Duration, ingestor BlockIngestor) (*Peer, *scriptedPeer) {
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
		Ingestor: ingestor,
	}

	return NewPeer(cfg), &scriptedPeer{nc: b}
}

// blockingIngestor holds a block stream open the way a real ingest of a fat
// block does, so a test can drive the peer's idle timer against it.
type blockingIngestor struct {
	// readFirstByte makes the ingest take one payload byte before it stalls,
	// which is what separates a stalled peer from our own local pre-read
	// waits (bridge.ProgressReader's documented rule).
	readFirstByte bool

	started chan struct{}
	release chan struct{}
}

func newBlockingIngestor(readFirstByte bool) *blockingIngestor {
	return &blockingIngestor{
		readFirstByte: readFirstByte,
		started:       make(chan struct{}, 1),
		release:       make(chan struct{}),
	}
}

func (b *blockingIngestor) WatchProgress(r io.ReadCloser) IngestProgress {
	return newTestProgress(r)
}

func (b *blockingIngestor) Ingest(ctx context.Context, req BlockIngestRequest) IngestOutcome {
	defer func() { _ = req.TxReader.Close() }()

	if b.readFirstByte {
		if _, err := io.ReadFull(req.TxReader, make([]byte, 1)); err != nil {
			return IngestOutcome{Err: err}
		}
	}

	select {
	case b.started <- struct{}{}:
	default:
	}

	select {
	case <-b.release:
	case <-req.Quit:
	case <-ctx.Done():
	}

	return IngestOutcome{}
}

// TestPeerIdleTimerToleratesLocalIngestWait is the ProgressReader rule the
// peer loop must honour: an ingest that has read no payload byte yet is
// waiting on OUR services (WaitForBlockAssemblyReady,
// waitForPreviousBlockMined), and the peer must not be dropped for it.
func TestPeerIdleTimerToleratesLocalIngestWait(t *testing.T) {
	const idle = 150 * time.Millisecond

	ingestor := newBlockingIngestor(false)

	p, far := newIngestingTestPeer(t, idle, time.Hour, ingestor)

	errCh := make(chan error, 1)

	go func() { errCh <- p.Run(context.Background()) }()

	defer func() {
		p.Disconnect("test teardown")
		close(ingestor.release)
	}()

	completeHandshake(t, far)

	genesis := syncGenesis()
	far.writeAsync(blockFor(minedChild(genesis, testEasyBits, 9)))

	select {
	case <-ingestor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the block never reached the ingestor")
	}

	select {
	case err := <-errCh:
		t.Fatalf("peer disconnected during a local ingest wait: %v", err)
	case <-time.After(4 * idle):
	}
}

// TestPeerIdleTimerDropsStalledIngest is the other half of the rule: once
// payload bytes have started moving, they have to keep moving.
func TestPeerIdleTimerDropsStalledIngest(t *testing.T) {
	const idle = 150 * time.Millisecond

	ingestor := newBlockingIngestor(true)

	p, far := newIngestingTestPeer(t, idle, time.Hour, ingestor)

	errCh := make(chan error, 1)

	go func() { errCh <- p.Run(context.Background()) }()

	defer close(ingestor.release)

	completeHandshake(t, far)

	genesis := syncGenesis()
	far.writeAsync(blockFor(minedChild(genesis, testEasyBits, 10)))

	select {
	case <-ingestor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the block never reached the ingestor")
	}

	select {
	case err := <-errCh:
		require.Error(t, err)
		require.Contains(t, err.Error(), "idle")
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not disconnect after the ingest stopped making progress")
	}
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

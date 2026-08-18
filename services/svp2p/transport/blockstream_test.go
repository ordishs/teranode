package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

func testMsgTx(seq uint32) *wire.MsgTx {
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{byte(seq)}, seq), []byte{0x51, byte(seq)}))
	tx.AddTxOut(wire.NewTxOut(int64(1000+seq), []byte{0x76, 0xa9, byte(seq)}))

	return tx
}

func testMsgBlock(t *testing.T, txCount int) *wire.MsgBlock {
	t.Helper()

	hdr := wire.NewBlockHeader(1, &chainhash.Hash{0x11}, &chainhash.Hash{0x22}, 0x1d00ffff, 0x9988)
	hdr.Timestamp = time.Unix(1700000000, 0)

	blk := wire.NewMsgBlock(hdr)

	for i := 0; i < txCount; i++ {
		require.NoError(t, blk.AddTransaction(testMsgTx(uint32(i)))) //nolint:gosec // small test loop counter
	}

	return blk
}

// txRegion returns the payload bytes that follow the 80 byte block header and
// the transaction count varint: exactly what TxReader must deliver.
func txRegion(t *testing.T, blk *wire.MsgBlock) []byte {
	t.Helper()

	var buf bytes.Buffer

	for _, tx := range blk.Transactions {
		require.NoError(t, tx.Serialize(&buf))
	}

	return buf.Bytes()
}

func blockPayload(t *testing.T, blk *wire.MsgBlock) []byte {
	t.Helper()

	var buf bytes.Buffer

	require.NoError(t, blk.BsvEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding))

	return buf.Bytes()
}

// writeRawFrame writes a message frame with a caller-chosen payload so tests
// can declare a length the payload does not honour.
func writeRawFrame(t *testing.T, w io.Writer, magic wire.BitcoinNet, cmd string, payload []byte) {
	t.Helper()

	var hdr [wire.MessageHeaderSize]byte

	binary.LittleEndian.PutUint32(hdr[0:4], uint32(magic))
	copy(hdr[4:4+wire.CommandSize], cmd)
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(len(payload))) //nolint:gosec // test payloads are small
	copy(hdr[20:24], chainhash.DoubleHashB(payload)[0:4])

	_, err := w.Write(hdr[:])
	require.NoError(t, err)

	_, err = w.Write(payload)
	require.NoError(t, err)
}

func recvBlock(t *testing.T, c *Conn) *BlockStream {
	t.Helper()

	select {
	case bs, open := <-c.InboundBlocks():
		require.True(t, open, "inbound block channel closed")
		require.NotNil(t, bs)

		return bs
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a block stream")

		return nil
	}
}

func recvPing(t *testing.T, c *Conn, nonce uint64) {
	t.Helper()

	select {
	case msg, open := <-c.Inbound():
		require.True(t, open, "inbound channel closed")

		ping, ok := msg.(*wire.MsgPing)
		require.True(t, ok, "expected a ping, got %T", msg)
		require.Equal(t, nonce, ping.Nonce)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the message after the block")
	}
}

func TestBlockStreamRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 3)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)

	gotHdr := bs.Header()

	require.Equal(t, blk.Header.BlockHash(), gotHdr.BlockHash())
	require.Equal(t, blk.Header.Timestamp.Unix(), gotHdr.Timestamp.Unix())
	require.Equal(t, blk.Header.Bits, gotHdr.Bits)
	require.Equal(t, blk.Header.Nonce, gotHdr.Nonce)
	require.Equal(t, uint64(3), bs.TxCount())

	got, err := io.ReadAll(bs.TxReader())
	require.NoError(t, err)
	require.Equal(t, txRegion(t, blk), got)

	require.NoError(t, bs.Close())
}

func TestBlockStreamNoBlockOnInbound(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 1)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
		_ = wire.WriteMessage(a, wire.NewMsgPing(7), wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)

	select {
	case msg := <-cb.Inbound():
		t.Fatalf("block leaked onto Inbound() as %T", msg)
	default:
	}

	require.NoError(t, bs.Close())
	recvPing(t, cb, 7)
}

// A payload that declares more transactions than it carries must surface the
// shortfall to the consumer while leaving the connection byte aligned.
func TestBlockStreamTruncatedPayloadKeepsAlignment(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 1)
	payload := blockPayload(t, blk)

	// Byte 80 is the transaction count varint; claim three, carry one.
	require.Equal(t, byte(1), payload[80])
	payload[80] = 3

	go func() {
		writeRawFrame(t, a, wire.MainNet, wire.CmdBlock, payload)
		_ = wire.WriteMessage(a, wire.NewMsgPing(99), wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)
	require.Equal(t, uint64(3), bs.TxCount())

	var first wire.MsgTx
	require.NoError(t, first.Bsvdecode(bs.TxReader(), wire.ProtocolVersion, wire.BaseEncoding))

	var second wire.MsgTx
	require.Error(t, second.Bsvdecode(bs.TxReader(), wire.ProtocolVersion, wire.BaseEncoding),
		"a transaction beyond the payload boundary must fail")

	require.NoError(t, bs.Close())

	recvPing(t, cb, 99)
}

func TestBlockStreamEarlyCloseKeepsAlignment(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 4)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
		_ = wire.WriteMessage(a, wire.NewMsgPing(11), wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)

	var first wire.MsgTx
	require.NoError(t, first.Bsvdecode(bs.TxReader(), wire.ProtocolVersion, wire.BaseEncoding))

	// Abandon the remaining three transactions.
	require.NoError(t, bs.Close())

	recvPing(t, cb, 11)
}

func TestBlockStreamCloseWithoutReadingKeepsAlignment(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 5)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
		_ = wire.WriteMessage(a, wire.NewMsgPing(12), wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)
	require.NoError(t, bs.Close())

	recvPing(t, cb, 12)
}

func TestBlockStreamReadAfterCloseFails(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 2)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)
	require.NoError(t, bs.Close())

	_, err := bs.TxReader().Read(make([]byte, 8))
	require.ErrorIs(t, err, ErrBlockStreamClosed)
}

// Close is reachable from any goroutine and must be idempotent.
func TestBlockStreamCloseIdempotentAndConcurrent(t *testing.T) {
	t.Parallel()

	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 2)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
		_ = wire.WriteMessage(a, wire.NewMsgPing(13), wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)

	var wg sync.WaitGroup

	errs := make([]error, 4)

	for i := 0; i < 4; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			errs[i] = bs.Close()
		}(i)
	}

	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}

	recvPing(t, cb, 13)
}

// The read loop blocks until the consumer closes the stream: a slow consumer
// delays every later message on the connection.
func TestBlockStreamBlocksReadLoop(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 1)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
		_ = wire.WriteMessage(a, wire.NewMsgPing(21), wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)

	select {
	case msg := <-cb.Inbound():
		t.Fatalf("read loop advanced past an open block stream: %T", msg)
	case <-time.After(250 * time.Millisecond):
	}

	require.NoError(t, bs.Close())

	recvPing(t, cb, 21)
}

// A consumer that never closes the stream must not wedge Conn shutdown.
func TestBlockStreamAbandonedReleasedByConnClose(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 2)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
	}()

	_ = recvBlock(t, cb)

	require.NoError(t, cb.Close())

	select {
	case <-cb.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("conn did not shut down with an abandoned block stream")
	}

	select {
	case _, open := <-cb.InboundBlocks():
		require.False(t, open)
	case <-time.After(5 * time.Second):
		t.Fatal("inbound block channel did not close")
	}
}

// The peer declares more payload than it sends and then hangs up: the drain
// cannot reach the boundary, so Close reports the shortfall.
func TestBlockStreamShortStreamSurfacesError(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 2)
	payload := blockPayload(t, blk)

	go func() {
		var hdr [wire.MessageHeaderSize]byte

		binary.LittleEndian.PutUint32(hdr[0:4], uint32(wire.MainNet))
		copy(hdr[4:4+wire.CommandSize], wire.CmdBlock)
		binary.LittleEndian.PutUint32(hdr[16:20], uint32(len(payload)+512)) //nolint:gosec // test payload is small
		copy(hdr[20:24], chainhash.DoubleHashB(payload)[0:4])

		_, _ = a.Write(hdr[:])
		_, _ = a.Write(payload)
		_ = a.Close()
	}()

	bs := recvBlock(t, cb)

	_, _ = io.Copy(io.Discard, bs.TxReader())

	require.Error(t, bs.Close(), "a short stream must surface as an error")

	select {
	case <-cb.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("conn did not shut down after a short block stream")
	}

	require.Error(t, cb.Err())
}

func TestBlockStreamMalformedHeaderFailsConn(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	go func() {
		// 40 bytes cannot hold an 80 byte block header.
		writeRawFrame(t, a, wire.MainNet, wire.CmdBlock, make([]byte, 40))
	}()

	select {
	case bs, open := <-cb.InboundBlocks():
		require.False(t, open, "a malformed block must not be delivered, got %v", bs)
	case <-time.After(5 * time.Second):
		t.Fatal("inbound block channel did not close")
	}

	<-cb.Done()
	require.Error(t, cb.Err())
}

func TestBlockStreamAccountsFullPayloadBytes(t *testing.T) {
	a, b := net.Pipe()
	cb := New(b, testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb.Start(ctx)

	blk := testMsgBlock(t, 3)
	payload := blockPayload(t, blk)

	go func() {
		_ = wire.WriteMessage(a, blk, wire.ProtocolVersion, wire.MainNet)
		_ = wire.WriteMessage(a, wire.NewMsgPing(31), wire.ProtocolVersion, wire.MainNet)
	}()

	bs := recvBlock(t, cb)
	require.NoError(t, bs.Close())

	recvPing(t, cb, 31)

	// Block frame plus the 32 byte ping frame, whatever the consumer read.
	want := uint64(wire.MessageHeaderSize+len(payload)) + uint64(wire.MessageHeaderSize+8)
	require.Equal(t, want, cb.BytesReceived())
}

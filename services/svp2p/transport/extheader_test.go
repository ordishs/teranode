package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

// protocol.cpp:220-237: extended header = basic header with command "extmsg",
// length 0xffffffff, zero checksum, then command(12) + payload length(8).
func TestReadWireHeader_ParsesExtendedBlockHeader(t *testing.T) {
	want := uint64(math.MaxUint32) + 1
	hdr := extBlockFrameHeader(wire.MainNet, want)
	require.Len(t, hdr, extHeaderSize)

	n, h, err := readWireHeader(bytes.NewReader(hdr))
	require.NoError(t, err)
	require.Equal(t, extHeaderSize, n)
	require.True(t, h.extended)
	require.Equal(t, wire.CmdBlock, h.command)
	require.Equal(t, want, h.length)
	require.Equal(t, wire.MainNet, h.magic)
	require.Equal(t, [4]byte{}, h.checksum)
}

func TestReadWireHeader_BasicHeaderUnchanged(t *testing.T) {
	hdr := blockFrameHeader(wire.MainNet, 1000, [4]byte{1, 2, 3, 4})

	n, h, err := readWireHeader(bytes.NewReader(hdr))
	require.NoError(t, err)
	require.Equal(t, wire.MessageHeaderSize, n)
	require.False(t, h.extended)
	require.Equal(t, uint64(1000), h.length)
	require.Equal(t, [4]byte{1, 2, 3, 4}, h.checksum)
}

// An "extmsg" command whose length is not 0xffffffff is not an extended
// header; it is a malformed basic message and is read as such.
func TestReadWireHeader_ExtmsgCommandWithoutMarkerIsBasic(t *testing.T) {
	raw := make([]byte, wire.MessageHeaderSize)
	binary.LittleEndian.PutUint32(raw[0:4], uint32(wire.MainNet))
	copy(raw[4:16], wire.CmdExtMsg)
	binary.LittleEndian.PutUint32(raw[16:20], 7)

	_, h, err := readWireHeader(bytes.NewReader(raw))
	require.NoError(t, err)
	require.False(t, h.extended)
	require.Equal(t, wire.CmdExtMsg, h.command)
}

func TestReadWireHeader_TruncatedExtensionIsAnError(t *testing.T) {
	hdr := extBlockFrameHeader(wire.MainNet, uint64(math.MaxUint32)+1)

	_, _, err := readWireHeader(bytes.NewReader(hdr[:30]))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

// An extended block streams to the consumer with its 64-bit length.
func TestReadLoop_ExtendedBlockIsStreamed(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()

	c := New(b, Config{Net: wire.MainNet, ProtocolVersion: 70016, SendBudgetBytes: 1 << 20, RecvQueueLen: 4, WriteTimeout: time.Second, MaxBlockPayload: 8 << 30})
	c.Start(context.Background())
	defer c.Close()

	length := uint64(math.MaxUint32) + 1
	go func() {
		_, _ = a.Write(extBlockFrameHeader(wire.MainNet, length))
		_, _ = io.CopyN(a, zeroReader{}, 200) // only the first bytes; the test reads the declared length, not the body
		_ = a.Close()                         // let bs.Close()'s drain see EOF instead of blocking on bytes never sent
	}()

	select {
	case bs := <-c.InboundBlocks():
		require.Equal(t, length, bs.Length())
		_ = bs.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("no block stream delivered")
	}
}

// A peer below 70016 may not send extended messages (version.h:51).
func TestReadLoop_ExtendedFromOldPeerDisconnects(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()

	c := New(b, Config{Net: wire.MainNet, ProtocolVersion: 70015, SendBudgetBytes: 1 << 20, RecvQueueLen: 4, WriteTimeout: time.Second})
	c.Start(context.Background())

	go func() { _, _ = a.Write(extBlockFrameHeader(wire.MainNet, uint64(math.MaxUint32)+1)) }()

	<-c.Done()
	require.ErrorIs(t, c.Err(), ErrExtendedVersion)
}

// An extended NON-block message is bounded by the advertised receive limit.
func TestReadLoop_ExtendedNonBlockOverLimitDisconnects(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()

	c := New(b, Config{Net: wire.MainNet, ProtocolVersion: 70016, SendBudgetBytes: 1 << 20, RecvQueueLen: 4, WriteTimeout: time.Second})
	c.Start(context.Background())

	hdr := extBlockFrameHeader(wire.MainNet, uint64(math.MaxUint32)+1)
	copy(hdr[24:36], make([]byte, 12))
	copy(hdr[24:36], wire.CmdInv)

	go func() { _, _ = a.Write(hdr) }()

	<-c.Done()
	require.ErrorIs(t, c.Err(), ErrExtendedTooLarge)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}

	return len(p), nil
}

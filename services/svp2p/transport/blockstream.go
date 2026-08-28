package transport

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// ErrBlockStreamClosed is returned by TxReader once the stream is closed.
var ErrBlockStreamClosed = errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: block stream is closed")

// minTxPayloadBytes is the smallest byte count a serialized transaction can
// occupy: version 4 + input count varint 1 + output count varint 1 +
// nLockTime 4. It mirrors minTxPayload in go-wire msg_tx.go:72, which
// maxTxPerBlock in msg_block.go:40-42 uses to bound the transaction count on
// the buffered decode path.
const minTxPayloadBytes = 10

// wireHeader is the 24 byte message header, kept raw so a non-block message
// can be replayed into go-wire's own framing path unchanged. An extended
// header (protocol.cpp:220-237) additionally carries ext, the 20 extension
// bytes read past the basic header, also kept raw for replay.
type wireHeader struct {
	raw      [wire.MessageHeaderSize]byte
	magic    wire.BitcoinNet
	command  string
	length   uint64
	checksum [4]byte
	extended bool
	ext      [wire.CommandSize + 8]byte
}

// readWireHeader reads one message header. It deliberately stops before the
// payload so the read loop can pick the streaming path for "block" before any
// payload byte is materialized.
//
// protocol.cpp:257-263 CMessageHeader::Read: the header is extended when the
// COMMAND is "extmsg" and the basic length field carries the 0xffffffff
// marker; the real command and a 64-bit length follow in the extension.
func readWireHeader(r io.Reader) (int, wireHeader, error) {
	var h wireHeader

	n, err := io.ReadFull(r, h.raw[:])
	if err != nil {
		return n, h, err
	}

	h.magic = wire.BitcoinNet(binary.LittleEndian.Uint32(h.raw[0:4]))
	h.command = string(bytes.TrimRight(h.raw[4:4+wire.CommandSize], "\x00"))
	basicLen := binary.LittleEndian.Uint32(h.raw[16:20])
	copy(h.checksum[:], h.raw[20:24])
	h.length = uint64(basicLen)

	if h.command == wire.CmdExtMsg && basicLen == extLengthMarker {
		m, err := io.ReadFull(r, h.ext[:])
		n += m

		if err != nil {
			if err == io.EOF { //nolint:errorlint // io.ReadFull never wraps io.EOF
				err = io.ErrUnexpectedEOF
			}

			return n, h, err
		}

		h.extended = true
		h.command = string(bytes.TrimRight(h.ext[:wire.CommandSize], "\x00"))
		h.length = binary.LittleEndian.Uint64(h.ext[wire.CommandSize:])
	}

	return n, h, nil
}

// BlockStream is one inbound block payload that stays on the socket. The read
// loop decodes the 80 byte block header and the transaction count, then hands
// the connection to the consumer bounded to the declared payload length. The
// transactions are never buffered: on fat blocks (multi-GB testnet stress
// blocks) the payload buffer alone reached ~2.86 GB of legacy heap inuse
// during sync, the second-largest contributor to RSS after the per-tx scratch
// buffer.
//
// Note on the wire-level DoubleHash checksum: the buffered path in
// wire.ReadMessageWithEncodingN verifies the peer-supplied checksum over the
// payload bytes. This path skips it, so the early-rejection signal that
// checksum provides is lost. Integrity is preserved by downstream block
// validation — PoW, merkle root reconstruction, and per-tx parse plus
// validate. Any payload corruption that a wire-level checksum would have
// caught also fails one of those downstream checks; what we give up is
// rejecting a bad block before paying the decode cost. Preserving the checksum
// under streaming would require a TeeReader → SHA-256 pass over multi-GB
// payloads, which is not justified given the downstream guarantees. Same
// tradeoff as services/legacy/peer/wire_streaming.go streamingBlockHandler,
// scoped per connection instead of the process-global wire handler.
//
// The consumer owns the stream until it calls Close. Close is idempotent and
// safe from any goroutine.
type BlockStream struct {
	header   wire.BlockHeader
	txCount  uint64
	length   uint64
	extended bool

	// mu serializes TxReader reads against the drain that Close performs,
	// because Close may run on a goroutine other than the reading one.
	mu       sync.Mutex
	lr       *io.LimitedReader
	closed   bool
	drainErr error

	done      chan struct{}
	closeOnce sync.Once
}

// newBlockStream decodes the fixed part of a block payload and returns the
// stream positioned at the first transaction. It returns a stream even on
// error so the caller can account for the bytes consumed.
func newBlockStream(r io.Reader, length uint64, pver uint32) (*BlockStream, error) {
	// Cap the reader at the declared payload length so a malformed varint
	// cannot read past the message boundary and desync the next header.
	// Mirrors services/legacy/peer/wire_streaming.go streamingBlockHandler.
	b := &BlockStream{
		length: length,
		lr:     &io.LimitedReader{R: r, N: int64(length)}, //nolint:gosec // length is bounded by MaxBlockPayload
		done:   make(chan struct{}),
	}

	if err := b.header.Bsvdecode(b.lr, pver, wire.BaseEncoding); err != nil {
		return b, err
	}

	count, err := wire.ReadVarInt(b.lr, pver)
	if err != nil {
		return b, err
	}

	// Bound the count the way the buffered path does. go-wire rejects above
	// maxTxPerBlock, which is (MaxBlockPayload/minTxPayload) + 1 — the
	// quotient PLUS ONE, msg_block.go:40-42 (phase-2 ledger, Task 7 nit: this
	// comment used to name the bare quotient). Here the divisor applies to the
	// unread remainder of this payload, so the bound is tighter than the
	// buffered path's and never looser: the remainder can at most equal
	// MaxBlockPayload, which leaves this bound one below theirs even then.
	// Consumers size their ingest from TxCount, so this must not admit a
	// number the buffered path would have refused.
	if count > uint64(b.lr.N)/minTxPayloadBytes { //nolint:gosec // lr.N is non-negative
		return b, errors.New(errors.ERR_NETWORK_INVALID_RESPONSE,
			"svp2p: block declares %d transactions in %d remaining payload bytes", count, b.lr.N)
	}

	b.txCount = count

	return b, nil
}

// Header returns the decoded 80 byte block header.
func (b *BlockStream) Header() wire.BlockHeader { return b.header }

// TxCount returns the transaction count the peer declared.
func (b *BlockStream) TxCount() uint64 { return b.txCount }

// Length returns the payload length the peer declared for this block. It is
// known before a single transaction byte is read, which makes it the honest
// weight for an admission budget keyed on block size — the consumer cannot
// derive that from the header or the transaction count.
func (b *BlockStream) Length() uint64 { return b.length }

// Extended reports whether this block arrived framed with the extended
// message header (protocol.cpp:220-237), i.e. its declared payload exceeded
// MaxBlockFrameBytes.
func (b *BlockStream) Extended() bool { return b.extended }

// TxReader returns the transaction bytes, bounded to the declared payload
// length. It reports io.EOF at the payload boundary, so a payload that carries
// fewer transactions than TxCount surfaces as a decode error to the consumer.
//
// It is an io.ReadCloser, not a bare io.Reader, so a consumer that only ever
// sees the reader can still release the stream: Close forwards to
// BlockStream.Close. That matters because the read loop stays parked on this
// connection until the stream closes, and a consumer handed only the reader
// would otherwise have no way to release it.
func (b *BlockStream) TxReader() io.ReadCloser { return txReader{b: b} }

// Close drains any unread payload bytes so the connection stays aligned on the
// next message header, and releases the read loop. It is idempotent: every
// call returns the same result. Close waits for any in-flight TxReader read to
// return, so a reader blocked on a silent peer holds Close until the socket
// closes.
func (b *BlockStream) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.drainErr = b.drainLocked()
		b.mu.Unlock()

		close(b.done)
	})

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.drainErr
}

// drainLocked mirrors services/legacy/peer/wire_streaming.go
// streamingBlockHandler: io.Copy on a LimitedReader returns nil if the
// underlying reader EOFs before N reaches 0, so lr.N must be checked
// explicitly. Otherwise an undersized stream would silently desync every
// later read.
func (b *BlockStream) drainLocked() error {
	if b.lr.N <= 0 {
		return nil
	}

	if _, err := io.Copy(io.Discard, b.lr); err != nil {
		return err
	}

	if b.lr.N > 0 {
		return errors.New(errors.ERR_NETWORK_INVALID_RESPONSE,
			"svp2p: peer declared %d byte block payload but stream ended with %d bytes unread", b.length, b.lr.N)
	}

	return nil
}

// consumed reports the payload bytes taken off the socket so far.
func (b *BlockStream) consumed() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.length - uint64(b.lr.N) //nolint:gosec // lr.N is non-negative and never exceeds length
}

func (b *BlockStream) read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return 0, ErrBlockStreamClosed
	}

	return b.lr.Read(p)
}

type txReader struct{ b *BlockStream }

func (t txReader) Read(p []byte) (int, error) { return t.b.read(p) }

// Close releases the whole stream, not just this reader: it is the same
// idempotent BlockStream.Close, so closing the reader drains any unread
// payload and frees the parked read loop.
func (t txReader) Close() error { return t.b.Close() }

package protocol

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/stretchr/testify/require"
)

// compactFixtureTxCount is the size of the fixture block every table row in
// this file reconstructs. It is large enough that the prefilled/short-ID index
// offset walk (blockencodings.cpp:140-146) is exercised over many slots.
const compactFixtureTxCount = 50

// compactFixtureNonce is an arbitrary but fixed cmpctblock nonce, so the short
// IDs a row computes are stable across runs.
const compactFixtureNonce = uint64(0x0123456789abcdef)

// compactFixtureTx builds transaction i of the fixture block. Every
// transaction differs in its outpoint, script and value, so no two share a
// txid and therefore no two share a short ID by accident.
func compactFixtureTx(i int) *wire.MsgTx {
	tx := wire.NewMsgTx(1)

	var prev chainhash.Hash

	binary.LittleEndian.PutUint64(prev[:8], uint64(i)+1) //nolint:gosec // fixture indexes are small

	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&prev, uint32(i)), []byte{0x51, byte(i)})) //nolint:gosec // fixture indexes are small
	tx.AddTxOut(wire.NewTxOut(int64(1000+i), []byte{0x76, 0xa9, byte(i), 0x88, 0xac}))

	return tx
}

// compactFixtureBlock builds a block of n transactions with a non-null header
// (Bits != 0, primitives/block.h:60 CBlockHeader::IsNull).
func compactFixtureBlock(t *testing.T, n int) *wire.MsgBlock {
	t.Helper()

	var prev, merkle chainhash.Hash

	prev[0] = 0xaa
	merkle[0] = 0xbb

	blk := wire.NewMsgBlock(wire.NewBlockHeader(1, &prev, &merkle, 0x207fffff, 7))

	for i := 0; i < n; i++ {
		require.NoError(t, blk.AddTransaction(compactFixtureTx(i)))
	}

	return blk
}

// encodeTx returns the wire bytes of one transaction, the same bytes a
// blocktxn payload or a block body carries.
func encodeTx(t *testing.T, tx *wire.MsgTx) []byte {
	t.Helper()

	var buf bytes.Buffer

	require.NoError(t, tx.BsvEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding))

	return buf.Bytes()
}

// encodeBlock returns the wire bytes of a whole block payload: the 80 byte
// header, the compactsize transaction count, then the transactions in order.
func encodeBlock(t *testing.T, blk *wire.MsgBlock) []byte {
	t.Helper()

	var buf bytes.Buffer

	require.NoError(t, blk.BsvEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding))

	return buf.Bytes()
}

// compactMsgFor builds the cmpctblock a peer would send for blk, prefilling
// exactly the given block indexes and carrying a short ID for every other
// transaction, in block order.
func compactMsgFor(t *testing.T, blk *wire.MsgBlock, prefilled ...int) *wire.MsgCmpctBlock {
	t.Helper()

	isPrefilled := make(map[int]bool, len(prefilled))
	for _, i := range prefilled {
		isPrefilled[i] = true
	}

	msg := &wire.MsgCmpctBlock{Header: blk.Header, Nonce: compactFixtureNonce}
	k0, k1 := ShortIDKeys(&blk.Header, msg.Nonce)

	for i, tx := range blk.Transactions {
		if isPrefilled[i] {
			require.NoError(t, msg.AddPrefilledTransaction(uint32(i), tx)) //nolint:gosec // fixture indexes are small

			continue
		}

		msg.ShortIDs = append(msg.ShortIDs, ShortID(k0, k1, tx.TxHash()))
	}

	return msg
}

// compactTestIndex is a TxIndex whose held set and collision reports the test
// controls exactly, so a row can put any slot into any of the three states
// InitData produces: prefilled, held, or missing.
type compactTestIndex struct {
	raw       map[chainhash.Hash][]byte
	openFails map[chainhash.Hash]bool
	// collideAt marks positions in the block's short ID list that the index
	// reports as a collision: a nil hash plus collision=true, which is what
	// RecentTxIndex.Match does for a short ID two indexed hashes share
	// (blockencodings.cpp:181-190).
	collideAt map[int]bool
}

func newCompactTestIndex() *compactTestIndex {
	return &compactTestIndex{
		raw:       make(map[chainhash.Hash][]byte),
		openFails: make(map[chainhash.Hash]bool),
		collideAt: make(map[int]bool),
	}
}

// hold adds a transaction to the index under its own txid.
func (f *compactTestIndex) hold(t *testing.T, tx *wire.MsgTx) {
	t.Helper()

	f.raw[tx.TxHash()] = encodeTx(t, tx)
}

func (f *compactTestIndex) Match(k0, k1 uint64, shortIDs []uint64) ([]*chainhash.Hash, bool) {
	byShort := make(map[uint64]chainhash.Hash, len(f.raw))
	for h := range f.raw {
		byShort[ShortID(k0, k1, h)] = h
	}

	out := make([]*chainhash.Hash, len(shortIDs))
	collision := false

	for i, id := range shortIDs {
		if f.collideAt[i] {
			collision = true

			continue
		}

		if h, ok := byShort[id]; ok {
			found := h
			out[i] = &found
		}
	}

	return out, collision
}

func (f *compactTestIndex) Open(_ context.Context, hash chainhash.Hash) (io.ReadCloser, uint64, error) {
	if f.openFails[hash] {
		return nil, 0, ErrTxUnknown
	}

	raw, ok := f.raw[hash]
	if !ok {
		return nil, 0, ErrTxUnknown
	}

	return io.NopCloser(bytes.NewReader(raw)), uint64(len(raw)), nil
}

// gapStream concatenates the wire encodings of the transactions at the given
// block indexes, which is the shape transport.TxnStream's Reader() hands the
// state: back to back transactions with no framing between them.
func gapStream(t *testing.T, blk *wire.MsgBlock, indexes []uint32) io.Reader {
	t.Helper()

	var buf bytes.Buffer

	for _, i := range indexes {
		buf.Write(encodeTx(t, blk.Transactions[i]))
	}

	return &buf
}

// drain reads a reader to EOF and closes it, returning the bytes and the read
// error, so a row can assert on both the payload and a mid-stream failure.
func drain(t *testing.T, rc io.ReadCloser) ([]byte, error) {
	t.Helper()

	out, err := io.ReadAll(rc)
	require.NoError(t, rc.Close())

	return out, err
}

// TestCompactState_AllHeld_AssemblesWithoutGapRequest is SVNode's best case:
// every short ID resolves in the index, so FillBlock has nothing to wait for
// and the block is assembled straight away (spec §6 step 5,
// net_processing.cpp:3931-3945).
func TestCompactState_AllHeld_AssemblesWithoutGapRequest(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	idx := newCompactTestIndex()
	for i := 1; i < len(blk.Transactions); i++ {
		idx.hold(t, blk.Transactions[i])
	}

	c, status := newCompactState(msg, idx)
	require.Equal(t, readOK, status)
	require.NotNil(t, c)
	require.Empty(t, c.missing)
	require.Nil(t, c.gapRequest())

	count, rc := c.assemble(context.Background(), idx, nil)
	require.Equal(t, uint64(compactFixtureTxCount), count)

	got, err := drain(t, rc)
	require.NoError(t, err)
	require.Equal(t, encodeBlock(t, blk), got)
}

// TestCompactState_HalfHeld_GapRequestIndexes proves the getblocktxn a partial
// match produces: the absolute indexes of exactly the slots the index could
// not fill, strictly increasing so the differential encoding accepts them
// (blockencodings.h:36-82).
func TestCompactState_HalfHeld_GapRequestIndexes(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	idx := newCompactTestIndex()

	var want []uint32

	for i := 1; i < len(blk.Transactions); i++ {
		if i%2 == 0 {
			idx.hold(t, blk.Transactions[i])

			continue
		}

		want = append(want, uint32(i)) //nolint:gosec // fixture indexes are small
	}

	c, status := newCompactState(msg, idx)
	require.Equal(t, readOK, status)

	req := c.gapRequest()
	require.NotNil(t, req)
	require.Equal(t, blk.BlockHash(), req.BlockHash)
	require.Equal(t, want, req.Indexes)
	require.True(t, c.requested)

	var buf bytes.Buffer

	require.NoError(t, req.BsvEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding))

	var round wire.MsgGetBlockTxn

	require.NoError(t, round.Bsvdecode(&buf, wire.ProtocolVersion, wire.BaseEncoding))
	require.Equal(t, want, round.Indexes)
}

// TestCompactState_FillAndAssemble runs the full round trip: gaps requested,
// a blocktxn carrying exactly those transactions, then a block body identical
// to the one the peer would have sent as a plain block message.
func TestCompactState_FillAndAssemble(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0, 17, 33)

	idx := newCompactTestIndex()
	for i := 1; i < len(blk.Transactions); i++ {
		if i != 17 && i != 33 && i%3 != 0 {
			idx.hold(t, blk.Transactions[i])
		}
	}

	c, status := newCompactState(msg, idx)
	require.Equal(t, readOK, status)

	req := c.gapRequest()
	require.NotNil(t, req)
	require.NotEmpty(t, req.Indexes)

	gaps := gapStream(t, blk, req.Indexes)

	fillStatus, err := c.fill(uint64(len(req.Indexes)), gaps)
	require.NoError(t, err)
	require.Equal(t, readOK, fillStatus)

	count, rc := c.assemble(context.Background(), idx, gaps)
	require.Equal(t, uint64(compactFixtureTxCount), count)

	got, drainErr := drain(t, rc)
	require.NoError(t, drainErr)
	require.Equal(t, encodeBlock(t, blk), got)
	require.Equal(t, readOK, c.fillStatus())
}

// TestCompactState_AssembleTxs_OmitsHeaderAndCount pins the reader the ingest
// seam takes: BlockIngestRequest carries the header and the count as fields,
// so its TxReader is the transactions alone (spec §6 step 7).
func TestCompactState_AssembleTxs_OmitsHeaderAndCount(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	idx := newCompactTestIndex()
	for i := 1; i < len(blk.Transactions); i++ {
		idx.hold(t, blk.Transactions[i])
	}

	c, status := newCompactState(msg, idx)
	require.Equal(t, readOK, status)

	count, rc := c.assembleTxs(context.Background(), idx, nil)
	require.Equal(t, uint64(compactFixtureTxCount), count)

	got, err := drain(t, rc)
	require.NoError(t, err)

	body := encodeBlock(t, blk)
	require.Equal(t, body[len(body)-len(got):], got)
	require.Less(t, len(got), len(body))
}

// TestCompactState_Fill_WrongCount is FillBlock's count check: a blocktxn that
// does not carry exactly the requested number of transactions is a bogus
// message, not a reconstruction failure (blockencodings.cpp:268-270, :283-285).
func TestCompactState_Fill_WrongCount(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	idx := newCompactTestIndex()
	for i := 1; i < len(blk.Transactions); i += 2 {
		idx.hold(t, blk.Transactions[i])
	}

	c, status := newCompactState(msg, idx)
	require.Equal(t, readOK, status)

	req := c.gapRequest()
	require.NotNil(t, req)

	for _, count := range []uint64{uint64(len(req.Indexes)) - 1, uint64(len(req.Indexes)) + 1, 0} {
		fillStatus, err := c.fill(count, bytes.NewReader(nil))
		require.Equal(t, readInvalid, fillStatus)
		require.Error(t, err)
	}
}

// TestCompactState_Fill_ShortIDMismatch is the collision case SVNode detects
// through CheckBlock's merkle root (blockencodings.cpp:288-298,
// "Possible Short ID collision" → READ_STATUS_FAILED). This port checks the
// short ID of every supplied transaction against its own slot while the
// stream runs, so the failure is FAILED and never a score.
func TestCompactState_Fill_ShortIDMismatch(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	idx := newCompactTestIndex()
	for i := 1; i < len(blk.Transactions); i++ {
		if i%2 == 0 {
			idx.hold(t, blk.Transactions[i])
		}
	}

	c, status := newCompactState(msg, idx)
	require.Equal(t, readOK, status)

	req := c.gapRequest()
	require.NotNil(t, req)
	require.NotEmpty(t, req.Indexes)

	var buf bytes.Buffer

	buf.Write(encodeTx(t, compactFixtureTx(9999)))

	for _, i := range req.Indexes[1:] {
		buf.Write(encodeTx(t, blk.Transactions[i]))
	}

	fillStatus, err := c.fill(uint64(len(req.Indexes)), &buf)
	require.NoError(t, err)
	require.Equal(t, readOK, fillStatus)

	_, rc := c.assemble(context.Background(), idx, &buf)

	_, drainErr := drain(t, rc)
	require.Error(t, drainErr)
	require.ErrorIs(t, drainErr, ErrCompactFillFailed)
	require.Equal(t, readFailed, c.fillStatus())
}

// TestCompactState_DuplicateShortIDs is InitData's own short ID map check
// (blockencodings.cpp:148-169). SVNode returns FAILED there; the Phase 5 spec
// makes a duplicate INSIDE the message INVALID, because only the sender can
// produce one, and reserves FAILED for the index-side collision it cannot.
func TestCompactState_DuplicateShortIDs(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	msg.ShortIDs[7] = msg.ShortIDs[3]

	c, status := newCompactState(msg, newCompactTestIndex())
	require.Equal(t, readInvalid, status)
	require.Nil(t, c)
}

// TestCompactState_PrefilledIndexOutOfRange is blockencodings.cpp:119-125: a
// prefilled index beyond the short IDs plus the prefilled transactions before
// it names a slot the message never described.
func TestCompactState_PrefilledIndexOutOfRange(t *testing.T) {
	blk := compactFixtureBlock(t, 4)
	msg := compactMsgFor(t, blk, 0)

	msg.PrefilledTxn[0].Index = uint32(len(msg.ShortIDs) + 1)

	c, status := newCompactState(msg, newCompactTestIndex())
	require.Equal(t, readInvalid, status)
	require.Nil(t, c)
}

// TestCompactState_IndexCollision_IsMissingNotFailed is the rule corrected on
// 2026-08-29: SVNode clears a slot two indexed transactions both match and
// requests it with the other gaps (blockencodings.cpp:181-190). It is not a
// reconstruction failure.
func TestCompactState_IndexCollision_IsMissingNotFailed(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	idx := newCompactTestIndex()
	for i := 1; i < len(blk.Transactions); i++ {
		idx.hold(t, blk.Transactions[i])
	}

	idx.collideAt[11] = true

	c, status := newCompactState(msg, idx)
	require.Equal(t, readOK, status)

	req := c.gapRequest()
	require.NotNil(t, req)
	require.Equal(t, []uint32{12}, req.Indexes)

	fillStatus, err := c.fill(1, nil)
	require.NoError(t, err)
	require.Equal(t, readOK, fillStatus)

	gaps := gapStream(t, blk, req.Indexes)

	_, rc := c.assemble(context.Background(), idx, gaps)

	got, drainErr := drain(t, rc)
	require.NoError(t, drainErr)
	require.Equal(t, encodeBlock(t, blk), got)
}

// TestCompactState_HeldTxGoneAtAssembly is the second FAILED path: the index
// named a hash the store can no longer supply, so reconstruction falls back to
// getdata without scoring the peer (spec §6 step 4).
func TestCompactState_HeldTxGoneAtAssembly(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	idx := newCompactTestIndex()
	for i := 1; i < len(blk.Transactions); i++ {
		idx.hold(t, blk.Transactions[i])
	}

	idx.openFails[blk.Transactions[20].TxHash()] = true

	c, status := newCompactState(msg, idx)
	require.Equal(t, readOK, status)
	require.Nil(t, c.gapRequest())

	_, rc := c.assemble(context.Background(), idx, nil)

	_, drainErr := drain(t, rc)
	require.Error(t, drainErr)
	require.ErrorIs(t, drainErr, ErrCompactFillFailed)
	require.Equal(t, readFailed, c.fillStatus())
}

// TestCompactState_InvalidMessages covers the InitData rejections that need no
// index at all.
func TestCompactState_InvalidMessages(t *testing.T) {
	blk := compactFixtureBlock(t, 4)

	nullHeader := compactMsgFor(t, blk, 0)
	nullHeader.Header.Bits = 0

	empty := &wire.MsgCmpctBlock{Header: blk.Header, Nonce: compactFixtureNonce}

	nullPrefilled := compactMsgFor(t, blk, 0)
	nullPrefilled.PrefilledTxn[0].Tx = wire.NewMsgTx(1)

	tests := []struct {
		name string
		msg  *wire.MsgCmpctBlock
	}{
		// blockencodings.cpp:88-91, primitives/block.h:60 (nBits == 0).
		{name: "null header", msg: nullHeader},
		// blockencodings.cpp:88-91: no short IDs and no prefilled txn.
		{name: "no transactions", msg: empty},
		// blockencodings.cpp:110-112, primitives/transaction.h:307.
		{name: "null prefilled tx", msg: nullPrefilled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, status := newCompactState(tc.msg, newCompactTestIndex())
			require.Equal(t, readInvalid, status)
			require.Nil(t, c)
		})
	}
}

// TestCompactState_TooManyTransactions is the bound InitData applies before it
// sizes anything (blockencodings.cpp:92-95: shorttxids + prefilledtxn against
// the excessive block size divided by the smallest possible transaction,
// validation.h:67-68 MIN_TRANSACTION_SIZE).
func TestCompactState_TooManyTransactions(t *testing.T) {
	restore := wire.MaxBlockPayload()

	wire.SetLimits(1000000)

	t.Cleanup(func() { wire.SetLimits(restore) })

	blk := compactFixtureBlock(t, 2)

	msg := compactMsgFor(t, blk, 0)
	msg.ShortIDs = make([]uint64, maxCompactBlockTxs()+1)

	c, status := newCompactState(msg, newCompactTestIndex())
	require.Equal(t, readInvalid, status)
	require.Nil(t, c)
}

// TestCompactState_NoIndex_RequestsEverything is the flag-on, index-empty
// case: with nothing held every short ID slot is a gap, and the exchange
// degrades to one getblocktxn carrying the whole block (spec §11).
func TestCompactState_NoIndex_RequestsEverything(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	c, status := newCompactState(msg, nil)
	require.Equal(t, readOK, status)

	req := c.gapRequest()
	require.NotNil(t, req)
	require.Len(t, req.Indexes, compactFixtureTxCount-1)

	gaps := gapStream(t, blk, req.Indexes)

	fillStatus, err := c.fill(uint64(len(req.Indexes)), gaps)
	require.NoError(t, err)
	require.Equal(t, readOK, fillStatus)

	_, rc := c.assemble(context.Background(), nil, gaps)

	got, drainErr := drain(t, rc)
	require.NoError(t, drainErr)
	require.Equal(t, encodeBlock(t, blk), got)
}

// TestCompactState_GapStreamTruncated guards the attacker-controlled tail: a
// blocktxn that declares the requested count but ends early must stop the
// assembly with an error, never a short block.
func TestCompactState_GapStreamTruncated(t *testing.T) {
	blk := compactFixtureBlock(t, compactFixtureTxCount)
	msg := compactMsgFor(t, blk, 0)

	c, status := newCompactState(msg, nil)
	require.Equal(t, readOK, status)

	req := c.gapRequest()
	require.NotNil(t, req)

	full, err := io.ReadAll(gapStream(t, blk, req.Indexes))
	require.NoError(t, err)

	gaps := bytes.NewReader(full[:len(full)/2])

	fillStatus, fillErr := c.fill(uint64(len(req.Indexes)), gaps)
	require.NoError(t, fillErr)
	require.Equal(t, readOK, fillStatus)

	_, rc := c.assemble(context.Background(), nil, gaps)

	_, drainErr := drain(t, rc)
	require.Error(t, drainErr)
	require.ErrorIs(t, drainErr, ErrCompactFillFailed)
	require.Equal(t, readFailed, c.fillStatus())
}

// TestCompactState_PrefilledIndexesNotIncreasing is the monotonicity
// blockencodings.cpp:114-117 gets for free from the differential encoding. Two
// prefilled transactions in one slot would leave a slot with no source at all,
// so a message that names one is rejected before any slot is laid out.
func TestCompactState_PrefilledIndexesNotIncreasing(t *testing.T) {
	blk := compactFixtureBlock(t, 6)
	msg := compactMsgFor(t, blk, 0, 2)

	msg.PrefilledTxn[1].Index = msg.PrefilledTxn[0].Index

	c, status := newCompactState(msg, newCompactTestIndex())
	require.Equal(t, readInvalid, status)
	require.Nil(t, c)
}

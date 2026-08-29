package protocol

import (
	"bytes"
	"context"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// readStatus is ReadStatus_t (blockencodings.h:143-153). Only the three codes
// the receive path can produce are carried: READ_STATUS_CHECKBLOCK_FAILED
// (blockencodings.h:152) belongs to SVNode's in-FillBlock CheckBlock call, and
// this port validates the assembled block downstream in the ingestor instead.
//
//	readOK      READ_STATUS_OK       blockencodings.h:145
//	readInvalid READ_STATUS_INVALID  blockencodings.h:148 — bogus message, score 100
//	readFailed  READ_STATUS_FAILED   blockencodings.h:150 — reconstruction lost, no score
type readStatus int

const (
	readOK readStatus = iota
	readInvalid
	readFailed
)

// minCompactTxSize is MIN_TRANSACTION_SIZE (validation.h:67-68), the serialized
// size of an empty CTransaction: 4 byte version, an empty input vector, an
// empty output vector, 4 byte nLockTime. go-wire calls the same number
// minTxPayload (msg_tx.go:72).
const minCompactTxSize = 10

var (
	// ErrCompactBlockInvalid marks READ_STATUS_INVALID: only the sender can
	// produce the fault, so the peer is scored and disconnected.
	ErrCompactBlockInvalid = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, "svp2p: invalid compact block")

	// ErrCompactFillFailed marks READ_STATUS_FAILED: reconstruction lost, the
	// peer is not at fault, and the block is re-fetched by getdata. It is the
	// sentinel the assembled stream carries when a slot cannot be supplied,
	// which is the only way a FAILED can surface once bytes are already
	// flowing to the ingestor.
	ErrCompactFillFailed = errors.New(errors.ERR_PROCESSING, "svp2p: compact block reconstruction failed")
)

// maxCompactBlockTxs is InitData's transaction-count ceiling
// (blockencodings.cpp:92-95): the excessive block size divided by the smallest
// transaction that could occupy a slot. It bounds the message before anything
// is sized from its counts.
func maxCompactBlockTxs() uint64 {
	return wire.MaxBlockPayload() / minCompactTxSize
}

// txSlot is one entry of PartiallyDownloadedBlock::txns_available
// (blockencodings.h:119-121) in the three states this port distinguishes:
// prefilled (bytes came with the message), held (the index named a hash whose
// bytes TxIndex.Open supplies), or missing (neither, so the peer must send it
// in a blocktxn).
type txSlot struct {
	prefilled []byte
	held      *chainhash.Hash
	shortID   uint64
}

func (s *txSlot) filled() bool { return s.prefilled != nil || s.held != nil }

// compactState is PartiallyDownloadedBlock (blockencodings.h:117-137): one
// compact block being reconstructed for one peer. It is not safe for
// concurrent use; the peer loop owns it.
type compactState struct {
	hash      chainhash.Hash
	header    wire.BlockHeader
	k0, k1    uint64
	txs       []txSlot
	missing   []uint32
	requested bool
	status    readStatus
	gaps      io.Reader
}

// newCompactState is PartiallyDownloadedBlock::InitData
// (blockencodings.cpp:84-242): it validates the message, lays the short IDs
// out over the slots the prefilled transactions leave free, and asks the index
// which of them the node already holds.
//
// idx may be nil, which leaves every short ID slot missing and degrades the
// exchange to one getblocktxn carrying the whole block.
//
// The error is non-nil only when the TxIndex answers something the message
// cannot be reconciled with; the peer is blameless there, so the status is
// readFailed.
func newCompactState(m *wire.MsgCmpctBlock, idx TxIndex) (*compactState, readStatus, error) {
	// blockencodings.cpp:88-91 cmpctblock.header.IsNull(): CBlockHeader::IsNull
	// is nBits == 0 (primitives/block.h:60).
	if m.Header.Bits == 0 {
		return nil, readInvalid, nil
	}

	// blockencodings.cpp:88-91: a compact block that describes no transaction
	// at all describes no block.
	if len(m.ShortIDs) == 0 && len(m.PrefilledTxn) == 0 {
		return nil, readInvalid, nil
	}

	// blockencodings.cpp:92-95, before txns_available is resized at :105. Both
	// counts are attacker controlled, so the bound is applied to their sum
	// before anything is allocated from it.
	total := uint64(len(m.ShortIDs)) + uint64(len(m.PrefilledTxn))
	if total > maxCompactBlockTxs() {
		return nil, readInvalid, nil
	}

	c := &compactState{
		hash:   m.Header.BlockHash(),
		header: m.Header,
		txs:    make([]txSlot, total),
	}

	c.k0, c.k1 = ShortIDKeys(&m.Header, m.Nonce)

	if status := c.placePrefilled(m); status != readOK {
		return nil, status, nil
	}

	slotOf, status := c.placeShortIDs(m)
	if status != readOK {
		return nil, status, nil
	}

	if err := c.matchIndex(m, idx, slotOf); err != nil {
		return nil, readFailed, err
	}

	c.collectMissing()

	return c, readOK, nil
}

// placePrefilled is InitData's prefilled loop (blockencodings.cpp:107-127).
// go-wire has already decoded the differential indexes into absolute ones and
// rejected a non-strictly-increasing or 32-bit-overflowing sequence
// (msg_cmpct_block.go readDifferentialIndex), so the two checks left are
// SVNode's null-transaction and out-of-range tests.
func (c *compactState) placePrefilled(m *wire.MsgCmpctBlock) readStatus {
	last := int64(-1)

	for i, p := range m.PrefilledTxn {
		// blockencodings.cpp:114-117 accumulates lastprefilledindex from the
		// differential form, so it can never decrease. The absolute indexes
		// go-wire hands back carry the same guarantee; the check is repeated
		// here because the short ID walk below indexes c.txs from it.
		if int64(p.Index) <= last {
			return readInvalid
		}

		last = int64(p.Index)

		// blockencodings.cpp:110-112 prefilledtxn.tx->IsNull():
		// CTransaction::IsNull is vin.empty() && vout.empty()
		// (primitives/transaction.h:307).
		if p.Tx == nil || (len(p.Tx.TxIn) == 0 && len(p.Tx.TxOut) == 0) {
			return readInvalid
		}

		// blockencodings.cpp:119-125: an index past the short IDs plus the
		// prefilled transactions already placed names a slot for which the
		// message carries neither a prefilled transaction nor a short ID.
		if uint64(p.Index) > uint64(len(m.ShortIDs))+uint64(i) {
			return readInvalid
		}

		var buf bytes.Buffer

		if err := p.Tx.BsvEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding); err != nil {
			return readInvalid
		}

		c.txs[p.Index].prefilled = buf.Bytes()
	}

	return readOK
}

// placeShortIDs is InitData's short ID map (blockencodings.cpp:140-169). The
// i-th short ID takes the i-th slot that no prefilled transaction occupies,
// which is what SVNode's index_offset walk at :142-146 computes. It returns
// the slot each short ID landed in, so the index's answers can be placed back.
//
// Deviation, named: SVNode reports a duplicate short ID inside the message as
// READ_STATUS_FAILED (:165-169, sharing the code with the mempool-side
// collision it cannot attribute). Only the sender can put two equal short IDs
// in one message, so this port reports READ_STATUS_INVALID and scores it
// (spec §6 step 3). SVNode's bucket_size > 12 hash-flooding guard at :157-161
// is an std::unordered_map detail with no counterpart in Go, whose map hashing
// is seeded per process.
func (c *compactState) placeShortIDs(m *wire.MsgCmpctBlock) ([]uint32, readStatus) {
	seen := make(map[uint64]struct{}, len(m.ShortIDs))
	slotOf := make([]uint32, len(m.ShortIDs))

	offset := 0

	for i, id := range m.ShortIDs {
		for c.txs[i+offset].filled() {
			offset++
		}

		if _, dup := seen[id]; dup {
			return nil, readInvalid
		}

		seen[id] = struct{}{}

		slot := i + offset
		slotOf[i] = uint32(slot) //nolint:gosec // slot < total, bounded above by maxCompactBlockTxs
		c.txs[slot].shortID = id
	}

	return slotOf, readOK
}

// matchIndex is InitData's mempool walk (blockencodings.cpp:171-199), moved
// behind the TxIndex seam: bridge holds the hashes and answers for every short
// ID at once. A slot the index reports as a collision is left empty and joins
// the gaps, which is what SVNode does at :181-190 when two transactions match
// one short ID — it is not a reconstruction failure.
//
// An answer slice of the wrong length cannot be placed: slot i of the answers
// belongs to slot slotOf[i] of the block, so a short slice would silently move
// every later hash onto the wrong transaction. That is a fault in the index,
// never in the peer, so it ends the block with an error and no score.
func (c *compactState) matchIndex(m *wire.MsgCmpctBlock, idx TxIndex, slotOf []uint32) error {
	if idx == nil {
		return nil
	}

	hashes, _ := idx.Match(c.k0, c.k1, m.ShortIDs)
	if len(hashes) != len(slotOf) {
		return errors.New(errors.ERR_PROCESSING,
			"svp2p: block %s index answered %d short ids of %d", c.hash, len(hashes), len(slotOf),
			ErrCompactFillFailed)
	}

	for i, h := range hashes {
		if h == nil {
			continue
		}

		c.txs[slotOf[i]].held = h
	}

	return nil
}

// collectMissing lists, in block order, the slots neither the message nor the
// index filled. They are the indexes of the getblocktxn
// (net_processing.cpp:3915-3925).
func (c *compactState) collectMissing() {
	for i := range c.txs {
		if !c.txs[i].filled() {
			c.missing = append(c.missing, uint32(i)) //nolint:gosec // i < total, bounded above by maxCompactBlockTxs
		}
	}
}

// gapRequest returns the getblocktxn for the slots still missing, or nil when
// the block can be assembled immediately (net_processing.cpp:3931-3945). The
// indexes are strictly increasing by construction, which is what the
// differential wire encoding requires (blockencodings.h:36-82).
//
// The message carries a copy of the missing list. fill checks the blocktxn
// count against that list, so a caller that sorts, truncates or appends to
// Indexes must not be able to move the number fill compares against. Calling
// it twice describes the same gaps; whether a second getblocktxn may go on
// the wire is the manager's rule, not this state machine's.
func (c *compactState) gapRequest() *wire.MsgGetBlockTxn {
	if len(c.missing) == 0 {
		return nil
	}

	c.requested = true

	indexes := make([]uint32, len(c.missing))
	copy(indexes, c.missing)

	return &wire.MsgGetBlockTxn{BlockHash: c.hash, Indexes: indexes}
}

// fill is FillBlock's arity check (blockencodings.cpp:264-285): a blocktxn
// that carries fewer transactions than the gaps need fails at :268-270, and
// one that carries more fails at :283-285. Both are READ_STATUS_INVALID.
//
// It reads nothing. A blocktxn payload can approach the size of the block, so
// the transactions stay on the socket. fill latches the stream instead, and
// assemble pulls the transactions from that same latched reader, checking each
// one's short ID against its own slot as it goes. Latching is what keeps the
// arity check honest: the stream fill counted is the stream assemble consumes.
func (c *compactState) fill(count uint64, txs io.Reader) (readStatus, error) {
	if count != uint64(len(c.missing)) {
		c.status = readInvalid

		return readInvalid, errors.New(errors.ERR_NETWORK_PEER_MALICIOUS,
			"svp2p: blocktxn for %s carries %d transactions, %d requested", c.hash, count, len(c.missing),
			ErrCompactBlockInvalid)
	}

	c.gaps = txs

	return readOK, nil
}

// fillStatus reports the sticky outcome of the streaming half of FillBlock.
// It is readFailed once assemble has hit a slot nobody can supply and
// readInvalid once the blocktxn payload itself proved short, which the stream
// also reports to its reader as ErrCompactFillFailed or ErrCompactBlockInvalid.
func (c *compactState) fillStatus() readStatus { return c.status }

// assemble returns the transaction count and a reader over the whole block
// payload: the 80 byte header, the compactsize transaction count, then the
// transactions in block order. It is the byte-for-byte equal of the block
// message the peer would otherwise have sent.
//
// Nothing is materialized. Prefilled transactions come from the message, held
// ones are opened lazily one at a time through TxIndex.Open, and the gaps are
// pulled from the blocktxn stream fill latched as the consumer reads.
func (c *compactState) assemble(ctx context.Context, idx TxIndex) (uint64, io.ReadCloser) {
	var head bytes.Buffer

	_ = c.header.Serialize(&head)
	_ = wire.WriteVarInt(&head, wire.ProtocolVersion, uint64(len(c.txs)))

	a := c.newAssembler(ctx, idx)
	a.head = &head

	return uint64(len(c.txs)), a
}

// assembleTxs is assemble without the header and the count, the shape
// BlockIngestRequest.TxReader takes: the transactions alone, in block order
// (spec §6 step 7).
func (c *compactState) assembleTxs(ctx context.Context, idx TxIndex) (uint64, io.ReadCloser) {
	return uint64(len(c.txs)), c.newAssembler(ctx, idx)
}

func (c *compactState) newAssembler(ctx context.Context, idx TxIndex) *compactAssembler {
	return &compactAssembler{ctx: ctx, state: c, idx: idx}
}

// compactAssembler is FillBlock's loop (blockencodings.cpp:271-281) turned
// inside out: instead of writing every transaction into a CBlock, it hands
// them to the consumer one slot at a time, so a block is never held in memory.
type compactAssembler struct {
	ctx   context.Context
	state *compactState
	idx   TxIndex

	head io.Reader
	cur  io.Reader
	open io.Closer
	next int
	err  error

	sized bool
	want  uint64
	got   uint64
}

func (a *compactAssembler) Read(p []byte) (int, error) {
	if a.err != nil {
		return 0, a.err
	}

	if len(p) == 0 {
		return 0, nil
	}

	if a.head != nil {
		n, err := a.head.Read(p)
		if errors.Is(err, io.EOF) {
			a.head = nil
			err = nil
		}

		if err != nil {
			a.err = err

			return n, err
		}

		if n > 0 {
			return n, nil
		}
	}

	for {
		if a.cur == nil {
			if err := a.advance(); err != nil {
				a.err = err

				return 0, err
			}
		}

		n, err := a.cur.Read(p)
		a.got += uint64(n) //nolint:gosec // n is never negative

		if errors.Is(err, io.EOF) {
			err = a.endSlot()
		}

		if err != nil {
			a.err = err

			return n, err
		}

		if n > 0 {
			return n, nil
		}
	}
}

// Close releases whatever held transaction is still open. It is idempotent,
// and it does not touch the gap reader: that one belongs to the transport's
// TxnStream, which the peer loop closes.
func (a *compactAssembler) Close() error {
	a.release()

	if a.err == nil {
		a.err = io.EOF
	}

	return nil
}

func (a *compactAssembler) release() {
	if a.open != nil {
		_ = a.open.Close()
		a.open = nil
	}

	a.cur = nil
	a.sized = false
	a.want = 0
	a.got = 0
}

// endSlot closes the slot the consumer just drained. A held transaction ends
// when TxIndex.Open's declared size is reached, and io.LimitReader reports the
// same clean io.EOF whether the store supplied that many bytes or stopped
// early, so the count is compared here. A store that under-supplies is the
// same fault class as ErrTxUnknown at openHeld: nobody's fault, no score.
func (a *compactAssembler) endSlot() error {
	sized, want, got, slot := a.sized, a.want, a.got, a.next-1

	a.release()

	if sized && got != want {
		return a.fail("svp2p: block %s slot %d received %d bytes of the %d the index declared",
			a.state.hash, slot, got, want)
	}

	return nil
}

// advance opens the next slot's source. blockencodings.cpp:271-281 walks
// txns_available in the same order, taking a transaction from vtx_missing
// whenever the slot is empty.
func (a *compactAssembler) advance() error {
	if a.next >= len(a.state.txs) {
		return io.EOF
	}

	slot := &a.state.txs[a.next]
	a.next++

	switch {
	case slot.prefilled != nil:
		a.cur = bytes.NewReader(slot.prefilled)

		return nil

	case slot.held != nil:
		return a.openHeld(*slot.held)

	default:
		return a.readGap(slot.shortID)
	}
}

// openHeld reads a transaction the index named. ErrTxUnknown here is the
// second READ_STATUS_FAILED path (spec §6 step 4): the index still names a
// hash whose bytes the store no longer has, which is nobody's fault and ends
// in a plain getdata for the block.
func (a *compactAssembler) openHeld(hash chainhash.Hash) error {
	if a.idx == nil {
		return a.fail("svp2p: block %s needs held transaction %s with no index", a.state.hash, hash)
	}

	rc, size, err := a.idx.Open(a.ctx, hash)
	if err != nil {
		return a.fail("svp2p: block %s cannot open held transaction %s: %v", a.state.hash, hash, err)
	}

	a.open = rc
	a.cur = io.LimitReader(rc, int64(size)) //nolint:gosec // size is the store's own byte count
	a.sized = true
	a.want = size
	a.got = 0

	return nil
}

// readGap takes the next transaction off the blocktxn stream and checks it
// against the slot that asked for it. A payload that cannot yield the declared
// transaction is READ_STATUS_INVALID, the same verdict fill gives the count
// mismatch. Only a wrong but decodable transaction is READ_STATUS_FAILED:
// SVNode catches that one downstream, when CheckBlock's merkle root fails and
// CorruptionPossible turns into READ_STATUS_FAILED (blockencodings.cpp:288-298);
// checking the short ID directly reaches the same verdict a round earlier and
// without hashing the whole block.
//
// Exactly one transaction is buffered, which is what decoding one costs
// anyway. The rest of the stream stays on the socket.
func (a *compactAssembler) readGap(shortID uint64) error {
	if a.state.gaps == nil {
		return a.fail("svp2p: block %s needs a gap transaction with no blocktxn stream", a.state.hash)
	}

	var buf bytes.Buffer

	tx := &wire.MsgTx{}
	if err := tx.Bsvdecode(io.TeeReader(a.state.gaps, &buf), wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		return a.invalid("svp2p: block %s cannot decode a gap transaction: %v", a.state.hash, err)
	}

	raw := buf.Bytes()

	if got := ShortID(a.state.k0, a.state.k1, chainhash.DoubleHashH(raw)); got != shortID {
		return a.fail("svp2p: block %s gap transaction has short id %d, slot %d wants %d",
			a.state.hash, got, a.next-1, shortID)
	}

	a.cur = bytes.NewReader(raw)

	return nil
}

func (a *compactAssembler) fail(format string, args ...any) error {
	a.state.status = readFailed

	return errors.New(errors.ERR_PROCESSING, format, append(args, ErrCompactFillFailed)...)
}

// invalid is the streaming half of FillBlock's arity check
// (blockencodings.cpp:268-270, :283-285). A blocktxn message is length framed,
// so a payload that declares the requested count and then runs out of
// transactions is a fault only the sender can produce, exactly like the count
// mismatch fill rejects before any byte is read.
func (a *compactAssembler) invalid(format string, args ...any) error {
	a.state.status = readInvalid

	return errors.New(errors.ERR_NETWORK_PEER_MALICIOUS, format, append(args, ErrCompactBlockInvalid)...)
}

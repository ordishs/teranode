package protocol

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// testTxIndex is a TxIndex over a fixed set of transactions, keyed by hash.
// Unlike fakeTxIndex (txindex_test.go), which only proves the manager seam
// wires through, this one answers Match and Open honestly, so a test can
// choose exactly which of a block's transactions the node already holds.
type testTxIndex struct {
	mu sync.Mutex

	txs map[chainhash.Hash][]byte

	// collide makes Match report the BIP152 collision flag, so a test can
	// drive the index-side collision rule.
	collide bool

	// openErr, when set, is what Open returns instead of the bytes.
	openErr error
}

func newTestTxIndex(txs ...*wire.MsgTx) *testTxIndex {
	idx := &testTxIndex{txs: make(map[chainhash.Hash][]byte, len(txs))}

	for _, tx := range txs {
		idx.add(tx)
	}

	return idx
}

func (i *testTxIndex) add(tx *wire.MsgTx) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.txs[tx.TxHash()] = rawTx(tx)
}

func (i *testTxIndex) Match(k0, k1 uint64, shortIDs []uint64) ([]*chainhash.Hash, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	byID := make(map[uint64]chainhash.Hash, len(i.txs))

	for hash := range i.txs {
		byID[ShortID(k0, k1, hash)] = hash
	}

	out := make([]*chainhash.Hash, len(shortIDs))

	for n, id := range shortIDs {
		if hash, held := byID[id]; held {
			h := hash
			out[n] = &h
		}
	}

	return out, i.collide
}

func (i *testTxIndex) Open(_ context.Context, hash chainhash.Hash) (io.ReadCloser, uint64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.openErr != nil {
		return nil, 0, i.openErr
	}

	raw, held := i.txs[hash]
	if !held {
		return nil, 0, ErrTxUnknown
	}

	return io.NopCloser(bytes.NewReader(raw)), uint64(len(raw)), nil
}

// rawTx is a transaction's wire encoding, which is what a compact block
// carries prefilled and what the assembled stream must reproduce.
func rawTx(tx *wire.MsgTx) []byte {
	var buf bytes.Buffer

	_ = tx.BsvEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding)

	return buf.Bytes()
}

// testTxs returns count distinct, syntactically valid transactions. Only the
// bytes matter here: the fake ingestor copies the assembled stream and never
// validates it, exactly as recordingIngestor does for a plain block.
func testTxs(count int, salt byte) []*wire.MsgTx {
	out := make([]*wire.MsgTx, 0, count)

	for i := 0; i < count; i++ {
		tx := wire.NewMsgTx(1)

		prev := chainhash.Hash{salt, byte(i)}
		tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&prev, uint32(i)), []byte{0x51, salt, byte(i)})) //nolint:gosec // test index is small
		tx.AddTxOut(wire.NewTxOut(int64(1000+i), []byte{0x51}))

		out = append(out, tx)
	}

	return out
}

// compactBlockFor builds a cmpctblock announcing header with txs, prefilling
// exactly the indexes in prefilled and carrying a short ID for every other
// slot. It is the message shape SVNode's SendCompactBlock produces.
func compactBlockFor(t *testing.T, header *wire.BlockHeader, txs []*wire.MsgTx, prefilled ...int) *wire.MsgCmpctBlock {
	t.Helper()

	const nonce = 0x0102030405060708

	msg := wire.NewMsgCmpctBlock(header, nonce)

	isPrefilled := make(map[int]bool, len(prefilled))
	for _, i := range prefilled {
		isPrefilled[i] = true
	}

	k0, k1 := ShortIDKeys(header, nonce)

	for i, tx := range txs {
		if isPrefilled[i] {
			require.NoError(t, msg.AddPrefilledTransaction(uint32(i), tx)) //nolint:gosec // test index is small

			continue
		}

		msg.ShortIDs = append(msg.ShortIDs, ShortID(k0, k1, tx.TxHash()))
	}

	return msg
}

// compactSyncManager is syncTestManager plus the two things compact-block
// reception is gated on: legacy_compactBlocks and a wired TxIndex
// (SetTxIndex's own doc comment).
func compactSyncManager(t *testing.T, idx *HeaderIndex, ingestor BlockIngestor, txIdx TxIndex) *PeerManager {
	t.Helper()

	tSettings := managerSettings()
	tSettings.ChainCfgParams = syncTestParams(nil)
	tSettings.Legacy.CompactBlocks = txIdx != nil

	banList, err := NewBanList("")
	require.NoError(t, err)

	m := NewPeerManager(ulogger.TestLogger{}, tSettings, banList)

	if txIdx != nil {
		m.SetTxIndex(txIdx)
	}

	require.NoError(t, m.ConfigureSync(SyncConfig{
		Index:        idx,
		Ingestor:     ingestor,
		TickInterval: 20 * time.Millisecond,
	}))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))
	t.Cleanup(func() { require.NoError(t, m.Stop()) })

	return m
}

// connectCompactPeer completes the handshake as a block-serving peer and
// answers the node's opening getheaders with an empty batch, so headers-first
// sync is quiet and the only thing that can move the node is the cmpctblock
// the test sends next.
func connectCompactPeer(t *testing.T, m *PeerManager) *scriptedPeer {
	t.Helper()

	far := dialScripted(t, m.ListenAddrs()[0])
	t.Cleanup(func() { _ = far.nc.Close() })

	version := remoteVersion(4321)
	version.Services = wire.SFNodeNetwork
	far.completeOutboundHandshakeAs(t, version)

	far.readUntil(t, wire.CmdGetHeaders)
	far.write(t, wire.NewMsgHeaders())

	return far
}

// blockTxnFor is the blocktxn reply a peer sends for the gaps a getblocktxn
// named, in the order the request listed them.
func blockTxnFor(t *testing.T, hash chainhash.Hash, txs []*wire.MsgTx, indexes []uint32) *wire.MsgBlockTxn {
	t.Helper()

	msg := wire.NewMsgBlockTxn(&hash)

	for _, i := range indexes {
		require.Less(t, int(i), len(txs))
		require.NoError(t, msg.AddTransaction(txs[i]))
	}

	return msg
}

// assembledBody is what the ingest must have read off the assembled stream:
// every transaction of the block, wire-encoded back to back, in block order.
func assembledBody(txs []*wire.MsgTx) []byte {
	var buf bytes.Buffer

	for _, tx := range txs {
		buf.Write(rawTx(tx))
	}

	return buf.Bytes()
}

// TestCompactBlock_AllTransactionsKnownIngestsWithoutAGapRequest is the
// no-round-trip case of net_processing.cpp:3870-3877: every slot is filled
// from the index or prefilled, req.indices is empty, and SVNode jumps
// straight to the blocktxn completion path with an empty message.
func TestCompactBlock_AllTransactionsKnownIngestsWithoutAGapRequest(t *testing.T) {
	genesis := syncGenesis()
	header := minedRun(genesis, 1, 11)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	txs := testTxs(4, 0x10)
	txIdx := newTestTxIndex(txs[1], txs[2], txs[3])

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	require.Eventually(t, func() bool { return ingestor.count() == 1 }, 10*time.Second, 20*time.Millisecond,
		"the compact block never reached the ingest interface")

	require.Equal(t, []chainhash.Hash{header.BlockHash()}, ingestor.hashes())
	require.Equal(t, uint64(len(txs)), ingestor.txCount(0))
	require.Equal(t, assembledBody(txs), ingestor.body(0),
		"the assembled stream must carry every transaction of the block in block order")

	// The peer is never asked for anything: no getblocktxn, and no getdata
	// fallback either.
	far.mustNotReceive(t, 300*time.Millisecond, wire.CmdGetBlockTxn, wire.CmdGetData)
}

// TestCompactBlock_GapsAreFetchedWithGetBlockTxn is the round-trip case:
// net_processing.cpp:3864-3881 collects every slot the index could not fill
// into a getblocktxn, and the peer's blocktxn completes the block
// (ProcessBlockTxnMessage :3608-3609, :3646).
func TestCompactBlock_GapsAreFetchedWithGetBlockTxn(t *testing.T) {
	genesis := syncGenesis()
	header := minedRun(genesis, 1, 12)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	txs := testTxs(5, 0x20)

	// The node holds slots 1 and 3; slots 2 and 4 are the gaps, and slot 0 is
	// prefilled.
	txIdx := newTestTxIndex(txs[1], txs[3])

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	req, ok := far.readUntil(t, wire.CmdGetBlockTxn).(*wire.MsgGetBlockTxn)
	require.True(t, ok)
	require.Equal(t, header.BlockHash(), req.BlockHash)
	require.Equal(t, []uint32{2, 4}, req.Indexes, "the gaps must be requested in increasing slot order")

	far.write(t, blockTxnFor(t, header.BlockHash(), txs, req.Indexes))

	require.Eventually(t, func() bool { return ingestor.count() == 1 }, 10*time.Second, 20*time.Millisecond,
		"the filled compact block never reached the ingest interface")

	require.Equal(t, []chainhash.Hash{header.BlockHash()}, ingestor.hashes())
	require.Equal(t, assembledBody(txs), ingestor.body(0),
		"the gap transactions must land in the slots that asked for them")
}

// TestCompactBlock_BlockTxnWithTheWrongCountIsPeerFault is FillBlock's arity
// check reaching net_processing.cpp:3610-3616: READ_STATUS_INVALID earns
// Misbehaving(pfrom, 100, "invalid-cmpctblk-txns") and the block is failed.
func TestCompactBlock_BlockTxnWithTheWrongCountIsPeerFault(t *testing.T) {
	genesis := syncGenesis()
	header := minedRun(genesis, 1, 13)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	txs := testTxs(4, 0x30)
	txIdx := newTestTxIndex(txs[1])

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	req, ok := far.readUntil(t, wire.CmdGetBlockTxn).(*wire.MsgGetBlockTxn)
	require.True(t, ok)
	require.Equal(t, []uint32{2, 3}, req.Indexes)

	// Two were asked for; one is sent.
	far.write(t, blockTxnFor(t, header.BlockHash(), txs, req.Indexes[:1]))

	require.Eventually(t, func() bool { return m.ConnectedCount() == 0 }, 10*time.Second, 20*time.Millisecond,
		"a blocktxn with the wrong transaction count must disconnect the peer")

	require.Equal(t, 0, ingestor.count(), "a rejected compact block must never be ingested")
}

// TestCompactBlock_BlockWeAlreadyHoldIsIgnored is
// net_processing.cpp:3795-3799: pindex->getStatus().hasData() means "nothing
// to do here" — the claim is released and the message goes no further.
func TestCompactBlock_BlockWeAlreadyHoldIsIgnored(t *testing.T) {
	genesis := syncGenesis()
	header := minedRun(genesis, 1, 14)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	connected, err := idx.AddHeader(header)
	require.NoError(t, err)
	require.True(t, connected)

	txs := testTxs(3, 0x40)
	txIdx := newTestTxIndex(txs[1], txs[2])

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	// Our own chain still stands at genesis, so every OTHER guard wantCompact
	// applies passes and the have-data rule is the only thing that can stop
	// this announcement. Without this the header would be the index tip, and
	// the chainwork guard would refuse it for a different reason.
	require.True(t, m.SetActiveTip(genesis.BlockHash()))

	// The node already holds this block's data, which is what hasData reports.
	m.syncMu.Lock()
	m.blockDownloader.haveData[header.BlockHash()] = 1
	m.syncMu.Unlock()

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	far.mustNotReceive(t, 500*time.Millisecond, wire.CmdGetBlockTxn)

	require.Equal(t, 0, ingestor.count(), "a block we already hold must not be reconstructed")
	require.Equal(t, int32(1), m.ConnectedCount(), "ignoring a block must not cost the peer its connection")
}

// TestCompactBlock_SecondAnnouncementWhileOneIsOutstandingIsIgnored is
// net_processing.cpp:3839-3844, "Peer sent us compact block we were already
// syncing!": one partial block per peer, and the outstanding one is kept.
func TestCompactBlock_SecondAnnouncementWhileOneIsOutstandingIsIgnored(t *testing.T) {
	genesis := syncGenesis()
	first := minedRun(genesis, 1, 15)[0]
	second := minedRun(genesis, 1, 16)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	firstTxs := testTxs(3, 0x50)
	secondTxs := testTxs(3, 0x60)

	txIdx := newTestTxIndex(firstTxs[1], secondTxs[1], secondTxs[2])

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	far := connectCompactPeer(t, m)

	// The first announcement leaves slot 2 outstanding.
	far.write(t, compactBlockFor(t, first, firstTxs, 0))

	req, ok := far.readUntil(t, wire.CmdGetBlockTxn).(*wire.MsgGetBlockTxn)
	require.True(t, ok)
	require.Equal(t, first.BlockHash(), req.BlockHash)
	require.Equal(t, []uint32{2}, req.Indexes)

	// The second would need no round trip at all, so if it were accepted it
	// would ingest on its own and overwrite the outstanding state.
	far.write(t, compactBlockFor(t, second, secondTxs, 0))

	far.mustNotReceive(t, 500*time.Millisecond, wire.CmdGetBlockTxn)
	require.Equal(t, 0, ingestor.count(), "the second compact block must be ignored while one is outstanding")

	// The outstanding block still completes, which is what proves its state
	// survived the second announcement.
	far.write(t, blockTxnFor(t, first.BlockHash(), firstTxs, req.Indexes))

	require.Eventually(t, func() bool { return ingestor.count() == 1 }, 10*time.Second, 20*time.Millisecond,
		"the outstanding compact block must still complete")

	require.Equal(t, []chainhash.Hash{first.BlockHash()}, ingestor.hashes())
}

// TestCompactBlock_IgnoredWhenTheFlagIsOff is spec §8's flag-off half: no
// sendcmpct is ever sent, and an unsolicited cmpctblock is ignored rather
// than acted on or punished.
func TestCompactBlock_IgnoredWhenTheFlagIsOff(t *testing.T) {
	genesis := syncGenesis()
	header := minedRun(genesis, 1, 17)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	txs := testTxs(3, 0x70)

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, nil)

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	far.mustNotReceive(t, 500*time.Millisecond, wire.CmdSendcmpct, wire.CmdGetBlockTxn, wire.CmdGetData)

	require.Equal(t, 0, ingestor.count(), "with the flag off no compact block may be reconstructed")
	require.Equal(t, int32(1), m.ConnectedCount(), "an ignored cmpctblock must not cost the peer its connection")
}

// TestCompactBlock_HeaderThatDoesNotConnectAsksForHeaders is
// net_processing.cpp:3721-3733: a cmpctblock whose parent we do not hold is
// NOT run through AcceptBlockHeader — "Doesn't connect (or is genesis),
// instead of DoSing in AcceptBlockHeader, request deeper headers". The peer
// pays nothing for announcing a block we are behind on.
func TestCompactBlock_HeaderThatDoesNotConnectAsksForHeaders(t *testing.T) {
	genesis := syncGenesis()

	// The announced header is two above genesis, so its parent is a header
	// the node has never seen.
	run := minedRun(genesis, 2, 21)
	header := run[1]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	txs := testTxs(3, 0xa0)
	txIdx := newTestTxIndex(txs[1], txs[2])

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	ask, ok := far.readUntil(t, wire.CmdGetHeaders).(*wire.MsgGetHeaders)
	require.True(t, ok)
	require.NotEmpty(t, ask.BlockLocatorHashes, "the getheaders must carry our own locator")

	require.Equal(t, 0, ingestor.count(), "a block whose header does not connect must not be reconstructed")
	require.Equal(t, int32(1), m.ConnectedCount())

	snaps := m.Snapshots()
	require.Len(t, snaps, 1)
	require.Equal(t, 0, snaps[0].MisbehaviorScore,
		"a header that does not connect must not be scored (net_processing.cpp:3723-3724)")
}

// TestCompactBlock_TooHighAboveOurTipIsIgnored is net_processing.cpp:3823-3825,
// "We want to be a bit conservative just to be extra careful about DoS
// possibilities in compact block processing": a block more than two heights
// above our own tip is not reconstructed. Its header still reaches the index,
// which is what the :3913-3921 revert-to-headers branch is for — see
// CompactBlock's own doc comment.
func TestCompactBlock_TooHighAboveOurTipIsIgnored(t *testing.T) {
	genesis := syncGenesis()
	run := minedRun(genesis, 3, 22)
	header := run[2]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	// The two headers below the announced one are held, so it connects; only
	// its height above our own chain can refuse it.
	for _, h := range run[:2] {
		connected, addErr := idx.AddHeader(h)
		require.NoError(t, addErr)
		require.True(t, connected)
	}

	txs := testTxs(3, 0xb0)
	txIdx := newTestTxIndex(txs[1], txs[2])

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	// Our own chain stands at genesis while the header index runs ahead, which
	// is exactly the headers-first shape this rule guards.
	require.True(t, m.SetActiveTip(genesis.BlockHash()))

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	far.mustNotReceive(t, 500*time.Millisecond, wire.CmdGetBlockTxn)

	require.Equal(t, 0, ingestor.count(), "a block %d above our tip must not be reconstructed", 3)
	require.Equal(t, int32(1), m.ConnectedCount())

	// The header itself IS accepted, which is what makes the :3913-3921
	// revert-to-headers branch unnecessary here: the announcement has already
	// had the effect a plain headers message would have had.
	// The peer's state is snapshotted under m.mu BEFORE syncMu is taken: this
	// package never holds the two together (see the note on syncMu).
	state := onlySyncPeerState(t, m)

	m.syncMu.Lock()
	_, indexed := m.headerIndex.Lookup(header.BlockHash())
	best := state.pindexBestKnownBlock
	m.syncMu.Unlock()

	require.True(t, indexed, "a refused compact block must still leave its header in the index")
	require.NotNil(t, best, "the announcement must still count as block availability")
	require.Equal(t, header.BlockHash(), best.Hash)
}

// TestBlockTxn_WithoutAnOutstandingRequestIsDroppedUnscored is
// net_processing.cpp:3595-3606: GetBlockDetails throws for a block this peer
// does not owe us, and the catch block logs "Peer %d sent us block
// transactions for block we weren't expecting" and returns. There is no
// Misbehaving call on that path — the only score in ProcessBlockTxnMessage is
// the READ_STATUS_INVALID branch at :3610-3616.
func TestBlockTxn_WithoutAnOutstandingRequestIsDroppedUnscored(t *testing.T) {
	genesis := syncGenesis()
	header := minedRun(genesis, 1, 23)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	txs := testTxs(3, 0xc0)
	txIdx := newTestTxIndex(txs[1], txs[2])

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	far := connectCompactPeer(t, m)

	// Nothing was ever announced, so nothing is outstanding.
	far.write(t, blockTxnFor(t, header.BlockHash(), txs, []uint32{1, 2}))

	// The connection survives, and a later ping proves the stream was released
	// rather than left holding the transport read loop.
	far.write(t, wire.NewMsgPing(99))

	pong, ok := far.readUntil(t, wire.CmdPong).(*wire.MsgPong)
	require.True(t, ok)
	require.Equal(t, uint64(99), pong.Nonce, "the connection must still be serving after an unsolicited blocktxn")

	require.Equal(t, int32(1), m.ConnectedCount())

	snaps := m.Snapshots()
	require.Len(t, snaps, 1)
	require.Equal(t, 0, snaps[0].MisbehaviorScore, "an unsolicited blocktxn must not be scored")

	require.Equal(t, 0, ingestor.count())
}

// TestCompactBlock_IndexCollisionJoinsTheGaps is blockencodings.cpp:181-190
// as the Task 5 ruling carries it: a short ID two indexed hashes share leaves
// the slot empty, so it is requested by getblocktxn like any other gap rather
// than failing reconstruction.
func TestCompactBlock_IndexCollisionJoinsTheGaps(t *testing.T) {
	genesis := syncGenesis()
	header := minedRun(genesis, 1, 18)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	txs := testTxs(3, 0x80)

	// The index reports the collision flag and matches nothing, which is what
	// an index that cannot name a slot's transaction answers.
	txIdx := newTestTxIndex()
	txIdx.collide = true

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	req, ok := far.readUntil(t, wire.CmdGetBlockTxn).(*wire.MsgGetBlockTxn)
	require.True(t, ok)
	require.Equal(t, []uint32{1, 2}, req.Indexes, "a collided slot must be requested, not fail the block")

	far.write(t, blockTxnFor(t, header.BlockHash(), txs, req.Indexes))

	require.Eventually(t, func() bool { return ingestor.count() == 1 }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, assembledBody(txs), ingestor.body(0))
}

// TestCompactBlock_UnknownHeldTransactionFallsBackWithoutAScore is the Task 7
// ruling's READ_STATUS_FAILED path: the index named a transaction whose bytes
// it can no longer open, which SVNode treats as a collision rather than
// malice (net_processing.cpp:3617-3622, "Might have collided, fall back to
// getdata now"). No score, and the block is offered again.
func TestCompactBlock_UnknownHeldTransactionFallsBackWithoutAScore(t *testing.T) {
	genesis := syncGenesis()
	header := minedRun(genesis, 1, 19)[0]

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	txs := testTxs(3, 0x90)

	txIdx := newTestTxIndex(txs[1], txs[2])
	txIdx.openErr = ErrTxUnknown

	ingestor := &recordingBodyIngestor{}
	m := compactSyncManager(t, idx, ingestor, txIdx)

	far := connectCompactPeer(t, m)
	far.write(t, compactBlockFor(t, header, txs, 0))

	// The scheduler offers the block again, which reaches the wire as the
	// ordinary getdata this node's download tick sends.
	getData, ok := far.readUntil(t, wire.CmdGetData).(*wire.MsgGetData)
	require.True(t, ok)
	require.Len(t, getData.InvList, 1)
	require.Equal(t, wire.InvTypeBlock, getData.InvList[0].Type)
	require.Equal(t, header.BlockHash(), getData.InvList[0].Hash)

	require.Equal(t, int32(1), m.ConnectedCount(), "a failed reconstruction must not disconnect the peer")

	snaps := m.Snapshots()
	require.Len(t, snaps, 1)
	require.Equal(t, 0, snaps[0].MisbehaviorScore, "a failed reconstruction must score nothing")
}

// ---------------------------------------------------------------------------

// recordingBodyIngestor is recordingIngestor plus the assembled bytes, which
// is the only way to prove a reconstructed block carries the right
// transactions in the right slots.
type recordingBodyIngestor struct {
	outcome IngestOutcome

	mu       sync.Mutex
	ingested []chainhash.Hash
	bodies   [][]byte
	counts   []uint64
}

func (r *recordingBodyIngestor) WatchProgress(rd io.ReadCloser) IngestProgress {
	return newTestProgress(rd)
}

func (r *recordingBodyIngestor) Ingest(_ context.Context, req BlockIngestRequest) IngestOutcome {
	var body bytes.Buffer

	_, err := io.Copy(&body, req.TxReader)

	if closeErr := req.TxReader.Close(); closeErr != nil && err == nil {
		err = closeErr
	}

	if err != nil {
		return IngestOutcome{Err: err}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.ingested = append(r.ingested, req.Header.BlockHash())
	r.bodies = append(r.bodies, body.Bytes())
	r.counts = append(r.counts, req.TxCount)

	return r.outcome
}

func (r *recordingBodyIngestor) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.ingested)
}

func (r *recordingBodyIngestor) hashes() []chainhash.Hash {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]chainhash.Hash(nil), r.ingested...)
}

func (r *recordingBodyIngestor) body(i int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.bodies[i]
}

func (r *recordingBodyIngestor) txCount(i int) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.counts[i]
}

// mustNotReceive reads for the given window and fails if any of the named
// commands arrives. It is the negative half of readUntil.
func (s *scriptedPeer) mustNotReceive(t *testing.T, window time.Duration, commands ...string) {
	t.Helper()

	banned := make(map[string]struct{}, len(commands))
	for _, cmd := range commands {
		banned[cmd] = struct{}{}
	}

	deadline := time.Now().Add(window)

	for time.Now().Before(deadline) {
		require.NoError(t, s.nc.SetReadDeadline(time.Now().Add(50*time.Millisecond)))

		_, msg, _, err := wire.ReadMessageWithEncodingN(s.nc, wire.ProtocolVersion, wire.MainNet, wire.BaseEncoding)
		if err != nil {
			continue
		}

		_, forbidden := banned[msg.Command()]
		require.False(t, forbidden, "the node must not send %s", msg.Command())
	}
}

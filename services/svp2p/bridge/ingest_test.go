package bridge

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockvalidation"
	"github.com/bsv-blockchain/teranode/services/svp2p/bridge/bsvutil"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	utxosql "github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// parentHeight is the height the blockchain mock reports for the parent block,
// so every ingested test block lands at parentHeight+1 — below the synthetic
// checkpoint newOutpointOnlySettings installs.
const parentHeight = uint32(100)

// captureBlockValidation records the model.Block the pipeline hands to
// blockvalidation, which is what the two ingestion entries must agree on.
type captureBlockValidation struct {
	blockvalidation.Interface

	mu  sync.Mutex
	got *model.Block
}

func (c *captureBlockValidation) ProcessBlock(_ context.Context, block *model.Block, _ uint32, _, _ string, _ uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.got = block

	return nil
}

func (c *captureBlockValidation) block() *model.Block {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.got
}

// ingestHarness is one isolated end of a parity run: its own UTXO store, its
// own subtree blob store, and its own captured block.
type ingestHarness struct {
	sm           *svp2pBridge
	utxoStore    utxo.Store
	subtreeStore *memory.Memory
	captured     *captureBlockValidation
}

// newIngestHarness builds a bridge whose stores are real (sqlitememory UTXO +
// in-memory blob) and whose blockchain / blockvalidation edges are mocked, so
// both ingestion entries can be driven over identical inputs and compared.
func newIngestHarness(ctx context.Context, t *testing.T, dbName string, unified bool, maxMerkleItems int) *ingestHarness {
	t.Helper()

	initPrometheusMetrics()

	tSettings, params := newOutpointOnlySettings(t, true, true, 1000)
	tSettings.BlockValidation.LegacyUnifiedBelowCheckpoint = unified

	if maxMerkleItems > 0 {
		tSettings.BlockAssembly.MaximumMerkleItemsPerSubtree = maxMerkleItems
	}

	storeURL, err := url.Parse("sqlitememory:///" + dbName)
	require.NoError(t, err)

	tSettings.UtxoStore.UtxoStore = storeURL

	logger := ulogger.TestLogger{}

	store, err := utxosql.New(ctx, logger, tSettings, storeURL)
	require.NoError(t, err)

	t.Cleanup(func() { _ = store.Close(ctx) })

	mockBC := &blockchain.Mock{}
	mockBC.On("GetBlockExists", mock.Anything, mock.Anything).Return(false, nil).Maybe()
	mockBC.On("GetBlockHeader", mock.Anything, mock.Anything).
		Return((*model.BlockHeader)(nil), &model.BlockHeaderMeta{Height: parentHeight}, nil).Maybe()
	mockBC.On("AssignBlockID", mock.Anything, mock.Anything).Return(uint64(4242), nil).Maybe()
	mockBC.On("GetBlockIsMined", mock.Anything, mock.Anything).Return(true, nil).Maybe()

	captured := &captureBlockValidation{Interface: &blockvalidation.MockBlockValidation{}}
	subtreeStore := memory.New()

	sm := &svp2pBridge{
		logger:           logger,
		settings:         tSettings,
		chainParams:      params,
		blockchainClient: mockBC,
		validationClient: makeSpendValidator(store),
		utxoStore:        store,
		subtreeStore:     subtreeStore,
		blockValidation:  captured,
		headerEvents:     make(chan HeaderEvent, 1),
	}

	return &ingestHarness{sm: sm, utxoStore: store, subtreeStore: subtreeStore, captured: captured}
}

// serializeTxStream returns exactly what BlockStream.TxReader yields: the
// block's transactions back to back, with no header and no count varint.
func serializeTxStream(t *testing.T, txs []*bsvutil.Tx) []byte {
	t.Helper()

	var buf bytes.Buffer

	for _, tx := range txs {
		require.NoError(t, tx.MsgTx().Serialize(&buf))
	}

	return buf.Bytes()
}

// grindEasyPoW gives a synthetic block the regtest power-of-work limit and a
// nonce that meets it, so HasMetTargetDifficulty passes without a real miner.
func grindEasyPoW(t *testing.T, msgBlock *wire.MsgBlock) {
	t.Helper()

	msgBlock.Header.Bits = 0x207fffff

	for nonce := uint32(0); nonce < 100000; nonce++ {
		msgBlock.Header.Nonce = nonce

		var buf bytes.Buffer
		require.NoError(t, msgBlock.Header.Serialize(&buf))

		header, err := model.NewBlockHeaderFromBytes(buf.Bytes())
		require.NoError(t, err)

		if ok, _, _ := header.HasMetTargetDifficulty(); ok {
			return
		}
	}

	t.Fatal("no nonce met the regtest target")
}

// buildSpendableBlock makes a synthetic block whose transactions exercise the
// inline below-checkpoint route end to end: two transactions spending outputs
// from outside the block, and two more spending those in-block parents.
func buildSpendableBlock(t *testing.T) *bsvutil.Block {
	t.Helper()

	coinbase := wire.NewMsgTx(1)
	coinbase.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{}, Index: 0xffffffff},
		SignatureScript:  []byte{0x51, 0x01, 0x65},
		Sequence:         0xffffffff,
	})
	coinbase.AddTxOut(&wire.TxOut{Value: 5000000000, PkScript: []byte{0x76, 0xa9, 0x14, 0x01}})

	makeTx := func(prev chainhash.Hash, idx uint32, value int64, tag byte) *wire.MsgTx {
		tx := wire.NewMsgTx(1)
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: prev, Index: idx},
			SignatureScript:  []byte{0x00, tag},
			Sequence:         0xffffffff,
		})
		tx.AddTxOut(&wire.TxOut{Value: value, PkScript: []byte{0x76, 0xa9, 0x14, tag}})

		return tx
	}

	txA := makeTx(chainhash.Hash{0xde, 0xad, 0x01}, 0, 1000, 0xaa)
	txB := makeTx(chainhash.Hash{0xde, 0xad, 0x02}, 0, 1000, 0xbb)
	txC := makeTx(txA.TxHash(), 0, 999, 0xcc)
	txD := makeTx(txC.TxHash(), 0, 998, 0xdd)

	msgBlock := &wire.MsgBlock{
		Header:       wire.BlockHeader{Version: 1, PrevBlock: chainhash.Hash{0x11}, Timestamp: time.Unix(1600000000, 0)},
		Transactions: []*wire.MsgTx{coinbase, txA, txB, txC, txD},
	}

	grindEasyPoW(t, msgBlock)

	return bsvutil.NewBlock(msgBlock)
}

// requireSameIngestOutcome compares everything the two entries are supposed to
// agree on: the block handed to blockvalidation, and the subtree blob store.
func requireSameIngestOutcome(ctx context.Context, t *testing.T, buffered, streamed *ingestHarness) {
	t.Helper()

	want := buffered.captured.block()
	got := streamed.captured.block()

	require.NotNil(t, want, "the buffered entry never reached ProcessBlock")
	require.NotNil(t, got, "the streaming entry never reached ProcessBlock")

	require.Equal(t, want.Header.Bytes(), got.Header.Bytes(), "block header")
	require.Equal(t, want.Height, got.Height, "block height")
	require.Equal(t, want.ID, got.ID, "block id")
	require.Equal(t, want.TransactionCount, got.TransactionCount, "transaction count")
	require.Equal(t, want.SizeInBytes, got.SizeInBytes, "serialized block size")
	require.Equal(t, want.CoinbaseTx.Bytes(), got.CoinbaseTx.Bytes(), "coinbase transaction")

	require.Len(t, got.Subtrees, len(want.Subtrees), "subtree count")

	for i, root := range want.Subtrees {
		require.Equal(t, root.String(), got.Subtrees[i].String(), "subtree root %d", i)
	}

	require.Len(t, streamed.subtreeStore.ListKeys(), len(buffered.subtreeStore.ListKeys()),
		"the two entries must write the same number of subtree files")

	for _, root := range want.Subtrees {
		for _, fileType := range []fileformat.FileType{
			fileformat.FileTypeSubtree,
			fileformat.FileTypeSubtreeData,
			fileformat.FileTypeSubtreeMeta,
		} {
			wantBytes, err := buffered.subtreeStore.Get(ctx, root[:], fileType)
			require.NoError(t, err, "buffered %s for subtree %s", fileType, root)

			gotBytes, err := streamed.subtreeStore.Get(ctx, root[:], fileType)
			require.NoError(t, err, "streamed %s for subtree %s", fileType, root)

			require.Equal(t, wantBytes, gotBytes, "%s content for subtree %s", fileType, root)
		}
	}
}

// TestIngestBlock_ParityWithHandleBlockDirect_RealBlock streams a real mainnet
// block through IngestBlock and drives the same block through the relocated
// HandleBlockDirect entry, then requires the two runs to be indistinguishable.
// The unified route is on, so the merkle root is reconstructed from the
// locally-built subtrees — the streaming transaction order has to be exact.
// MaximumMerkleItemsPerSubtree is lowered so the block spans several subtrees.
func TestIngestBlock_ParityWithHandleBlockDirect_RealBlock(t *testing.T) {
	ctx := context.Background()

	block, err := ReadBlockFromFile("testdata/00000000000000000ad4cd15bbeaf6cb4583c93e13e311f9774194aadea87386.bin")
	require.NoError(t, err)

	txs := block.Transactions()
	require.Greater(t, len(txs), 256, "the fixture must span several subtrees at the reduced subtree size")

	buffered := newIngestHarness(ctx, t, "ingest_parity_real_buffered", true, 256)
	streamed := newIngestHarness(ctx, t, "ingest_parity_real_streamed", true, 256)

	require.NoError(t, buffered.sm.HandleBlockDirect(ctx, "peer-a", *block.Hash(), block.MsgBlock()))

	header := block.MsgBlock().Header
	require.NoError(t, streamed.sm.IngestBlock(ctx, &header, uint64(len(txs)),
		bytes.NewReader(serializeTxStream(t, txs)), "peer-b"))

	requireSameIngestOutcome(ctx, t, buffered, streamed)
}

// TestIngestBlock_ParityWithHandleBlockDirect_UTXOs drives the inline
// below-checkpoint route, where the pipeline really creates and spends UTXOs,
// and requires both entries to leave identical UTXO state.
func TestIngestBlock_ParityWithHandleBlockDirect_UTXOs(t *testing.T) {
	ctx := context.Background()

	block := buildSpendableBlock(t)
	txs := block.Transactions()

	buffered := newIngestHarness(ctx, t, "ingest_parity_utxo_buffered", false, 0)
	streamed := newIngestHarness(ctx, t, "ingest_parity_utxo_streamed", false, 0)

	require.NoError(t, buffered.sm.HandleBlockDirect(ctx, "peer-a", *block.Hash(), block.MsgBlock()))

	header := block.MsgBlock().Header
	require.NoError(t, streamed.sm.IngestBlock(ctx, &header, uint64(len(txs)),
		bytes.NewReader(serializeTxStream(t, txs)), "peer-b"))

	requireSameIngestOutcome(ctx, t, buffered, streamed)

	for _, tx := range txs[1:] {
		hash := *tx.Hash()

		wantMeta, err := buffered.utxoStore.Get(ctx, &hash, fields.BlockIDs, fields.Utxos)
		require.NoError(t, err, "buffered run must have created UTXOs for %s", hash)

		gotMeta, err := streamed.utxoStore.Get(ctx, &hash, fields.BlockIDs, fields.Utxos)
		require.NoError(t, err, "streaming run must have created UTXOs for %s", hash)

		require.Equal(t, wantMeta.BlockIDs, gotMeta.BlockIDs, "block ids for %s", hash)
		require.Equal(t, len(wantMeta.SpendingDatas), len(gotMeta.SpendingDatas), "spending data length for %s", hash)

		for i := range wantMeta.SpendingDatas {
			require.Equal(t, wantMeta.SpendingDatas[i], gotMeta.SpendingDatas[i], "spending data %d for %s", i, hash)
		}
	}
}

// TestIngestBlock_TruncatedStream proves a short payload fails cleanly: an
// error out, and nothing written that the buffered entry would not also have
// left behind (it writes nothing before the transaction map is complete).
func TestIngestBlock_TruncatedStream(t *testing.T) {
	ctx := context.Background()

	block := buildSpendableBlock(t)
	txs := block.Transactions()
	full := serializeTxStream(t, txs)

	coinbaseLen := txs[0].MsgTx().SerializeSize()

	tests := []struct {
		name string
		body []byte
	}{
		{name: "truncated inside the coinbase", body: full[:coinbaseLen/2]},
		{name: "truncated after the coinbase", body: full[:coinbaseLen+4]},
		{name: "truncated before the last transaction", body: full[:len(full)-8]},
		{name: "empty stream", body: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newIngestHarness(ctx, t, "ingest_truncated_"+t.Name(), false, 0)

			header := block.MsgBlock().Header
			err := h.sm.IngestBlock(ctx, &header, uint64(len(txs)), bytes.NewReader(tt.body), "peer")
			require.Error(t, err, "a truncated transaction stream must fail")

			require.Empty(t, h.subtreeStore.ListKeys(), "a failed ingest must not write subtree files")
			require.Nil(t, h.captured.block(), "a failed ingest must not reach blockvalidation")

			for _, tx := range txs[1:] {
				hash := *tx.Hash()
				_, getErr := h.utxoStore.Get(ctx, &hash, fields.BlockIDs)
				require.Error(t, getErr, "a failed ingest must not create UTXOs for %s", hash)
			}
		})
	}
}

// countingCloser is a reader that records how often it was closed, so every
// IngestBlock exit path can be checked for stream release.
type countingCloser struct {
	io.Reader

	mu     sync.Mutex
	closes int
}

func (c *countingCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closes++

	return nil
}

func (c *countingCloser) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closes
}

// TestIngestBlock_ClosesStreamOnEveryPath covers the liveness carry from Task 7:
// the transport read loop parks until the block stream closes, so every exit —
// success, early return, decode failure — has to release it.
func TestIngestBlock_ClosesStreamOnEveryPath(t *testing.T) {
	ctx := context.Background()

	block := buildSpendableBlock(t)
	txs := block.Transactions()
	full := serializeTxStream(t, txs)
	header := block.MsgBlock().Header

	t.Run("success", func(t *testing.T) {
		h := newIngestHarness(ctx, t, "ingest_close_success", false, 0)
		stream := &countingCloser{Reader: bytes.NewReader(full)}

		require.NoError(t, h.sm.IngestBlock(ctx, &header, uint64(len(txs)), stream, "peer"))
		require.Equal(t, 1, stream.count(), "a successful ingest must close the stream exactly once")
	})

	t.Run("block already exists", func(t *testing.T) {
		h := newIngestHarness(ctx, t, "ingest_close_exists", false, 0)

		exists := &blockchain.Mock{}
		exists.On("GetBlockExists", mock.Anything, mock.Anything).Return(true, nil)
		h.sm.blockchainClient = exists

		stream := &countingCloser{Reader: bytes.NewReader(full)}

		require.NoError(t, h.sm.IngestBlock(ctx, &header, uint64(len(txs)), stream, "peer"))
		require.Equal(t, 1, stream.count(), "an already-known block must still release the stream")
		require.Nil(t, h.captured.block(), "an already-known block must not be re-processed")
	})

	t.Run("decode failure", func(t *testing.T) {
		h := newIngestHarness(ctx, t, "ingest_close_truncated", false, 0)
		stream := &countingCloser{Reader: bytes.NewReader(full[:12])}

		require.Error(t, h.sm.IngestBlock(ctx, &header, uint64(len(txs)), stream, "peer"))
		require.Equal(t, 1, stream.count(), "a failed ingest must still release the stream")
	})

	t.Run("missing parent", func(t *testing.T) {
		h := newIngestHarness(ctx, t, "ingest_close_orphan", false, 0)

		orphan := &blockchain.Mock{}
		orphan.On("GetBlockExists", mock.Anything, mock.Anything).Return(false, nil)
		orphan.On("GetBlockHeader", mock.Anything, mock.Anything).
			Return((*model.BlockHeader)(nil), (*model.BlockHeaderMeta)(nil), errors.NewBlockNotFoundError("no parent"))
		h.sm.blockchainClient = orphan

		stream := &countingCloser{Reader: bytes.NewReader(full)}

		require.Error(t, h.sm.IngestBlock(ctx, &header, uint64(len(txs)), stream, "peer"))
		require.Equal(t, 1, stream.count(), "an orphan must still release the stream")
	})
}

// gateReader blocks the reader after its first read until the gate is released,
// so a test can observe ingest progress while the payload is still arriving.
type gateReader struct {
	r    io.Reader
	gate chan struct{}
	mu   sync.Mutex
	n    int
}

func (g *gateReader) Read(p []byte) (int, error) {
	g.mu.Lock()
	seen := g.n
	g.mu.Unlock()

	if seen > 0 {
		<-g.gate
	}

	n, err := g.r.Read(p)

	g.mu.Lock()
	g.n += n
	g.mu.Unlock()

	return n, err
}

// TestIngestBlock_ProgressIsObservableMidStream proves the progress surface
// Task 11 needs: while a long payload is still being consumed, a watcher on
// another goroutine can see bytes already taken off the stream. That is what
// keeps the peer idle timer honest during a minutes-long ingest, and it also
// demonstrates the ingest pulls the payload incrementally rather than
// materializing the whole block first.
func TestIngestBlock_ProgressIsObservableMidStream(t *testing.T) {
	ctx := context.Background()

	block, err := ReadBlockFromFile("testdata/00000000000000000ad4cd15bbeaf6cb4583c93e13e311f9774194aadea87386.bin")
	require.NoError(t, err)

	txs := block.Transactions()
	full := serializeTxStream(t, txs)

	h := newIngestHarness(ctx, t, "ingest_progress", true, 256)

	gate := &gateReader{r: bytes.NewReader(full), gate: make(chan struct{})}
	progress := NewProgressReader(gate)

	header := block.MsgBlock().Header
	done := make(chan error, 1)

	go func() {
		done <- h.sm.IngestBlock(ctx, &header, uint64(len(txs)), progress, "peer")
	}()

	require.Eventually(t, func() bool { return progress.BytesRead() > 0 }, 5*time.Second, time.Millisecond,
		"the ingest must report progress before the whole payload has arrived")

	midway := progress.BytesRead()
	require.Less(t, midway, uint64(len(full)), "progress must be observable while the payload is still incomplete")
	require.False(t, progress.LastProgress().IsZero(), "a progressing ingest must stamp its last progress time")

	close(gate.gate)

	select {
	case ingestErr := <-done:
		require.NoError(t, ingestErr)
	case <-time.After(60 * time.Second):
		t.Fatal("ingest did not finish after the stream was released")
	}

	require.Equal(t, uint64(len(full)), progress.BytesRead(), "a completed ingest must have consumed the whole payload")
}

// TestProgressReader covers the wrapper on its own: monotonic byte counting, a
// last-progress stamp that only moves on real progress, and Close forwarded to
// the wrapped stream so IngestBlock's release reaches the transport.
func TestProgressReader(t *testing.T) {
	payload := []byte("the quick brown fox")
	inner := &countingCloser{Reader: bytes.NewReader(payload)}
	pr := NewProgressReader(inner)

	require.Zero(t, pr.BytesRead())
	require.True(t, pr.LastProgress().IsZero(), "no progress has been made yet")

	buf := make([]byte, 4)

	n, err := pr.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, uint64(4), pr.BytesRead())

	first := pr.LastProgress()
	require.False(t, first.IsZero())

	rest, err := io.ReadAll(pr)
	require.NoError(t, err)
	require.Equal(t, payload[4:], rest)
	require.Equal(t, uint64(len(payload)), pr.BytesRead())
	require.False(t, pr.LastProgress().Before(first), "the progress stamp must never go backwards")

	require.NoError(t, pr.Close())
	require.Equal(t, 1, inner.count(), "Close must reach the wrapped stream")

	// A reader with no Close of its own must still satisfy the Closer contract.
	plain := NewProgressReader(bytes.NewReader(payload))
	require.NoError(t, plain.Close())
}

// TestIngestBlock_RejectsBadEnvelope covers the arguments the transport
// envelope supplies: a block must have a header and at least the coinbase.
func TestIngestBlock_RejectsBadEnvelope(t *testing.T) {
	ctx := context.Background()

	block := buildSpendableBlock(t)
	txs := block.Transactions()
	full := serializeTxStream(t, txs)
	header := block.MsgBlock().Header

	h := newIngestHarness(ctx, t, "ingest_envelope", false, 0)

	require.Error(t, h.sm.IngestBlock(ctx, nil, uint64(len(txs)), bytes.NewReader(full), "peer"),
		"a nil header must be rejected")

	require.Error(t, h.sm.IngestBlock(ctx, &header, 0, bytes.NewReader(full), "peer"),
		"a block declaring no transactions must be rejected")
}

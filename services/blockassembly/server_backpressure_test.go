package blockassembly

// Tests for the ingest back-pressure gate (blockassembly_max_queued_transactions).
//
// The subtree processor's input queue is unbounded with a single consumer, so a
// stalled processor under sustained ingest previously grew memory at the full
// ingest rate until the OOM killer intervened. The gate converts that overload
// into THRESHOLD_EXCEEDED rejections at the AddTx / AddTxBatch /
// AddTxBatchColumnar boundary, propagating back-pressure to the validator.

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockassembly/blockassembly_api"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/ordishs/gocore"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newBackpressureServer builds a BlockAssembly whose subtree processor reports a
// fixed queue length, so the gate can be exercised deterministically.
func newBackpressureServer(t *testing.T, limit int64, queueLength int64) (*BlockAssembly, *subtreeprocessor.MockSubtreeProcessor) {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.BlockAssembly.MaxQueuedTransactions = limit

	mockSTP := &subtreeprocessor.MockSubtreeProcessor{}
	mockSTP.On("QueueLength").Return(queueLength).Maybe()

	ba := &BlockAssembly{
		logger:   ulogger.TestLogger{},
		settings: tSettings,
		stats:    gocore.NewStat("blockassembly-backpressure-test"),
		blockAssembler: &BlockAssembler{
			settings:         tSettings,
			subtreeProcessor: mockSTP,
		},
	}

	return ba, mockSTP
}

func TestCheckIngestBackpressure(t *testing.T) {
	t.Run("disabled limit never rejects", func(t *testing.T) {
		ba, _ := newBackpressureServer(t, 0, 1_000_000_000)
		require.NoError(t, ba.checkIngestBackpressure())
	})

	t.Run("queue at limit is allowed", func(t *testing.T) {
		ba, _ := newBackpressureServer(t, 100, 100)
		require.NoError(t, ba.checkIngestBackpressure())
	})

	t.Run("queue over limit rejects with THRESHOLD_EXCEEDED", func(t *testing.T) {
		ba, _ := newBackpressureServer(t, 100, 101)
		err := ba.checkIngestBackpressure()
		require.Error(t, err)
		require.True(t, errors.Is(err, errors.ErrThresholdExceeded), "expected THRESHOLD_EXCEEDED, got: %v", err)
	})
}

func TestAddTxBackpressure(t *testing.T) {
	txid := chainhash.HashH([]byte("backpressure-tx"))

	ti := subtreepkg.TxInpoints{}
	tiBytes, err := ti.Serialize()
	require.NoError(t, err)

	t.Run("AddTx rejected over limit", func(t *testing.T) {
		ba, _ := newBackpressureServer(t, 100, 101)

		_, err := ba.AddTx(context.Background(), &blockassembly_api.AddTxRequest{Txid: txid[:], TxInpoints: tiBytes})
		require.Error(t, err)
		require.True(t, errors.Is(err, errors.ErrThresholdExceeded), "expected THRESHOLD_EXCEEDED, got: %v", err)
	})

	t.Run("AddTx accepted under limit", func(t *testing.T) {
		ba, mockSTP := newBackpressureServer(t, 100, 50)
		mockSTP.On("AddBatch", mock.Anything, mock.Anything).Return()

		resp, err := ba.AddTx(context.Background(), &blockassembly_api.AddTxRequest{Txid: txid[:], TxInpoints: tiBytes})
		require.NoError(t, err)
		require.True(t, resp.Ok)
		mockSTP.AssertCalled(t, "AddBatch", mock.Anything, mock.Anything)
	})

	t.Run("AddTxBatch rejected over limit", func(t *testing.T) {
		ba, _ := newBackpressureServer(t, 100, 101)

		_, err := ba.AddTxBatch(context.Background(), &blockassembly_api.AddTxBatchRequest{
			TxRequests: []*blockassembly_api.AddTxRequest{{Txid: txid[:], TxInpoints: tiBytes}},
		})
		require.Error(t, err)
		require.True(t, errors.Is(err, errors.ErrThresholdExceeded), "expected THRESHOLD_EXCEEDED, got: %v", err)
	})

	t.Run("AddTxBatchColumnar rejected over limit", func(t *testing.T) {
		ba, _ := newBackpressureServer(t, 100, 101)

		_, err := ba.AddTxBatchColumnar(context.Background(), &blockassembly_api.AddTxBatchColumnarRequest{
			TxidsPacked: txid[:],
			Fees:        []uint64{1},
			Sizes:       []uint64{1},
		})
		require.Error(t, err)
		require.True(t, errors.Is(err, errors.ErrThresholdExceeded), "expected THRESHOLD_EXCEEDED, got: %v", err)
	})
}

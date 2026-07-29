package validator

// Tests for the block-assembly back-pressure gate.
//
// The gate sheds MEMPOOL ingest at the very top of validateInternal — before
// any UTXO work — while block assembly's queue is over
// blockassembly_max_queued_transactions. Placement is the whole point: a
// reject must leave the transaction completely untouched (no spends, no
// created record, nothing Locked) so the submitter can simply retry.
// Block-context traffic (InBlock) and callers that skip block assembly are
// exempt.

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockassembly"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestBackpressureGate(t *testing.T) {
	t.Run("mempool tx rejected untouched while overloaded", func(t *testing.T) {
		ctx := context.Background()
		mockStore := &utxo.MockUtxostore{}
		tSettings := test.CreateBaseTestSettings(t)

		validator, err := New(ctx, ulogger.TestLogger{}, tSettings, mockStore, nil, nil, nil, nil, nil)
		require.NoError(t, err)

		v := validator.(*Validator)
		v.blockAssemblyQueueOverloaded.Store(true)

		// GetBlockState is a cached-state read used by the in-memory finality
		// check that deliberately runs BEFORE the gate (terminal verdicts beat
		// a retryable "busy"). It touches no UTXO records.
		mockStore.On("GetBlockState").Return(utxo.BlockState{Height: 100, MedianTime: 1000000000}).Maybe()

		tx, _ := makeExtendGraceTxAndParent(t)

		_, err = v.validateInternal(ctx, tx, 100, &Options{AddTXToBlockAssembly: true})
		require.Error(t, err)
		require.True(t, errors.Is(err, errors.ErrThresholdExceeded), "expected THRESHOLD_EXCEEDED, got: %v", err)

		// The reject must happen before ANY UTXO record interaction: no
		// Get/Spend/SpendAndCreate expectation is registered, so any such call
		// fails the test here.
		mockStore.AssertExpectations(t)
	})

	t.Run("terminal verdicts beat the busy signal while overloaded", func(t *testing.T) {
		ctx := context.Background()
		mockStore := &utxo.MockUtxostore{}
		tSettings := test.CreateBaseTestSettings(t)

		validator, err := New(ctx, ulogger.TestLogger{}, tSettings, mockStore, nil, nil, nil, nil, nil)
		require.NoError(t, err)

		v := validator.(*Validator)
		v.blockAssemblyQueueOverloaded.Store(true)

		mockStore.On("GetBlockState").Return(utxo.BlockState{Height: 100, MedianTime: 1000000000}).Maybe()

		// A coinbase submission is structurally unacceptable: it must get its
		// terminal reject, not a retryable THRESHOLD_EXCEEDED a broadcaster
		// would replay forever.
		_, coinbaseTx := makeExtendGraceTxAndParent(t)

		_, err = v.validateInternal(ctx, coinbaseTx, 100, &Options{AddTXToBlockAssembly: true})
		require.Error(t, err)
		require.False(t, errors.Is(err, errors.ErrThresholdExceeded), "coinbase must get a terminal verdict, got: %v", err)
	})

	t.Run("block-context traffic exempt while overloaded", func(t *testing.T) {
		ctx := context.Background()
		mockStore := &utxo.MockUtxostore{}
		tSettings := test.CreateBaseTestSettings(t)

		validator, err := New(ctx, ulogger.TestLogger{}, tSettings, mockStore, nil, nil, nil, nil, nil)
		require.NoError(t, err)

		v := validator.(*Validator)
		v.blockAssemblyQueueOverloaded.Store(true)

		// Distinctive downstream failure: parent lookup fails, proving the tx
		// got PAST the gate and into real validation work.
		mockStore.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.NewTxNotFoundError("parent gone"))
		mockStore.On("GetMeta", mock.Anything, mock.Anything, mock.Anything).Return(errors.NewTxNotFoundError("not found"))
		mockStore.On("GetBlockState").Return(utxo.BlockState{Height: 100, MedianTime: 1000000000}).Maybe()

		tx, _ := makeExtendGraceTxAndParent(t)

		_, err = v.validateInternal(ctx, tx, 100, &Options{AddTXToBlockAssembly: true, InBlock: true})
		require.Error(t, err)
		require.False(t, errors.Is(err, errors.ErrThresholdExceeded), "block-context tx must not be shed, got: %v", err)
		require.True(t, errors.Is(err, errors.ErrTxMissingParent), "expected the downstream missing-parent failure, got: %v", err)
	})

	t.Run("non-block-assembly callers exempt while overloaded", func(t *testing.T) {
		ctx := context.Background()
		mockStore := &utxo.MockUtxostore{}
		tSettings := test.CreateBaseTestSettings(t)

		validator, err := New(ctx, ulogger.TestLogger{}, tSettings, mockStore, nil, nil, nil, nil, nil)
		require.NoError(t, err)

		v := validator.(*Validator)
		v.blockAssemblyQueueOverloaded.Store(true)

		mockStore.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.NewTxNotFoundError("parent gone"))
		mockStore.On("GetMeta", mock.Anything, mock.Anything, mock.Anything).Return(errors.NewTxNotFoundError("not found"))
		mockStore.On("GetBlockState").Return(utxo.BlockState{Height: 100, MedianTime: 1000000000}).Maybe()

		tx, _ := makeExtendGraceTxAndParent(t)

		_, err = v.validateInternal(ctx, tx, 100, &Options{AddTXToBlockAssembly: false})
		require.Error(t, err)
		require.False(t, errors.Is(err, errors.ErrThresholdExceeded), "caller skipping block assembly must not be shed, got: %v", err)
	})

	t.Run("not overloaded passes the gate", func(t *testing.T) {
		ctx := context.Background()
		mockStore := &utxo.MockUtxostore{}
		tSettings := test.CreateBaseTestSettings(t)

		validator, err := New(ctx, ulogger.TestLogger{}, tSettings, mockStore, nil, nil, nil, nil, nil)
		require.NoError(t, err)

		v := validator.(*Validator)

		mockStore.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.NewTxNotFoundError("parent gone"))
		mockStore.On("GetMeta", mock.Anything, mock.Anything, mock.Anything).Return(errors.NewTxNotFoundError("not found"))
		mockStore.On("GetBlockState").Return(utxo.BlockState{Height: 100, MedianTime: 1000000000}).Maybe()

		tx, _ := makeExtendGraceTxAndParent(t)

		_, err = v.validateInternal(ctx, tx, 100, &Options{AddTXToBlockAssembly: true})
		require.Error(t, err)
		require.False(t, errors.Is(err, errors.ErrThresholdExceeded))
	})
}

// TestMonitorBlockAssemblyQueue drives the poller against a mocked block
// assembly client and asserts the cached verdict follows the reported queue
// depth: a couple of poll errors keep the last verdict (no flapping), but
// persistent failure fails OPEN so a crash-looping block assembly cannot leave
// the validator rejecting the world on a stale verdict.
func TestMonitorBlockAssemblyQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockStore := &utxo.MockUtxostore{}
	tSettings := test.CreateBaseTestSettings(t)

	validator, err := New(ctx, ulogger.TestLogger{}, tSettings, mockStore, nil, nil, nil, nil, nil)
	require.NoError(t, err)

	v := validator.(*Validator)

	baMock := blockassembly.NewMock()
	// First poll: over the limit. Subsequent polls: errors — the verdict must
	// survive the first few, then clear at the fail-open threshold.
	baMock.On("GetQueueLength", mock.Anything).Return(int64(101), nil).Once()
	baMock.On("GetQueueLength", mock.Anything).Return(int64(0), errors.NewServiceError("block assembly unavailable"))

	go v.monitorBlockAssemblyQueue(ctx, baMock, 100)

	require.Eventually(t, func() bool {
		return v.blockAssemblyQueueOverloaded.Load()
	}, 5*time.Second, 50*time.Millisecond, "monitor must flag the queue as overloaded")

	// A single failed poll keeps the last verdict (no flapping on a blip)…
	time.Sleep(2 * blockAssemblyQueueMonitorInterval)
	require.True(t, v.blockAssemblyQueueOverloaded.Load(), "early poll errors must keep the last verdict")

	// …but persistent failure crosses blockAssemblyQueueMonitorFailOpenAfter
	// and clears it.
	require.Eventually(t, func() bool {
		return !v.blockAssemblyQueueOverloaded.Load()
	}, (blockAssemblyQueueMonitorFailOpenAfter+3)*blockAssemblyQueueMonitorInterval, 100*time.Millisecond, "persistent poll failure must fail open")
}

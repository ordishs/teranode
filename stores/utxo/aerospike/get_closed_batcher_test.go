package aerospike

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/stretchr/testify/require"
)

// closedGetBatcher is a batcherIfc whose PutCtx panics exactly like go-batcher
// v2.0.4 does when Put is called after Close ("send on closed channel").
type closedGetBatcher struct{}

func (closedGetBatcher) Put(*batchGetItem, ...int) {}
func (closedGetBatcher) PutCtx(context.Context, *batchGetItem, ...int) {
	panic("send on closed channel")
}
func (closedGetBatcher) PutBatch([]*batchGetItem, ...int) {}
func (closedGetBatcher) PutBatchCtx(context.Context, []*batchGetItem, ...int) {
	panic("send on closed channel")
}
func (closedGetBatcher) Trigger()                      {}
func (closedGetBatcher) SetDrainMode(bool)             {}
func (closedGetBatcher) SetTickInterval(time.Duration) {}
func (closedGetBatcher) Close()                        {}

// TestGet_ClosedBatcherReturnsErrorNotPanic is the regression test for the
// production crash: during graceful shutdown Store.Close closes the get
// batcher while an in-flight block-validation goroutine
// (checkParentsExistOnChain -> Get) is still calling Get. Enqueuing into the
// closed batcher panics "send on closed channel" and crashes the process. The
// store must surface this as an error, not panic.
func TestGet_ClosedBatcherReturnsErrorNotPanic(t *testing.T) {
	s := newTestStoreForGet(t)
	s.getBatcher = closedGetBatcher{}

	var (
		err error
	)

	require.NotPanics(t, func() {
		_, err = s.get(context.Background(), &chainhash.Hash{0x01}, []fields.FieldName{fields.Fee})
	}, "get() must not propagate the batcher's send-on-closed-channel panic")

	require.Error(t, err)
	require.Contains(t, err.Error(), "shutting down")
}

// TestGet_CancelledContextSkipsBatcher verifies the cheap fast-path: a cancelled
// context (graceful shutdown) returns before the batcher is touched at all, so
// the read aborts without risking the enqueue panic.
func TestGet_CancelledContextSkipsBatcher(t *testing.T) {
	s := newTestStoreForGet(t)
	s.getBatcher = closedGetBatcher{} // would panic if reached

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var err error

	require.NotPanics(t, func() {
		_, err = s.get(ctx, &chainhash.Hash{0x01}, []fields.FieldName{fields.Fee})
	})

	require.ErrorIs(t, err, context.Canceled)
}

// sendOnClosedBatcher is a generic batcherIfc whose Put/PutCtx panic exactly as
// go-batcher v2.0.4 does after Close. okBatcher is a no-op (open) batcher.
type sendOnClosedBatcher[T any] struct{}

func (sendOnClosedBatcher[T]) Put(*T, ...int)                     { panic("send on closed channel") }
func (sendOnClosedBatcher[T]) PutCtx(context.Context, *T, ...int) { panic("send on closed channel") }
func (sendOnClosedBatcher[T]) PutBatch([]*T, ...int)              { panic("send on closed channel") }
func (sendOnClosedBatcher[T]) PutBatchCtx(context.Context, []*T, ...int) {
	panic("send on closed channel")
}
func (sendOnClosedBatcher[T]) Trigger()                      {}
func (sendOnClosedBatcher[T]) SetDrainMode(bool)             {}
func (sendOnClosedBatcher[T]) SetTickInterval(time.Duration) {}
func (sendOnClosedBatcher[T]) Close()                        {}

type okBatcher[T any] struct{}

func (okBatcher[T]) Put(*T, ...int)                            {}
func (okBatcher[T]) PutCtx(context.Context, *T, ...int)        {}
func (okBatcher[T]) PutBatch([]*T, ...int)                     {}
func (okBatcher[T]) PutBatchCtx(context.Context, []*T, ...int) {}
func (okBatcher[T]) Trigger()                                  {}
func (okBatcher[T]) SetDrainMode(bool)                         {}
func (okBatcher[T]) SetTickInterval(time.Duration)             {}
func (okBatcher[T]) Close()                                    {}

// TestSafeBatcherPut_RecoversSendOnClosed locks the shared guard used by the
// spend / locked / outpoint / get enqueue paths: a send-on-closed-channel panic
// becomes a returned shutdown error, and the open-batcher path returns nil.
func TestSafeBatcherPut_RecoversSendOnClosed(t *testing.T) {
	closed := sendOnClosedBatcher[batchGetItem]{}

	errCtx := safeBatcherPutCtx[batchGetItem](closed, context.Background(), &batchGetItem{}, "spend")
	require.Error(t, errCtx)
	require.Contains(t, errCtx.Error(), "shutting down")

	errPut := safeBatcherPut[batchGetItem](closed, &batchGetItem{}, "outpoint")
	require.Error(t, errPut)
	require.Contains(t, errPut.Error(), "shutting down")

	require.NoError(t, safeBatcherPutCtx[batchGetItem](okBatcher[batchGetItem]{}, context.Background(), &batchGetItem{}, "get"))
	require.NoError(t, safeBatcherPut[batchGetItem](okBatcher[batchGetItem]{}, &batchGetItem{}, "get"))
}

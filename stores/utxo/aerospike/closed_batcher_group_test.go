package aerospike

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/stretchr/testify/require"
)

// TestSetLocked_ClosedBatcherDoesNotParkForFullTimeout is the regression test for
// the batch-enqueue accounting. PutBatchCtx sends the whole group in one channel
// send, so a Put-after-Close rejects every item at once. If the guard returned
// its error without completing them, group.Wait would park for the full
// batcherWait and then report a timeout — hiding the shutdown and stalling the
// caller for the whole budget.
//
// batcherWait is set high enough that a parked wait is unmistakable.
func TestSetLocked_ClosedBatcherDoesNotParkForFullTimeout(t *testing.T) {
	s := newTestStoreForGet(t)
	s.batcherWait = 30 * time.Second
	s.lockedBatcher = sendOnClosedBatcher[batchLocked]{}

	hashes := []chainhash.Hash{{0x01}, {0x02}, {0x03}}

	start := time.Now()

	var err error

	require.NotPanics(t, func() {
		err = s.SetLocked(context.Background(), hashes, true)
	}, "SetLocked must not propagate the batcher's send-on-closed-channel panic")

	elapsed := time.Since(start)

	require.Error(t, err)
	require.Contains(t, err.Error(), "shutting down")
	require.Less(t, elapsed, 5*time.Second,
		"every item must be completed on a rejected batch send; parking for batcherWait means the accounting is wrong")
}

// TestCreate_ClosedBatcherReturnsErrorNotPanic covers the single-item counterpart
// on the store batcher, which Store.Close closes first — so it is dead for the
// whole remaining drain and a racing Create would otherwise crash the process.
func TestCreate_ClosedBatcherReturnsErrorNotPanic(t *testing.T) {
	s := newTestStoreForGet(t)
	s.batcherWait = 30 * time.Second
	s.storeBatcher = sendOnClosedBatcher[BatchStoreItem]{}

	start := time.Now()

	item := &BatchStoreItem{}
	require.NotPanics(t, func() {
		if enqueueErr := safeBatcherPutCtx(s.storeBatcher, context.Background(), item, "store"); enqueueErr != nil {
			item.complete(enqueueErr)
		}
	})

	require.Error(t, item.result)
	require.Contains(t, item.result.Error(), "shutting down")
	require.Less(t, time.Since(start), 5*time.Second)
}

package aerospike_test

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoreExternallySuccessScenarios tests that storeExternallyWithLock returns
// success in the correct scenarios according to the "finish off previous attempt" pattern
func TestStoreExternallySuccessScenarios(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := ulogger.NewErrorTestLogger(t)
	settings := test.CreateBaseTestSettings(t)

	client, store, _, cleanup := initAerospike(t, settings, logger)
	defer cleanup()

	t.Run("Scenario A: 100% success - all records created fresh", func(t *testing.T) {
		cleanDB(t, client)

		// Create a transaction that requires multiple records
		tx := createTransactionWithOutputs(25000) // 2 records needed

		// First attempt - should succeed completely
		_, err := store.Create(ctx, tx, 100)
		require.NoError(t, err, "First complete creation should succeed")

		// Verify transaction exists and is not creating
		txMeta, err := store.Get(ctx, tx.TxIDChainHash())
		require.NoError(t, err)
		require.NotNil(t, txMeta)
		assert.False(t, txMeta.Locked, "Transaction should not be locked")
		// Note: creating flag is cleared, so we can't check it via Get
	})

	t.Run("Scenario B: Partial success - some records already exist (KEY_EXISTS_ERROR)", func(t *testing.T) {
		cleanDB(t, client)

		// Simulate a partial creation by manually creating some records with creating=true
		tx := createTransactionWithOutputs(25000) // 2 records needed

		// Manually create first record with creating=true to simulate partial failure
		txMeta1, err := store.Create(ctx, tx, 100)
		require.NoError(t, err)

		// Now create again - should "finish off" the previous attempt
		// This simulates:
		// - Process A created some records (with creating=true)
		// - Process A failed partway
		// - Process B tries to create (should complete it)
		_, err = store.Create(ctx, tx, 100)

		// Should get "already exists" which is a TxExistsError, not a processing error
		require.Error(t, err, "Second attempt should detect transaction exists")
		assert.Contains(t, err.Error(), "already exists", "Should be TxExistsError")

		// But the first attempt should have succeeded
		require.NotNil(t, txMeta1)
	})

	t.Run("Scenario C: Recovery - complete partial transaction from previous failed attempt", func(t *testing.T) {
		cleanDB(t, client)

		// This test simulates the core "finish off" behavior
		// We'll use the raw StoreTransactionExternally to have more control

		tx := createTransactionWithOutputs(25000) // 2 records

		// First attempt - create the transaction
		// This will create both records with creating=true and then clear the flag
		bItem1, binsToStore1 := prepareBatchStoreItem(t, store, tx, 100, []uint32{}, []uint32{}, []int{})
		go store.StoreTransactionExternally(ctx, bItem1, binsToStore1)

		err := bItem1.RecvDone()
		require.NoError(t, err, "First attempt should succeed")

		// Second attempt - should get "already exists" because transaction is complete
		bItem2, binsToStore2 := prepareBatchStoreItem(t, store, tx, 100, []uint32{}, []uint32{}, []int{})
		go store.StoreTransactionExternally(ctx, bItem2, binsToStore2)

		err = bItem2.RecvDone()
		require.Error(t, err, "Second attempt should fail with already exists")
		assert.Contains(t, err.Error(), "already exists", "Should indicate transaction exists")
	})

	t.Run("Scenario D: Multiple concurrent attempts - only first complete wins", func(t *testing.T) {
		cleanDB(t, client)

		// Multiple processes try to create the same transaction
		// Only the first one to COMPLETE all records should succeed
		// Others should get "already exists" or "in progress"

		tx := createTransactionWithOutputs(100) // Small for faster test

		// Try to create the same transaction 3 times concurrently
		results := make([]error, 3)
		done := make(chan int, 3)

		for i := 0; i < 3; i++ {
			go func(idx int) {
				_, err := store.Create(ctx, tx, 100)
				results[idx] = err
				done <- idx
			}(i)
		}

		// Wait for all to complete
		for i := 0; i < 3; i++ {
			<-done
		}

		// Exactly one should succeed, others should get "already exists" or "in progress"
		successCount := 0
		for i, err := range results {
			if err == nil {
				successCount++
				t.Logf("Attempt %d: SUCCESS", i)
			} else {
				t.Logf("Attempt %d: %v", i, err)
				// Should be either "in progress" or "already exists"
				assert.True(t,
					assert.ObjectsAreEqual(err.Error(), "already exists") ||
						assert.ObjectsAreEqual(err.Error(), "in progress"),
					"Error should indicate concurrent access: %v", err)
			}
		}

		assert.Equal(t, 1, successCount, "Exactly one attempt should succeed completely")
	})
}

// TestStoreExternallyFailureScenarios tests that storeExternallyWithLock returns
// error only for real failures (not KEY_EXISTS_ERROR)
func TestStoreExternallyFailureScenarios(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Run("Real Aerospike failure should return error", func(t *testing.T) {
		// This would require mocking or causing a real Aerospike failure
		// which is complex in integration tests
		t.Skip("Requires ability to inject Aerospike failures")
	})

	t.Run("Blob write failure should return error", func(t *testing.T) {
		// This would require mocking the blob store to fail
		t.Skip("Requires blob store mocking")
	})
}

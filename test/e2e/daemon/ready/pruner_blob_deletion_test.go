package smoke

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/daemon"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob/storetypes"
	"github.com/bsv-blockchain/teranode/test"
	"github.com/bsv-blockchain/teranode/util/usql"
	"github.com/stretchr/testify/require"
)

// TestBlobDeletionScheduling verifies that the blockchain service correctly schedules
// blob deletions in its queue via the BlockchainClient API and that the pruner
// service correctly executes those deletions when blocks are mined.
//
// Test flow:
// 1. Create TestDaemon with Pruner service enabled
// 2. Get current blockchain height
// 3. Schedule test blob deletions at various heights
// 4. Verify scheduling via ListScheduledDeletions
// 5. Test cancellation via CancelBlobDeletion
// 6. Generate blocks to trigger pruner
// 7. Verify deletions are executed
func TestBlobDeletionScheduling(t *testing.T) {
	node := daemon.NewTestDaemon(t, daemon.TestOptions{
		EnablePruner:     true,
		EnableRPC:        true,
		UTXOStoreType:    "aerospike",
		UseUnifiedLogger: false,
		SettingsOverrideFunc: test.ComposeSettings(
			test.SystemTestSettings(),
			func(s *settings.Settings) {
				s.Pruner.BlobDeletionEnabled = true
				s.Pruner.BlobDeletionSafetyWindow = 0
				s.Pruner.BlobDeletionBatchSize = 100
				s.Pruner.BlobDeletionMaxRetries = 3
				s.Pruner.BlockTrigger = settings.PrunerBlockTriggerOnBlockMined
			},
		),
	})
	defer node.Stop(t, true)

	ctx := context.Background()

	db := node.BlockchainStore.GetDB()

	_, meta, err := node.BlockchainClient.GetBestBlockHeader(ctx)
	require.NoError(t, err)
	currentHeight := meta.Height
	t.Logf("Current blockchain height: %d", currentHeight)

	testBlobs := []struct {
		key            []byte
		fileType       string
		storeType      storetypes.BlobStoreType
		deleteAtHeight uint32
	}{
		{
			key:            make([]byte, 32),
			fileType:       "test",
			storeType:      storetypes.TXSTORE,
			deleteAtHeight: currentHeight + 1,
		},
		{
			key:            make([]byte, 32),
			fileType:       "test",
			storeType:      storetypes.TXSTORE,
			deleteAtHeight: currentHeight + 2,
		},
		{
			key:            make([]byte, 32),
			fileType:       "test",
			storeType:      storetypes.TXSTORE,
			deleteAtHeight: currentHeight + 1,
		},
	}

	// Create random blob keys
	for i := range testBlobs {
		_, err := rand.Read(testBlobs[i].key)
		require.NoError(t, err)
	}

	// Schedule test blob deletions
	for i, blob := range testBlobs {
		_, scheduled, err := node.BlockchainClient.ScheduleBlobDeletion(ctx, blob.key, blob.fileType, blob.storeType, blob.deleteAtHeight)
		require.NoError(t, err)
		require.True(t, scheduled, "Blob %d should be scheduled", i)
		t.Logf("Scheduled test blob %d with key=%x, DAH=%d", i, blob.key[:8], blob.deleteAtHeight)
	}

	// Get the number of rows in the scheduled_blob_deletions table
	dbCount := getDBDeletionCount(t, db)
	require.Equal(t, 3, dbCount, "Should have 3 scheduled deletions in DB")
	t.Log("Verified 3 deletions in database")

	// Now check with the API
	deletions, _, err := node.BlockchainClient.ListScheduledDeletions(ctx, 0, 0, storetypes.TXSTORE, false, 100, 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(deletions), 3, "Should have at least 3 scheduled deletions")
	t.Logf("ListScheduledDeletions returned %d deletions", len(deletions))

	deletions, _, err = node.BlockchainClient.ListScheduledDeletions(ctx, 0, currentHeight+1, storetypes.TXSTORE, false, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 2, len(deletions), "Should have 2 deletions for DAH <= %d", currentHeight+1)
	t.Log("Height range filtering works")

	deletions, _, err = node.BlockchainClient.ListScheduledDeletions(ctx, currentHeight+2, 0, storetypes.TXSTORE, false, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 1, len(deletions), "Should have 1 deletion for DAH >= %d", currentHeight+2)
	t.Log("Future deletions queried correctly")

	// Now cancel the first deletion
	testBlob := testBlobs[0]
	cancelled, err := node.BlockchainClient.CancelBlobDeletion(ctx, testBlob.key, testBlob.fileType, testBlob.storeType)
	require.NoError(t, err)
	require.True(t, cancelled, "Cancellation should succeed")
	t.Log("Cancellation works")

	deletions, _, err = node.BlockchainClient.ListScheduledDeletions(ctx, 0, 0, storetypes.TXSTORE, false, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 2, len(deletions), "Should have 2 deletions after cancelling 1")
	t.Log("Queue updated after cancellation")

	dbCount = getDBDeletionCount(t, db)
	require.Equal(t, 2, dbCount, "Should have 2 scheduled deletions in DB after cancellation")

	t.Log("Scheduling operations validated successfully")

	t.Log("Generating block to trigger pruner...")
	_, err = node.CallRPC(node.Ctx, "generate", []any{1})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, newMeta, err := node.BlockchainClient.GetBestBlockHeader(ctx)
		if err != nil {
			return false
		}
		return newMeta.Height >= currentHeight+1
	}, 10*time.Second, 100*time.Millisecond, "Block was not mined")
	t.Log("Block mined successfully")

	require.Eventually(t, func() bool {
		count := getDBDeletionCount(t, db)
		t.Logf("Waiting for pruner... deletions remaining: %d (expecting 1)", count)
		return count == 1
	}, 10*time.Second, 500*time.Millisecond, "Pruner did not process deletions at height %d", currentHeight+1)

	t.Logf("Pruner executed deletions for height %d", currentHeight+1)

	deletions, _, err = node.BlockchainClient.ListScheduledDeletions(ctx, 0, 0, storetypes.TXSTORE, false, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 1, len(deletions), "Should have 1 deletion remaining (DAH=%d)", currentHeight+2)
	t.Log("E2E blob deletion scheduling and execution validated successfully")
}

func getDBDeletionCount(t *testing.T, db *usql.DB) int {
	var count int
	err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM scheduled_blob_deletions").Scan(&count)
	require.NoError(t, err)
	return count
}

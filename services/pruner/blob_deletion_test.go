package pruner

// TODO: These tests need to be updated to work with the new blob deletion architecture
// where blob deletion scheduling is managed by the blockchain service instead of the pruner service.
//
// The tests should be refactored to:
// 1. Use blockchain.Mock to mock the blockchain client's blob deletion methods
// 2. Test the processBlobDeletionsAtHeight worker logic independently
// 3. Add integration tests that verify the full flow with a real blockchain service
//
// Key changes needed:
// - Mock ScheduleBlobDeletion, GetPendingBlobDeletions, AcquireBlobDeletionBatch, CompleteBlobDeletionBatch
// - Remove direct database access (now handled by blockchain service)
// - Focus tests on the pruner worker's interaction with blob stores, not database operations

// The following tests are commented out until they can be properly refactored:
// - TestBlobDeletionSchedulingAndExecution
// - TestBlobDeletionCancellation
// - TestBlobDeletionIdempotency
// - TestBlobDeletionUpsert
// - TestBlobDeletionListFilters

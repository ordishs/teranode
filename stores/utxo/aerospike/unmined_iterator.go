package aerospike

import (
	"context"
	"math"
	"runtime"
	"sync"

	as "github.com/aerospike/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/util"
)

// unminedTxIterator implements utxo.UnminedTxIterator for Aerospike
// It scans all records in the set and yields those that are not mined (i.e., unmined/mempool)
// Uses multiple workers to read from Aerospike in parallel for improved throughput
type unminedTxIterator struct {
	store         *Store
	fullScan      bool
	err           error
	done          bool
	recordset     *as.Recordset
	resultChan    chan []*utxo.UnminedTransaction
	errorChan     chan error
	cancelWorkers context.CancelFunc
	wg            sync.WaitGroup
}

// newUnminedTxIterator creates a new iterator for scanning unmined transactions in Aerospike.
// The iterator uses a scan operation to traverse all records in the set and filters
// for transactions that don't have block IDs (indicating they are unmined/mempool transactions).
// Multiple workers are spawned to process records in parallel for improved throughput.
//
// Parameters:
//   - store: The Aerospike store instance to iterate over
//   - fullScan: If true, performs a full scan of all records; if false, applies a filter to limit results
//
// Returns:
//   - *unminedTxIterator: A new iterator instance ready for use
//   - error: Any error encountered during iterator initialization
func newUnminedTxIterator(store *Store, fullScan bool) (*unminedTxIterator, error) {
	stmt := as.NewStatement(store.namespace, store.setName)

	if !fullScan {
		if err := stmt.SetFilter(as.NewRangeFilter(fields.UnminedSince.String(), 1, int64(math.MaxUint32))); err != nil {
			return nil, err
		}
	}

	// Set the bins to retrieve only the necessary fields for unmined transactions
	stmt.BinNames = []string{
		fields.TxID.String(),
		fields.Fee.String(),
		fields.SizeInBytes.String(),
		fields.External.String(),
		fields.Inputs.String(),
		fields.CreatedAt.String(),
		fields.Conflicting.String(),
		fields.Locked.String(),
		fields.BlockIDs.String(),
		fields.UnminedSince.String(),
		fields.IsCoinbase.String(),
	}

	policy := util.GetAerospikeQueryPolicy(store.settings)
	policy.IncludeBinData = true
	policy.RecordQueueSize = 10 * 1024 * 1024 // 10MB queue for better throughput

	store.logger.Infof("[newUnminedTxIterator] Starting Aerospike query for unmined transactions (fullScan=%t)", fullScan)
	recordset, err := store.client.Query(policy, stmt)
	if err != nil {
		store.logger.Errorf("[newUnminedTxIterator] Aerospike query failed: %v", err)
		return nil, err
	}
	store.logger.Infof("[newUnminedTxIterator] Aerospike query completed successfully")

	// Create context for workers
	workerCtx, cancel := context.WithCancel(context.Background())

	// Calculate optimal number of workers (use half the CPUs)
	numWorkers := runtime.NumCPU() / 2
	if numWorkers < 1 {
		numWorkers = 1
	}

	// Buffer size for batch channel - smaller since each item is a batch
	// With batches of 16K items, 16 buffers = 256K items buffered
	resultChanSize := numWorkers * 2

	it := &unminedTxIterator{
		store:         store,
		fullScan:      fullScan,
		recordset:     recordset,
		resultChan:    make(chan []*utxo.UnminedTransaction, resultChanSize),
		errorChan:     make(chan error, 1),
		cancelWorkers: cancel,
	}

	// Start worker goroutines
	result := recordset.Results()
	for i := 0; i < numWorkers; i++ {
		it.wg.Add(1)
		go it.worker(workerCtx, result)
	}

	// Start a goroutine to close resultChan after all workers finish
	go func() {
		it.wg.Wait()
		close(it.resultChan)
		close(it.errorChan)
	}()

	return it, nil
}

// worker processes records from Aerospike in parallel
func (it *unminedTxIterator) worker(ctx context.Context, result <-chan *as.Result) {
	defer it.wg.Done()

	const (
		batchSize          = 16 * 1024
		contextCheckPeriod = 16 * 1024
	)

	// Pre-compute field names to avoid repeated String() calls
	conflictingField := fields.Conflicting.String()
	coinbaseField := fields.IsCoinbase.String()

	// Local buffer to batch sends and reduce channel contention
	localBuffer := make([]*utxo.UnminedTransaction, 0, batchSize)
	itemsProcessed := 0

	// Flush function sends the entire batch as a single slice
	flush := func() error {
		if len(localBuffer) == 0 {
			return nil
		}

		// Create a copy of the slice to send
		batchToSend := make([]*utxo.UnminedTransaction, len(localBuffer))
		copy(batchToSend, localBuffer)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case it.resultChan <- batchToSend:
		}

		localBuffer = localBuffer[:0]
		return nil
	}

	defer func() {
		_ = flush() // Best effort flush on exit
	}()

	for {
		// Only check context every contextCheckPeriod iterations for performance
		if itemsProcessed%contextCheckPeriod == 0 {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}

		// Read from result channel without select for performance
		rec, ok := <-result
		if !ok || rec == nil {
			return
		}

		if rec.Err != nil {
			it.errorChan <- rec.Err
			return
		}

		itemsProcessed++

		// Quick inline filtering checks
		if conflictingVal := rec.Record.Bins[conflictingField]; conflictingVal != nil {
			if conflicting, ok := conflictingVal.(bool); ok && conflicting {
				continue
			}
		}

		if coinbaseBool, ok := rec.Record.Bins[coinbaseField].(bool); ok && coinbaseBool {
			localBuffer = append(localBuffer, &utxo.UnminedTransaction{Skip: true})
			if len(localBuffer) >= batchSize {
				if err := flush(); err != nil {
					return
				}
			}
			continue
		}

		// Process the record
		unminedTx, err := it.processRecord(ctx, rec.Record.Bins)
		if err != nil {
			it.errorChan <- err
			return
		}

		if unminedTx != nil {
			localBuffer = append(localBuffer, unminedTx)
			if len(localBuffer) >= batchSize {
				if err := flush(); err != nil {
					return
				}
			}
		}
	}
}

// processRecord processes a single Aerospike record and returns the unmined transaction
func (it *unminedTxIterator) processRecord(ctx context.Context, bins map[string]interface{}) (*utxo.UnminedTransaction, error) {
	// Extract transaction data from the record
	txData, err := it.extractTransactionData(bins)
	if err != nil {
		return nil, errors.NewProcessingError("invalid transaction data", err)
	}

	blockIDs, err := processBlockIDs(bins)
	if err != nil {
		return nil, errors.NewProcessingError("invalid block IDs for %s", txData.hash.String(), err)
	}

	// Process external transaction if needed
	txInpoints, err := it.processTransactionInpoints(ctx, txData, bins)
	if err != nil {
		if it.fullScan {
			// In full scan mode, if we encounter an error processing inpoints and the transaction
			// has block IDs, it has already been mined. We can skip it.
			return &utxo.UnminedTransaction{
				Node: &subtree.Node{
					Hash:        *txData.hash,
					Fee:         txData.fee,
					SizeInBytes: txData.size,
				},
				Skip: true,
			}, nil
		}

		return nil, errors.NewProcessingError("failed to process transaction inpoints for %s", txData.hash.String(), err)
	}

	// Extract created_at timestamp
	createdAt, err := it.extractCreatedAt(bins)
	if err != nil {
		return nil, errors.NewProcessingError("invalid created_at for %s", txData.hash.String(), err)
	}

	locked, err := it.extractLocked(bins)
	if err != nil {
		return nil, errors.NewProcessingError("invalid locked status for %s", txData.hash.String(), err)
	}

	return &utxo.UnminedTransaction{
		Node: &subtree.Node{
			Hash:        *txData.hash,
			Fee:         txData.fee,
			SizeInBytes: txData.size,
		},
		UnminedSince: txData.unminedSince,
		TxInpoints:   &txInpoints,
		CreatedAt:    createdAt,
		Locked:       locked,
		BlockIDs:     blockIDs,
	}, nil
}

// Next advances the iterator and returns a batch of unmined transactions.
// This method reads multiple transactions from the result channel populated by worker goroutines
// for improved performance by amortizing overhead across multiple transactions.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - []*utxo.UnminedTransaction: A batch of unmined transactions, or empty slice if iteration is complete
//   - error: Any error encountered during iteration
func (it *unminedTxIterator) Next(ctx context.Context) ([]*utxo.UnminedTransaction, error) {
	if it.done || it.err != nil {
		return nil, it.err
	}

	// If resultChan is nil, no workers were started (e.g., nil recordset)
	if it.resultChan == nil {
		it.closeWithLogging()
		return nil, nil
	}

	// Check for errors from workers
	select {
	case <-ctx.Done():
		it.err = ctx.Err()
		it.closeWithLogging()
		return nil, it.err
	case err := <-it.errorChan:
		if err != nil {
			it.err = err
			it.closeWithLogging()
			return nil, err
		}
	default:
	}

	batch, ok := <-it.resultChan
	if !ok {
		it.closeWithLogging()
		return nil, nil
	}

	return batch, nil
}

// transactionData holds the basic transaction data extracted from a record
type transactionData struct {
	hash         *chainhash.Hash
	fee          uint64
	size         uint64
	unminedSince int
}

// closeWithLogging closes the iterator with error logging
func (it *unminedTxIterator) closeWithLogging() {
	if err := it.Close(); err != nil {
		it.store.logger.Warnf("failed to close iterator: %v", err)
	}
}

// extractTransactionData extracts basic transaction data from Aerospike record bins
func (it *unminedTxIterator) extractTransactionData(bins map[string]interface{}) (*transactionData, error) {
	// Extract and validate txid
	txidVal := bins[fields.TxID.String()]
	if txidVal == nil {
		return nil, errors.NewProcessingError("txid not found")
	}

	txidValBytes, ok := txidVal.([]byte)
	if !ok {
		return nil, errors.NewProcessingError("txid not []byte")
	}

	hash, err := chainhash.NewHash(txidValBytes)
	if err != nil {
		return nil, err
	}

	// Extract and validate fee
	feeVal := bins[fields.Fee.String()]
	if feeVal == nil {
		return nil, errors.NewProcessingError("fee not found")
	}

	fee, err := toUint64(feeVal)
	if err != nil {
		return nil, errors.NewProcessingError("Failed to convert fee")
	}

	// Extract and validate size
	sizeVal := bins[fields.SizeInBytes.String()]
	if sizeVal == nil {
		return nil, errors.NewProcessingError("size not found")
	}

	size, _ := toUint64(sizeVal)

	unminedSince, _ := bins[fields.UnminedSince.String()].(int)

	return &transactionData{
		hash:         hash,
		fee:          fee,
		size:         size,
		unminedSince: unminedSince,
	}, nil
}

// processTransactionInpoints processes transaction inputs based on whether it's external or internal
func (it *unminedTxIterator) processTransactionInpoints(ctx context.Context, txData *transactionData, bins map[string]interface{}) (subtree.TxInpoints, error) {
	external, ok := bins[fields.External.String()].(bool)
	if !ok || !external {
		return it.processInternalTransactionInpoints(bins)
	}

	return it.processExternalTransactionInpoints(ctx, txData.hash)
}

// processExternalTransactionInpoints processes inputs for external transactions
func (it *unminedTxIterator) processExternalTransactionInpoints(ctx context.Context, hash *chainhash.Hash) (subtree.TxInpoints, error) {
	var externalTx *bt.Tx
	var err error

	externalTx, err = it.store.GetTxFromExternalStore(ctx, *hash)
	if err != nil {
		return subtree.TxInpoints{}, err
	}

	txInpoints, err := subtree.NewTxInpointsFromTx(externalTx)
	if err != nil {
		return subtree.TxInpoints{}, errors.NewTxInvalidError("could not process tx inpoints", err)
	}

	return txInpoints, nil
}

// processInternalTransactionInpoints processes inputs for internal transactions
func (it *unminedTxIterator) processInternalTransactionInpoints(bins map[string]interface{}) (subtree.TxInpoints, error) {
	txInpoints, err := processInputsToTxInpoints(bins)
	if err != nil {
		return subtree.TxInpoints{}, errors.NewTxInvalidError("could not process input interfaces", err)
	}

	return txInpoints, nil
}

// extractCreatedAt extracts the created_at timestamp from record bins
func (it *unminedTxIterator) extractCreatedAt(bins map[string]interface{}) (int, error) {
	createdAtVal, ok := bins[fields.CreatedAt.String()]
	if !ok || createdAtVal == nil {
		return 0, errors.NewProcessingError("created_at not found")
	}

	createdAt, ok := createdAtVal.(int)
	if !ok {
		return 0, errors.NewProcessingError("created_at not int64")
	}

	return createdAt, nil
}

// extractLocked extracts the locked status from record bins
func (it *unminedTxIterator) extractLocked(bins map[string]interface{}) (bool, error) {
	// Locked status is optional, so we check if it exists
	lockedVal, ok := bins[fields.Locked.String()].(bool)
	if ok {
		return lockedVal, nil
	}

	return false, nil
}

// Err returns the first error encountered during iteration, if any.
// This should be called after Next returns nil to check if iteration
// completed successfully or due to an error.
//
// Returns:
//   - error: The error that caused iteration to stop, or nil if no error occurred
func (it *unminedTxIterator) Err() error {
	return it.err
}

// Close releases resources held by the iterator and marks it as done.
// It cancels all worker goroutines and closes the recordset.
// It's safe to call Close multiple times. After calling Close, subsequent
// calls to Next will return nil.
//
// Returns:
//   - error: Always returns nil (kept for interface compatibility)
func (it *unminedTxIterator) Close() error {
	if it.done {
		return nil
	}

	it.done = true

	// Cancel workers
	if it.cancelWorkers != nil {
		it.cancelWorkers()
	}

	// Close recordset
	if it.recordset != nil {
		return it.recordset.Close()
	}

	return nil
}

// toUint64 converts various numeric interface{} types to uint64.
// This utility function handles type assertions for Aerospike record values
// which can come in different numeric types depending on the data source.
//
// Parameters:
//   - val: The interface{} value to convert (should be a numeric type)
//
// Returns:
//   - uint64: The converted value
//   - error: Error if the value cannot be converted to uint64
//
// nolint: gosec
func toUint64(val interface{}) (uint64, error) {
	switch v := val.(type) {
	case int:
		return uint64(v), nil
	case int64:
		return uint64(v), nil
	case uint64:
		return v, nil
	case float64:
		return uint64(v), nil
	case uint32:
		return uint64(v), nil
	case float32:
		return uint64(v), nil
	case nil:
		return 0, nil
	default:
		return 0, errors.NewProcessingError("unknown type for uint64 conversion")
	}
}

// GetUnminedTxIterator implements utxo.Store for Aerospike
func (s *Store) GetUnminedTxIterator(fullScan bool) (utxo.UnminedTxIterator, error) {
	if s.client == nil {
		return nil, errors.NewProcessingError("aerospike client not initialized")
	}

	return newUnminedTxIterator(s, fullScan)
}

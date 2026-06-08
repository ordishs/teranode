package aerospike

import (
	"context"
	"time"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
	"golang.org/x/sync/errgroup"
)

// batchLocked represents a batch operation to set the locked flag on a transaction
type batchLocked struct {
	ctx        context.Context
	txHash     chainhash.Hash
	childIndex uint32 // This will default to 0 which is the master record
	setValue   bool
	errCh      chan error // Channel for completion notification
}

// waitForLockedResult waits for a single locked-batch item to complete, bounded
// so a wedged lockedBatcher can never pin the caller — or a dispatch worker —
// forever.
func (s *Store) waitForLockedResult(ctx context.Context, errCh chan error) error {
	if s.batcherWait <= 0 {
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	timer := time.NewTimer(s.batcherWait)
	defer timer.Stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.NewServiceUnavailableError("set locked did not complete within %s", s.batcherWait)
	}
}

func (s *Store) SetLocked(ctx context.Context, txHashes []chainhash.Hash, setValue bool) error {
	g, ctx := errgroup.WithContext(ctx)

	for _, txHash := range txHashes {
		txHash := txHash

		g.Go(func() error {
			errCh := make(chan error, 1)

			s.lockedBatcher.PutCtx(ctx, &batchLocked{
				ctx:      ctx,
				txHash:   txHash,
				setValue: setValue,
				errCh:    errCh,
			})

			// Now we need to get totalRecords and do all the child records if necessary...

			return s.waitForLockedResult(ctx, errCh)
		})
	}

	return g.Wait()
}

// setLockedBatch sets the locked flag on the given transactions in a batch.
//
// Child/extra records of a multi-record (externalised) tx are written inline
// here rather than re-queued into the lockedBatcher. Re-enqueuing from inside
// the batcher's own callback panics ("send on closed channel") and deadlocks
// during a draining Close — the worker that would service the re-queued item is
// the very one shutting down. Handling children inline (one extra BatchOperate)
// mirrors how the create path writes a tx's extra/external records, and keeps
// the lockedBatcher free of self-referential edges so Close can drain it safely.
func (s *Store) setLockedBatch(batch []*batchLocked) {
	// go-batcher recovers panics in this fn; re-signal every errCh on panic so a
	// crash (e.g. in ParseLuaMapResponse) cannot orphan the waiting submitters.
	defer func() {
		signalBatchPanic(recover(), batch, "setLockedBatch", s.logger, func(it *batchLocked, err error) {
			trySignal(it.errCh, err)
		})
	}()

	var (
		batchUDFPolicy = aerospike.NewBatchUDFPolicy()
		batchRecords   = make([]aerospike.BatchRecordIfc, len(batch))
		handled        = make([]bool, len(batch))
	)

	// Go through each batch item and set the tx to be locked
	for idx, batchItem := range batch {
		// We will do the master record first...
		keySource := uaerospike.CalculateKeySourceInternal(&batchItem.txHash, batchItem.childIndex)

		key, err := aerospike.NewKey(s.namespace, s.setName, keySource)
		if err != nil {
			// Previously this called os.Exit(1), turning a recoverable key error
			// into a process crash. Surface it to the caller and keep the batch
			// index aligned with a NOOP placeholder instead.
			var keyErr error = errors.NewProcessingError("[setLockedBatch] failed to create key", err)
			trySignal(batchItem.errCh, keyErr)

			handled[idx] = true
			batchRecords[idx] = aerospike.NewBatchRead(nil, placeholderKey, nil)

			continue
		}

		batchRecords[idx] = s.teranodeBatchRecord(
			batchUDFPolicy, LuaPackage, key, subOpSetLocked, "setLocked",
			batchItem.setValue,
		)
	}

	if err := s.batchOperate(util.GetAerospikeBatchPolicy(s.settings), batchRecords); err != nil {
		for idx, batchItem := range batch {
			if handled[idx] {
				continue
			}

			s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][%s] BatchOperate failed while setting locked=%t: %s", describeLockedBatchItem(batchItem), lockedBatchSetValue(batchItem), err.Error(), err))
		}

		return
	}

	// Process master results. Items reporting child/extra records defer their
	// errCh signal to the inline child pass below (tracked via childErr, one
	// terminal result per item so each errCh is signalled exactly once).
	childErr := make(map[int]error)

	var (
		childRecords []aerospike.BatchRecordIfc
		childOwner   []int // childRecords[k] belongs to batch[childOwner[k]]
	)

	for idx, batchRecord := range batchRecords {
		if handled[idx] {
			continue
		}

		batchItem := lockedBatchItemAt(batch, idx)
		if batchItem == nil {
			s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][<nil>] missing locked batch item for idx=%d", idx))
			continue
		}

		if batchRecord == nil {
			s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][%s] missing batch record while setting locked=%t; %s", describeLockedBatchItem(batchItem), lockedBatchSetValue(batchItem), describeAerospikeBatchRecord(batchRecord)))
			continue
		}

		batchRec := batchRecord.BatchRec()
		if batchRec == nil {
			s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][%s] missing batch record while setting locked=%t; %s", describeLockedBatchItem(batchItem), lockedBatchSetValue(batchItem), describeAerospikeBatchRecord(batchRecord)))
			continue
		}

		if batchRec.Err != nil {
			s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][%s] batch record failed while setting locked=%t; %s: %s", describeLockedBatchItem(batchItem), lockedBatchSetValue(batchItem), describeAerospikeBatchRecord(batchRecord), batchRec.Err.Error(), batchRec.Err))
			continue
		}

		response := batchRec.Record
		if response == nil || response.Bins == nil || response.Bins[LuaSuccess.String()] == nil {
			s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][%s] missing expected response bin %q while setting locked=%t; %s", describeLockedBatchItem(batchItem), LuaSuccess.String(), lockedBatchSetValue(batchItem), describeAerospikeBatchRecord(batchRecord)))
			continue
		}

		rawResponse := response.Bins[LuaSuccess.String()]
		res, err := s.ParseLuaMapResponse(rawResponse)
		if err != nil {
			s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][%s] failed to parse response bin %q (value %s) while setting locked=%t; %s: %s", describeLockedBatchItem(batchItem), LuaSuccess.String(), describeAerospikeValue(rawResponse), lockedBatchSetValue(batchItem), describeAerospikeBatchRecord(batchRecord), err.Error(), err))
			continue
		}

		if res.Status != LuaStatusOK {
			if res.ErrorCode == LuaErrorCodeTxNotFound {
				s.sendLockedBatchItemError(batch, idx, errors.NewTxNotFoundError("transaction not found: %s", describeLockedBatchItem(batchItem)))
			} else {
				s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][%s] error from setLocked while setting locked=%t: %s", describeLockedBatchItem(batchItem), lockedBatchSetValue(batchItem), res.Message))
			}
			continue
		}

		extraRecords := res.ChildCount
		if extraRecords == 0 {
			s.sendLockedBatchItemError(batch, idx, nil)
			continue
		}

		// Child/extra records are written inline by the pass below rather than
		// re-queued into the lockedBatcher: re-entry from inside the batcher
		// callback deadlocks a draining Close (see the function doc). Defer this
		// item's errCh signal to that pass via childErr (one terminal result each).
		childErr[idx] = nil

		for i := 1; i <= extraRecords; i++ {
			keySource := uaerospike.CalculateKeySourceInternal(&batch[idx].txHash, uint32(i)) // nolint:gosec

			key, err := aerospike.NewKey(s.namespace, s.setName, keySource)
			if err != nil {
				childErr[idx] = errors.NewProcessingError("[setLocked][%s] could not create child key for locked flag", describeLockedBatchItem(batchItem), err)
				break
			}

			childRecords = append(childRecords, s.teranodeBatchRecord(
				batchUDFPolicy, LuaPackage, key, subOpSetLocked, "setLocked",
				batch[idx].setValue,
			))
			childOwner = append(childOwner, idx)
		}
	}

	// Write all collected child records inline (no batcher re-entry, so this is
	// safe to run while the batcher is draining on Close). batchOperate shares the
	// same retry/short-circuit handling as the master batch above.
	if len(childRecords) > 0 {
		if err := s.batchOperate(util.GetAerospikeBatchPolicy(s.settings), childRecords); err != nil {
			for idx := range childErr {
				if childErr[idx] == nil {
					childErr[idx] = errors.NewProcessingError("could not batch write locked child records", err)
				}
			}
		} else {
			for k, childRecord := range childRecords {
				idx := childOwner[k]
				if childErr[idx] != nil {
					continue // already errored for this item
				}

				if childRecord.BatchRec().Err != nil {
					childErr[idx] = errors.NewProcessingError("could not write locked child record", childRecord.BatchRec().Err)
					continue
				}

				resp := childRecord.BatchRec().Record
				if resp == nil || resp.Bins == nil || resp.Bins[LuaSuccess.String()] == nil {
					continue
				}

				cres, perr := s.ParseLuaMapResponse(resp.Bins[LuaSuccess.String()])
				if perr != nil {
					childErr[idx] = errors.NewProcessingError("could not parse child response", perr)
				} else if cres.Status != LuaStatusOK {
					childErr[idx] = errors.NewProcessingError("error from setLocked child: %s", cres.Message)
				}
			}
		}
	}

	// Signal each child-bearing item exactly once with its terminal result.
	for idx, e := range childErr {
		trySignal(batch[idx].errCh, e)
	}
}

func lockedBatchItemAt(batch []*batchLocked, idx int) *batchLocked {
	if idx < 0 || idx >= len(batch) {
		return nil
	}
	return batch[idx]
}

func describeLockedBatchItem(batchItem *batchLocked) string {
	if batchItem == nil {
		return "<nil>"
	}
	return batchItem.txHash.String()
}

func lockedBatchSetValue(batchItem *batchLocked) bool {
	if batchItem == nil {
		return false
	}
	return batchItem.setValue
}

func (s *Store) sendLockedBatchItemError(batch []*batchLocked, idx int, err error) {
	batchItem := lockedBatchItemAt(batch, idx)
	if batchItem == nil {
		if s.logger != nil {
			s.logger.Errorf("[setLocked] unable to send batch item result for idx=%d: %v", idx, err)
		}
		return
	}
	if batchItem.errCh == nil {
		if s.logger != nil {
			s.logger.Errorf("[setLocked][%s] unable to send batch item result because errCh is nil: %v", describeLockedBatchItem(batchItem), err)
		}
		return
	}

	// trySignal (not a blocking send): errCh is buffered-1 and may already hold a
	// queued result, so a blocking send here could wedge the dispatch worker.
	trySignal(batchItem.errCh, err)
}

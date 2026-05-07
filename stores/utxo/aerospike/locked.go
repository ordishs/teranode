package aerospike

import (
	"context"
	"fmt"
	"os"

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

			return <-errCh
		})
	}

	return g.Wait()
}

// setLockedBatch sets the locked flag on the given transactions in a batch
func (s *Store) setLockedBatch(batch []*batchLocked) {
	var (
		batchUDFPolicy = aerospike.NewBatchUDFPolicy()
		batchRecords   = make([]aerospike.BatchRecordIfc, 0, len(batch))
	)

	// Go through each batch item and set the tx to be locked
	for _, batchItem := range batch {
		// We will do the master record first...
		keySource := uaerospike.CalculateKeySourceInternal(&batchItem.txHash, batchItem.childIndex)

		key, err := aerospike.NewKey(s.namespace, s.setName, keySource)
		if err != nil {
			fmt.Printf("Failed to create key: %s\n", err)
			os.Exit(1)
		}

		// Now we need to get totalRecords and do all the child records if necessary...

		batchRecords = append(batchRecords, s.teranodeBatchRecord(
			batchUDFPolicy, LuaPackage, key, subOpSetLocked, "setLocked",
			batchItem.setValue,
		))
	}

	if err := s.client.BatchOperate(util.GetAerospikeBatchPolicy(s.settings), batchRecords); err != nil {
		for idx, batchItem := range batch {
			s.sendLockedBatchItemError(batch, idx, errors.NewProcessingError("[setLocked][%s] BatchOperate failed while setting locked=%t: %s", describeLockedBatchItem(batchItem), lockedBatchSetValue(batchItem), err.Error(), err))
		}

		return
	}

	// Now we need to get totalRecords and do all the child records if necessary...
	for idx, batchRecord := range batchRecords {
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

		// We need to do the child records...
		g, _ := errgroup.WithContext(batchItem.ctx)

		for i := 1; i <= extraRecords; i++ {
			i := i

			g.Go(func() error {
				errCh := make(chan error, 1)

				s.lockedBatcher.PutCtx(batchItem.ctx, &batchLocked{
					txHash:     batchItem.txHash,
					childIndex: uint32(i), // nolint:gosec
					setValue:   batchItem.setValue,
					errCh:      errCh,
				})

				return <-errCh
			})
		}

		s.sendLockedBatchItemError(batch, idx, g.Wait())
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
	batchItem.errCh <- err
}

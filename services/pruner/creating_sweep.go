package pruner

import (
	"context"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
)

// creatingSweepMaxPerPass bounds how many stale creating txs a single sweep pass
// rolls forward. It caps both the store-side scan and the serial roll-forward loop so
// neither grows with the unmined backlog after a crash burst; any remainder is swept on
// the next block.
const creatingSweepMaxPerPass = 1000

// sweepCreatingTxs rolls forward transactions abandoned in the create-first
// "creating" state: it re-runs their input spends from the stored tx bytes and
// finalizes them. Spends are idempotent for the same spender, so re-spending
// inputs an earlier attempt already spent is safe. Double-spent txs land in the
// conflicting terminal state (marked recursively, then finalized: exists,
// conflicting, unspendable, DAH-pruned). Unrecoverable ones (e.g. parent evicted)
// are left creating — retried next block, with prunable-unmined eviction as the
// backstop. The sweep does NOT notify block assembly; the tx reaches mining via
// subtree/mempool re-encounter, acceptable for a rare crash-recovery path.
func (s *Server) sweepCreatingTxs(ctx context.Context, blockHeight uint32) {
	minAge := s.settings.Pruner.CreatingTxSweepMinAgeBlocks
	if minAge == 0 || blockHeight <= minAge {
		return
	}

	hashes, err := s.utxoStore.QueryStaleCreatingTxs(ctx, blockHeight-minAge, creatingSweepMaxPerPass)
	if err != nil {
		s.logger.Errorf("[pruner][creating-sweep][%d] query failed: %v", blockHeight, err)
		return
	}

	if len(hashes) == 0 {
		return
	}

	if len(hashes) == creatingSweepMaxPerPass {
		s.logger.Infof("[pruner][creating-sweep][%d] hit per-pass cap of %d; remaining stale creating txs will be swept next block", blockHeight, creatingSweepMaxPerPass)
	}

	s.logger.Infof("[pruner][creating-sweep][%d] rolling forward %d stale creating txs", blockHeight, len(hashes))

	for _, hash := range hashes {
		hash := hash

		if ctx.Err() != nil {
			s.logger.Infof("[pruner][creating-sweep][%d] context cancelled, aborting sweep: %v", blockHeight, ctx.Err())
			return
		}

		md, err := s.utxoStore.Get(ctx, &hash, fields.Tx, fields.Creating)
		if err != nil {
			s.logger.Warnf("[pruner][creating-sweep][%d] get %s failed: %v", blockHeight, hash, err)
			continue
		}

		if !md.Creating || md.Tx == nil {
			// Finalized (or unreadable) between the query and this get.
			continue
		}

		// Re-spend the inputs only (the record already exists in the creating state).
		// #1326 removed Store.Spend; the spend-only phase of SpendAndCreate is the
		// equivalent. Idempotent for the same spender, which makes roll-forward safe.
		_, spends, spendErr := s.utxoStore.SpendAndCreate(ctx, md.Tx, blockHeight, utxo.WithSpendOnly())
		if spendErr != nil {
			if errors.Is(spendErr, errors.ErrSpent) || errors.Is(spendErr, errors.ErrTxConflicting) {
				if _, _, setErr := utxo.MarkConflictingRecursively(ctx, s.utxoStore, []chainhash.Hash{hash}); setErr != nil {
					s.logger.Errorf("[pruner][creating-sweep][%d] mark conflicting %s failed: %v", blockHeight, hash, setErr)
					continue
				}

				if finErr := s.utxoStore.FinalizeTransaction(ctx, md.Tx); finErr != nil {
					s.logger.Errorf("[pruner][creating-sweep][%d] finalize conflicting %s failed: %v", blockHeight, hash, finErr)
				}

				continue
			}

			s.logger.Warnf("[pruner][creating-sweep][%d] spend %s failed, will retry next block: %v", blockHeight, hash, spendErr)

			continue
		}

		// Defence-in-depth: only finalize when every input was actually spent this pass.
		// A nil top-level error alone is not proof — guard against a swallowed per-input
		// error (e.g. an "already blessed" fallback) leaving an input unspent while we
		// make the outputs spendable.
		if !utxo.AllInputsSpent(md.Tx, spends) {
			s.logger.Warnf("[pruner][creating-sweep][%d] spend of %s did not cover all inputs (%d spends for %d inputs); not finalizing", blockHeight, hash, len(spends), len(md.Tx.Inputs))
			continue
		}

		if finErr := s.utxoStore.FinalizeTransaction(ctx, md.Tx); finErr != nil {
			s.logger.Errorf("[pruner][creating-sweep][%d] finalize %s failed: %v", blockHeight, hash, finErr)
			continue
		}

		s.logger.Infof("[pruner][creating-sweep][%d] rolled forward %s", blockHeight, hash)
	}
}

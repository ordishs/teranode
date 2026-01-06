package pruner

import (
	"context"
	"fmt"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/util/retry"
)

// checkBlockAssemblySafeForPruner verifies that block assembly is in "running" state
// and safe to proceed with pruner operations. Returns true if safe, false otherwise.
// This function will retry checking the block assembly state until the configured
// timeout is reached, allowing for temporary state transitions (e.g., brief reorgs).
func (s *Server) checkBlockAssemblySafeForPruner(ctx context.Context, phase string, height uint32) bool {
	// Create a context with timeout based on settings
	timeoutCtx, cancel := context.WithTimeout(ctx, s.settings.Pruner.BlockAssemblyWaitTimeout)
	defer cancel()

	// Use retry logic to wait for Block Assembly to be in "running" state
	_, err := retry.Retry(timeoutCtx, s.logger, func() (bool, error) {
		state, err := s.blockAssemblyClient.GetBlockAssemblyState(ctx)
		if err != nil {
			return false, errors.NewProcessingError("failed to get block assembly state", err)
		}

		if state.BlockAssemblyState != "running" {
			return false, errors.NewProcessingError("block assembly state is %s (not running)", state.BlockAssemblyState)
		}

		// State is "running", success!
		return true, nil
	},
		retry.WithBackoffDurationType(1*time.Second),
		retry.WithBackoffMultiplier(2),
		retry.WithRetryCount(0), // Will be controlled by timeout context
		retry.WithMessage(fmt.Sprintf("[Pruner] Waiting for block assembly to be ready for %s at height %d", phase, height)),
	)

	if err != nil {
		// Timeout or persistent error - log and skip pruning
		s.logger.Warnf("Skipping %s for height %d: block assembly wait timeout or error: %v", phase, height, err)
		prunerSkipped.WithLabelValues("block_assembly_timeout").Inc()
		return false
	}

	// Block Assembly is ready
	return true
}

// prunerProcessor processes pruner requests from the pruner channel.
// It drains the channel to get the latest height (deduplication), then performs
// pruner in two sequential steps:
// 1. Preserve parents of old unmined transactions
// 2. Delete-at-height (DAH) pruner
//
// Safety checks (block assembly state) are performed immediately before each phase
// to prevent race conditions where state could change between queueing and execution.
//
// This goroutine ensures only one pruner operation runs at a time by processing
// from a buffered channel (size 1).
func (s *Server) prunerProcessor(ctx context.Context) {
	s.logger.Infof("Starting pruner processor")

	for {
		select {
		case <-ctx.Done():
			s.logger.Infof("Stopping pruner processor")
			return

		case height := <-s.prunerCh:
			// Deduplicate: drain channel and process latest height only
			// This is important during block catchup when multiple heights may be queued
			latestHeight := height
			drained := false
		drainLoop:
			for {
				select {
				case nextHeight := <-s.prunerCh:
					latestHeight = nextHeight
					drained = true
				default:
					break drainLoop
				}
			}

			if drained {
				s.logger.Debugf("Deduplicating pruner operations, skipping to height %d", latestHeight)
			}

			// Safety check before preserve parents phase
			if !s.checkBlockAssemblySafeForPruner(ctx, "preserve parents", latestHeight) {
				continue
			}

			// Step 1: Preserve parents of old unmined transactions FIRST
			// This ensures parents of old unmined transactions are not deleted by DAH pruner
			// CRITICAL: If this phase fails, we MUST NOT proceed to subsequent phases,
			// as DAH pruner could delete parents that should be preserved.
			s.logger.Infof("Starting pruner for height %d: preserving parents", latestHeight)
			startTime := time.Now()

			if s.utxoStore != nil {
				_, err := utxo.PreserveParentsOfOldUnminedTransactions(
					ctx, s.utxoStore, latestHeight, s.settings, s.logger)
				if err != nil {
					s.logger.Errorf("CRITICAL: Failed to preserve parents at height %d, ABORTING pruner to prevent data loss: %v", latestHeight, err)
					prunerErrors.WithLabelValues("preserve_parents_failed").Inc()
					prunerSkipped.WithLabelValues("preserve_failed").Inc()
					// ABORT: Do not proceed to Phase 2 (DAH pruner) - could cause data loss
					continue
				}
				prunerDuration.WithLabelValues("preserve_parents").Observe(time.Since(startTime).Seconds())
			}

			// Step 2: Call DAH pruner directly (synchronous)
			// DAH pruner deletes transactions marked for deletion at or before the current height
			if s.prunerService != nil {
				s.logger.Infof("Starting pruner for height %d: DAH pruner", latestHeight)
				startTime = time.Now()

				recordsProcessed, err := s.prunerService.Prune(ctx, latestHeight)
				if err != nil {
					s.logger.Errorf("Pruner failed for height %d: %v", latestHeight, err)
					prunerErrors.WithLabelValues("dah_pruner").Inc()
				} else {
					s.logger.Infof("Pruner for height %d completed successfully, pruned %d records", latestHeight, recordsProcessed)
					prunerDuration.WithLabelValues("dah_pruner").Observe(time.Since(startTime).Seconds())
					prunerProcessed.Inc()
				}
			}

			// Update last processed height atomically
			s.lastProcessedHeight.Store(latestHeight)
		}
	}
}

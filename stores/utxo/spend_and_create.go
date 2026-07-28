package utxo

import (
	"context"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// unspendRetryBackoffBase is the base delay for the exponential backoff between
// rollback attempts. A variable so tests can shorten it.
var unspendRetryBackoffBase = time.Second

// prometheusCreateFirstFinalizeFailed counts create-first FinalizeTransaction failures
// that occurred after the spends succeeded (the tx is left in the creating state for
// recovery). A steady non-zero rate means finalize is failing systemically.
var prometheusCreateFirstFinalizeFailed = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "teranode",
	Subsystem: "utxo",
	Name:      "create_first_finalize_failed_total",
	Help:      "Number of create-first FinalizeTransaction failures after successful spends (tx left creating for recovery)",
})

// ParseCreateOptions applies opts to a fresh CreateOptions and validates the
// combination. Every Store implementation of SpendAndCreate should use this so
// option validation stays identical across backends.
//
// Combinations that are ignored rather than rejected: with SpendOnly the
// create-side fields (MinedBlockInfos, TxID, IsCoinbase, Frozen, Conflicting,
// Locked, SkipExtendedInputs) are unused; with CreateOnly the IgnoreFlags are
// unused.
func ParseCreateOptions(opts ...CreateOption) (*CreateOptions, error) {
	options := &CreateOptions{}
	for _, opt := range opts {
		opt(options)
	}

	if options.CreateOnly && options.SpendOnly {
		return nil, errors.NewInvalidArgumentError("SpendAndCreate: WithCreateOnly and WithSpendOnly are mutually exclusive")
	}

	return options, nil
}

// SequentialStore is the subset of a concrete store's methods needed by
// SequentialSpendAndCreate. Out-of-tree backends that want the sequential
// behaviour implement these three methods and delegate.
type SequentialStore interface {
	Create(ctx context.Context, tx *bt.Tx, blockHeight uint32, opts ...CreateOption) (*meta.Data, error)
	Spend(ctx context.Context, tx *bt.Tx, blockHeight uint32, ignoreFlags ...IgnoreFlags) ([]*Spend, error)
	Unspend(ctx context.Context, spends []*Spend, flagAsLocked ...bool) error
	// FinalizeTransaction clears the tentative creating state; used by the create-first path.
	FinalizeTransaction(ctx context.Context, tx *bt.Tx) error
}

// SequentialSpendAndCreate implements the Store.SpendAndCreate contract as the
// original two-call sequence: spend the inputs, create the outputs, and roll the
// spends back (with retries) when the create fails with anything other than
// ErrTxExists. Concrete stores delegate to it until they implement SpendAndCreate
// atomically (Postgres transactions, Aerospike-specific logic).
//
// Returned spends: nil when the create failed and the rollback succeeded (the
// spends are no longer in effect); the live spends when the create failed with
// ErrTxExists or the rollback itself failed.
func SequentialSpendAndCreate(ctx context.Context, logger ulogger.Logger, s SequentialStore,
	tx *bt.Tx, blockHeight uint32, createFirst bool, opts ...CreateOption) (*meta.Data, []*Spend, error) {
	options, err := ParseCreateOptions(opts...)
	if err != nil {
		return nil, nil, err
	}

	// Create-first ordering (create tentative → spend → finalize) applies only to the
	// combined path. Single-phase callers (CreateOnly / SpendOnly, e.g. coinbase, seeding,
	// reorg/conflict helpers) keep the direct behaviour — there is nothing to sequence.
	if createFirst && !options.CreateOnly && !options.SpendOnly {
		return sequentialCreateFirst(ctx, logger, s, tx, blockHeight, options.IgnoreFlags, opts...)
	}

	var spends []*Spend

	if !options.CreateOnly {
		spends, err = s.Spend(ctx, tx, blockHeight, options.IgnoreFlags)
		if err != nil {
			return nil, spends, err
		}

		if options.SpendOnly {
			return nil, spends, nil
		}
	}

	md, err := s.Create(ctx, tx, blockHeight, opts...)
	if err != nil {
		if errors.Is(err, errors.ErrTxExists) {
			// the tx already exists; leave the spends in place for the caller to decide
			return nil, spends, err
		}

		if len(spends) > 0 {
			if rollbackErr := unspendWithRetry(ctx, logger, s, spends); rollbackErr != nil {
				return nil, spends, errors.NewProcessingError("SpendAndCreate: error reversing utxo spends: %v", rollbackErr, err)
			}
		}

		return nil, nil, err
	}

	return md, spends, nil
}

// sequentialCreateFirst persists a tx create-first: the record is created in the
// tentative "creating" state, the inputs are spent, then the flag is cleared. Because
// the record exists (spend-gated) before its inputs are spent, a crash in the
// spend→finalize window leaves an inspectable, recoverable record rather than a
// dangling spender reference (the durable fix for the spend-then-create dangling ref).
//
// Recovery is roll-forward, not rollback:
//   - Create returns ErrTxExists (a prior attempt already created the record): return it
//     unchanged; the caller treats it as a duplicate and the pruner sweeper rolls a
//     still-creating record forward (re-spend + finalize).
//   - Spend fails after the tentative create: the creating record is left in place as the
//     recovery WAL (NOT finalized, NOT deleted); the error + spends are returned so the
//     caller's conflicting handling can act and the sweeper can roll it forward.
//   - FinalizeTransaction fails after successful spends: log-and-continue — all durable
//     state is committed, and retry/sweeper/setMined recovery converge; returning an error
//     would make callers resubmit an accepted tx.
func sequentialCreateFirst(ctx context.Context, logger ulogger.Logger, s SequentialStore,
	tx *bt.Tx, blockHeight uint32, ignoreFlags IgnoreFlags, opts ...CreateOption) (*meta.Data, []*Spend, error) {
	createOpts := append([]CreateOption{WithCreating(true)}, opts...)

	md, err := s.Create(ctx, tx, blockHeight, createOpts...)
	if err != nil {
		// ErrTxExists or any create error: nothing has been spent yet, nothing to roll back.
		return nil, nil, err
	}

	spends, err := s.Spend(ctx, tx, blockHeight, ignoreFlags)
	if err != nil {
		// A definitive double-spend is terminal: resolve it here (mark conflicting +
		// finalize) so a rejected mempool double-spend reaches its final state with no
		// window, instead of leaving an unresolved tentative record for the sweeper to
		// clean up ~CreatingTxSweepMinAgeBlocks later. On anything else, leave the creating
		// record as the recovery WAL. Either way, hand back spends+error so the caller's
		// own conflicting handling and the sweeper can still act.
		resolveTerminalCreatingConflict(ctx, logger, s, tx, err, spends)

		return md, spends, err
	}

	if finErr := s.FinalizeTransaction(ctx, tx); finErr != nil {
		// Log-and-continue: durable state is committed and recovery (retry/sweeper/setMined)
		// converges. Leave md.Creating=true so the returned/published/cached view matches the
		// store (still creating) — otherwise a consumer would treat a mid-flight record as
		// finalized, defeating the cache-miss fallback that the Creating flag exists for.
		logger.Errorf("SpendAndCreate create-first: FinalizeTransaction failed for %s, tx remains creating until recovery: %v", tx.TxIDChainHash().String(), finErr)
		prometheusCreateFirstFinalizeFailed.Inc()

		return md, spends, nil
	}

	md.Creating = false

	return md, spends, nil
}

// AllInputsSpent reports whether spends covered every input of tx with no per-input
// error — the completeness check every create-first roll-forward site must pass before
// finalizing. A nil top-level error is not sufficient proof on its own: a per-input error
// can be swallowed (e.g. by an "already blessed" fallback), which would otherwise let a
// caller finalize a tx whose input was never actually spent.
func AllInputsSpent(tx *bt.Tx, spends []*Spend) bool {
	if len(spends) != len(tx.Inputs) {
		return false
	}

	for _, sp := range spends {
		if sp == nil || sp.Err != nil {
			return false
		}
	}

	return true
}

// CreatingRollForwarder is the store subset needed to roll a creating record forward:
// re-run the spend-only phase and clear the tentative flag.
type CreatingRollForwarder interface {
	SpendAndCreate(ctx context.Context, tx *bt.Tx, blockHeight uint32, opts ...CreateOption) (*meta.Data, []*Spend, error)
	FinalizeTransaction(ctx context.Context, tx *bt.Tx) error
}

// RollForwardCreating completes a create-first transition abandoned in the tentative
// "creating" state: it re-runs the input spends (idempotent for the same spender) and
// finalizes the record. It is FAIL-CLOSED — it returns an error unless every input was
// actually spent and the finalize succeeded, so a caller can never mistake a partial
// roll-forward for a completed one. Shared by the ErrTxExists validator fast-path, the
// setMined pre-flight, and the pruner sweeper so the "is this really spent?" rule has a
// single definition.
func RollForwardCreating(ctx context.Context, s CreatingRollForwarder, tx *bt.Tx, blockHeight uint32) error {
	_, spends, err := s.SpendAndCreate(ctx, tx, blockHeight, WithSpendOnly())
	if err != nil {
		return err
	}

	if !AllInputsSpent(tx, spends) {
		return errors.NewProcessingError("RollForwardCreating: re-spend of %s did not cover all inputs (%d spends for %d inputs)",
			tx.TxIDChainHash().String(), len(spends), len(tx.Inputs))
	}

	if err := s.FinalizeTransaction(ctx, tx); err != nil {
		return err
	}

	return nil
}

// resolveTerminalCreatingConflict marks a still-creating tx conflicting and finalizes it
// when its create-first spend failed with a definitive double-spend outcome (ErrSpent /
// ErrTxConflicting, top-level or per-input), reaching the same terminal state the pruner
// sweeper would — but immediately, with no unresolved-record window. It is best-effort and
// capability-gated: MarkConflictingRecursively needs the full Store surface, so an
// out-of-tree SequentialStore that is not a Store simply defers to the sweeper.
func resolveTerminalCreatingConflict(ctx context.Context, logger ulogger.Logger, s SequentialStore, tx *bt.Tx, spendErr error, spends []*Spend) {
	terminal := errors.Is(spendErr, errors.ErrSpent) || errors.Is(spendErr, errors.ErrTxConflicting)
	for _, sp := range spends {
		if sp != nil && sp.Err != nil && (errors.Is(sp.Err, errors.ErrSpent) || errors.Is(sp.Err, errors.ErrTxConflicting)) {
			terminal = true
		}
	}

	if !terminal {
		return
	}

	st, ok := s.(Store)
	if !ok {
		return // out-of-tree store lacking the full surface; sweeper resolves it later
	}

	if _, _, err := MarkConflictingRecursively(ctx, st, []chainhash.Hash{*tx.TxIDChainHash()}); err != nil {
		logger.Warnf("SpendAndCreate create-first: mark-conflicting of double-spend %s failed, deferring to sweeper: %v", tx.TxIDChainHash().String(), err)
		return
	}

	if err := st.FinalizeTransaction(ctx, tx); err != nil {
		logger.Warnf("SpendAndCreate create-first: finalize of conflicting %s failed, deferring to sweeper: %v", tx.TxIDChainHash().String(), err)
	}
}

// unspendWithRetry reverses spends with up to 3 attempts and exponential backoff,
// ported from the validator's reverseSpends. The backoff aborts early when ctx
// is cancelled.
func unspendWithRetry(ctx context.Context, logger ulogger.Logger, s SequentialStore, spends []*Spend) error {
	for retries := uint(0); retries < 3; retries++ {
		if errReset := s.Unspend(ctx, spends); errReset != nil {
			if retries < 2 {
				backoff := time.Duration(1<<retries) * unspendRetryBackoffBase
				logger.Errorf("error resetting utxos, retrying in %s: %v", backoff.String(), errReset)

				timer := time.NewTimer(backoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return errors.NewProcessingError("error resetting utxos, context cancelled during retry backoff", errors.Join(errReset, ctx.Err()))
				case <-timer.C:
				}
			} else {
				return errors.NewProcessingError("error resetting utxos", errReset)
			}
		} else {
			break
		}
	}

	return nil
}

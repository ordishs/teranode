package utxo

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	txmap "github.com/bsv-blockchain/go-tx-map"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"golang.org/x/sync/errgroup"
)

// ConflictRecovery is one operation's durable checklist. The before-image is
// immutable after publication; only Phase changes. No process-local addresses
// or transaction objects are serialized.
type ConflictRecovery struct {
	Version        int
	Phase          string
	Transactions   []ConflictTransaction
	OriginalSpends []*Spend
	WinningSpends  []*Spend
	Parents        []chainhash.Hash
	// Winners are the transactions the operation promotes. They are never part of Losers.
	Winners []chainhash.Hash
	Losers  []chainhash.Hash
}

type ConflictTransaction struct {
	Hash        chainhash.Hash
	Conflicting bool
}

const (
	ConflictRecoveryPlanning    = "planning"
	ConflictRecoveryMutating    = "mutating"
	ConflictRecoveryRollingBack = "rolling_back"
	ConflictRecoveryFinished    = "finished"
	ConflictRecoveryPrepared    = "prepared"
	ConflictRecoveryApplied     = "applied"
	ConflictRecoveryRolledBack  = "rolled_back"
)

func EncodeConflictRecovery(r *ConflictRecovery) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

func DecodeConflictRecovery(data []byte) (*ConflictRecovery, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var r ConflictRecovery
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errors.NewStorageError("invalid conflict recovery checklist", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *ConflictRecovery) Validate() error {
	if r == nil || r.Version != 1 {
		return errors.NewStorageError("unsupported conflict recovery checklist version")
	}
	switch r.Phase {
	case ConflictRecoveryPlanning, ConflictRecoveryPrepared, ConflictRecoveryMutating, ConflictRecoveryApplied, ConflictRecoveryRolledBack, ConflictRecoveryRollingBack, ConflictRecoveryFinished:
	default:
		return errors.NewStorageError("unknown conflict recovery phase %q", r.Phase)
	}
	for _, list := range [][]*Spend{r.OriginalSpends, r.WinningSpends} {
		for _, sp := range list {
			if sp == nil || sp.TxID == nil || sp.UTXOHash == nil || (sp.SpendingData != nil && sp.SpendingData.TxID == nil) {
				return errors.NewStorageError("invalid spend in conflict recovery checklist")
			}
		}
	}
	return nil
}

// prepareConflictRecovery snapshots the whole cascade before SetConflicting or
// Unspend can erase any edge. Parents already locked by another holder stay out of
// this operation's lock and unlock list. The reads run concurrently, bounded by
// conflictingWalkFanOut; the checklist keeps discovery order.
func prepareConflictRecovery(ctx context.Context, s Store, hashes []chainhash.Hash, winningSpends []*Spend) (*ConflictRecovery, error) {
	r := &ConflictRecovery{Version: 1, Phase: ConflictRecoveryPrepared, WinningSpends: winningSpends}

	metas := make([]*meta.Data, len(hashes))
	if err := forEachConflictRead(ctx, len(hashes), func(ctx context.Context, i int) error {
		m, err := s.Get(ctx, &hashes[i], fields.Tx, fields.Conflicting)
		if err != nil {
			return err
		}

		if m == nil || m.Tx == nil {
			return errors.NewProcessingError("cannot snapshot conflict transaction %s", hashes[i].String())
		}

		metas[i] = m

		return nil
	}); err != nil {
		return nil, err
	}

	type outpoint struct {
		hash  chainhash.Hash
		index uint32
	}

	seen := make(map[outpoint]struct{})

	var parents hashSet

	addSpends := func(spends []*Spend) {
		for _, sp := range spends {
			key := outpoint{*sp.TxID, sp.Vout}
			if _, ok := seen[key]; ok {
				continue
			}

			seen[key] = struct{}{}
			r.OriginalSpends = append(r.OriginalSpends, sp.Clone())
			parents.add(*sp.TxID)
		}
	}

	for i, h := range hashes {
		r.Transactions = append(r.Transactions, ConflictTransaction{Hash: h, Conflicting: metas[i].Conflicting})

		spends, err := GetSpends(metas[i].Tx)
		if err != nil {
			return nil, err
		}

		addSpends(spends)
	}

	// Winning-only inputs also need before-images (including originally unspent inputs).
	addSpends(winningSpends)

	if err := forEachConflictRead(ctx, len(r.OriginalSpends), func(ctx context.Context, i int) error {
		original := r.OriginalSpends[i]

		response, err := s.GetSpend(ctx, original)
		if err != nil {
			return err
		}

		if response == nil || response.Status == int(Status_NOT_FOUND) || response.Status == int(Status_FROZEN) {
			return errors.NewProcessingError("cannot snapshot conflict outpoint %s:%d", original.TxID, original.Vout)
		}

		original.SpendingData = response.SpendingData.Clone()

		return nil
	}); err != nil {
		return nil, err
	}

	locked := make([]bool, len(parents.order))
	if err := forEachConflictRead(ctx, len(parents.order), func(ctx context.Context, i int) error {
		m, err := s.Get(ctx, &parents.order[i], fields.Locked)
		if err != nil {
			return err
		}

		if m == nil {
			return errors.NewProcessingError("conflict parent %s is missing", parents.order[i].String())
		}

		locked[i] = m.Locked

		return nil
	}); err != nil {
		return nil, err
	}

	// A lock held by someone else (a validator, or a lock an older build leaked) is not this
	// operation's to take or release. Leave it out of the lock set instead of failing every attempt.
	for i, h := range parents.order {
		if !locked[i] {
			r.Parents = append(r.Parents, h)
		}
	}

	return r, nil
}

// forEachConflictRead runs read for every index in [0, n), at most conflictingWalkFanOut at a
// time. Each call writes only its own index, so callers keep a deterministic order.
func forEachConflictRead(ctx context.Context, n int, read func(ctx context.Context, i int) error) error {
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(conflictingWalkFanOut)

	for i := 0; i < n; i++ {
		g.Go(func() error {
			return read(gCtx, i)
		})
	}

	return g.Wait()
}

// rollbackConflictRecovery restores recorded owners, never every transaction
// visited by the cascade. Unexpected spenders cause an error, not an overwrite.
func rollbackConflictRecovery(ctx context.Context, s Store, r *ConflictRecovery, height uint32, id chainhash.Hash) error {
	// A saved applied operation can have usable winners. Disable all recorded
	// transactions before removing any winning spend during compensation.
	hashes := make([]chainhash.Hash, len(r.Transactions))
	for i, tx := range r.Transactions {
		hashes[i] = tx.Hash
	}
	if len(hashes) > 0 {
		_, children, err := s.SetConflicting(ctx, hashes, true)
		if err != nil {
			return err
		}
		known := make(map[chainhash.Hash]struct{}, len(hashes))
		for _, h := range hashes {
			known[h] = struct{}{}
		}
		for _, child := range children {
			if _, ok := known[child]; !ok {
				return errors.NewProcessingError("conflict rollback found a descendant outside the recovery checklist: %s", child.String())
			}
		}
	}
	var failures []error
	if err := s.Unspend(ctx, cloneConflictSpends(r.WinningSpends)); err != nil {
		failures = append(failures, err)
	}
	var restore []*Spend
	for _, original := range r.OriginalSpends {
		if original.SpendingData != nil {
			restore = append(restore, original.Clone())
		}
	}
	if err := s.RestoreConflictSpends(ctx, restore, height); err != nil {
		failures = append(failures, err)
	}
	// Verify before making any loser usable again. A failed restoration keeps
	// the cascade conflicting and the checklist pending.
	mismatches := make([]error, len(r.OriginalSpends))
	_ = forEachConflictRead(ctx, len(r.OriginalSpends), func(ctx context.Context, i int) error {
		original := r.OriginalSpends[i]

		owned, err := conflictOwnerMatches(ctx, s, original)
		if err != nil {
			mismatches[i] = err
		} else if !owned {
			mismatches[i] = errors.NewProcessingError("conflict rollback ownership mismatch at %s:%d", original.TxID, original.Vout)
		}

		// Collect every mismatch: one failure must not cancel the other checks.
		return nil
	})

	for _, mismatch := range mismatches {
		if mismatch != nil {
			failures = append(failures, mismatch)
		}
	}
	if len(failures) != 0 {
		return capRollbackFailures(failures)
	}
	for _, value := range []bool{true, false} {
		var hashes []chainhash.Hash
		for _, tx := range r.Transactions {
			if tx.Conflicting == value {
				hashes = append(hashes, tx.Hash)
			}
		}
		if len(hashes) != 0 {
			if _, _, err := s.SetConflicting(ctx, hashes, value); err != nil {
				return err
			}
		}
	}
	return s.SetConflictLocks(ctx, id, r.Parents, false)
}

func cloneConflictSpends(spends []*Spend) []*Spend {
	out := make([]*Spend, len(spends))
	for i, sp := range spends {
		out[i] = sp.Clone()
	}
	return out
}

func verifyConflictSpends(ctx context.Context, s Store, spends []*Spend) error {
	return forEachConflictRead(ctx, len(spends), func(ctx context.Context, i int) error {
		original := spends[i]

		owned, err := conflictOwnerMatches(ctx, s, original)
		if err != nil {
			return err
		}

		if !owned {
			return errors.NewProcessingError("conflict ownership changed at %s:%d", original.TxID, original.Vout)
		}

		return nil
	})
}

// conflictOwnerMatches reports whether the outpoint of original still has the spender the checklist recorded.
func conflictOwnerMatches(ctx context.Context, s Store, original *Spend) (bool, error) {
	got, err := s.GetSpend(ctx, original)
	if err != nil {
		return false, err
	}

	return got != nil && got.Status != int(Status_NOT_FOUND) && bytes.Equal(got.SpendingData.Bytes(), original.SpendingData.Bytes()), nil
}

// RollbackConflictIntent also handles stale forward intents after a reorg.
// Legacy records cannot establish the prior state and remain pending for repair.
func RollbackConflictIntent(ctx context.Context, s Store, intent ConflictIntent, guards ...CurrentChainGuard) error {
	r, err := s.LoadConflictRecovery(ctx, intent.IntentID())
	if err != nil {
		return err
	}
	if r == nil {
		return errors.NewProcessingError("legacy conflict intent %s has no recovery checklist; manual repair required", intent.IntentID().String())
	}
	var protected []*chainProtectedConflictStore
	for _, guard := range guards {
		if p := guard.protectForward(ctx, s, r.Winners, intent.BlockHash); p != nil {
			s = p
			protected = append(protected, p)
		}
	}
	if r.Phase == ConflictRecoveryRolledBack {
		return nil
	}
	if r.Phase == ConflictRecoveryPlanning || r.Phase == ConflictRecoveryPrepared {
		if err := s.SetConflictLocks(ctx, intent.IntentID(), r.Parents, false); err != nil {
			return err
		}
		r.Phase = ConflictRecoveryRolledBack
		return s.SaveConflictRecovery(ctx, intent.IntentID(), r)
	}
	hashes := make([]chainhash.Hash, len(r.Transactions))
	for i, tx := range r.Transactions {
		hashes[i] = tx.Hash
	}
	for _, p := range protected {
		if err := p.checkUnconfirmed(ctx, hashes); err != nil {
			return err
		}
	}
	// Do not compensate under a lock that another operation has taken over.
	if err := s.SetConflictLocks(ctx, intent.IntentID(), r.Parents, true); err != nil {
		return err
	}
	r.Phase = ConflictRecoveryRollingBack
	if err = s.SaveConflictRecovery(ctx, intent.IntentID(), r); err != nil {
		return err
	}
	if err = rollbackConflictRecovery(ctx, s, r, intent.BlockHeight, intent.IntentID()); err != nil {
		return err
	}
	r.Phase = ConflictRecoveryRolledBack
	return s.SaveConflictRecovery(ctx, intent.IntentID(), r)
}

func finishConflictRecovery(ctx context.Context, s Store, intent ConflictIntent, r *ConflictRecovery, deferCompletion bool) (txmap.TxMap, []chainhash.Hash, error) {
	if r.Phase != ConflictRecoveryFinished {
		if err := s.SetConflictLocks(ctx, intent.IntentID(), r.Parents, false); err != nil {
			return nil, nil, err
		}
		r.Phase = ConflictRecoveryFinished
		if err := s.SaveConflictRecovery(ctx, intent.IntentID(), r); err != nil {
			return nil, nil, err
		}
	}
	result := txmap.NewSplitSwissMap(len(r.Losers))
	for _, h := range r.Losers {
		_ = result.Put(h, 1)
	}
	if !deferCompletion {
		if err := CompleteConflictIntent(ctx, s, intent.IntentID()); err != nil {
			return nil, nil, err
		}
	}
	return result, r.Losers, nil
}

// CompleteConflictIntentsForBlock is called only after subtree and queue work
// for the block has succeeded. Prepared or failed operations remain pending.
func CompleteConflictIntentsForBlock(ctx context.Context, s Store, blockHash chainhash.Hash) error {
	intents, err := s.PendingConflictIntents(ctx)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intent.Kind != ConflictIntentForward || intent.BlockHash != blockHash {
			continue
		}
		r, err := s.LoadConflictRecovery(ctx, intent.IntentID())
		if err != nil {
			return err
		}
		if r != nil && r.Phase == ConflictRecoveryFinished {
			if err := CompleteConflictIntent(ctx, s, intent.IntentID()); err != nil {
				return err
			}
		}
	}
	return nil
}

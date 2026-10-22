package utxo

import (
	"context"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// CurrentChainGuard protects transactions confirmed in any current-chain block.
// The triggering WAL block alone cannot establish whether undo is still valid.
// BlockAssembly serializes chain movement; this is not a distributed writer lock.
type CurrentChainGuard struct {
	check func(context.Context, []uint32) (bool, error)
}

func NewCurrentChainGuard(check func(context.Context, []uint32) (bool, error)) CurrentChainGuard {
	return CurrentChainGuard{check: check}
}

func (g CurrentChainGuard) protect(s Store) Store {
	if g.check == nil {
		return s
	}
	return &chainProtectedConflictStore{Store: s, guard: g}
}

func (g CurrentChainGuard) checkTransactions(ctx context.Context, s Store, hashes []chainhash.Hash) error {
	if g.check == nil {
		return nil
	}
	return (&chainProtectedConflictStore{Store: s, guard: g}).checkUnconfirmed(ctx, hashes)
}

type chainProtectedConflictStore struct {
	Store
	guard CurrentChainGuard
}

func (s *chainProtectedConflictStore) checkUnconfirmed(ctx context.Context, hashes []chainhash.Hash) error {
	unresolved := make([]*UnresolvedMetaData, len(hashes))
	for i, h := range hashes {
		unresolved[i] = &UnresolvedMetaData{Hash: h, Idx: i}
	}
	if len(unresolved) == 0 {
		return nil
	}
	if err := s.Store.BatchDecorate(ctx, unresolved, fields.BlockIDs); err != nil {
		return err
	}
	ids := make(map[uint32]struct{})
	for _, item := range unresolved {
		if item.Err != nil {
			return errors.NewProcessingError("cannot authorize conflict undo for %s", item.Hash.String(), item.Err)
		}
		if item.Data == nil {
			return errors.NewProcessingError("cannot authorize conflict undo: transaction %s is missing", item.Hash.String())
		}
		for _, id := range item.Data.BlockIDs {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	blockIDs := make([]uint32, 0, len(ids))
	for id := range ids {
		blockIDs = append(blockIDs, id)
	}
	confirmed, err := s.guard.check(ctx, blockIDs)
	if err != nil {
		return errors.NewProcessingError("cannot authorize conflict undo: current-chain lookup failed", err)
	}
	if confirmed {
		return errors.NewProcessingError("refusing conflict undo: transaction is confirmed on the current chain")
	}
	return nil
}

func (s *chainProtectedConflictStore) SetConflicting(ctx context.Context, hashes []chainhash.Hash, value bool) ([]*Spend, []chainhash.Hash, error) {
	if value {
		if err := s.checkUnconfirmed(ctx, hashes); err != nil {
			return nil, nil, err
		}
	}
	return s.Store.SetConflicting(ctx, hashes, value)
}

func (s *chainProtectedConflictStore) Unspend(ctx context.Context, spends []*Spend, locked ...bool) error {
	var owners hashSet
	for _, sp := range spends {
		if sp != nil && sp.SpendingData != nil && sp.SpendingData.TxID != nil {
			owners.add(*sp.SpendingData.TxID)
		}
	}
	if err := s.checkUnconfirmed(ctx, owners.order); err != nil {
		return err
	}
	return s.Store.Unspend(ctx, spends, locked...)
}

var prometheusConflictWALCompletionFailures = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "teranode", Subsystem: "utxo", Name: "conflict_wal_completion_failures_total",
	Help: "Conflict WAL completion writes that failed; the operation remains pending",
})

// CompleteConflictIntent counts and returns cleanup failures. Callers must log
// or propagate the error; successful state writes do not imply WAL deletion.
func CompleteConflictIntent(ctx context.Context, s Store, id chainhash.Hash) error {
	if err := s.CompleteConflictIntent(ctx, id); err != nil {
		prometheusConflictWALCompletionFailures.Inc()
		return errors.NewStorageError("failed to complete conflict WAL intent %s", id.String(), err)
	}
	return nil
}

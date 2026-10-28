package sql

import (
	"context"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/tests"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

func TestConflictRecoveryDescendants(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryDescendants(t, store)
}

func TestConflictRecoveryForwardOwnBlock(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryForwardOwnBlock(t, store)
}

func TestConflictRecoveryForeignLock(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryForeignLock(t, store)
}

func TestConflictRecoveryFailures(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryFailures(t, store)
}

func TestConflictRecoveryLockOwnership(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()
	parent := newExtendedTxWithOutputs(t, 1)
	_, _, err := store.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	parents := []chainhash.Hash{*parent.TxIDChainHash()}
	owner := chainhash.HashH([]byte("recovery-operation"))
	other := chainhash.HashH([]byte("other-operation"))
	require.NoError(t, store.SetConflictLocks(ctx, owner, parents, true))
	require.Error(t, store.SetConflictLocks(ctx, other, parents, false))
	require.NoError(t, store.SetConflictLocks(ctx, owner, parents, true))
	// A normal lock writer replaces the operation's ownership atomically.
	require.NoError(t, store.SetLocked(ctx, parents, true))
	require.Error(t, store.SetConflictLocks(ctx, owner, parents, false))
	m, err := store.Get(ctx, parent.TxIDChainHash(), fields.Locked)
	require.NoError(t, err)
	require.True(t, m.Locked)
}

func TestConflictRecoveryPersistence(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryPersistence(t, store)
}

func TestConflictRecoveryReopen(t *testing.T) {
	ctx := t.Context()
	settings := test.CreateBaseTestSettings(t)
	settings.BatcherDrainMode = true
	location := &url.URL{Scheme: "sqlite", Path: filepath.Join(t.TempDir(), "recovery.db")}
	store, err := New(ctx, ulogger.TestLogger{}, settings, location)
	require.NoError(t, err)
	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHash: chainhash.HashH([]byte("reopen")), Recovery: &utxo.ConflictRecovery{Version: 1, Phase: utxo.ConflictRecoveryPlanning}}
	require.NoError(t, store.BeginConflictIntent(ctx, intent))
	recovery := &utxo.ConflictRecovery{Version: 1, Phase: utxo.ConflictRecoveryMutating, Transactions: []utxo.ConflictTransaction{{Hash: intent.BlockHash, Conflicting: true}}}
	require.NoError(t, store.SaveConflictRecovery(ctx, intent.IntentID(), recovery))
	require.NoError(t, store.Close(ctx))
	reopened, err := New(ctx, ulogger.TestLogger{}, settings, location)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close(context.Background())) })
	loaded, err := reopened.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, recovery, loaded)
	require.NoError(t, reopened.BeginConflictIntent(ctx, intent))
	loaded, err = reopened.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, recovery, loaded)
}

func TestConflictRecoveryUnexpectedOwner(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryUnexpectedOwner(t, store)
}

func TestConflictRecoveryRelatedWinners(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryRelatedWinners(t, store)
}

func TestConflictRecoveryConfirmedWinner(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryConfirmedWinner(t, store)
}

func TestConflictRecoveryWideCascade(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryWideCascade(t, store)
}

// cancelAfterChecks reports no error for the first n Err calls and context.Canceled after that.
type cancelAfterChecks struct {
	context.Context
	n      int32
	checks atomic.Int32
}

func (c *cancelAfterChecks) Err() error {
	if c.checks.Add(1) > c.n {
		return context.Canceled
	}

	return nil
}

// A timed-out rollback must not send further restore writes. The write that already ran stays.
func TestRestoreConflictSpendsStopsOnContext(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()

	parent := newExtendedTxWithOutputs(t, 3)
	_, _, err := store.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)

	spender := newExtendedSpendingTx(t, parent, 0)
	for vout := uint32(1); vout < 3; vout++ {
		input := &bt.Input{
			PreviousTxOutIndex: vout,
			SequenceNumber:     0xFFFFFFFF,
			UnlockingScript:    &bscript.Script{},
			PreviousTxSatoshis: parent.Outputs[vout].Satoshis,
			PreviousTxScript:   parent.Outputs[vout].LockingScript,
		}
		require.NoError(t, input.PreviousTxIDAdd(parent.TxIDChainHash()))
		spender.Inputs = append(spender.Inputs, input)
	}

	spends, err := utxo.GetSpends(spender)
	require.NoError(t, err)
	require.Len(t, spends, 3)

	requireSpentBy := func(sp *utxo.Spend, spent bool) {
		t.Helper()

		res, err := store.GetSpend(ctx, sp)
		require.NoError(t, err)

		if !spent {
			require.Nil(t, res.SpendingData, "output %d must stay unspent", sp.Vout)
			return
		}

		require.NotNil(t, res.SpendingData, "output %d must be restored", sp.Vout)
		require.Equal(t, spender.TxIDChainHash(), res.SpendingData.TxID)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	err = store.RestoreConflictSpends(cancelled, spends, 2)
	require.ErrorIs(t, err, context.Canceled)

	for _, sp := range spends {
		requireSpentBy(sp, false)
	}

	err = store.RestoreConflictSpends(&cancelAfterChecks{Context: ctx, n: 1}, spends, 2)
	require.ErrorIs(t, err, context.Canceled)

	requireSpentBy(spends[0], true)
	requireSpentBy(spends[1], false)
	requireSpentBy(spends[2], false)

	require.NoError(t, store.RestoreConflictSpends(ctx, spends, 2))

	for _, sp := range spends {
		requireSpentBy(sp, true)
	}
}

func TestConflictRecoveryLateLoserChild(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryLateLoserChild(t, store)
}

func TestConflictRecoveryLateFreeInputSpender(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryLateFreeInputSpender(t, store)
}

func TestConflictRecoveryOutsiderOwnedInput(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryOutsiderOwnedInput(t, store)
}

func TestConflictRecoveryEarlyFailure(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryEarlyFailure(t, store)
}

func TestConflictRecoveryRefusedAfterRollback(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryRefusedAfterRollback(t, store)
}

func TestConflictRecoveryHealStaleForward(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryHealStaleForward(t, store)
}

func TestConflictRecoverySetConflictingChildren(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoverySetConflictingChildren(t, store, false)
}

func TestConflictRecoveryReverseCompletionFailure(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryReverseCompletionFailure(t, store)
}

func TestConflictRecoveryWinnerWithConflictingChild(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryWinnerWithConflictingChild(t, store)
}

func TestConflictRecoveryLateWinnerChild(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryLateWinnerChild(t, store)
}

func TestConflictRecoveryReverseThenReadd(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryReverseThenReadd(t, store)
}

// lockAfterPrepare wraps the store. Once the checklist is prepared, another holder locks the parent,
// so the operation cannot take its locks.
type lockAfterPrepare struct {
	*Store
	parent chainhash.Hash
}

func (s *lockAfterPrepare) SaveConflictRecovery(ctx context.Context, id chainhash.Hash, r *utxo.ConflictRecovery) error {
	if err := s.Store.SaveConflictRecovery(ctx, id, r); err != nil {
		return err
	}
	if r.Phase == utxo.ConflictRecoveryPrepared {
		return s.Store.SetLocked(ctx, []chainhash.Hash{s.parent}, true)
	}
	return nil
}

// A parent locked by another holder after preparation stops the operation before it mutates
// anything. The rollback must not try to release that lock, which the operation never took.
func TestConflictRecoveryPreparedForeignLock(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()

	parent := newExtendedTxWithOutputs(t, 1)
	_, _, err := store.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := newExtendedSpendingTx(t, parent, 0)
	_, _, err = store.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := newExtendedSpendingTx(t, parent, 0)
	winner.Inputs[0].SequenceNumber = 1
	_, _, err = store.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)

	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	wrapper := &lockAfterPrepare{Store: store, parent: *parent.TxIDChainHash()}
	_, _, err = utxo.ProcessConflicting(ctx, wrapper, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "MANUAL INTERVENTION", "nothing was mutated, so the rollback must succeed")

	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, TxHashes: winners}
	r, err := store.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, utxo.ConflictRecoveryRolledBack, r.Phase)

	m, err := store.Get(ctx, parent.TxIDChainHash(), fields.Locked)
	require.NoError(t, err)
	require.True(t, m.Locked, "the other holder keeps its lock")

	m, err = store.Get(ctx, winner.TxIDChainHash(), fields.Conflicting)
	require.NoError(t, err)
	require.True(t, m.Conflicting)
}

func TestConflictRecoveryLegacyIntent(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryLegacyIntent(t, store)
}

func TestConflictRecoveryCancelledVerify(t *testing.T) {
	store, _ := newTestStore(t)
	tests.ConflictRecoveryCancelledVerify(t, store)
}

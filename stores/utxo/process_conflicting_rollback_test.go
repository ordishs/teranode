package utxo

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestProcessConflictingRollback_Step3Failure verifies that when the winning-tx Spend
// (step 3) fails after step 1 (mark losing tx conflicting) and step 2 (unspend parents +
// lock them) have committed, the deferred rollback restores the original state:
//   - step-3 partial spends are unspent (we accumulate spends per-tx; the failing tx had no
//     successful per-input spend in this fixture, so step 3 contributes nothing to undo)
//   - losing tx is re-spent against the parent so the parent's spending_data is restored
//   - the losing tx is no longer marked conflicting
//   - the parents are no longer locked
func TestProcessConflictingRollback_Step3Failure(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	conflictingTxHash := createTestHash("rollback-step3-winning-tx")
	conflictingTxHashes := []chainhash.Hash{conflictingTxHash}
	losingTxHash := createTestHash("rollback-step3-losing-tx")

	winningTx := createTestTransaction()
	losingTx := createTestTransaction()

	// step 0: fetch winning tx + check it is conflicting + get counter conflicting
	mockStore.On("Get", mock.Anything, &conflictingTxHash, mock.Anything).Return(&meta.Data{
		Tx:          winningTx,
		Conflicting: true,
	}, nil).Once()

	mockStore.On("GetCounterConflicting", mock.Anything, conflictingTxHash).
		Return([]chainhash.Hash{losingTxHash}, nil).Once()

	// step 1: MarkConflictingRecursively → SetConflicting(losing, true)
	affectedSpends := []*Spend{{TxID: &losingTxHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{losingTxHash}, true).
		Return(affectedSpends, []chainhash.Hash{}, nil).Once()

	// step 2: Unspend(affectedSpends, true) commits successfully (locks parents)
	mockStore.On("Unspend", mock.Anything, affectedSpends, []bool{true}).Return(nil).Once()

	// step 3: Spend(winningTx) FAILS — no per-input partial successes
	mockStore.On("Spend", mock.Anything, winningTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, errors.NewTxInvalidError("spend failed")).Once()

	// rollback expectations (reverse order):
	// 3a. step-3 had no successful spends → no Unspend call for step3SuccessfulSpends
	// 3b. re-spend losing tx (need to fetch it first)
	mockStore.On("Get", mock.Anything, &losingTxHash, mock.Anything).Return(&meta.Data{
		Tx: losingTx,
	}, nil).Once()
	mockStore.On("Spend", mock.Anything, losingTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, nil).Once()
	// 3c. SetConflicting(allMarkedHashes, false) — undoes step 1
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{losingTxHash}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()
	// 3d. SetLocked(parents, false) — undoes step 2's lock
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{losingTxHash}, false).
		Return(nil).Once()

	result, _, err := ProcessConflicting(ctx, mockStore, 1, conflictingTxHashes, map[chainhash.Hash]bool{})

	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "spend failed")
	mockStore.AssertExpectations(t)
}

// TestProcessConflictingRollback_RollbackAlsoFails verifies that when the rollback itself
// fails (here: the SetConflicting(false) sub-step fails), the returned error mentions both
// the original failure and the rollback failure, and includes the "MANUAL INTERVENTION
// REQUIRED" tag so an operator notices.
func TestProcessConflictingRollback_RollbackAlsoFails(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	conflictingTxHash := createTestHash("rollback-fail-winning-tx")
	conflictingTxHashes := []chainhash.Hash{conflictingTxHash}
	losingTxHash := createTestHash("rollback-fail-losing-tx")

	winningTx := createTestTransaction()
	losingTx := createTestTransaction()

	mockStore.On("Get", mock.Anything, &conflictingTxHash, mock.Anything).Return(&meta.Data{
		Tx:          winningTx,
		Conflicting: true,
	}, nil).Once()

	mockStore.On("GetCounterConflicting", mock.Anything, conflictingTxHash).
		Return([]chainhash.Hash{losingTxHash}, nil).Once()

	affectedSpends := []*Spend{{TxID: &losingTxHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{losingTxHash}, true).
		Return(affectedSpends, []chainhash.Hash{}, nil).Once()

	mockStore.On("Unspend", mock.Anything, affectedSpends, []bool{true}).Return(nil).Once()

	// step 3 fails
	mockStore.On("Spend", mock.Anything, winningTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, errors.NewTxInvalidError("spend failed")).Once()

	// rollback path:
	// re-fetch losing tx
	mockStore.On("Get", mock.Anything, &losingTxHash, mock.Anything).Return(&meta.Data{
		Tx: losingTx,
	}, nil).Once()
	// re-spend losing tx (rollback step 2) succeeds
	mockStore.On("Spend", mock.Anything, losingTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, nil).Once()
	// SetConflicting(false) FAILS — rollback failure
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{losingTxHash}, false).
		Return([]*Spend{}, []chainhash.Hash{}, errors.NewProcessingError("set conflicting false failed")).Once()
	// SetLocked(false) still attempted (best-effort) and succeeds
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{losingTxHash}, false).
		Return(nil).Once()

	result, _, err := ProcessConflicting(ctx, mockStore, 1, conflictingTxHashes, map[chainhash.Hash]bool{})

	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "MANUAL INTERVENTION REQUIRED")
	require.Contains(t, err.Error(), "spend failed")
	require.Contains(t, err.Error(), "set conflicting false failed")
	mockStore.AssertExpectations(t)
}

// TestProcessConflictingRollback_Step5RetrySucceeds verifies the bounded retry on step-5
// SetLocked(false) failure: when SetLocked fails the first two times and succeeds on the
// third attempt, ProcessConflicting reports success without rolling back the rest of the
// commit.
func TestProcessConflictingRollback_Step5RetrySucceeds(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	conflictingTxHash := createTestHash("rollback-retry-winning-tx")
	conflictingTxHashes := []chainhash.Hash{conflictingTxHash}
	losingTxHash := createTestHash("rollback-retry-losing-tx")

	winningTx := createTestTransaction()

	mockStore.On("Get", mock.Anything, &conflictingTxHash, mock.Anything).Return(&meta.Data{
		Tx:          winningTx,
		Conflicting: true,
	}, nil).Once()

	mockStore.On("GetCounterConflicting", mock.Anything, conflictingTxHash).
		Return([]chainhash.Hash{losingTxHash}, nil).Once()

	affectedSpends := []*Spend{{TxID: &losingTxHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{losingTxHash}, true).
		Return(affectedSpends, []chainhash.Hash{}, nil).Once()

	mockStore.On("Unspend", mock.Anything, affectedSpends, []bool{true}).Return(nil).Once()

	mockStore.On("Spend", mock.Anything, winningTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, nil).Once()

	mockStore.On("SetConflicting", mock.Anything, conflictingTxHashes, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	// step 5: SetLocked(false) fails twice, succeeds the third time
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{losingTxHash}, false).
		Return(errors.NewProcessingError("transient lock failure")).Twice()
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{losingTxHash}, false).
		Return(nil).Once()

	result, _, err := ProcessConflicting(ctx, mockStore, 1, conflictingTxHashes, map[chainhash.Hash]bool{})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Exists(losingTxHash))
	mockStore.AssertExpectations(t)
}

// TestProcessConflictingRollback_Step5RetryExhausted verifies that when all SetLocked(false)
// retries fail, the returned error surfaces the failure but the function does NOT roll back
// the rest of the (correct) commit — only parents are still locked, which is recoverable
// by manual SetLocked.
func TestProcessConflictingRollback_Step5RetryExhausted(t *testing.T) {
	ctx := context.Background()
	mockStore := &MockUtxostore{}

	conflictingTxHash := createTestHash("rollback-retry-exhausted-winning-tx")
	conflictingTxHashes := []chainhash.Hash{conflictingTxHash}
	losingTxHash := createTestHash("rollback-retry-exhausted-losing-tx")

	winningTx := createTestTransaction()

	mockStore.On("Get", mock.Anything, &conflictingTxHash, mock.Anything).Return(&meta.Data{
		Tx:          winningTx,
		Conflicting: true,
	}, nil).Once()

	mockStore.On("GetCounterConflicting", mock.Anything, conflictingTxHash).
		Return([]chainhash.Hash{losingTxHash}, nil).Once()

	affectedSpends := []*Spend{{TxID: &losingTxHash, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{losingTxHash}, true).
		Return(affectedSpends, []chainhash.Hash{}, nil).Once()

	mockStore.On("Unspend", mock.Anything, affectedSpends, []bool{true}).Return(nil).Once()

	mockStore.On("Spend", mock.Anything, winningTx, mock.Anything, mock.Anything).
		Return([]*Spend{}, nil).Once()

	mockStore.On("SetConflicting", mock.Anything, conflictingTxHashes, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	// step 5 fails on every attempt — 3 attempts total
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{losingTxHash}, false).
		Return(errors.NewProcessingError("persistent lock failure")).Times(3)

	result, _, err := ProcessConflicting(ctx, mockStore, 1, conflictingTxHashes, map[chainhash.Hash]bool{})

	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "persistent lock failure")
	// step-5 exhausted retry must NOT roll back: no extra Spend / SetConflicting / Unspend.
	mockStore.AssertExpectations(t)
}

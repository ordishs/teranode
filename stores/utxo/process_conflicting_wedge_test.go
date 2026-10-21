package utxo

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestRollback_KeepsPreFlaggedWinnerConflicting covers issue 4991: the BFS in step 1 can reach a
// winner that was already Conflicting=true before the run. The rollback must not clear that flag,
// or every later retry fails the step-0 "tx is not conflicting" guard.
func TestRollback_KeepsPreFlaggedWinnerConflicting(t *testing.T) {
	mockStore := &MockUtxostore{}

	winner := createTestHash("wedge-winner")
	loser := createTestHash("wedge-loser")

	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{loser}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	err := rollbackProcessConflicting(context.Background(), mockStore, []chainhash.Hash{winner},
		[]chainhash.Hash{loser, winner}, nil, nil, 1, false, false,
		map[chainhash.Hash]struct{}{winner: {}})

	require.NoError(t, err)
	mockStore.AssertExpectations(t)
}

// TestRollback_ClearsEveryMarkedHashWhenNonePreFlagged guards the normal path: with no pre-flagged
// winner the whole cascade is still cleared.
func TestRollback_ClearsEveryMarkedHashWhenNonePreFlagged(t *testing.T) {
	mockStore := &MockUtxostore{}

	loser := createTestHash("clear-loser")
	child := createTestHash("clear-child")

	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{loser, child}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	err := rollbackProcessConflicting(context.Background(), mockStore, nil,
		[]chainhash.Hash{loser, child}, nil, nil, 1, false, false, nil)

	require.NoError(t, err)
	mockStore.AssertExpectations(t)
}

// TestRollback_CapsJoinedErrors covers the 1.4 MB error line from issue 4991: 30 failing
// re-spends must report the first 20 and a count of the rest.
func TestRollback_CapsJoinedErrors(t *testing.T) {
	mockStore := &MockUtxostore{}

	hashes := make([]chainhash.Hash, 30)
	for i := range hashes {
		hashes[i] = createTestHash(fmt.Sprintf("cap-loser-%d", i))
	}

	mockStore.On("Get", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.NewProcessingError("store down"))
	mockStore.On("SetConflicting", mock.Anything, hashes, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	err := rollbackProcessConflicting(context.Background(), mockStore, nil, hashes, nil, nil, 1, true, false, nil)

	require.Error(t, err)
	require.Equal(t, 20, strings.Count(err.Error(), "rollback step 2 (fetch tx"))
	require.Contains(t, err.Error(), "10 more rollback failures omitted")
}

// TestProcessConflicting_AlreadyAppliedWinnerIsSkipped covers the live retry of issue 4991: a
// winner whose flag is already false and which is the recorded spender of every input has been
// applied, so the retry must succeed without touching the store, as the WAL replay does.
func TestProcessConflicting_AlreadyAppliedWinnerIsSkipped(t *testing.T) {
	mockStore := &MockUtxostore{}

	winner := createTestHash("applied-winner")
	parent := createTestHash("applied-parent")
	winnerTx := createTestTransactionWithInputs(parent, 0)

	mockStore.On("Get", mock.Anything, &winner, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: false}, nil)
	mockStore.On("Get", mock.Anything, &parent, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spend.NewSpendingData(&winner, 0)}}, nil)
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{parent}, false).Return(nil).Once()

	losers, marked, err := ProcessConflicting(context.Background(), mockStore, 1, chainhash.Hash{},
		[]chainhash.Hash{winner}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)

	require.NoError(t, err)
	mockStore.AssertCalled(t, "SetLocked", mock.Anything, []chainhash.Hash{parent}, false)
	require.NotNil(t, losers)
	require.Equal(t, 0, losers.Length())
	require.Empty(t, marked)
	mockStore.AssertNotCalled(t, "SetConflicting", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertNotCalled(t, "Unspend", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertNotCalled(t, "SpendAndCreate", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestProcessConflicting_NotConflictingAndNotAppliedStillFails keeps the guard: a winner that is
// not flagged and whose input is held by another transaction is genuinely unsafe to process.
func TestProcessConflicting_NotConflictingAndNotAppliedStillFails(t *testing.T) {
	mockStore := &MockUtxostore{}

	winner := createTestHash("unapplied-winner")
	other := createTestHash("unapplied-other-spender")
	parent := createTestHash("unapplied-parent")
	winnerTx := createTestTransactionWithInputs(parent, 0)

	mockStore.On("Get", mock.Anything, &winner, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: false}, nil)
	mockStore.On("Get", mock.Anything, &parent, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spend.NewSpendingData(&other, 0)}}, nil)

	losers, _, err := ProcessConflicting(context.Background(), mockStore, 1, chainhash.Hash{},
		[]chainhash.Hash{winner}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)

	require.Nil(t, losers)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tx is not conflicting")
}

// TestProcessConflicting_AppliedWinnerSkippedOthersProcessed checks a mixed batch: the applied
// winner is left alone and the flagged winner is processed normally.
func TestProcessConflicting_AppliedWinnerSkippedOthersProcessed(t *testing.T) {
	mockStore := &MockUtxostore{}

	applied := createTestHash("mixed-applied")
	pending := createTestHash("mixed-pending")
	loser := createTestHash("mixed-loser")
	parentA := createTestHash("mixed-parent-a")
	parentB := createTestHash("mixed-parent-b")
	appliedTx := createTestTransactionWithInputs(parentA, 0)
	pendingTx := createTestTransactionWithInputs(parentB, 0)

	mockStore.On("Get", mock.Anything, &applied, mock.Anything).
		Return(&meta.Data{Tx: appliedTx, Conflicting: false}, nil)
	mockStore.On("Get", mock.Anything, &parentA, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spend.NewSpendingData(&applied, 0)}}, nil)
	mockStore.On("Get", mock.Anything, &pending, mock.Anything).
		Return(&meta.Data{Tx: pendingTx, Conflicting: true}, nil)
	mockStore.On("GetCounterConflicting", mock.Anything, pending).
		Return([]chainhash.Hash{loser}, nil)

	affected := []*Spend{{TxID: &loser, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{loser}, true).
		Return(affected, []chainhash.Hash{}, nil)
	mockStore.On("Unspend", mock.Anything, affected, mock.Anything).Return(nil)
	mockStore.On("SpendAndCreate", mock.Anything, pendingTx, mock.Anything, mock.Anything).
		Return(nil, []*Spend{}, nil)
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{pending}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil)
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{loser}, false).Return(nil)
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{parentA}, false).Return(nil)

	losers, _, err := ProcessConflicting(context.Background(), mockStore, 1, chainhash.Hash{},
		[]chainhash.Hash{applied, pending}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)

	require.NoError(t, err)
	require.True(t, losers.Exists(loser))
	mockStore.AssertExpectations(t)
}

// TestProcessConflicting_AppliedWinnerReachedByCascadeFails covers related winners: B is a child of
// A and was already applied, but A's counter-conflicting set reaches B. Demoting B while only A is
// restored would corrupt the store, so the run must fail and roll the flags back.
func TestProcessConflicting_AppliedWinnerReachedByCascadeFails(t *testing.T) {
	mockStore := &MockUtxostore{}

	winnerA := createTestHash("related-winner-a")
	winnerB := createTestHash("related-winner-b")
	loser := createTestHash("related-loser")
	parentA := createTestHash("related-parent-a")
	parentB := winnerA
	txA := createTestTransactionWithInputs(parentA, 0)
	txB := createTestTransactionWithInputs(parentB, 0)

	mockStore.On("Get", mock.Anything, &winnerA, mock.Anything).
		Return(&meta.Data{Tx: txA, Conflicting: true,
			SpendingDatas: []*spend.SpendingData{spend.NewSpendingData(&winnerB, 0)}}, nil)
	mockStore.On("Get", mock.Anything, &winnerB, mock.Anything).
		Return(&meta.Data{Tx: txB, Conflicting: false}, nil)
	mockStore.On("GetCounterConflicting", mock.Anything, winnerA).
		Return([]chainhash.Hash{loser, winnerB}, nil)
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{winnerA}, false).Return(nil)

	marked := mock.MatchedBy(func(h []chainhash.Hash) bool {
		return len(h) == 2
	})
	mockStore.On("SetConflicting", mock.Anything, marked, true).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()
	mockStore.On("SetConflicting", mock.Anything, marked, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()

	losers, _, err := ProcessConflicting(context.Background(), mockStore, 1, chainhash.Hash{},
		[]chainhash.Hash{winnerA, winnerB}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)

	require.Nil(t, losers)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already applied")
	mockStore.AssertNotCalled(t, "Unspend", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertExpectations(t)
}

// TestProcessConflicting_AppliedWinnerRetryReconstructsDemotedSet covers a failure after UTXO
// processing but before the subtree updates finish. The retry finds the winner applied, and must
// still hand the caller the demoted transactions so it can finish marking them in the subtrees.
func TestProcessConflicting_AppliedWinnerRetryReconstructsDemotedSet(t *testing.T) {
	mockStore := &MockUtxostore{}

	winner := createTestHash("reconstruct-winner")
	loser := createTestHash("reconstruct-loser")
	loserChild := createTestHash("reconstruct-loser-child")
	unrelated := createTestHash("reconstruct-unrelated-conflicting-child")
	parent := createTestHash("reconstruct-parent")
	winnerTx := createTestTransactionWithInputs(parent, 0)
	loserTx := createTestTransactionWithInputs(parent, 0)
	unrelatedTx := createTestTransactionWithInputs(parent, 1)

	mockStore.On("Get", mock.Anything, &winner, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: false}, nil)
	mockStore.On("Get", mock.Anything, &parent, mock.Anything).
		Return(&meta.Data{
			SpendingDatas:       []*spend.SpendingData{spend.NewSpendingData(&winner, 0)},
			ConflictingChildren: []chainhash.Hash{loser, unrelated},
		}, nil)
	mockStore.On("Get", mock.Anything, &loser, mock.Anything).
		Return(&meta.Data{Tx: loserTx, Conflicting: true,
			SpendingDatas: []*spend.SpendingData{spend.NewSpendingData(&loserChild, 0)}}, nil)
	mockStore.On("Get", mock.Anything, &unrelated, mock.Anything).
		Return(&meta.Data{Tx: unrelatedTx, Conflicting: true}, nil)
	mockStore.On("Get", mock.Anything, &loserChild, mock.Anything).
		Return(&meta.Data{Conflicting: true}, nil)
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{parent}, false).Return(nil).Once()

	losers, marked, err := ProcessConflicting(context.Background(), mockStore, 1, chainhash.Hash{},
		[]chainhash.Hash{winner}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)

	require.NoError(t, err)
	require.Equal(t, 2, losers.Length())
	require.True(t, losers.Exists(loser))
	require.True(t, losers.Exists(loserChild))
	require.False(t, losers.Exists(unrelated))
	require.ElementsMatch(t, []chainhash.Hash{loser, loserChild}, marked)
	mockStore.AssertNotCalled(t, "SetConflicting", mock.Anything, mock.Anything, mock.Anything)
	mockStore.AssertNotCalled(t, "Unspend", mock.Anything, mock.Anything, mock.Anything)
}

// TestProcessConflicting_MixedBatchMergesReconstructedDemotedSet checks that the demoted set
// rebuilt for an applied winner is merged with the live result for a flagged winner.
func TestProcessConflicting_MixedBatchMergesReconstructedDemotedSet(t *testing.T) {
	mockStore := &MockUtxostore{}

	applied := createTestHash("merge-applied")
	pending := createTestHash("merge-pending")
	oldLoser := createTestHash("merge-old-loser")
	newLoser := createTestHash("merge-new-loser")
	parentA := createTestHash("merge-parent-a")
	parentB := createTestHash("merge-parent-b")
	appliedTx := createTestTransactionWithInputs(parentA, 0)
	pendingTx := createTestTransactionWithInputs(parentB, 0)
	oldLoserTx := createTestTransactionWithInputs(parentA, 0)

	mockStore.On("Get", mock.Anything, &applied, mock.Anything).
		Return(&meta.Data{Tx: appliedTx, Conflicting: false}, nil)
	mockStore.On("Get", mock.Anything, &parentA, mock.Anything).
		Return(&meta.Data{
			SpendingDatas:       []*spend.SpendingData{spend.NewSpendingData(&applied, 0)},
			ConflictingChildren: []chainhash.Hash{oldLoser},
		}, nil)
	mockStore.On("Get", mock.Anything, &oldLoser, mock.Anything).
		Return(&meta.Data{Tx: oldLoserTx, Conflicting: true}, nil)
	mockStore.On("Get", mock.Anything, &pending, mock.Anything).
		Return(&meta.Data{Tx: pendingTx, Conflicting: true}, nil)
	mockStore.On("GetCounterConflicting", mock.Anything, pending).
		Return([]chainhash.Hash{newLoser}, nil)

	affected := []*Spend{{TxID: &newLoser, Vout: 0}}
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{newLoser}, true).
		Return(affected, []chainhash.Hash{}, nil)
	mockStore.On("Unspend", mock.Anything, affected, mock.Anything).Return(nil)
	mockStore.On("SpendAndCreate", mock.Anything, pendingTx, mock.Anything, mock.Anything).
		Return(nil, []*Spend{}, nil)
	mockStore.On("SetConflicting", mock.Anything, []chainhash.Hash{pending}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil)
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{newLoser}, false).Return(nil)
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{parentA}, false).Return(nil)

	losers, marked, err := ProcessConflicting(context.Background(), mockStore, 1, chainhash.Hash{},
		[]chainhash.Hash{applied, pending}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)

	require.NoError(t, err)
	require.Equal(t, 2, losers.Length())
	require.True(t, losers.Exists(oldLoser))
	require.True(t, losers.Exists(newLoser))
	require.ElementsMatch(t, []chainhash.Hash{oldLoser, newLoser}, marked)
	mockStore.AssertExpectations(t)
}

// TestProcessConflicting_Step5FailureIsCompletedByRetry extends the step-5 exhausted case with a
// second invocation. After the first run every spend and flag is correct but the parents are still
// locked. The retry sees an applied winner and must finish the unlock before it reports success,
// otherwise the WAL intent is removed with the parents locked for good.
func TestProcessConflicting_Step5FailureIsCompletedByRetry(t *testing.T) {
	step5RetryDelaysBackup := step5RetryDelays
	step5RetryDelays = []time.Duration{0, 0, 0}

	defer func() { step5RetryDelays = step5RetryDelaysBackup }()

	winner := createTestHash("step5-retry-winner")
	loser := createTestHash("step5-retry-loser")
	parent := createTestHash("step5-retry-parent")
	winnerTx := createTestTransactionWithInputs(parent, 0)
	loserTx := createTestTransactionWithInputs(parent, 0)

	first := &MockUtxostore{}
	first.On("Get", mock.Anything, &winner, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: true}, nil).Once()
	first.On("GetCounterConflicting", mock.Anything, winner).
		Return([]chainhash.Hash{loser}, nil).Once()

	affected := []*Spend{{TxID: &parent, Vout: 0}}
	first.On("SetConflicting", mock.Anything, []chainhash.Hash{loser}, true).
		Return(affected, []chainhash.Hash{}, nil).Once()
	first.On("Unspend", mock.Anything, affected, []bool{true}).Return(nil).Once()
	first.On("SpendAndCreate", mock.Anything, winnerTx, mock.Anything, mock.Anything).
		Return(nil, []*Spend{}, nil).Once()
	first.On("SetConflicting", mock.Anything, []chainhash.Hash{winner}, false).
		Return([]*Spend{}, []chainhash.Hash{}, nil).Once()
	first.On("SetLocked", mock.Anything, []chainhash.Hash{parent}, false).
		Return(errors.NewProcessingError("persistent lock failure")).Times(3)

	_, _, err := ProcessConflicting(context.Background(), first, 1, chainhash.Hash{},
		[]chainhash.Hash{winner}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "persistent lock failure")
	first.AssertExpectations(t)

	second := &MockUtxostore{}
	second.On("Get", mock.Anything, &winner, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: false}, nil)
	second.On("Get", mock.Anything, &parent, mock.Anything).
		Return(&meta.Data{
			SpendingDatas:       []*spend.SpendingData{spend.NewSpendingData(&winner, 0)},
			ConflictingChildren: []chainhash.Hash{loser},
		}, nil)
	second.On("Get", mock.Anything, &loser, mock.Anything).
		Return(&meta.Data{Tx: loserTx, Conflicting: true}, nil)
	second.On("SetLocked", mock.Anything, []chainhash.Hash{parent}, false).Return(nil).Once()

	losers, _, err := ProcessConflicting(context.Background(), second, 1, chainhash.Hash{},
		[]chainhash.Hash{winner}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)

	require.NoError(t, err)
	require.True(t, losers.Exists(loser))
	second.AssertExpectations(t)
}

// TestProcessConflicting_AppliedWinnerRetryFailsWhenUnlockFails keeps the WAL intent: if the
// outstanding unlock still fails, the retry must return the error.
func TestProcessConflicting_AppliedWinnerRetryFailsWhenUnlockFails(t *testing.T) {
	step5RetryDelaysBackup := step5RetryDelays
	step5RetryDelays = []time.Duration{0, 0, 0}

	defer func() { step5RetryDelays = step5RetryDelaysBackup }()

	mockStore := &MockUtxostore{}

	winner := createTestHash("unlock-fail-winner")
	parent := createTestHash("unlock-fail-parent")
	winnerTx := createTestTransactionWithInputs(parent, 0)

	mockStore.On("Get", mock.Anything, &winner, mock.Anything).
		Return(&meta.Data{Tx: winnerTx, Conflicting: false}, nil)
	mockStore.On("Get", mock.Anything, &parent, mock.Anything).
		Return(&meta.Data{SpendingDatas: []*spend.SpendingData{spend.NewSpendingData(&winner, 0)}}, nil)
	mockStore.On("SetLocked", mock.Anything, []chainhash.Hash{parent}, false).
		Return(errors.NewProcessingError("still locked")).Times(3)

	losers, _, err := ProcessConflicting(context.Background(), mockStore, 1, chainhash.Hash{},
		[]chainhash.Hash{winner}, map[chainhash.Hash]struct{}{}, NoAncestryGuard)

	require.Nil(t, losers)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still locked")
	mockStore.AssertExpectations(t)
}

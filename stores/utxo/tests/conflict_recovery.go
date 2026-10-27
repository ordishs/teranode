package tests

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	utxo "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/stretchr/testify/require"
)

type failConflictUnlock struct {
	utxo.Store
	fail bool
}

func (s *failConflictUnlock) SetConflictLocks(ctx context.Context, id chainhash.Hash, hashes []chainhash.Hash, value bool) error {
	if !value && s.fail {
		return errors.NewStorageError("injected final unlock failure")
	}
	return s.Store.SetConflictLocks(ctx, id, hashes, value)
}

// ConflictRecoveryDescendants runs the real write path before retrying. In
// particular, it must not manufacture SpendingDatas that Unspend has removed.
func ConflictRecoveryDescendants(t *testing.T, db utxo.Store) {
	ctx := context.Background()
	parent, other := crashParentTx(0x91), crashParentTx(0x92)
	for _, tx := range []*bt.Tx{parent, other} {
		_, _, err := db.SpendAndCreate(ctx, tx, 1, utxo.WithCreateOnly())
		require.NoError(t, err)
	}
	loser := crashSpendTx(t, parent, 80000)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err := db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	child := crashSpendTx(t, loser, 60000)
	require.NoError(t, child.From(other.TxIDChainHash().String(), 0, other.Outputs[0].LockingScript.String(), other.Outputs[0].Satoshis))
	child.Inputs[1].UnlockingScript = dummyUnlockingScript
	_, _, err = db.SpendAndCreate(ctx, child, 2)
	require.NoError(t, err)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)
	wrapped := &failConflictUnlock{Store: db, fail: true}
	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	_, _, err = utxo.ProcessConflicting(ctx, wrapped, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard)
	require.Error(t, err)
	require.Nil(t, crashSpender(t, ctx, db, loser), "the first run must erase the descendant link")
	wrapped.fail = false
	losers, marked, err := utxo.ProcessConflicting(ctx, wrapped, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard)
	require.NoError(t, err)
	require.True(t, losers.Exists(*child.TxIDChainHash()), "retry must recover the child")
	require.Contains(t, marked, *child.TxIDChainHash())
	require.True(t, losers.Exists(*loser.TxIDChainHash()))
	require.False(t, losers.Exists(*winner.TxIDChainHash()), "a winner must not be returned as a loser")
	require.NotContains(t, marked, *winner.TxIDChainHash(), "a winner must not be returned as marked")
	require.False(t, crashLocked(t, ctx, db, other.TxIDChainHash()))
}

// failConflictPhase wraps a real store. crash=true suppresses compensation to
// model process loss; the next call uses the underlying store directly.
type failConflictPhase struct {
	utxo.Store
	stage string
	crash bool
	fired bool
}

func (s *failConflictPhase) fail(stage string) error {
	if s.stage == stage && !s.fired {
		s.fired = true
		return errors.NewStorageError("injected conflict failure at %s", stage)
	}
	return nil
}
func (s *failConflictPhase) SaveConflictRecovery(ctx context.Context, id chainhash.Hash, r *utxo.ConflictRecovery) error {
	if s.crash && s.fired && r.Phase == utxo.ConflictRecoveryRollingBack {
		return errors.NewStorageError("process stopped before compensation")
	}
	if r.Phase == utxo.ConflictRecoveryApplied {
		if err := s.fail("publish_applied"); err != nil {
			return err
		}
	}
	return s.Store.SaveConflictRecovery(ctx, id, r)
}
func (s *failConflictPhase) SetConflicting(ctx context.Context, hashes []chainhash.Hash, value bool) ([]*utxo.Spend, []chainhash.Hash, error) {
	spends, children, err := s.Store.SetConflicting(ctx, hashes, value)
	if err != nil {
		return spends, children, err
	}
	stage := "clear_winners"
	if value {
		stage = "mark"
	}
	return spends, children, s.fail(stage)
}
func (s *failConflictPhase) Unspend(ctx context.Context, spends []*utxo.Spend, locked ...bool) error {
	if err := s.Store.Unspend(ctx, spends, locked...); err != nil {
		return err
	}
	return s.fail("unspend")
}
func (s *failConflictPhase) RestoreConflictSpends(ctx context.Context, spends []*utxo.Spend, height uint32) error {
	if s.stage == "partial_spend" && !s.fired && len(spends) > 0 {
		if err := s.Store.RestoreConflictSpends(ctx, spends[:1], height); err != nil {
			return err
		}
		return s.fail("partial_spend")
	}
	return s.Store.RestoreConflictSpends(ctx, spends, height)
}

func ConflictRecoveryFailures(t *testing.T, db utxo.Store) {
	ctx := context.Background()
	for index, stage := range []string{"mark", "unspend", "partial_spend", "clear_winners", "publish_applied"} {
		for _, crash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/crash=%t", stage, crash), func(t *testing.T) {
				seed := []byte{0xa0, 0xa2, 0xa4, 0xa6, 0xa8}[index]
				if crash {
					seed++
				}
				parent := crashParentTx(seed)
				_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
				require.NoError(t, err)
				loser := crashSpendTx(t, parent, 80000)
				_, _, err = db.SpendAndCreate(ctx, loser, 2)
				require.NoError(t, err)
				child := crashSpendTx(t, loser, 60000)
				_, _, err = db.SpendAndCreate(ctx, child, 2)
				require.NoError(t, err)
				// This pre-existing flag belongs to an earlier operation and must survive rollback.
				_, _, err = db.SetConflicting(ctx, []chainhash.Hash{*child.TxIDChainHash()}, true)
				require.NoError(t, err)
				winner := crashSpendTx(t, parent, 70000)
				_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
				require.NoError(t, err)
				winners := []chainhash.Hash{*winner.TxIDChainHash()}
				wrapper := &failConflictPhase{Store: db, stage: stage, crash: crash}
				_, _, err = utxo.ProcessConflicting(ctx, wrapper, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard)
				require.Error(t, err)
				require.True(t, wrapper.fired)
				intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, TxHashes: winners}
				if crash {
					// Exercise the public recovery entrypoint separately so original state
					// can be checked before a subsequent forward attempt changes it again.
					_, err = utxo.RollbackConflictIntent(ctx, db, intent)
					require.NoError(t, err)
				}
				require.True(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()), "pre-flagged winner must remain flagged")
				require.True(t, crashConflicting(t, ctx, db, child.TxIDChainHash()), "pre-flagged descendant must remain flagged")
				require.False(t, crashConflicting(t, ctx, db, loser.TxIDChainHash()))
				require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent))
				require.Equal(t, child.TxIDChainHash(), crashSpender(t, ctx, db, loser))
				require.False(t, crashLocked(t, ctx, db, parent.TxIDChainHash()))
				r, err := db.LoadConflictRecovery(ctx, intent.IntentID())
				require.NoError(t, err)
				require.Equal(t, utxo.ConflictRecoveryRolledBack, r.Phase)
				losers, marked, err := utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
				require.NoError(t, err)
				require.True(t, losers.Exists(*child.TxIDChainHash()))
				require.Contains(t, marked, *child.TxIDChainHash())
				require.False(t, losers.Exists(*winner.TxIDChainHash()), "a winner must not be returned as a loser")
				require.NotContains(t, marked, *winner.TxIDChainHash(), "a winner must not be returned as marked")
				require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
				require.True(t, crashConflicting(t, ctx, db, loser.TxIDChainHash()))
				// Simulate a caller failure, then retry using the same saved output set.
				again, markedAgain, err := utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
				require.NoError(t, err)
				require.ElementsMatch(t, losers.Keys(), again.Keys())
				require.ElementsMatch(t, marked, markedAgain)
				require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
			})
		}
	}
}

// ConflictRecoveryPersistence crosses Aerospike's chunk boundary and verifies
// that an idempotent Begin cannot replace an existing checklist.
func ConflictRecoveryPersistence(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHash: chainhash.HashH([]byte("large-checklist")), Recovery: &utxo.ConflictRecovery{Version: 1, Phase: utxo.ConflictRecoveryPlanning}}
	require.NoError(t, db.BeginConflictIntent(ctx, intent))
	r := &utxo.ConflictRecovery{Version: 1, Phase: utxo.ConflictRecoveryPrepared}
	for i := 0; i < 2000; i++ {
		h := chainhash.HashH([]byte(fmt.Sprintf("checklist-%d", i)))
		r.Transactions = append(r.Transactions, utxo.ConflictTransaction{Hash: h, Conflicting: i%2 == 0})
		r.Losers = append(r.Losers, h)
	}
	encoded, err := utxo.EncodeConflictRecovery(r)
	require.NoError(t, err)
	require.Greater(t, len(encoded), 64*1024)
	require.NoError(t, db.SaveConflictRecovery(ctx, intent.IntentID(), r))
	require.NoError(t, db.BeginConflictIntent(ctx, intent))
	actual, err := db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, r, actual)
	r.Phase = utxo.ConflictRecoveryMutating
	require.NoError(t, db.SaveConflictRecovery(ctx, intent.IntentID(), r))
	actual, err = db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, r, actual)
	// Caller acknowledgement cannot delete unfinished work or another block's WAL.
	require.NoError(t, utxo.CompleteConflictIntentsForBlock(ctx, db, intent.BlockHash))
	actual, err = db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, r, actual)
	r.Phase = utxo.ConflictRecoveryFinished
	require.NoError(t, db.SaveConflictRecovery(ctx, intent.IntentID(), r))
	require.NoError(t, utxo.CompleteConflictIntentsForBlock(ctx, db, chainhash.Hash{}))
	actual, err = db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, r, actual)
	require.NoError(t, utxo.CompleteConflictIntentsForBlock(ctx, db, intent.BlockHash))
	require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
	actual, err = db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Nil(t, actual)
}

func ConflictRecoveryUnexpectedOwner(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xe1)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)
	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	_, _, err = utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)
	winningSpends, err := utxo.GetSpends(winner)
	require.NoError(t, err)
	require.NoError(t, db.Unspend(ctx, winningSpends))
	stranger := crashSpendTx(t, parent, 50000)
	_, _, err = db.SpendAndCreate(ctx, stranger, 3)
	require.NoError(t, err)
	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, TxHashes: winners}
	_, err = utxo.RollbackConflictIntent(ctx, db, intent)
	require.Error(t, err)
	require.Equal(t, stranger.TxIDChainHash(), crashSpender(t, ctx, db, parent), "rollback must not overwrite an unexpected owner")
	require.True(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
	require.True(t, crashConflicting(t, ctx, db, loser.TxIDChainHash()))
	r, err := db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, utxo.ConflictRecoveryRollingBack, r.Phase)
	// Restore the expected owner and finish recovery to leave the shared store clean.
	strangers, err := utxo.GetSpends(stranger)
	require.NoError(t, err)
	require.NoError(t, db.Unspend(ctx, strangers))
	_, err = utxo.RollbackConflictIntent(ctx, db, intent)
	require.NoError(t, err)
	require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
}

// Related winners must remain in the complete winner set during the ancestry
// guard. A winning child spending its winning parent is not a losing ancestor.
func ConflictRecoveryRelatedWinners(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xe2)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	child := crashSpendTx(t, winner, 60000)
	for _, tx := range []*bt.Tx{winner, child} {
		_, _, err = db.SpendAndCreate(ctx, tx, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
		require.NoError(t, err)
	}
	_, err = db.SetMinedMulti(ctx, []*chainhash.Hash{winner.TxIDChainHash(), child.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: 99, BlockHeight: 3})
	require.NoError(t, err)
	winners := []chainhash.Hash{*winner.TxIDChainHash(), *child.TxIDChainHash()}
	guard := utxo.NewAncestryGuard(func(context.Context, []uint32, *chainhash.Hash) (bool, error) { return true, nil })
	_, _, err = utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, guard)
	require.NoError(t, err)
	require.False(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
	require.False(t, crashConflicting(t, ctx, db, child.TxIDChainHash()))
	require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.Equal(t, child.TxIDChainHash(), crashSpender(t, ctx, db, winner))
}

// A WAL remains historical evidence even after its winner is mined again.
// Recovery must consult all mined block IDs, not just the triggering block.
func ConflictRecoveryConfirmedWinner(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xe3)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)
	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, TxHashes: []chainhash.Hash{*winner.TxIDChainHash()}}
	_, _, err = utxo.ProcessConflicting(ctx, db, intent.BlockHeight, intent.BlockHash, intent.TxHashes, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)
	for _, id := range []uint32{154, 155} {
		_, err = db.SetMinedMulti(ctx, []*chainhash.Hash{winner.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: id, BlockHeight: id})
		require.NoError(t, err)
	}
	for _, lookupFails := range []bool{false, true} {
		guard := utxo.NewCurrentChainGuard(func(_ context.Context, ids []uint32) (bool, error) {
			require.Contains(t, ids, uint32(155), "must check the later confirmation")
			if lookupFails {
				return false, errors.NewStorageError("injected chain lookup failure")
			}
			return true, nil
		})
		_, err = utxo.RollbackConflictIntent(ctx, db, intent, guard)
		require.Error(t, err)
		_, _, err = utxo.ReverseProcessConflicting(ctx, db, intent.BlockHeight, intent.BlockHash, intent.TxHashes, guard)
		require.Error(t, err)
		require.False(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
		require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
		require.False(t, crashLocked(t, ctx, db, parent.TxIDChainHash()))
		r, err := db.LoadConflictRecovery(ctx, intent.IntentID())
		require.NoError(t, err)
		require.Equal(t, utxo.ConflictRecoveryFinished, r.Phase)
	}
	// If neither mined block remains on the current chain, undo is permitted.
	orphaned := utxo.NewCurrentChainGuard(func(context.Context, []uint32) (bool, error) { return false, nil })
	_, err = utxo.RollbackConflictIntent(ctx, db, intent, orphaned)
	require.NoError(t, err)
	require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.True(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
	require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
	intent.Kind = utxo.ConflictIntentReverse
	require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
}

// The winners of a forward operation are mined in the block being added, which is on the current
// chain. That block alone must not make the rollback refuse them. Any other current-chain block still does.
func ConflictRecoveryForwardOwnBlock(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	ownBlock := chainhash.HashH([]byte("forward-own-block"))
	const ownID, otherID = uint32(401), uint32(402)
	onChain := map[uint32]bool{ownID: true}
	guard := utxo.NewCurrentChainGuard(func(_ context.Context, ids []uint32) (bool, error) {
		for _, id := range ids {
			if onChain[id] {
				return true, nil
			}
		}
		return false, nil
	}).WithBlockID(func(_ context.Context, h chainhash.Hash) (uint32, error) {
		require.Equal(t, ownBlock, h)
		return ownID, nil
	})

	for index, otherBlock := range []bool{false, true} {
		t.Run(fmt.Sprintf("otherCurrentChainBlock=%t", otherBlock), func(t *testing.T) {
			parent := crashParentTx(0xb1 + byte(index))
			_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
			require.NoError(t, err)
			loser := crashSpendTx(t, parent, 80000)
			_, _, err = db.SpendAndCreate(ctx, loser, 2)
			require.NoError(t, err)
			winner := crashSpendTx(t, parent, 70000)
			_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
			require.NoError(t, err)
			_, err = db.SetMinedMulti(ctx, []*chainhash.Hash{winner.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: ownID, BlockHeight: 3})
			require.NoError(t, err)

			if otherBlock {
				_, err = db.SetMinedMulti(ctx, []*chainhash.Hash{winner.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: otherID, BlockHeight: 4})
				require.NoError(t, err)

				onChain[otherID] = true
				defer delete(onChain, otherID)
			}

			winners := []chainhash.Hash{*winner.TxIDChainHash()}
			wrapper := &failConflictPhase{Store: db, stage: "unspend"}
			_, _, err = utxo.ProcessConflicting(ctx, wrapper, 3, ownBlock, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion(), utxo.WithRecoveryChainGuard(guard))
			require.Error(t, err)
			require.True(t, wrapper.fired)

			intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, BlockHash: ownBlock, TxHashes: winners}
			r, err := db.LoadConflictRecovery(ctx, intent.IntentID())
			require.NoError(t, err)

			if otherBlock {
				require.NotEqual(t, utxo.ConflictRecoveryRolledBack, r.Phase, "a winner confirmed elsewhere must keep the checklist pending")
				_, err = utxo.RollbackConflictIntent(ctx, db, intent, guard)
				require.ErrorContains(t, err, "confirmed on the current chain")
				_, err = utxo.RollbackConflictIntent(ctx, db, intent)
				require.NoError(t, err)
				require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))

				return
			}

			require.Equal(t, utxo.ConflictRecoveryRolledBack, r.Phase, "the rollback must not refuse the winners' own block")
			require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent))
			require.True(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
			require.False(t, crashConflicting(t, ctx, db, loser.TxIDChainHash()))

			losers, marked, err := utxo.ProcessConflicting(ctx, db, 3, ownBlock, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion(), utxo.WithRecoveryChainGuard(guard))
			require.NoError(t, err)
			require.True(t, losers.Exists(*loser.TxIDChainHash()))
			require.False(t, losers.Exists(*winner.TxIDChainHash()))
			require.Equal(t, []chainhash.Hash{*loser.TxIDChainHash()}, marked)
			require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
			require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
		})
	}
}

// A parent locked by another holder must not stop the operation, and the operation must neither
// take nor release that lock.
func ConflictRecoveryForeignLock(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent, other := crashParentTx(0xb5), crashParentTx(0xb6)
	for _, tx := range []*bt.Tx{parent, other} {
		_, _, err := db.SpendAndCreate(ctx, tx, 1, utxo.WithCreateOnly())
		require.NoError(t, err)
	}
	loser := crashSpendTx(t, parent, 80000)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err := db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	child := crashSpendTx(t, loser, 60000)
	require.NoError(t, child.From(other.TxIDChainHash().String(), 0, other.Outputs[0].LockingScript.String(), other.Outputs[0].Satoshis))
	child.Inputs[1].UnlockingScript = dummyUnlockingScript
	_, _, err = db.SpendAndCreate(ctx, child, 2)
	require.NoError(t, err)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)
	require.NoError(t, db.SetLocked(ctx, []chainhash.Hash{*other.TxIDChainHash()}, true))
	t.Cleanup(func() { _ = db.SetLocked(ctx, []chainhash.Hash{*other.TxIDChainHash()}, false) })

	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	losers, _, err := utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard)
	require.NoError(t, err)
	require.True(t, losers.Exists(*child.TxIDChainHash()))
	require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.False(t, crashLocked(t, ctx, db, parent.TxIDChainHash()), "the operation must release the lock it took")
	require.True(t, crashLocked(t, ctx, db, other.TxIDChainHash()), "the operation must not release another holder's lock")
}

// countConflictReads wraps a real store and records the largest number of GetSpend calls in flight
// at once. The short delay makes overlapping calls observable.
type countConflictReads struct {
	utxo.Store
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (s *countConflictReads) GetSpend(ctx context.Context, spend *utxo.Spend) (*utxo.SpendResponse, error) {
	n := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)

	for {
		m := s.maxInFlight.Load()
		if n <= m || s.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}

	time.Sleep(2 * time.Millisecond)

	return s.Store.GetSpend(ctx, spend)
}

// ConflictRecoveryWideCascade checks the checklist for a wide cascade. The snapshot reads run
// concurrently, bounded by the walk fan-out of 128, and the checklist keeps discovery order: the
// inputs of each recorded transaction in turn, then the winning inputs.
func ConflictRecoveryWideCascade(t *testing.T, db utxo.Store) {
	const width = 200

	ctx := t.Context()
	parent := crashParentTx(0xb7)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)

	loser := crashSpendTx(t, parent, 80000)
	loser.Outputs = nil

	for i := 0; i < width; i++ {
		loser.Outputs = append(loser.Outputs, &bt.Output{Satoshis: 300, LockingScript: bscript.NewFromBytes([]byte{0x52})})
	}

	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)

	children := make([]*bt.Tx, width)
	for i := range children {
		child := bt.NewTx()
		require.NoError(t, child.From(loser.TxIDChainHash().String(), uint32(i), loser.Outputs[i].LockingScript.String(), loser.Outputs[i].Satoshis))
		child.Inputs[0].UnlockingScript = dummyUnlockingScript
		child.Outputs = []*bt.Output{{Satoshis: 200, LockingScript: bscript.NewFromBytes([]byte{0x52})}}

		_, _, err = db.SpendAndCreate(ctx, child, 2)
		require.NoError(t, err)

		children[i] = child
	}

	winner := crashSpendTx(t, parent, 70000)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)

	counted := &countConflictReads{Store: db}
	blockHash := chainhash.HashH([]byte("wide-cascade-block"))
	winners := []chainhash.Hash{*winner.TxIDChainHash()}

	losers, marked, err := utxo.ProcessConflicting(ctx, counted, 3, blockHash, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)

	require.Equal(t, width+1, losers.Length())
	require.Len(t, marked, width+1)
	require.True(t, losers.Exists(*loser.TxIDChainHash()))
	require.False(t, losers.Exists(*winner.TxIDChainHash()), "a winner must not be returned as a loser")

	for _, child := range children {
		require.True(t, losers.Exists(*child.TxIDChainHash()))
		require.True(t, crashConflicting(t, ctx, db, child.TxIDChainHash()))
	}

	require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.False(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
	require.True(t, crashConflicting(t, ctx, db, loser.TxIDChainHash()))

	maxInFlight := counted.maxInFlight.Load()
	require.Greater(t, maxInFlight, int32(1), "the snapshot reads must overlap")
	require.LessOrEqual(t, maxInFlight, int32(128), "the snapshot reads must stay within the walk fan-out")

	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, BlockHash: blockHash, TxHashes: winners}
	r, err := db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.NotNil(t, r)
	require.Equal(t, utxo.ConflictRecoveryFinished, r.Phase)

	// Rebuild the expected order the way a sequential snapshot discovers it.
	type outpoint struct {
		hash  chainhash.Hash
		index uint32
	}

	var expected []outpoint

	seen := make(map[outpoint]struct{})
	addExpected := func(spends []*utxo.Spend) {
		for _, sp := range spends {
			key := outpoint{*sp.TxID, sp.Vout}
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				expected = append(expected, key)
			}
		}
	}

	txs := map[chainhash.Hash]*bt.Tx{*loser.TxIDChainHash(): loser, *winner.TxIDChainHash(): winner}
	for _, child := range children {
		txs[*child.TxIDChainHash()] = child
	}

	require.Len(t, r.Transactions, width+2)

	for _, recorded := range r.Transactions {
		tx, ok := txs[recorded.Hash]
		require.True(t, ok, "the checklist must record only cascade transactions")

		spends, err := utxo.GetSpends(tx)
		require.NoError(t, err)

		addExpected(spends)
	}

	winningSpends, err := utxo.GetSpends(winner)
	require.NoError(t, err)

	addExpected(winningSpends)

	require.Len(t, r.OriginalSpends, width+1)

	for i, original := range r.OriginalSpends {
		require.Equal(t, expected[i], outpoint{*original.TxID, original.Vout}, "checklist entry %d", i)
		require.NotNil(t, original.SpendingData)

		// The before-image holds the owner from before the operation: the loser for the parent
		// output, and each child for its loser output.
		if original.TxID.IsEqual(parent.TxIDChainHash()) {
			require.Equal(t, loser.TxIDChainHash(), original.SpendingData.TxID)
		} else {
			require.Equal(t, children[original.Vout].TxIDChainHash(), original.SpendingData.TxID)
		}
	}

	require.NoError(t, utxo.CompleteConflictIntent(ctx, db, intent.IntentID()))
}

// attachLoserChild wraps a real store. Just before the operation starts to mutate, it stores a new
// child of the loser, as a validator can while the checklist is prepared.
type attachLoserChild struct {
	utxo.Store
	child *bt.Tx
	done  bool
}

func (s *attachLoserChild) SaveConflictRecovery(ctx context.Context, id chainhash.Hash, r *utxo.ConflictRecovery) error {
	if r.Phase == utxo.ConflictRecoveryMutating && !s.done {
		s.done = true
		if _, _, err := s.Store.SpendAndCreate(ctx, s.child, 2); err != nil {
			return err
		}
	}
	return s.Store.SaveConflictRecovery(ctx, id, r)
}

// ConflictRecoveryLateLoserChild attaches a child to the loser after the checklist is prepared. The
// operation must stop and roll back, and the rollback must not fail on that child: it never touches
// the child's inputs. A retry then includes the child.
func ConflictRecoveryLateLoserChild(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xd1)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)
	child := crashSpendTx(t, loser, 60000)

	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	wrapper := &attachLoserChild{Store: db, child: child}
	_, _, err = utxo.ProcessConflicting(ctx, wrapper, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.ErrorContains(t, err, "retry required")
	require.NotContains(t, err.Error(), "MANUAL INTERVENTION", "the rollback must not fail on a child outside the checklist")

	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, TxHashes: winners}
	r, err := db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, utxo.ConflictRecoveryRolledBack, r.Phase)
	require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.False(t, crashConflicting(t, ctx, db, loser.TxIDChainHash()))
	require.False(t, crashConflicting(t, ctx, db, child.TxIDChainHash()), "the loser ends usable, so its child must stay usable")
	require.True(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
	require.False(t, crashLocked(t, ctx, db, parent.TxIDChainHash()))

	losers, _, err := utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)
	require.True(t, losers.Exists(*child.TxIDChainHash()), "the retry must include the late child")
	require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
}

// ConflictRecoveryLateWinnerChild rolls back a finished forward operation after a mempool child
// attached to the winner. The winner ends conflicting, so the child must end conflicting too, and
// the rollback must report it for txmeta cache eviction.
func ConflictRecoveryLateWinnerChild(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xd2)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)

	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	_, _, err = utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)

	child := crashSpendTx(t, winner, 60000)
	_, _, err = db.SpendAndCreate(ctx, child, 3)
	require.NoError(t, err)
	grandchild := crashSpendTx(t, child, 50000)
	_, _, err = db.SpendAndCreate(ctx, grandchild, 3)
	require.NoError(t, err)

	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, TxHashes: winners}
	cascaded, err := utxo.RollbackConflictIntent(ctx, db, intent)
	require.NoError(t, err)
	require.ElementsMatch(t, []chainhash.Hash{*child.TxIDChainHash(), *grandchild.TxIDChainHash()}, cascaded)

	r, err := db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.Equal(t, utxo.ConflictRecoveryRolledBack, r.Phase)
	require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.False(t, crashConflicting(t, ctx, db, loser.TxIDChainHash()))
	require.True(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
	require.True(t, crashConflicting(t, ctx, db, child.TxIDChainHash()), "a child of a conflicting winner must be conflicting")
	require.True(t, crashConflicting(t, ctx, db, grandchild.TxIDChainHash()))
	require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
}

// spendFreeInput wraps a real store. When the checklist snapshot first reads the free outpoint, it
// stores a stranger that spends it, as a validator can after discovery and before the snapshot.
type spendFreeInput struct {
	utxo.Store
	free     *utxo.Spend
	stranger *bt.Tx
	done     atomic.Bool
}

func (s *spendFreeInput) GetSpend(ctx context.Context, sp *utxo.Spend) (*utxo.SpendResponse, error) {
	if sp.TxID.IsEqual(s.free.TxID) && sp.Vout == s.free.Vout && s.done.CompareAndSwap(false, true) {
		if _, _, err := s.Store.SpendAndCreate(ctx, s.stranger, 2); err != nil {
			return nil, err
		}
	}
	return s.Store.GetSpend(ctx, sp)
}

// ConflictRecoveryLateFreeInputSpender spends a free input of the winner after discovery and before
// the snapshot. The stranger is outside the discovered set, so the operation must stop before it
// mutates anything. A retry then discovers the stranger and marks it conflicting.
func ConflictRecoveryLateFreeInputSpender(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xf1)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	freeParent := crashParentTx(0xf2)
	_, _, err = db.SpendAndCreate(ctx, freeParent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	require.NoError(t, winner.From(freeParent.TxIDChainHash().String(), 0, freeParent.Outputs[0].LockingScript.String(), freeParent.Outputs[0].Satoshis))
	winner.Inputs[1].UnlockingScript = dummyUnlockingScript
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)
	stranger := crashSpendTx(t, freeParent, 60000)
	winningSpends, err := utxo.GetSpends(winner)
	require.NoError(t, err)
	require.Len(t, winningSpends, 2)

	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	wrapper := &spendFreeInput{Store: db, free: winningSpends[1], stranger: stranger}
	_, _, err = utxo.ProcessConflicting(ctx, wrapper, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.True(t, wrapper.done.Load(), "the snapshot must read the free input")
	require.ErrorContains(t, err, "retry required")
	require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent), "nothing may change before the retry")
	require.Equal(t, stranger.TxIDChainHash(), crashSpender(t, ctx, db, freeParent))
	require.False(t, crashConflicting(t, ctx, db, stranger.TxIDChainHash()))
	require.False(t, crashLocked(t, ctx, db, parent.TxIDChainHash()))
	require.False(t, crashLocked(t, ctx, db, freeParent.TxIDChainHash()))

	losers, _, err := utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)
	require.True(t, losers.Exists(*loser.TxIDChainHash()))
	require.True(t, losers.Exists(*stranger.TxIDChainHash()), "the retry must include the stranger")
	require.True(t, crashConflicting(t, ctx, db, stranger.TxIDChainHash()), "a displaced spender must end conflicting")
	require.False(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))
	require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, freeParent))
	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, TxHashes: winners}
	require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
}

// ConflictRecoveryReverseThenReadd adds a block, moves it back and adds it again, with the forward
// intent of the first add still waiting for acknowledgement. The reverse must retire that finished
// checklist, so the second add runs a new forward operation instead of failing on the old one.
func ConflictRecoveryReverseThenReadd(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xd3)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)

	block := chainhash.HashH([]byte("reverse-then-readd-block"))
	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	forward := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, BlockHash: block, TxHashes: winners}

	for round := 0; round < 2; round++ {
		_, _, err = utxo.ProcessConflicting(ctx, db, 3, block, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
		require.NoError(t, err, "add %d", round+1)
		require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))

		_, _, err = utxo.ReverseProcessConflicting(ctx, db, 3, block, winners)
		require.NoError(t, err, "move back %d", round+1)
		require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent))
		require.True(t, crashConflicting(t, ctx, db, winner.TxIDChainHash()))

		r, err := db.LoadConflictRecovery(ctx, forward.IntentID())
		require.NoError(t, err)
		require.Nil(t, r, "the reverse must retire the finished forward checklist")
	}

	_, _, err = utxo.ProcessConflicting(ctx, db, 3, block, winners, nil, utxo.NoAncestryGuard)
	require.NoError(t, err, "final add")
	require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
}

// countPendingScans wraps a real store and counts the scans of the whole WAL.
type countPendingScans struct {
	utxo.Store
	scans atomic.Int32
}

func (s *countPendingScans) PendingConflictIntents(ctx context.Context) ([]utxo.ConflictIntent, error) {
	s.scans.Add(1)
	return s.Store.PendingConflictIntents(ctx)
}

// ConflictRecoveryLegacyIntent checks that a new operation finds an existing intent without a scan
// of the whole WAL, and that an intent recorded without a checklist is still refused.
func ConflictRecoveryLegacyIntent(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xd4)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)
	winners := []chainhash.Hash{*winner.TxIDChainHash()}

	legacy := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, BlockHash: chainhash.HashH([]byte("legacy-block")), TxHashes: winners, StartedAt: 1}
	require.NoError(t, db.BeginConflictIntent(ctx, legacy))

	counter := &countPendingScans{Store: db}
	_, _, err = utxo.ProcessConflicting(ctx, counter, 3, legacy.BlockHash, winners, nil, utxo.NoAncestryGuard)
	require.ErrorContains(t, err, "legacy conflict intent")
	require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent), "a refused legacy intent must change nothing")
	require.NoError(t, db.CompleteConflictIntent(ctx, legacy.IntentID()))

	_, _, err = utxo.ProcessConflicting(ctx, counter, 3, chainhash.HashH([]byte("new-block")), winners, nil, utxo.NoAncestryGuard)
	require.NoError(t, err)
	require.Equal(t, winner.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.Zero(t, counter.scans.Load(), "an operation must not scan the whole WAL")
}

// cancelDuringRestore wraps a real store. Its restore cancels the rollback's ctx and writes nothing,
// as when compensationTimeout expires during the restore.
type cancelDuringRestore struct {
	utxo.Store
	cancel context.CancelFunc
}

func (s *cancelDuringRestore) RestoreConflictSpends(context.Context, []*utxo.Spend, uint32) error {
	s.cancel()
	return nil
}

// The writes after the restore ignore the cancellation, so only the verify reads see it.
func (s *cancelDuringRestore) SetConflicting(ctx context.Context, hashes []chainhash.Hash, value bool) ([]*utxo.Spend, []chainhash.Hash, error) {
	return s.Store.SetConflicting(context.WithoutCancel(ctx), hashes, value)
}

func (s *cancelDuringRestore) SetConflictLocks(ctx context.Context, id chainhash.Hash, parents []chainhash.Hash, locked bool) error {
	return s.Store.SetConflictLocks(context.WithoutCancel(ctx), id, parents, locked)
}

func (s *cancelDuringRestore) SaveConflictRecovery(ctx context.Context, id chainhash.Hash, r *utxo.ConflictRecovery) error {
	return s.Store.SaveConflictRecovery(context.WithoutCancel(ctx), id, r)
}

// ConflictRecoveryCancelledVerify cancels the rollback before it verifies the restored owners. An
// outpoint that was never read is not verified, so the rollback must fail and keep the losers
// conflicting instead of marking them usable.
func ConflictRecoveryCancelledVerify(t *testing.T, db utxo.Store) {
	ctx := t.Context()
	parent := crashParentTx(0xd6)
	_, _, err := db.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)
	loser := crashSpendTx(t, parent, 80000)
	_, _, err = db.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)
	winner := crashSpendTx(t, parent, 70000)
	_, _, err = db.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)

	winners := []chainhash.Hash{*winner.TxIDChainHash()}
	_, _, err = utxo.ProcessConflicting(ctx, db, 3, chainhash.Hash{}, winners, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)

	rollbackCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	intent := utxo.ConflictIntent{Kind: utxo.ConflictIntentForward, BlockHeight: 3, TxHashes: winners}
	_, err = utxo.RollbackConflictIntent(rollbackCtx, &cancelDuringRestore{Store: db, cancel: cancel}, intent)
	require.Error(t, err, "unverified owners must fail the rollback")

	r, err := db.LoadConflictRecovery(ctx, intent.IntentID())
	require.NoError(t, err)
	require.NotEqual(t, utxo.ConflictRecoveryRolledBack, r.Phase)
	require.True(t, crashConflicting(t, ctx, db, loser.TxIDChainHash()), "the loser must stay conflicting")

	_, err = utxo.RollbackConflictIntent(ctx, db, intent)
	require.NoError(t, err)
	require.Equal(t, loser.TxIDChainHash(), crashSpender(t, ctx, db, parent))
	require.NoError(t, db.CompleteConflictIntent(ctx, intent.IntentID()))
}

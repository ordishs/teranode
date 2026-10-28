package aerospike_test

import (
	"strings"
	"testing"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/tests"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// The pending scan reads only the WAL root. It must not load a checklist, and it must finish the
// deletion of an intent whose chunk cleanup was interrupted instead of returning it.
func TestAerospikeConflictWALPendingScan(t *testing.T) {
	client, store, ctx, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	intent := utxo.ConflictIntent{
		Recovery:    &utxo.ConflictRecovery{Version: 1, Phase: utxo.ConflictRecoveryPlanning},
		Kind:        utxo.ConflictIntentForward,
		BlockHeight: 7,
		BlockHash:   chainhash.HashH([]byte("pending-scan-block")),
		TxHashes:    []chainhash.Hash{chainhash.HashH([]byte("pending-scan-winner"))},
		StartedAt:   1,
	}
	id := intent.IntentID()

	require.NoError(t, store.BeginConflictIntent(ctx, intent))
	require.NoError(t, store.SaveConflictRecovery(ctx, id, &utxo.ConflictRecovery{
		Version:      1,
		Phase:        utxo.ConflictRecoveryFinished,
		Transactions: []utxo.ConflictTransaction{{Hash: intent.TxHashes[0]}},
		Winners:      intent.TxHashes,
	}))

	pending, err := store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, id, pending[0].IntentID())
	require.Nil(t, pending[0].Recovery, "the pending scan must not load checklists")

	recovery, err := store.LoadConflictRecovery(ctx, id)
	require.NoError(t, err)
	require.Equal(t, utxo.ConflictRecoveryFinished, recovery.Phase)

	rootKey, err := aerospike.NewKey(store.GetNamespace(), store.GetName()+"_conflictwal", id[:])
	require.NoError(t, err)

	root, err := client.Get(nil, rootKey, "recoveryRefs")
	require.NoError(t, err)

	refs, ok := root.Bins["recoveryRefs"].([]interface{})
	require.True(t, ok)
	require.NotEmpty(t, refs)

	ref, ok := refs[0].(string)
	require.True(t, ok)

	digest, _, found := strings.Cut(ref, ":")
	require.True(t, found)

	chunkKey, err := aerospike.NewKey(store.GetNamespace(), store.GetName()+"_conflictwal_data", id.String()+":"+digest+":0")
	require.NoError(t, err)

	exists, err := client.Exists(nil, chunkKey)
	require.NoError(t, err)
	require.True(t, exists, "the checklist chunk must exist before cleanup")

	// Model a CompleteConflictIntent that stopped after it marked the root done.
	require.NoError(t, client.PutBins(nil, rootKey, aerospike.NewBin("recoveryDone", true)))

	pending, err = store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "a completed intent must not be returned as pending")

	exists, err = client.Exists(nil, rootKey)
	require.NoError(t, err)
	require.False(t, exists, "the scan must finish deleting the WAL root")

	exists, err = client.Exists(nil, chunkKey)
	require.NoError(t, err)
	require.False(t, exists, "the scan must finish deleting the checklist chunks")
}

func TestAerospikeConflictRecoveryWideCascade(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryWideCascade(t, store)
}

func TestAerospikeConflictRecoveryLateLoserChild(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryLateLoserChild(t, store)
}

func TestAerospikeConflictRecoveryLateFreeInputSpender(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryLateFreeInputSpender(t, store)
}

func TestAerospikeConflictRecoveryOutsiderOwnedInput(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryOutsiderOwnedInput(t, store)
}

func TestAerospikeConflictRecoveryEarlyFailure(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryEarlyFailure(t, store)
}

func TestAerospikeConflictRecoveryRefusedAfterRollback(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryRefusedAfterRollback(t, store)
}

func TestAerospikeConflictRecoveryHealStaleForward(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryHealStaleForward(t, store)
}

func TestAerospikeConflictRecoverySetConflictingChildren(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoverySetConflictingChildren(t, store, true)
}

func TestAerospikeConflictRecoveryReverseCompletionFailure(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryReverseCompletionFailure(t, store)
}

func TestAerospikeConflictRecoveryWinnerWithConflictingChild(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryWinnerWithConflictingChild(t, store)
}

func TestAerospikeConflictRecoveryLateWinnerChild(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryLateWinnerChild(t, store)
}

func TestAerospikeConflictRecoveryReverseThenReadd(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryReverseThenReadd(t, store)
}

func TestAerospikeConflictRecoveryLegacyIntent(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryLegacyIntent(t, store)
}

func TestAerospikeConflictRecoveryCancelledVerify(t *testing.T) {
	_, store, _, deferFn := initAerospike(t, test.CreateBaseTestSettings(t), ulogger.NewErrorTestLogger(t))
	t.Cleanup(deferFn)

	tests.ConflictRecoveryCancelledVerify(t, store)
}

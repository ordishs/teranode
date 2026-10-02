package sql

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/teranode/errors"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/tests"
	"github.com/stretchr/testify/require"
)

// GetCounterConflicting is the demotion-path entry point (ProcessConflicting). A
// parent slot naming a spender with no record must fail it closed even though the
// parent is recent and the store has retention configured: the absent spender's
// spend stays on the slot, so the winner's spend in step 3 would fail UTXO_SPENT
// after earlier steps had mutated. Validation keeps its own tolerance.
func TestGetCounterConflicting_FailsClosedOnDanglingSpender(t *testing.T) {
	ctx := context.Background()
	store, _ := setup(ctx, t)

	require.NotZero(t, store.settings.GetUtxoStoreBlockHeightRetention(),
		"retention must be configured, otherwise the test cannot tell the wiring from the default")

	// a zero tip would fail the guard closed by itself and hide the wiring under test
	require.NoError(t, store.SetBlockHeight(10))

	_, _, err := store.SpendAndCreate(ctx, tests.ParentTx, 1, utxostore.WithCreateOnly())
	require.NoError(t, err)

	// spend-only records the spend on the parent without creating the spender
	_, _, err = store.SpendAndCreate(ctx, tests.Tx, 2, utxostore.WithSpendOnly())
	require.NoError(t, err)

	winner := tests.Tx.Clone()
	winner.Version = 2
	require.NotEqual(t, tests.Tx.TxIDChainHash(), winner.TxIDChainHash())

	_, _, err = store.SpendAndCreate(ctx, winner, 3, utxostore.WithCreateOnly(), utxostore.WithConflicting(true))
	require.NoError(t, err)

	result, err := store.GetCounterConflicting(ctx, *winner.TxIDChainHash())

	require.Error(t, err)
	require.Nil(t, result)
	require.True(t, errors.Is(err, errors.ErrTxNotFound))

}

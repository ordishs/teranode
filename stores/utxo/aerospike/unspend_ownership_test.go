package aerospike_test

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	spendpkg "github.com/bsv-blockchain/teranode/stores/utxo/spend"
	utxo2 "github.com/bsv-blockchain/teranode/test/longtest/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// TestUnspendOwnership_HappyPath verifies that Unspend with matching SpendingData
// successfully clears the spend.
func TestUnspendOwnership_HappyPath(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	_, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	_, err := store.Create(ctx, tx, 101)
	require.NoError(t, err)

	localSpendTx := utxo2.GetSpendingTx(tx, 0)
	spendsRet, err := store.Spend(ctx, localSpendTx, store.GetBlockHeight()+1)
	require.NoError(t, err)
	require.Len(t, spendsRet, 1)

	// SpendingData here matches what GetSpends populated: NewSpendingData(localSpendTx.TxIDChainHash(), 0).
	err = store.Unspend(ctx, spendsRet)
	require.NoError(t, err, "happy path Unspend with matching SpendingData should succeed")

	// Verify the UTXO is now unspent — we should be able to Spend it again.
	spendTxAlt := utxo2.GetSpendingTx(tx, 0)
	_, err = store.Spend(ctx, spendTxAlt, store.GetBlockHeight()+1)
	require.NoError(t, err, "after successful Unspend the UTXO should be re-spendable")
}

// TestUnspendOwnership_Mismatch verifies that Unspend with a different SpendingData
// (caller doesn't own the spend) returns an error and leaves the UTXO still spent.
func TestUnspendOwnership_Mismatch(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	_, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	_, err := store.Create(ctx, tx, 101)
	require.NoError(t, err)

	// Spend with a real spending tx — store records SpendingData = NewSpendingData(localSpendTx.TxIDChainHash(), 0).
	localSpendTx := utxo2.GetSpendingTx(tx, 0)
	_, err = store.Spend(ctx, localSpendTx, store.GetBlockHeight()+1)
	require.NoError(t, err)

	utxoHash, err := util.UTXOHashFromOutput(tx.TxIDChainHash(), tx.Outputs[0], 0)
	require.NoError(t, err)

	wrongTxHash := chainhash.HashH([]byte("wrong-spender"))
	mismatched := []*utxo.Spend{{
		TxID:         tx.TxIDChainHash(),
		Vout:         0,
		UTXOHash:     utxoHash,
		SpendingData: spendpkg.NewSpendingData(&wrongTxHash, 0),
	}}

	err = store.Unspend(ctx, mismatched)
	require.Error(t, err, "Unspend with mismatched SpendingData must error")
	require.True(t, errors.Is(err, errors.ErrProcessing), "expected processing error, got: %v", err)

	// The UTXO must still be spent — attempting to Spend with a NEW spender should fail with ErrSpent.
	spendTxRetry := utxo2.GetSpendingTx(tx, 0)
	spendsRet, err := store.Spend(ctx, spendTxRetry, store.GetBlockHeight()+1)
	require.Error(t, err, "UTXO should still be spent after mismatched Unspend")
	require.Len(t, spendsRet, 1)
	require.Equal(t, localSpendTx.TxIDChainHash().String(), spendsRet[0].ConflictingTxID.String(),
		"the original spender's TxID must still be the recorded conflicting TxID")
}

// TestUnspendOwnership_NilSpendingDataRejected verifies that Unspend rejects
// callers that pass a nil SpendingData. The escape hatch was removed because
// every production caller derives spends from Spend()/SetConflicting()/GetSpends(),
// all of which populate SpendingData.
func TestUnspendOwnership_NilSpendingDataRejected(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	_, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	_, err := store.Create(ctx, tx, 101)
	require.NoError(t, err)

	localSpendTx := utxo2.GetSpendingTx(tx, 0)
	_, err = store.Spend(ctx, localSpendTx, store.GetBlockHeight()+1)
	require.NoError(t, err)

	utxoHash, err := util.UTXOHashFromOutput(tx.TxIDChainHash(), tx.Outputs[0], 0)
	require.NoError(t, err)

	noSpendingData := []*utxo.Spend{{
		TxID:         tx.TxIDChainHash(),
		Vout:         0,
		UTXOHash:     utxoHash,
		SpendingData: nil,
	}}

	err = store.Unspend(ctx, noSpendingData)
	require.Error(t, err, "Unspend with nil SpendingData must error")
	require.True(t, errors.Is(err, errors.ErrProcessing), "expected processing error, got: %v", err)

	// The UTXO must still be spent — attempting to Spend with a NEW spender should still fail.
	spendTxRetry := utxo2.GetSpendingTx(tx, 0)
	spendsRet, err := store.Spend(ctx, spendTxRetry, store.GetBlockHeight()+1)
	require.Error(t, err, "UTXO should still be spent after rejected nil-SpendingData Unspend")
	require.Len(t, spendsRet, 1)
	require.Equal(t, localSpendTx.TxIDChainHash().String(), spendsRet[0].ConflictingTxID.String(),
		"the original spender's TxID must still be the recorded conflicting TxID")
}

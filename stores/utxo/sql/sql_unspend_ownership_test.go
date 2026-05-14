package sql

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	spendpkg "github.com/bsv-blockchain/teranode/stores/utxo/spend"
	utxo2 "github.com/bsv-blockchain/teranode/test/longtest/stores/utxo"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/stretchr/testify/require"
)

// TestUnspendOwnership_HappyPath verifies that Unspend with matching SpendingData
// successfully clears spending_data on the UTXO.
func TestUnspendOwnership_HappyPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, tx := setup(ctx, t)

	_, err := store.Create(ctx, tx, 0)
	require.NoError(t, err)

	spendTx := utxo2.GetSpendingTx(tx, 0)
	_, err = store.Spend(ctx, spendTx, store.GetBlockHeight()+1)
	require.NoError(t, err)

	utxoHash, err := util.UTXOHashFromOutput(tx.TxIDChainHash(), tx.Outputs[0], 0)
	require.NoError(t, err)

	// Use the actual SpendingData that was used during Spend (matches what GetSpends produces).
	spend := &utxo.Spend{
		TxID:         tx.TxIDChainHash(),
		Vout:         0,
		UTXOHash:     utxoHash,
		SpendingData: spendpkg.NewSpendingData(spendTx.TxIDChainHash(), 0),
	}

	err = store.Unspend(ctx, []*utxo.Spend{spend})
	require.NoError(t, err)

	// Verify the spending_data column is now NULL.
	resp, err := store.GetSpend(ctx, spend)
	require.NoError(t, err)
	require.Equal(t, int(utxo.Status_OK), resp.Status,
		"expected UTXO to be unspent after matching Unspend")
	require.Nil(t, resp.SpendingData, "expected spending_data to be cleared after matching Unspend")
}

// TestUnspendOwnership_Mismatch verifies that Unspend with a different SpendingData
// (caller doesn't own the spend) returns an error and leaves spending_data UNCHANGED.
func TestUnspendOwnership_Mismatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, tx := setup(ctx, t)

	_, err := store.Create(ctx, tx, 0)
	require.NoError(t, err)

	// Spend with the real spendTx — actual stored SpendingData = NewSpendingData(spendTx.TxIDChainHash(), 0).
	spendTx := utxo2.GetSpendingTx(tx, 0)
	_, err = store.Spend(ctx, spendTx, store.GetBlockHeight()+1)
	require.NoError(t, err)

	utxoHash, err := util.UTXOHashFromOutput(tx.TxIDChainHash(), tx.Outputs[0], 0)
	require.NoError(t, err)

	// Build a Spend record with INTENTIONALLY mismatched SpendingData (different spending tx).
	wrongTxHash := chainhash.HashH([]byte("wrong-spender"))
	mismatchedSpend := &utxo.Spend{
		TxID:         tx.TxIDChainHash(),
		Vout:         0,
		UTXOHash:     utxoHash,
		SpendingData: spendpkg.NewSpendingData(&wrongTxHash, 0),
	}

	err = store.Unspend(ctx, []*utxo.Spend{mismatchedSpend})
	require.Error(t, err, "expected error when Unspend caller's SpendingData does not match the stored spend")
	require.True(t, errors.Is(err, errors.ErrProcessing), "expected processing error, got: %v", err)

	// The legitimate spending_data must still be intact — bitwise compare against expected bytes.
	expected := spendpkg.NewSpendingData(spendTx.TxIDChainHash(), 0).Bytes()

	probe := &utxo.Spend{
		TxID:     tx.TxIDChainHash(),
		Vout:     0,
		UTXOHash: utxoHash,
	}
	resp, err := store.GetSpend(ctx, probe)
	require.NoError(t, err)
	require.Equal(t, int(utxo.Status_SPENT), resp.Status,
		"UTXO must still be SPENT after a mismatched Unspend attempt")
	require.NotNil(t, resp.SpendingData, "spending_data must still be populated after a mismatched Unspend attempt")
	require.Equal(t, expected, resp.SpendingData.Bytes(),
		"stored spending_data must be unchanged after a mismatched Unspend attempt")
}

// TestUnspendOwnership_NotFound verifies that Unspend against a transaction the
// store has never seen returns NotFoundError. Mirrors the Aerospike Lua
// ERROR_CODE_TX_NOT_FOUND branch and the pre-existing SQL behaviour for genuinely
// unknown outputs.
func TestUnspendOwnership_NotFound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, tx := setup(ctx, t)

	utxoHash, err := util.UTXOHashFromOutput(tx.TxIDChainHash(), tx.Outputs[0], 0)
	require.NoError(t, err)

	unknownTxHash := chainhash.HashH([]byte("never-created"))
	spend := &utxo.Spend{
		TxID:         &unknownTxHash,
		Vout:         0,
		UTXOHash:     utxoHash,
		SpendingData: spendpkg.NewSpendingData(&unknownTxHash, 0),
	}

	err = store.Unspend(ctx, []*utxo.Spend{spend})
	require.Error(t, err, "expected error when unspending an unknown transaction")
	require.True(t, errors.Is(err, errors.ErrNotFound), "expected NotFoundError, got: %v", err)
}

// TestUnspendOwnership_MismatchOnUnspent verifies that Unspend with a non-nil
// SpendingData against a never-spent UTXO is rejected with ErrUtxoUnspent
// (case 2 of the SQL 3-way dispatch — row exists, spending_data IS NULL).
// This is the idempotent-retry signal: retry-safe callers like validator
// reverseSpends treat it as success.
func TestUnspendOwnership_MismatchOnUnspent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, tx := setup(ctx, t)

	_, err := store.Create(ctx, tx, 0)
	require.NoError(t, err)

	utxoHash, err := util.UTXOHashFromOutput(tx.TxIDChainHash(), tx.Outputs[0], 0)
	require.NoError(t, err)

	someTxHash := chainhash.HashH([]byte("some-other-tx"))
	spend := &utxo.Spend{
		TxID:         tx.TxIDChainHash(),
		Vout:         0,
		UTXOHash:     utxoHash,
		SpendingData: spendpkg.NewSpendingData(&someTxHash, 0),
	}

	err = store.Unspend(ctx, []*utxo.Spend{spend})
	require.Error(t, err, "expected error when unspending a UTXO that was never spent")
	require.True(t, errors.Is(err, errors.ErrUtxoUnspent), "expected ErrUtxoUnspent, got: %v", err)
	require.Contains(t, err.Error(), "is unspent", "expected case-2 'UTXO is unspent' message, got: %v", err)
}

// TestUnspendOwnership_NilSpendingDataRejected verifies that Unspend rejects
// callers that pass a nil SpendingData. The escape hatch was removed because
// every production caller derives spends from Spend()/SetConflicting()/GetSpends(),
// all of which populate SpendingData.
func TestUnspendOwnership_NilSpendingDataRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, tx := setup(ctx, t)

	_, err := store.Create(ctx, tx, 0)
	require.NoError(t, err)

	spendTx := utxo2.GetSpendingTx(tx, 0)
	_, err = store.Spend(ctx, spendTx, store.GetBlockHeight()+1)
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

	// The legitimate spend must still be intact.
	expected := spendpkg.NewSpendingData(spendTx.TxIDChainHash(), 0).Bytes()

	probe := &utxo.Spend{
		TxID:     tx.TxIDChainHash(),
		Vout:     0,
		UTXOHash: utxoHash,
	}
	resp, err := store.GetSpend(ctx, probe)
	require.NoError(t, err)
	require.Equal(t, int(utxo.Status_SPENT), resp.Status,
		"UTXO must still be SPENT after a rejected Unspend attempt")
	require.NotNil(t, resp.SpendingData, "spending_data must still be populated after a rejected Unspend attempt")
	require.Equal(t, expected, resp.SpendingData.Bytes(),
		"stored spending_data must be unchanged after a rejected Unspend attempt")
}

package sql

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
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

// TestUnspendOwnership_AlreadyUnspent verifies that Unspend on a never-spent UTXO
// returns an error (NotFound), preserving the existing pre-fix behaviour.
func TestUnspendOwnership_AlreadyUnspent(t *testing.T) {
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
}

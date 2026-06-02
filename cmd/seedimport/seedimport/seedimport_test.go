package seedimport

import (
	"context"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/utxopersister"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	utxosql "github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

func newTestUTXOStore(t *testing.T) utxo.Store {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)

	u, err := url.Parse("sqlitememory:///seedimport-" + t.Name())
	require.NoError(t, err)

	store, err := utxosql.New(t.Context(), ulogger.TestLogger{}, tSettings, u)
	require.NoError(t, err)

	return store
}

func TestLoadWrapperMakesOutputsSpendable(t *testing.T) {
	ctx := context.Background()
	store := newTestUTXOStore(t)

	txid := chainhash.HashH([]byte("wrapper-tx"))

	w := &utxopersister.UTXOWrapper{
		TxID:     txid,
		Height:   100,
		Coinbase: false,
		UTXOs: []*utxopersister.UTXO{
			{Index: 0, Value: 1000, Script: []byte{0x76, 0xa9, 0x51}},
			{Index: 2, Value: 2000, Script: []byte{0x6a}},
		},
	}

	require.NoError(t, loadWrapper(ctx, store, w, 42))

	for _, vout := range []uint32{0, 2} {
		resp, err := store.GetSpend(ctx, &utxo.Spend{TxID: &txid, Vout: vout})
		require.NoError(t, err)
		require.Equal(t, int(utxo.Status_OK), resp.Status, "vout %d should be spendable", vout)
		require.Nil(t, resp.SpendingData)
	}
}

func TestWrapperToTxUsesRealTxID(t *testing.T) {
	txid := chainhash.HashH([]byte("real-txid"))

	w := &utxopersister.UTXOWrapper{
		TxID:   txid,
		Height: 5,
		UTXOs:  []*utxopersister.UTXO{{Index: 0, Value: 1, Script: []byte{0x51}}},
	}

	tx := wrapperToTx(w)
	require.Equal(t, txid, *tx.TxIDChainHash(), "synthesized tx must report the real txid via SetTxHash")
	require.Empty(t, tx.Inputs)
	require.Len(t, tx.Outputs, 1)
	require.Equal(t, uint64(1), tx.Outputs[0].Satoshis)
}

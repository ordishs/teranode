// Package seedimport loads a verifiable UTXO seed into a node's UTXO store.
package seedimport

import (
	"context"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/teranode/services/utxopersister"
	"github.com/bsv-blockchain/teranode/stores/utxo"
)

// wrapperToTx synthesizes a partial transaction carrying only the surviving
// outputs of a UTXOWrapper. Inputs are empty; Outputs are sparse (nil at vouts
// that were already spent). The real txid is set via SetTxHash so any internal
// TxIDChainHash() call is correct; Create is additionally given WithTXID.
func wrapperToTx(w *utxopersister.UTXOWrapper) *bt.Tx {
	tx := bt.NewTx()

	maxVout := uint32(0)
	for _, u := range w.UTXOs {
		if u.Index > maxVout {
			maxVout = u.Index
		}
	}

	tx.Outputs = make([]*bt.Output, maxVout+1)
	for _, u := range w.UTXOs {
		tx.Outputs[u.Index] = &bt.Output{
			Satoshis:      u.Value,
			LockingScript: bscript.NewFromBytes(u.Script),
		}
	}

	txid := w.TxID
	tx.SetTxHash(&txid)

	return tx
}

// loadWrapper stores a wrapper's surviving outputs as UTXOs mined in block
// blockID, keyed by the wrapper's real txid.
func loadWrapper(ctx context.Context, store utxo.Store, w *utxopersister.UTXOWrapper, blockID uint32) error {
	tx := wrapperToTx(w)
	txid := w.TxID

	_, err := store.Create(ctx, tx, w.Height,
		utxo.WithTXID(&txid),
		utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{
			BlockID:        blockID,
			BlockHeight:    w.Height,
			SubtreeIdx:     0,
			OnLongestChain: true,
		}),
		utxo.WithSetCoinbase(w.Coinbase),
	)

	return err
}

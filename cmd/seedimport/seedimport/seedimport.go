// Package seedimport loads a verifiable UTXO seed into a node's UTXO store.
package seedimport

import (
	"context"
	"encoding/hex"
	"io"
	"strings"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/pkg/muhash"
	"github.com/bsv-blockchain/teranode/pkg/seedcheckpoint"
	"github.com/bsv-blockchain/teranode/pkg/utxoseed"
	"github.com/bsv-blockchain/teranode/services/utxopersister"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
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

// BlockHeaderLookup binds a seed's block hash to the PoW-validated header chain.
type BlockHeaderLookup interface {
	BlockIDAndHeight(ctx context.Context, blockHash *chainhash.Hash) (id uint32, height uint32, onMainChain bool, err error)
}

// blockchainStore is the minimal slice of the blockchain store needed to resolve
// a block hash to its ID, height, and main-chain membership.
type blockchainStore interface {
	GetBlockHeader(ctx context.Context, blockHash *chainhash.Hash) (*model.BlockHeader, *model.BlockHeaderMeta, error)
	CheckBlockIsInCurrentChain(ctx context.Context, blockIDs []uint32) (bool, error)
}

// blockchainLookup is the production BlockHeaderLookup backed by the blockchain store.
type blockchainLookup struct {
	store blockchainStore
}

// NewBlockchainLookup returns a BlockHeaderLookup that resolves block hashes
// against the given blockchain store.
func NewBlockchainLookup(store blockchainStore) BlockHeaderLookup {
	return &blockchainLookup{store: store}
}

func (l *blockchainLookup) BlockIDAndHeight(ctx context.Context, h *chainhash.Hash) (uint32, uint32, bool, error) {
	_, meta, err := l.store.GetBlockHeader(ctx, h)
	if err != nil {
		return 0, 0, false, err
	}

	onMain, err := l.store.CheckBlockIsInCurrentChain(ctx, []uint32{meta.ID})
	if err != nil {
		return 0, 0, false, err
	}

	return meta.ID, meta.Height, onMain, nil
}

// LoadTrustedKeys parses compiled-in + flag-provided compressed authority pubkeys (hex).
func LoadTrustedKeys(compiledIn []string, flagHex string) ([][]byte, error) {
	hexKeys := append([]string{}, compiledIn...)
	if flagHex != "" {
		hexKeys = append(hexKeys, flagHex)
	}

	if len(hexKeys) == 0 {
		return nil, errors.NewProcessingError("no trusted authority key: build with a compiled-in key or pass --authority-pubkey")
	}

	out := make([][]byte, 0, len(hexKeys))

	for _, h := range hexKeys {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, errors.NewProcessingError("invalid authority pubkey hex %q", h, err)
		}

		if _, err := bec.ParsePubKey(raw); err != nil {
			return nil, errors.NewProcessingError("invalid authority pubkey %q", h, err)
		}

		out = append(out, raw)
	}

	return out, nil
}

// Config holds everything Run needs; stores are injected so Run is testable.
type Config struct {
	SeedStore   blob.Store
	UTXOStore   utxo.Store
	Lookup      BlockHeaderLookup
	TrustedKeys [][]byte
	BlockHash   chainhash.Hash
}

// Run verifies and loads the seed identified by cfg.BlockHash.
func Run(ctx context.Context, logger ulogger.Logger, cfg Config) error {
	sc, err := readSignedCheckpointBlob(ctx, cfg.SeedStore, cfg.BlockHash)
	if err != nil {
		return err
	}

	if err := verifyAgainstTrusted(sc, cfg.TrustedKeys); err != nil {
		return err
	}

	if sc.Checkpoint.BlockHash != cfg.BlockHash {
		return errors.NewProcessingError("checkpoint blockHash does not match requested block")
	}

	blockID, height, onMain, err := cfg.Lookup.BlockIDAndHeight(ctx, &cfg.BlockHash)
	if err != nil {
		return errors.NewProcessingError("header lookup failed (is the node header-synced?)", err)
	}

	if !onMain {
		return errors.NewProcessingError("seed block %s is not on the most-work chain", cfg.BlockHash.String())
	}

	if height != sc.Checkpoint.Height {
		return errors.NewProcessingError("height mismatch: checkpoint %d, chain %d", sc.Checkpoint.Height, height)
	}

	created, digest, err := streamLoad(ctx, cfg, blockID)
	if err != nil {
		return err
	}

	if digest != sc.Checkpoint.SetHash {
		rollback(ctx, logger, cfg.UTXOStore, created)
		return errors.NewProcessingError("set hash mismatch: loaded set does not match signed checkpoint")
	}

	logger.Infof("[seedimport] loaded %d transactions for block %s height %d", len(created), cfg.BlockHash.String(), height)

	return nil
}

func readSignedCheckpointBlob(ctx context.Context, store blob.Store, blockHash chainhash.Hash) (*seedcheckpoint.SignedCheckpoint, error) {
	b, err := store.Get(ctx, blockHash[:], fileformat.FileTypeSeedCheckpoint)
	if err != nil {
		return nil, errors.NewProcessingError("reading signed checkpoint for %s", blockHash.String(), err)
	}

	return seedcheckpoint.ParseSignedCheckpoint(b)
}

func verifyAgainstTrusted(sc *seedcheckpoint.SignedCheckpoint, trusted [][]byte) error {
	for _, key := range trusted {
		if sc.VerifyWithKey(key) == nil {
			return nil
		}
	}

	return errors.NewProcessingError("checkpoint not signed by any trusted authority key")
}

func streamLoad(ctx context.Context, cfg Config, blockID uint32) ([]chainhash.Hash, [32]byte, error) {
	pr, pw := io.Pipe()

	go func() {
		pw.CloseWithError(utxopersister.StreamSeedPackage(ctx, cfg.SeedStore, cfg.BlockHash, pw))
	}()

	acc := muhash.New()

	var created []chainhash.Hash

	var hdr [68]byte
	if _, err := io.ReadFull(pr, hdr[:]); err != nil {
		return nil, [32]byte{}, errors.NewProcessingError("reading seed header", err)
	}

	var currentHash chainhash.Hash
	copy(currentHash[:], hdr[0:32])

	if currentHash != cfg.BlockHash {
		return nil, [32]byte{}, errors.NewProcessingError("seed body block hash does not match requested block")
	}

	for {
		w, err := utxopersister.NewUTXOWrapperFromReader(ctx, pr)
		if err == io.EOF || (err != nil && strings.Contains(err.Error(), "unexpected EOF")) {
			break
		}

		if err != nil {
			return nil, [32]byte{}, err
		}

		for _, u := range w.UTXOs {
			acc.Add(utxoseed.Element(w.TxID, u.Index, w.Height, w.Coinbase, u.Value, u.Script))
		}

		if err := loadWrapper(ctx, cfg.UTXOStore, w, blockID); err != nil {
			return nil, [32]byte{}, err
		}

		created = append(created, w.TxID)
	}

	return created, acc.Digest(), nil
}

func rollback(ctx context.Context, logger ulogger.Logger, store utxo.Store, created []chainhash.Hash) {
	for i := range created {
		if err := store.Delete(ctx, &created[i]); err != nil {
			logger.Warnf("[seedimport] rollback: failed to delete %s: %v", created[i].String(), err)
		}
	}
}

package seedimport

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/pkg/muhash"
	"github.com/bsv-blockchain/teranode/pkg/seedcheckpoint"
	"github.com/bsv-blockchain/teranode/pkg/seedpack"
	"github.com/bsv-blockchain/teranode/pkg/utxoseed"
	"github.com/bsv-blockchain/teranode/services/utxopersister"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
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

type stubLookup struct {
	id     uint32
	height uint32
	onMain bool
	err    error
}

func (s stubLookup) BlockIDAndHeight(ctx context.Context, h *chainhash.Hash) (uint32, uint32, bool, error) {
	return s.id, s.height, s.onMain, s.err
}

type stubBlockchainStore struct {
	meta   *model.BlockHeaderMeta
	onMain bool
}

func (s stubBlockchainStore) GetBlockHeader(ctx context.Context, blockHash *chainhash.Hash) (*model.BlockHeader, *model.BlockHeaderMeta, error) {
	return nil, s.meta, nil
}

func (s stubBlockchainStore) CheckBlockIsInCurrentChain(ctx context.Context, blockIDs []uint32) (bool, error) {
	return s.onMain, nil
}

func TestBlockchainLookupReturnsIDHeightAndOnMain(t *testing.T) {
	ctx := context.Background()

	stub := stubBlockchainStore{meta: &model.BlockHeaderMeta{ID: 5, Height: 101}, onMain: true}

	h := chainhash.HashH([]byte("block"))

	id, height, onMain, err := NewBlockchainLookup(stub).BlockIDAndHeight(ctx, &h)
	require.NoError(t, err)
	require.Equal(t, uint32(5), id)
	require.Equal(t, uint32(101), height)
	require.True(t, onMain)
}

func TestLoadTrustedKeysParsesValidKey(t *testing.T) {
	priv, err := bec.NewPrivateKey()
	require.NoError(t, err)

	keyHex := hex.EncodeToString(priv.PubKey().Compressed())

	keys, err := LoadTrustedKeys(nil, keyHex)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, priv.PubKey().Compressed(), keys[0])
}

func TestLoadTrustedKeysAcceptsCompiledIn(t *testing.T) {
	priv, err := bec.NewPrivateKey()
	require.NoError(t, err)

	keyHex := hex.EncodeToString(priv.PubKey().Compressed())

	keys, err := LoadTrustedKeys([]string{keyHex}, "")
	require.NoError(t, err)
	require.Len(t, keys, 1)
}

func TestLoadTrustedKeysErrorsWhenEmpty(t *testing.T) {
	_, err := LoadTrustedKeys(nil, "")
	require.Error(t, err)
}

func TestLoadTrustedKeysErrorsOnGarbageHex(t *testing.T) {
	_, err := LoadTrustedKeys(nil, "not-hex")
	require.Error(t, err)
}

func TestLoadTrustedKeysErrorsOnInvalidPubKey(t *testing.T) {
	_, err := LoadTrustedKeys(nil, "deadbeef")
	require.Error(t, err)
}

// buildSeed writes a utxo-set body (header|wrappers|footer) into a memory blob
// store as a seed package + signed checkpoint. Returns the store, block hash,
// and the trusted (compressed) pubkey that signed the checkpoint.
func buildSeed(t *testing.T, wrappers []*utxopersister.UTXOWrapper, height uint32) (*memory.Memory, chainhash.Hash, []byte) {
	t.Helper()

	ctx := context.Background()
	store := memory.New()

	blockHash := chainhash.HashH([]byte("seed-h"))
	prevHash := chainhash.HashH([]byte("seed-prev"))

	var body []byte
	body = append(body, blockHash[:]...)

	var hb [4]byte
	binary.LittleEndian.PutUint32(hb[:], height)
	body = append(body, hb[:]...)
	body = append(body, prevHash[:]...)

	acc := muhash.New()

	var utxoCount uint64
	for _, w := range wrappers {
		body = append(body, w.Bytes()...)
		utxoCount += uint64(len(w.UTXOs))

		for _, u := range w.UTXOs {
			acc.Add(utxoseed.Element(w.TxID, u.Index, w.Height, w.Coinbase, u.Value, u.Script))
		}
	}

	var footer [16]byte
	binary.LittleEndian.PutUint64(footer[0:8], uint64(len(wrappers)))
	binary.LittleEndian.PutUint64(footer[8:16], utxoCount)
	body = append(body, footer[:]...)

	setHash := acc.Digest()

	cfg := seedpack.Config{Min: 16, Max: 256, Mask: (1 << 6) - 1}
	require.NoError(t, utxopersister.BuildSeedPackage(ctx, store, bytes.NewReader(body), height, blockHash, setHash, cfg))

	priv, err := bec.NewPrivateKey()
	require.NoError(t, err)

	sc, err := seedcheckpoint.Sign(priv, seedcheckpoint.Checkpoint{Height: height, BlockHash: blockHash, SetHash: setHash})
	require.NoError(t, err)

	require.NoError(t, store.Set(ctx, blockHash[:], fileformat.FileTypeSeedCheckpoint, sc.Serialize(), options.WithAllowOverwrite(true)))

	return store, blockHash, priv.PubKey().Compressed()
}

func sampleWrappers() []*utxopersister.UTXOWrapper {
	txA := chainhash.HashH([]byte("tx-a"))
	txB := chainhash.HashH([]byte("tx-b"))

	return []*utxopersister.UTXOWrapper{
		{TxID: txA, Height: 100, Coinbase: true, UTXOs: []*utxopersister.UTXO{{Index: 0, Value: 5000000000, Script: []byte{0x51}}}},
		{TxID: txB, Height: 101, UTXOs: []*utxopersister.UTXO{{Index: 0, Value: 1000, Script: []byte{0x76, 0xa9}}}},
	}
}

func TestRunLoadsAndVerifies(t *testing.T) {
	ctx := context.Background()

	wrappers := sampleWrappers()
	seedStore, blockHash, trustedKey := buildSeed(t, wrappers, 101)
	utxoStore := newTestUTXOStore(t)

	cfg := Config{
		SeedStore:   seedStore,
		UTXOStore:   utxoStore,
		Lookup:      stubLookup{id: 7, height: 101, onMain: true},
		TrustedKeys: [][]byte{trustedKey},
		BlockHash:   blockHash,
	}

	require.NoError(t, Run(ctx, ulogger.TestLogger{}, cfg))

	// wrappers[1] is a non-coinbase tx, so its output is immediately spendable.
	txB := wrappers[1].TxID
	resp, err := utxoStore.GetSpend(ctx, &utxo.Spend{TxID: &txB, Vout: 0})
	require.NoError(t, err)
	require.Equal(t, int(utxo.Status_OK), resp.Status)

	// wrappers[0] is a coinbase mined at height 100; at the seed height it is
	// still within the maturity window and therefore loaded as IMMATURE.
	txA := wrappers[0].TxID
	respA, err := utxoStore.GetSpend(ctx, &utxo.Spend{TxID: &txA, Vout: 0})
	require.NoError(t, err)
	require.Equal(t, int(utxo.Status_IMMATURE), respA.Status)
}

func TestRunRejectsUntrustedKey(t *testing.T) {
	ctx := context.Background()

	seedStore, blockHash, _ := buildSeed(t, sampleWrappers(), 101)

	other, err := bec.NewPrivateKey()
	require.NoError(t, err)

	cfg := Config{SeedStore: seedStore, UTXOStore: newTestUTXOStore(t), Lookup: stubLookup{id: 1, height: 101, onMain: true}, TrustedKeys: [][]byte{other.PubKey().Compressed()}, BlockHash: blockHash}
	require.Error(t, Run(ctx, ulogger.TestLogger{}, cfg))
}

func TestRunRejectsNotOnMainChain(t *testing.T) {
	ctx := context.Background()

	seedStore, blockHash, trustedKey := buildSeed(t, sampleWrappers(), 101)

	cfg := Config{SeedStore: seedStore, UTXOStore: newTestUTXOStore(t), Lookup: stubLookup{id: 1, height: 101, onMain: false}, TrustedKeys: [][]byte{trustedKey}, BlockHash: blockHash}
	require.Error(t, Run(ctx, ulogger.TestLogger{}, cfg))
}

func TestRunRollsBackOnSetHashMismatch(t *testing.T) {
	ctx := context.Background()

	wrappers := sampleWrappers()
	seedStore, blockHash, _ := buildSeed(t, wrappers, 101)
	utxoStore := newTestUTXOStore(t)

	// Re-sign the checkpoint over a WRONG setHash with a key we control. The
	// chunks remain valid, so the set streams and loads; the recomputed digest
	// then disagrees with the signed setHash, forcing a rollback.
	priv, err := bec.NewPrivateKey()
	require.NoError(t, err)

	var wrongSetHash [32]byte
	for i := range wrongSetHash {
		wrongSetHash[i] = 0xee
	}

	sc, err := seedcheckpoint.Sign(priv, seedcheckpoint.Checkpoint{Height: 101, BlockHash: blockHash, SetHash: wrongSetHash})
	require.NoError(t, err)

	require.NoError(t, seedStore.Set(ctx, blockHash[:], fileformat.FileTypeSeedCheckpoint, sc.Serialize(), options.WithAllowOverwrite(true)))

	cfg := Config{
		SeedStore:   seedStore,
		UTXOStore:   utxoStore,
		Lookup:      stubLookup{id: 7, height: 101, onMain: true},
		TrustedKeys: [][]byte{priv.PubKey().Compressed()},
		BlockHash:   blockHash,
	}

	require.Error(t, Run(ctx, ulogger.TestLogger{}, cfg), "a set hash mismatch must fail")

	// Rollback must have deleted every record it created.
	for _, w := range wrappers {
		txid := w.TxID
		_, err := utxoStore.Get(ctx, &txid)
		require.Error(t, err, "record for %s should have been rolled back", txid.String())
	}
}

func TestRunRejectsTamperedSet(t *testing.T) {
	ctx := context.Background()

	seedStore, blockHash, trustedKey := buildSeed(t, sampleWrappers(), 101)

	// Corrupt the first chunk so the streamed body no longer hashes correctly.
	mb, err := seedStore.Get(ctx, blockHash[:], fileformat.FileTypeSeedManifest)
	require.NoError(t, err)

	m, err := seedpack.ParseManifest(mb)
	require.NoError(t, err)

	require.NoError(t, seedStore.Set(ctx, m.Chunks[0].Hash[:], fileformat.FileTypeSeedChunk, make([]byte, int(m.Chunks[0].Size)), options.WithAllowOverwrite(true)))

	cfg := Config{SeedStore: seedStore, UTXOStore: newTestUTXOStore(t), Lookup: stubLookup{id: 1, height: 101, onMain: true}, TrustedKeys: [][]byte{trustedKey}, BlockHash: blockHash}
	require.Error(t, Run(ctx, ulogger.TestLogger{}, cfg))
}

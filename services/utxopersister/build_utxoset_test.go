package utxopersister

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// p2pkhTx builds a distinct non-coinbase tx with a single spendable P2PKH
// output. Distinct `seed` bytes yield distinct txids. A P2PKH script is
// stored by ShouldStoreOutputAsUTXO in every era.
func p2pkhTx(t *testing.T, seed byte, satoshis uint64) *bt.Tx {
	t.Helper()

	b := make([]byte, 25)
	b[0], b[1], b[2] = 0x76, 0xa9, 0x14 // OP_DUP OP_HASH160 PUSH20
	for i := 3; i < 23; i++ {
		b[i] = seed
	}
	b[23], b[24] = 0x88, 0xac // OP_EQUALVERIFY OP_CHECKSIG

	ls := bscript.Script(b)

	tx := bt.NewTx()
	tx.AddOutput(&bt.Output{Satoshis: satoshis, LockingScript: &ls})

	return tx
}

// stageBlockDeltas writes real utxo-additions/utxo-deletions files for a
// synthetic block using the production writer (NewUTXOSet + ProcessTx + Close),
// keyed by blockHash.
func stageBlockDeltas(t *testing.T, ctx context.Context, tSettings *settings.Settings, store blob.Store, blockHash *chainhash.Hash, height uint32, txs ...*bt.Tx) {
	t.Helper()

	us, err := NewUTXOSet(ctx, ulogger.TestLogger{}, tSettings, store, blockHash, height)
	require.NoError(t, err)

	for _, tx := range txs {
		require.NoError(t, us.ProcessTx(tx))
	}

	require.NoError(t, us.Close())
}

// buildChainHeaders builds n contiguous headers for heights 1..n, each linking
// to the previous (block 1 links to genesis). Returns the header/meta slices
// (index 0 = height 1) and a height->hash map (indices 1..n).
func buildChainHeaders(t *testing.T, genesis *chainhash.Hash, n uint32) ([]*model.BlockHeader, []*model.BlockHeaderMeta, map[uint32]*chainhash.Hash) {
	t.Helper()

	nBits, err := model.NewNBitFromString("1d00ffff")
	require.NoError(t, err)

	merkle := chainhash.HashH([]byte("merkle-root-fixture"))

	var (
		headers = make([]*model.BlockHeader, 0, n)
		metas   = make([]*model.BlockHeaderMeta, 0, n)
		byH     = make(map[uint32]*chainhash.Hash, n)
		prev    = genesis
	)

	for h := uint32(1); h <= n; h++ {
		hdr := &model.BlockHeader{
			Version:        1,
			HashPrevBlock:  prev,
			HashMerkleRoot: &merkle,
			Timestamp:      1231006505 + h,
			Bits:           *nBits,
			Nonce:          h,
		}

		hash := hdr.Hash()
		byH[h] = hash
		prev = hash

		headers = append(headers, hdr)
		metas = append(metas, &model.BlockHeaderMeta{Height: h})
	}

	return headers, metas, byH
}

// readSetUTXOs parses a utxo-set file into a {txid:index -> value} map.
// GetUTXOSetReader strips the 8-byte fileformat magic; the per-file metadata
// CreateUTXOSet writes (32 block hash + 4 height + 32 previous hash = 68 bytes)
// is skipped before the wrapper records. End-of-records is the 16-byte footer,
// surfaced as io.EOF or "unexpected EOF" (mirrors CreateUTXOSet's own reader).
func readSetUTXOs(t *testing.T, ctx context.Context, tSettings *settings.Settings, store blob.Store, hash *chainhash.Hash) map[string]uint64 {
	t.Helper()

	us, err := GetUTXOSet(ctx, ulogger.TestLogger{}, tSettings, store, hash)
	require.NoError(t, err)

	r, err := us.GetUTXOSetReader(hash)
	require.NoError(t, err)
	defer r.Close()

	_, err = io.CopyN(io.Discard, r, 68)
	require.NoError(t, err)

	out := map[string]uint64{}

	for {
		w, err := NewUTXOWrapperFromReader(ctx, r)
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "unexpected EOF") {
				break
			}

			require.NoError(t, err)
		}

		for _, u := range w.UTXOs {
			out[fmt.Sprintf("%s:%d", w.TxID.String(), u.Index)] = u.Value
		}
	}

	return out
}

// newBuilderServer wires a Server whose header source is a mock blockchain
// client returning the given headers for the (from, endHeight) call. Direct
// blockchainStore is left nil, so the utxo-headers write is skipped — the
// consolidation, set write, and marker logic are all still exercised.
func newBuilderServer(t *testing.T, store blob.Store, headers []*model.BlockHeader, metas []*model.BlockHeaderMeta, from, end uint32) (*Server, *settings.Settings) {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)

	mockClient := &blockchain.Mock{}
	mockClient.On("GetBlockHeadersByHeight", mock.Anything, from, end).Return(headers, metas, nil)

	s := New(context.Background(), ulogger.TestLogger{}, tSettings, store, mockClient)

	return s, tSettings
}

func TestBuildUTXOSetToHeight_GenesisHappyPath(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	tSettings := test.CreateBaseTestSettings(t)
	genesis := tSettings.ChainCfgParams.GenesisHash

	headers, metas, byH := buildChainHeaders(t, genesis, 3)

	tx1 := p2pkhTx(t, 0x11, 1000)
	tx2 := p2pkhTx(t, 0x22, 2000)
	tx3 := p2pkhTx(t, 0x33, 3000)

	stageBlockDeltas(t, ctx, tSettings, store, byH[1], 1, tx1)
	stageBlockDeltas(t, ctx, tSettings, store, byH[2], 2, tx2)
	stageBlockDeltas(t, ctx, tSettings, store, byH[3], 3, tx3)

	s, _ := newBuilderServer(t, store, headers, metas, 1, 3)

	err := s.BuildUTXOSetToHeight(ctx, 0, 3, false)
	require.NoError(t, err)

	got := readSetUTXOs(t, ctx, tSettings, store, byH[3])
	require.Equal(t, map[string]uint64{
		fmt.Sprintf("%s:0", tx1.TxIDChainHash().String()): 1000,
		fmt.Sprintf("%s:0", tx2.TxIDChainHash().String()): 2000,
		fmt.Sprintf("%s:0", tx3.TxIDChainHash().String()): 3000,
	}, got)

	// updateLastProcessed=false must leave the marker untouched.
	h, err := s.readLastHeight(ctx)
	require.NoError(t, err)
	require.Equal(t, uint32(0), h)
}

func TestBuildUTXOSetToHeight_UpdatesLastProcessedWhenAsked(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	tSettings := test.CreateBaseTestSettings(t)
	genesis := tSettings.ChainCfgParams.GenesisHash

	headers, metas, byH := buildChainHeaders(t, genesis, 2)
	stageBlockDeltas(t, ctx, tSettings, store, byH[1], 1, p2pkhTx(t, 0x11, 1000))
	stageBlockDeltas(t, ctx, tSettings, store, byH[2], 2, p2pkhTx(t, 0x22, 2000))

	s, _ := newBuilderServer(t, store, headers, metas, 1, 2)

	err := s.BuildUTXOSetToHeight(ctx, 0, 2, true)
	require.NoError(t, err)

	h, err := s.readLastHeight(ctx)
	require.NoError(t, err)
	require.Equal(t, uint32(2), h)
}

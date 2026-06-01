package utxopersister

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/stretchr/testify/require"
)

func TestBuildAndReadSignedCheckpoint(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	blockHash := chainhash.HashH([]byte("checkpoint-block"))

	var setHash [32]byte
	for i := range setHash {
		setHash[i] = byte(i + 7)
	}

	require.NoError(t, persistSetHash(ctx, store, &blockHash, setHash))

	priv, err := bec.NewPrivateKey()
	require.NoError(t, err)

	sc, err := BuildSignedCheckpoint(ctx, store, blockHash, 750000, priv)
	require.NoError(t, err)

	require.Equal(t, setHash, sc.Checkpoint.SetHash)
	require.Equal(t, uint32(750000), sc.Checkpoint.Height)
	require.Equal(t, blockHash, sc.Checkpoint.BlockHash)
	require.NoError(t, sc.VerifyWithKey(priv.PubKey().Compressed()))

	got, err := readSignedCheckpoint(ctx, store, blockHash)
	require.NoError(t, err)
	require.NoError(t, got.Verify())
	require.Equal(t, sc.Checkpoint, got.Checkpoint)
}

func TestBuildSignedCheckpointMissingSetHash(t *testing.T) {
	ctx := context.Background()
	store := memory.New()

	priv, err := bec.NewPrivateKey()
	require.NoError(t, err)

	_, err = BuildSignedCheckpoint(ctx, store, chainhash.HashH([]byte("absent")), 1, priv)
	require.Error(t, err, "building a checkpoint without a persisted set hash must fail")
}

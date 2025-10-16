package sql

import (
	"net/url"
	"testing"

	"github.com/bsv-blockchain/teranode/stores/blockchain/options"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetBlockHeader(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)

	t.Run("missing", func(t *testing.T) {
		storeURL, err := url.Parse("sqlitememory:///")
		require.NoError(t, err)

		s, err := New(ulogger.TestLogger{}, storeURL, tSettings)
		require.NoError(t, err)

		_, _, err = s.GetBlockHeader(t.Context(), block2.Hash())
		require.Error(t, err, "should error on missing block")
	})

	t.Run("normal block header", func(t *testing.T) {
		storeURL, err := url.Parse("sqlitememory:///")
		require.NoError(t, err)

		s, err := New(ulogger.TestLogger{}, storeURL, tSettings)
		require.NoError(t, err)

		_, _, err = s.StoreBlock(t.Context(), block1, "test_peer")
		require.NoError(t, err)

		_, _, err = s.StoreBlock(t.Context(), block2, "test_peer")
		require.NoError(t, err)

		header, meta, err := s.GetBlockHeader(t.Context(), block2.Hash())
		require.NoError(t, err, "should get block header")
		require.NotNil(t, header)
		require.NotNil(t, meta)

		// Check header
		assert.Equal(t, uint32(2), meta.Height)
		assert.Equal(t, block2.Hash(), header.Hash())
		assert.Equal(t, uint32(1), header.Version)
		assert.Equal(t, block2PrevBlockHash, header.HashPrevBlock)
		assert.Equal(t, block2MerkleRootHash, header.HashMerkleRoot)
		assert.Equal(t, uint32(1729259727), header.Timestamp)
		assert.Equal(t, *bits, header.Bits)
		assert.Equal(t, uint32(1), header.Nonce)

		// Check meta
		assert.Equal(t, uint32(2), meta.Height)
		assert.Equal(t, uint64(1), meta.TxCount)
		assert.Equal(t, "test_peer", meta.PeerID)
		assert.Equal(t, uint64(0), meta.SizeInBytes)
		assert.Equal(t, uint32(2), meta.ID)
		assert.Equal(t, []byte{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x6}, meta.ChainWork)
		assert.Equal(t, false, meta.MinedSet)
		assert.Equal(t, false, meta.SubtreesSet)
		assert.Equal(t, false, meta.Invalid)
	})

	t.Run("with block creation options", func(t *testing.T) {
		storeURL, err := url.Parse("sqlitememory:///")
		require.NoError(t, err)

		s, err := New(ulogger.TestLogger{}, storeURL, tSettings)
		require.NoError(t, err)

		_, _, err = s.StoreBlock(t.Context(), block1, "test_peer", options.WithMinedSet(true), options.WithSubtreesSet(true), options.WithInvalid(true))
		require.NoError(t, err)

		header, meta, err := s.GetBlockHeader(t.Context(), block1.Hash())
		require.NoError(t, err, "should get block header")
		require.NotNil(t, header)
		require.NotNil(t, meta)

		// check the options
		assert.Equal(t, true, meta.MinedSet)
		assert.Equal(t, true, meta.SubtreesSet)
		assert.Equal(t, true, meta.Invalid)
	})
}

// func TestGetBlockHeaderProcessedAt(t *testing.T) {
// 	tSettings := test.CreateBaseTestSettings(t)
// 	tSettings.ChainCfgParams = &chaincfg.TeraTestNetParams

// 	storeURL, err := url.Parse("postgres://teranode:teranode@localhost:5432/teranode")
// 	require.NoError(t, err)

// 	s, err := New(ulogger.TestLogger{}, storeURL, tSettings)
// 	require.NoError(t, err)

// 	h, err := chainhash.NewHashFromStr("00000000a39333317cc1ace0814107cca6caf2fe012f4a096a4846ccefa72616")
// 	require.NoError(t, err)

// 	header, meta, err := s.GetBlockHeader(t.Context(), h)
// 	require.NoError(t, err, "should get block header")
// 	require.NotNil(t, header)

// 	require.NotNil(t, meta)

// 	t.Logf("meta: %#v", meta)
// }

package smoke

import (
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/daemon"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
	"github.com/bsv-blockchain/teranode/test"
	"github.com/bsv-blockchain/teranode/test/utils/transactions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManualSync(t *testing.T) {
	createNode := func(t *testing.T, nodeNumber int) *daemon.TestDaemon {
		return daemon.NewTestDaemon(t, daemon.TestOptions{
			UTXOStoreType:     "aerospike",
			SkipRemoveDataDir: nodeNumber > 1,
			// EnableDebugLogging: true,
			PreserveDataDir: true,
			SettingsOverrideFunc: func(s *settings.Settings) {
				test.MultiNodeSettings(nodeNumber)(s)
				s.GlobalBlockHeightRetention = 1
				s.BlockAssembly.InitialMerkleItemsPerSubtree = 1024
				s.BlockAssembly.MaximumMerkleItemsPerSubtree = 32768
			},
			FSMState: blockchain.FSMStateRUNNING,
		})
	}

	node1 := createNode(t, 1)
	defer node1.Stop(t)

	node2 := createNode(t, 2)
	defer node2.Stop(t)

	coinbaseTx := node1.MineToMaturityAndGetSpendableCoinbaseTx(t, node1.Ctx)
	t.Logf("Node1 mined to maturity, coinbase tx: %s", coinbaseTx.TxIDChainHash().String())

	block1, err := node1.BlockchainClient.GetBlockByHeight(node1.Ctx, 1)
	require.NoError(t, err)

	err = node2.BlockValidation.ValidateBlock(node2.Ctx, block1, "legacy", false)
	require.NoError(t, err)

	block2, err := node1.BlockchainClient.GetBlockByHeight(node1.Ctx, 2)
	require.NoError(t, err)

	err = node2.BlockValidation.ValidateBlock(node2.Ctx, block2, "legacy", false)
	require.NoError(t, err)

	// Phase 3: Create a chain of 10 transactions
	t.Log("Creating chain of 10 transactions...")

	var txs []*bt.Tx

	tx := coinbaseTx

	for i := 0; i < 10; i++ {
		tx = node1.CreateTransactionWithOptions(t,
			transactions.WithInput(tx, 0),
			transactions.WithP2PKHOutputs(1, tx.Outputs[0].Satoshis-1000),
		)

		if i < 4 {
			err = node1.PropagationClient.ProcessTransaction(node1.Ctx, tx)
			require.NoError(t, err, "Failed to process tx[%d] through propagation client", i)

			err = node1.WaitForTransactionInBlockAssembly(tx, 10*time.Second)
			require.NoError(t, err, "Tx[%d] not in node1 block assembly", i)
		}

		err = node2.PropagationClient.ProcessTransaction(node2.Ctx, tx)
		require.NoError(t, err, "Failed to process tx[%d] through propagation client", i)

		err = node2.WaitForTransactionInBlockAssembly(tx, 10*time.Second)
		require.NoError(t, err, "Tx[%d] not in node2 block assembly", i)

		txs = append(txs, tx)
	}

	// Node1 mines 10 blocks
	t.Log("Node1 mining 10 blocks...")
	for i := 0; i < 10; i++ {
		block := node1.MineAndWait(t, 1)

		if i == 0 {
			copySubtreesToNode(t, node1, node2, block, txs[0:4])

		}

		err := node2.BlockValidation.ValidateBlock(node2.Ctx, block, "legacy", false)
		require.NoError(t, err, "Failed to validate block %d", i)

		node2.WaitForBlockHeight(t, block, 10*time.Second)
	}

	// Check no transactions in node1's block assembly
	t.Log("Phase 8: Verifying node1 block assembly is empty...")
	node1Txs, err := node1.BlockAssemblyClient.GetTransactionHashes(node1.Ctx)
	require.NoError(t, err)
	assert.Len(t, node1Txs, 1, "Node1 should have 1 transaction in block assembly")

	// Check node2 has exactly 7 transactions in block assembly (coinbase placeholder + txs 5-10)
	t.Log("Verifying node2 has 7 transactions in block assembly...")
	node2Txs, err := node2.BlockAssemblyClient.GetTransactionHashes(node2.Ctx)
	require.NoError(t, err)
	assert.Len(t, node2Txs, 7, "Node2 should have 6 transactions in block assembly")

	node1.WaitForPruner(t, 10*time.Second)
	node2.WaitForPruner(t, 10*time.Second)

	node2block2 := node2.MineAndWait(t, 1)

	copySubtreesToNode(t, node2, node1, node2block2, txs[4:])

	err = node1.BlockValidation.ValidateBlock(node1.Ctx, node2block2, "legacy", false)
	require.NoError(t, err)

	node1.WaitForBlock(t, node2block2, 10*time.Second)

	node1Txs, err = node1.BlockAssemblyClient.GetTransactionHashes(node1.Ctx)
	require.NoError(t, err)
	assert.Len(t, node1Txs, 1, "Node1 should have 1 transaction in block assembly")

	node2Txs, err = node2.BlockAssemblyClient.GetTransactionHashes(node2.Ctx)
	require.NoError(t, err)
	assert.Len(t, node2Txs, 1, "Node2 should have 1 transaction in block assembly")
}

func copySubtreesToNode(t *testing.T, srcNode, dstNode *daemon.TestDaemon, block *model.Block, txs []*bt.Tx) {
	t.Helper()

	txByHash := make(map[[32]byte]*bt.Tx, len(txs))
	for _, ntx := range txs {
		txByHash[*ntx.TxIDChainHash()] = ntx
	}

	for _, subtreeHash := range block.Subtrees {
		subtreeBytes, err := srcNode.SubtreeStore.Get(srcNode.Ctx, subtreeHash[:], fileformat.FileTypeSubtree)
		require.NoError(t, err)

		err = dstNode.SubtreeStore.Set(dstNode.Ctx, subtreeHash[:], fileformat.FileTypeSubtreeToCheck, subtreeBytes, options.WithAllowOverwrite(true))
		require.NoError(t, err)

		st, err := subtree.NewSubtreeFromBytes(subtreeBytes)
		require.NoError(t, err)

		subtreeData := subtree.NewSubtreeData(st)
		subtreeMeta := subtree.NewSubtreeMeta(st)

		for idx, node := range st.Nodes {
			if node.Hash.Equal(subtree.CoinbasePlaceholderHashValue) {
				continue
			}

			ntx, ok := txByHash[node.Hash]
			require.True(t, ok, "transaction not found for subtree node hash %s", node.Hash)

			err = subtreeData.AddTx(ntx, idx)
			require.NoError(t, err)

			err = subtreeMeta.SetTxInpointsFromTx(ntx)
			require.NoError(t, err)
		}

		subtreeDataBytes, err := subtreeData.Serialize()
		require.NoError(t, err)

		err = dstNode.SubtreeStore.Set(dstNode.Ctx, subtreeHash[:], fileformat.FileTypeSubtreeData, subtreeDataBytes, options.WithAllowOverwrite(true))
		require.NoError(t, err)

		subtreeMetaBytes, err := subtreeMeta.Serialize()
		require.NoError(t, err)

		err = dstNode.SubtreeStore.Set(dstNode.Ctx, subtreeHash[:], fileformat.FileTypeSubtreeMeta, subtreeMetaBytes, options.WithAllowOverwrite(true))
		require.NoError(t, err)
	}
}

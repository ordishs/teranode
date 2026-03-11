package smoke

import (
	"fmt"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/teranode/daemon"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/test"
	"github.com/bsv-blockchain/teranode/test/utils/transactions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_MultiNode_BlockAssembly_After_Sync tests that transactions submitted only to node2
// remain in node2's block assembly after node1 mines blocks and node2 syncs.
//
// Scenario:
//  1. Create two nodes and inject them as peers
//  2. Mine to maturity on node1, sync node2
//  3. Create a chain of 10 transactions from the coinbase
//  4. Submit txs 1-5 to both nodes
//  5. Submit txs 6-10 to node2 only
//  6. Node1 mines 10 blocks (includes txs 1-5)
//  7. Node2 syncs to node1's height
//  8. Verify node1 has no transactions in block assembly
//  9. Verify node2 has exactly 5 transactions (txs 6-10) in block assembly
func Test_MultiNode_BlockAssembly_After_Sync(t *testing.T) {
	const (
		numTxs       = 2
		numSharedTxs = 1
	)

	p2pPort1, err := daemon.GetFreePort()
	require.NoError(t, err)

	p2pPort2, err := daemon.GetFreePort()
	require.NoError(t, err)

	// print the ports
	t.Logf("P2P Port 1: %d", p2pPort1)
	t.Logf("P2P Port 2: %d", p2pPort2)

	staticPeer1 := fmt.Sprintf("/dns/localhost/tcp/%d/p2p/12D3KooWAFXWuxgdJoRsaA4J4RRRr8yu6WCrAPf8FaS7UfZg3ceG", p2pPort1)
	staticPeer2 := fmt.Sprintf("/dns/localhost/tcp/%d/p2p/12D3KooWG6aCkDmi5tqx4G4AvVDTQdSVvTSzzQvk1vh9CtSR8KEW", p2pPort2)

	createNode := func(t *testing.T, nodeNumber int) *daemon.TestDaemon {
		var port int
		switch nodeNumber {
		case 1:
			port = p2pPort1
		case 2:
			port = p2pPort2
		default:
			t.Fatalf("Invalid node number: %d", nodeNumber)
		}
		return daemon.NewTestDaemon(t, daemon.TestOptions{
			// EnableP2P:         true,
			UTXOStoreType:     "aerospike",
			SkipRemoveDataDir: nodeNumber > 1,
			// EnableDebugLogging: true,
			PreserveDataDir: true,
			SettingsOverrideFunc: func(s *settings.Settings) {
				test.MultiNodeSettings(nodeNumber)(s)
				s.P2P.PeerCacheDir = t.TempDir()
				s.P2P.SyncCoordinatorPeriodicEvaluationInterval = 1 * time.Second
				s.GlobalBlockHeightRetention = 1
				s.P2P.DHTMode = "client"
				s.P2P.Port = port
				s.P2P.StaticPeers = []string{staticPeer1, staticPeer2}
				s.P2P.BootstrapPeers = []string{staticPeer1, staticPeer2}
				s.P2P.AllowPrivateIPs = true
				s.P2P.ListenMode = "full"
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

	// Wait for both nodes to discover each other via static peers
	// require.Eventually(t, func() bool {
	// 	peers, err := node1.P2PClient.GetPeerRegistry(node1.Ctx)
	// 	return err == nil && len(peers) > 0
	// }, 30*time.Second, 1*time.Second, "Node1 should discover at least one peer")

	// require.Eventually(t, func() bool {
	// 	peers, err := node2.P2PClient.GetPeerRegistry(node2.Ctx)
	// 	return err == nil && len(peers) > 0
	// }, 30*time.Second, 1*time.Second, "Node2 should discover at least one peer")

	// Phase 2: Mine to maturity on node1 and sync node2
	t.Log("Phase 2: Mining to maturity on node1...")
	coinbaseTx := node1.MineToMaturityAndGetSpendableCoinbaseTx(t, node1.Ctx)
	t.Logf("Node1 mined to maturity, coinbase tx: %s", coinbaseTx.TxIDChainHash().String())

	_, node1Meta, err := node1.BlockchainClient.GetBestBlockHeader(node1.Ctx)
	require.NoError(t, err)
	t.Logf("Node1 at height %d", node1Meta.Height)

	block1Node1, err := node1.BlockchainClient.GetBlockByHeight(node1.Ctx, 1)
	require.NoError(t, err)
	t.Logf("Node1 block 1: %s", block1Node1.Header.Hash().String())

	node2.WaitForBlock(t, block1Node1, 30*time.Second)

	// Phase 3: Create a chain of 10 transactions
	t.Log("Phase 3: Creating chain of 10 transactions...")
	chainTxs := make([]*bt.Tx, numTxs)

	// tx0 spends the coinbase
	chainTxs[0] = node1.CreateTransactionWithOptions(t,
		transactions.WithInput(coinbaseTx, 0),
		transactions.WithP2PKHOutputs(1, coinbaseTx.Outputs[0].Satoshis-1000),
	)

	// Each subsequent tx spends the previous one
	for i := 1; i < numTxs; i++ {
		chainTxs[i] = node2.CreateTransactionWithOptions(t,
			transactions.WithInput(chainTxs[i-1], 0),
			transactions.WithP2PKHOutputs(1, chainTxs[i-1].Outputs[0].Satoshis-1000),
		)
	}

	// for i, tx := range chainTxs {
	// 	t.Logf("Chain tx[%d]: %s", i, tx.TxIDChainHash().String())
	// }

	// Phase 4: Submit txs 1-5 (indices 0-4) to both nodes
	t.Log("Phase 4: Submitting txs 1-5 to both nodes...")
	ctx := node1.Ctx
	for i := 0; i < numSharedTxs; i++ {
		// txHex := hex.EncodeToString(chainTxs[i].ExtendedBytes())

		// _, err := node1.CallRPC(ctx, "sendrawtransaction", []any{txHex})
		// require.NoError(t, err, "Failed to submit tx[%d] to node1", i)

		err = node1.PropagationClient.ProcessTransaction(ctx, chainTxs[i])
		require.NoError(t, err, "Failed to process tx[%d] through propagation client", i)

		err = node1.WaitForTransactionInBlockAssembly(chainTxs[i], 10*time.Second)
		require.NoError(t, err, "Tx[%d] not in node1 block assembly", i)

		// _, err = node2.CallRPC(node2.Ctx, "sendrawtransaction", []any{txHex})
		// require.NoError(t, err, "Failed to submit tx[%d] to node2", i)

		err = node2.PropagationClient.ProcessTransaction(node2.Ctx, chainTxs[i])
		require.NoError(t, err, "Failed to process tx[%d] through propagation client", i)

		err = node2.WaitForTransactionInBlockAssembly(chainTxs[i], 10*time.Second)
		require.NoError(t, err, "Tx[%d] not in node2 block assembly", i)

		t.Logf("Tx[%d] confirmed in both nodes' block assembly", i)
	}

	// Phase 5: Submit txs 6-10 (indices 5-9) to node2 only
	t.Log("Phase 5: Submitting txs 6-10 to node2 only...")
	for i := numSharedTxs; i < numTxs; i++ {
		// txHex := hex.EncodeToString(chainTxs[i].ExtendedBytes())

		// _, err := node2.CallRPC(node2.Ctx, "sendrawtransaction", []any{txHex})
		// require.NoError(t, err, "Failed to submit tx[%d] to node2", i)

		err := node2.PropagationClient.ProcessTransaction(node2.Ctx, chainTxs[i])
		require.NoError(t, err, "Failed to process tx[%d] through propagation client", i)

		err = node2.WaitForTransactionInBlockAssembly(chainTxs[i], 10*time.Second)
		require.NoError(t, err, "Tx[%d] not in node2 block assembly", i)

		t.Logf("Tx[%d] confirmed in node2 block assembly only", i)
	}

	// Debug: Check node1's block assembly before mining to see if node2-only txs propagated
	t.Log("=== Node1 block assembly BEFORE mining ===")
	node1TxsBefore, err := node1.BlockAssemblyClient.GetTransactionHashes(node1.Ctx)
	require.NoError(t, err)
	t.Logf("Node1 has %d transactions in block assembly before mining", len(node1TxsBefore))

	// Check node1's UTXO store for node2-only txs
	// t.Log("=== Node1 UTXO store for node2-only txs BEFORE mining ===")
	// for i := numSharedTxs; i < numTxs; i++ {
	// 	txHash := chainTxs[i].TxIDChainHash()
	// 	_, getErr := node1.UtxoStore.Get(node1.Ctx, txHash)
	// 	if getErr != nil {
	// 		t.Logf("  Node1 UTXO tx[%d] %s: NOT in UTXO store (%v)", i, txHash, getErr)
	// 	} else {
	// 		t.Logf("  Node1 UTXO tx[%d] %s: EXISTS in UTXO store!", i, txHash)
	// 	}
	// }

	// Phase 6: Node1 mines 10 blocks
	t.Log("Phase 6: Node1 mining 9 blocks...")
	node1.MineAndWait(t, 9)
	lastBlock := node1.MineAndWait(t, 1)
	t.Logf("Node1 mined 10 blocks, now at height %d", lastBlock.Height)

	time.Sleep(10 * time.Second)

	// Phase 7: Wait for node2 to catch up
	t.Log("Phase 7: Waiting for node2 to sync...")
	// node2.InjectPeer(t, node1)
	node2.WaitForBlockhash(t, lastBlock.Hash(), 30*time.Second)

	node2BestHeader, node2Meta, err := node2.BlockchainClient.GetBestBlockHeader(node2.Ctx)
	require.NoError(t, err)
	require.Equal(t, lastBlock.Height, node2Meta.Height, "Node2 should be at same height as Node1")
	require.Equal(t, lastBlock.Hash().String(), node2BestHeader.Hash().String(), "Node2 should have same best block as Node1")
	t.Logf("Node2 synced to height %d", node2Meta.Height)

	// Debug: Check UTXO store state for all 10 transactions on node2
	// t.Log("=== UTXO Store state on Node2 after sync ===")
	// for i, tx := range chainTxs {
	// 	txHash := tx.TxIDChainHash()
	// 	rawTx := GetRawTx(t, node2.UtxoStore, *txHash,
	// 		fields.DeleteAtHeight.String(),
	// 		fields.BlockHeights.String(),
	// 		string(fields.Utxos),
	// 		fields.TotalUtxos.String(),
	// 		fields.UnminedSince.String(),
	// 		fields.External.String(),
	// 	)
	// 	if rawTx != nil {
	// 		label := fmt.Sprintf("Node2 chainTx[%d] %s", i, txHash.String())
	// 		if i < numSharedTxs {
	// 			label += " (shared)"
	// 		} else {
	// 			label += " (node2-only)"
	// 		}
	// 		PrintRawTx(t, label, rawTx.(map[string]interface{}))
	// 	} else {
	// 		t.Logf("Node2 chainTx[%d] %s: NOT FOUND in UTXO store", i, txHash.String())
	// 	}
	// }
	// t.Log("=== End UTXO Store state ===")

	// Phase 8: Check no transactions in node1's block assembly
	t.Log("Phase 8: Verifying node1 block assembly is empty...")
	node1Txs, err := node1.BlockAssemblyClient.GetTransactionHashes(node1.Ctx)
	require.NoError(t, err)
	assert.Len(t, node1Txs, 1, "Node1 should have 1 transaction in block assembly")

	// Phase 9: Check node2 has exactly 5 transactions in block assembly (txs 6-10)
	t.Log("Phase 9: Verifying node2 has 6 transactions in block assembly...")
	node2Txs, err := node2.BlockAssemblyClient.GetTransactionHashes(node2.Ctx)
	require.NoError(t, err)
	assert.Len(t, node2Txs, 6, "Node2 should have 6 transactions in block assembly")
	t.Logf("PASS: Node2 has %d transactions in block assembly", len(node2Txs))

	// Verify the specific transactions are the ones we expect (txs 6-10)
	node2.VerifyInBlockAssembly(t, chainTxs[numSharedTxs:]...)
	t.Log("PASS: All expected transactions verified in node2 block assembly")

	node1.WaitForPruner(t, 10*time.Second)
	node2.WaitForPruner(t, 10*time.Second)

	node2block2 := node2.MineAndWait(t, 1)
	// node1.InjectPeer(t, node2)
	node1.WaitForBlockhash(t, node2block2.Hash(), 30*time.Second)

	node1Txs, err = node1.BlockAssemblyClient.GetTransactionHashes(node1.Ctx)
	require.NoError(t, err)
	assert.Len(t, node1Txs, 1, "Node1 should have 1 transaction in block assembly")

	node2Txs, err = node2.BlockAssemblyClient.GetTransactionHashes(node2.Ctx)
	require.NoError(t, err)
	assert.Len(t, node2Txs, 1, "Node2 should have 1 transaction in block assembly")

	time.Sleep(10 * time.Second)

	err = node1.BlockValidation.ValidateBlock(node1.Ctx, node2block2, "legacyurl", false)
	require.NoError(t, err)

}

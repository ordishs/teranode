package subtreeprocessor

import (
	"context"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	blob_memory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	blockchainoptions "github.com/bsv-blockchain/teranode/stores/blockchain/options"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// countPendingScans wraps a real store and counts the pending-intent scans. failComplete makes every
// CompleteConflictIntent fail.
type countPendingScans struct {
	utxo.Store
	scans        atomic.Int32
	failComplete atomic.Bool
}

func (s *countPendingScans) CompleteConflictIntent(ctx context.Context, intentID chainhash.Hash) error {
	if s.failComplete.Load() {
		return errors.NewStorageError("injected WAL completion failure")
	}

	return s.Store.CompleteConflictIntent(ctx, intentID)
}

func (s *countPendingScans) PendingConflictIntents(ctx context.Context) ([]utxo.ConflictIntent, error) {
	s.scans.Add(1)

	return s.Store.PendingConflictIntents(ctx)
}

// walTestChain is a real sqlitememory blockchain: genesis and two blocks on top of it, stored with
// their mined flag set. moveForward holds the two blocks for a reset or a moveForwardBlock.
type walTestChain struct {
	genesis     *model.BlockHeader
	moveForward []*model.Block
}

func newWALTestChain(t *testing.T) (blockchain.ClientI, *walTestChain) {
	t.Helper()

	ctx := t.Context()
	tSettings := test.CreateBaseTestSettings(t)

	storeURL, err := url.Parse("sqlitememory://")
	require.NoError(t, err)

	blockchainStore, err := blockchainstore.NewStore(ulogger.TestLogger{}, storeURL, tSettings)
	require.NoError(t, err)

	client, err := blockchain.NewLocalClient(ulogger.TestLogger{}, tSettings, blockchainStore, nil, nil)
	require.NoError(t, err)

	genesis, _, err := client.GetBestBlockHeader(ctx)
	require.NoError(t, err)

	first := childHeader(genesis, 301)
	chain := &walTestChain{
		genesis: genesis,
		moveForward: []*model.Block{
			{Height: 1, CoinbaseTx: coinbaseTx, TransactionCount: 1, Subtrees: []*chainhash.Hash{}, Header: first},
			{Height: 2, CoinbaseTx: coinbaseTx3, TransactionCount: 1, Subtrees: []*chainhash.Hash{}, Header: childHeader(first, 302)},
		},
	}

	for _, block := range chain.moveForward {
		require.NoError(t, client.AddBlock(ctx, block, "", blockchainoptions.WithMinedSet(true)))
	}

	return client, chain
}

func newResetWALTestProcessor(t *testing.T, start bool) (*SubtreeProcessor, *countPendingScans, *blob_memory.Memory, *walTestChain) {
	t.Helper()

	ctx := t.Context()
	blockchainClient, chain := newWALTestChain(t)

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	sqlStore, err := sql.New(ctx, ulogger.TestLogger{}, test.CreateBaseTestSettings(t), utxoStoreURL)
	require.NoError(t, err)

	store := &countPendingScans{Store: sqlStore}
	subtreeStore := blob_memory.New()

	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()

	stp, err := NewSubtreeProcessor(ctx, ulogger.TestLogger{}, test.CreateBaseTestSettings(t), subtreeStore, blockchainClient, store, newSubtreeChan)
	require.NoError(t, err)

	if start {
		stp.Start(ctx)
	}

	t.Cleanup(func() {
		stp.Stop(context.Background())
		close(newSubtreeChan)
	})

	stp.InitCurrentBlockHeader(chain.genesis)

	return stp, store, subtreeStore, chain
}

func resetWALSpendTx(t *testing.T, parent *bt.Tx, satoshis uint64) *bt.Tx {
	t.Helper()

	tx := bt.NewTx()
	require.NoError(t, tx.From(parent.TxIDChainHash().String(), 0, parent.Outputs[0].LockingScript.String(), parent.Outputs[0].Satoshis))
	tx.Inputs[0].UnlockingScript = bscript.NewFromBytes([]byte{})
	tx.Outputs = []*bt.Output{{Satoshis: satoshis, LockingScript: bscript.NewFromBytes([]byte{0x52})}}

	return tx
}

// A reset scans the pending intents once for all its moveForward blocks, not once per block.
func TestReset_ScansPendingIntentsOnce(t *testing.T) {
	stp, store, _, chain := newResetWALTestProcessor(t, true)
	moveForward := chain.moveForward

	response := stp.Reset(chain.genesis, nil, moveForward, false, func() error { return nil })
	require.NoError(t, response.Err)
	require.Equal(t, moveForward[1].Header.Hash(), stp.GetCurrentBlockHeader().Hash(), "reset must land on the last moveForward block")

	require.Equal(t, int32(1), store.scans.Load(), "a reset must scan the pending intents exactly once")
}

// createResetWALConflict stores a parent, a loser that spends it and a conflicting winner that
// spends the same output, and returns the loser and the winner.
func createResetWALConflict(t *testing.T, store utxo.Store, seed byte) (*bt.Tx, *bt.Tx) {
	t.Helper()

	ctx := t.Context()

	parent := bt.NewTx()
	parentIn := &bt.Input{PreviousTxOutIndex: 0, PreviousTxSatoshis: 200000, SequenceNumber: 0xFFFFFFFF, UnlockingScript: bscript.NewFromBytes([]byte{})}
	_ = parentIn.PreviousTxIDAdd(&chainhash.Hash{9, 9, seed})
	parent.Inputs = []*bt.Input{parentIn}
	parent.Outputs = []*bt.Output{{Satoshis: 100000, LockingScript: bscript.NewFromBytes([]byte{0x52})}}
	_, _, err := store.SpendAndCreate(ctx, parent, 1, utxo.WithCreateOnly())
	require.NoError(t, err)

	loser := resetWALSpendTx(t, parent, 90000)
	_, _, err = store.SpendAndCreate(ctx, loser, 2)
	require.NoError(t, err)

	winner := resetWALSpendTx(t, parent, 80000)
	_, _, err = store.SpendAndCreate(ctx, winner, 2, utxo.WithCreateOnly(), utxo.WithConflicting(true))
	require.NoError(t, err)

	return loser, winner
}

// A deferred drain whose acknowledgement failed leaves a finished forward intent pending. The
// follow-up reset may take the fast-forward branch, and it must still complete that intent.
func TestReset_FastForwardCompletesWALIntent(t *testing.T) {
	ctx := t.Context()
	stp, store, _, chain := newResetWALTestProcessor(t, true)
	moveForward := chain.moveForward

	_, winner := createResetWALConflict(t, store, 1)
	_, _, err := utxo.ProcessConflicting(ctx, store, 1, *moveForward[0].Hash(), []chainhash.Hash{*winner.TxIDChainHash()}, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)

	otherBlock := chainhash.HashH([]byte("reset-wal-other-block"))
	_, otherWinner := createResetWALConflict(t, store, 2)
	_, _, err = utxo.ProcessConflicting(ctx, store, 1, otherBlock, []chainhash.Hash{*otherWinner.TxIDChainHash()}, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)

	pending, err := store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 2)

	store.scans.Store(0)

	response := stp.Reset(chain.genesis, nil, moveForward, true, func() error { return nil })
	require.NoError(t, response.Err)
	require.Equal(t, moveForward[1].Header.Hash(), stp.GetCurrentBlockHeader().Hash(), "reset must land on the last moveForward block")

	scans := store.scans.Load()

	pending, err = store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the fast-forward reset must complete the intent of its moveForward block")
	require.Equal(t, otherBlock, pending[0].BlockHash, "an intent for a block outside the reset must stay pending")

	require.Equal(t, int32(1), scans, "a reset must scan the pending intents exactly once")
}

// A reset over a block with a conflicting node runs ProcessConflicting with deferred completion. The
// reset must still complete that block's WAL intent after the block is applied.
func TestReset_ConflictingNodeCompletesWALIntent(t *testing.T) {
	ctx := t.Context()
	stp, store, subtreeStore, chain := newResetWALTestProcessor(t, true)

	loser, winner := createResetWALConflict(t, store, 9)

	moveForward := chain.moveForward
	moveForward[0].Subtrees = []*chainhash.Hash{storeConflictSubtree(t, subtreeStore, winner)}

	response := stp.Reset(chain.genesis, nil, moveForward, false, func() error { return nil })
	require.NoError(t, response.Err)

	pending, err := store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "the reset must complete the WAL intent of the conflicting block")

	spend, err := utxo.GetSpends(winner)
	require.NoError(t, err)

	owner, err := store.GetSpend(ctx, spend[0])
	require.NoError(t, err)
	require.Equal(t, winner.TxIDChainHash(), owner.SpendingData.TxID)

	m, err := store.Get(ctx, loser.TxIDChainHash(), fields.Conflicting)
	require.NoError(t, err)
	require.True(t, m.Conflicting)
}

// storeConflictSubtree stores a subtree that lists winner as a conflicting node and returns its hash.
func storeConflictSubtree(t *testing.T, subtreeStore *blob_memory.Memory, winner *bt.Tx) *chainhash.Hash {
	t.Helper()

	st, err := subtreepkg.NewTreeByLeafCount(4)
	require.NoError(t, err)
	require.NoError(t, st.AddCoinbaseNode())
	require.NoError(t, st.AddNode(*winner.TxIDChainHash(), 1, 1))
	require.NoError(t, st.AddConflictingNode(*winner.TxIDChainHash()))

	subtreeBytes, err := st.Serialize()
	require.NoError(t, err)
	require.NoError(t, subtreeStore.Set(t.Context(), st.RootHash()[:], fileformat.FileTypeSubtree, subtreeBytes))

	return st.RootHash()
}

// A failed WAL acknowledgement after moveForwardBlock's commit point must not fail the call. The
// call's swap-back would install the cleared shadow over the applied block. It requests a reset.
func TestMoveForwardBlock_AckFailureAfterCommitRequestsReset(t *testing.T) {
	ctx := t.Context()
	stp, store, subtreeStore, chain := newResetWALTestProcessor(t, false)

	loser, winner := createResetWALConflict(t, store, 8)

	block := chain.moveForward[0]
	block.Subtrees = []*chainhash.Hash{storeConflictSubtree(t, subtreeStore, winner)}

	store.failComplete.Store(true)

	_, _, err := stp.moveForwardBlock(ctx, block, true, map[chainhash.Hash]struct{}{}, true, false)
	require.NoError(t, err, "the block is applied; a failed acknowledgement must not fail the call")
	require.True(t, stp.TakeDrainResetRequested(), "the failed acknowledgement must request a reset")

	pending, err := store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the unacknowledged intent stays pending")

	m, err := store.Get(ctx, loser.TxIDChainHash(), fields.Conflicting)
	require.NoError(t, err)
	require.True(t, m.Conflicting)
}

// The deferred drain acknowledges the block's forward intent because the block has conflicting
// nodes, even when the operation demoted no transaction.
func TestRunDeferredBlockDrain_AcknowledgesWithoutDemotedTxs(t *testing.T) {
	ctx := t.Context()
	stp, store, _, chain := newResetWALTestProcessor(t, false)

	_, winner := createResetWALConflict(t, store, 7)

	block := chain.moveForward[0]

	_, _, err := utxo.ProcessConflicting(ctx, store, 1, *block.Hash(), []chainhash.Hash{*winner.TxIDChainHash()}, nil, utxo.NoAncestryGuard, utxo.WithDeferredConflictCompletion())
	require.NoError(t, err)

	stp.runDeferredBlockDrain(ctx, block, &deferredBlockDrain{ackConflictIntents: true})

	require.False(t, stp.TakeDrainResetRequested())

	pending, err := store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "the drain must acknowledge the intent of a block with conflicting nodes")
}

// A reset has no rollback, so a failed WAL acknowledgement after it committed is only reported. The
// reset succeeds and the intent stays pending for the next acknowledgement.
func TestReset_AckFailureAfterCommitDoesNotFailReset(t *testing.T) {
	ctx := t.Context()
	stp, store, subtreeStore, chain := newResetWALTestProcessor(t, true)

	_, winner := createResetWALConflict(t, store, 6)

	moveForward := chain.moveForward
	moveForward[0].Subtrees = []*chainhash.Hash{storeConflictSubtree(t, subtreeStore, winner)}

	store.failComplete.Store(true)

	response := stp.Reset(chain.genesis, nil, moveForward, false, func() error { return nil })
	require.NoError(t, response.Err, "the reset committed; a failed acknowledgement must not fail it")
	require.Equal(t, moveForward[1].Header.Hash(), stp.GetCurrentBlockHeader().Hash())

	pending, err := store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the unacknowledged intent stays pending")
}

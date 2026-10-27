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
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	blob_memory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// countPendingScans wraps a real store and counts the pending-intent scans.
type countPendingScans struct {
	utxo.Store
	scans atomic.Int32
}

func (s *countPendingScans) PendingConflictIntents(ctx context.Context) ([]utxo.ConflictIntent, error) {
	s.scans.Add(1)

	return s.Store.PendingConflictIntents(ctx)
}

func newResetWALTestProcessor(t *testing.T) (*SubtreeProcessor, *countPendingScans, *blob_memory.Memory) {
	t.Helper()

	ctx := t.Context()

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

	stp, err := NewSubtreeProcessor(ctx, ulogger.TestLogger{}, test.CreateBaseTestSettings(t), subtreeStore, resetBlockchainMock(), store, newSubtreeChan)
	require.NoError(t, err)

	stp.Start(ctx)
	t.Cleanup(func() {
		stp.Stop(context.Background())
		close(newSubtreeChan)
	})

	return stp, store, subtreeStore
}

func resetWALSpendTx(t *testing.T, parent *bt.Tx, satoshis uint64) *bt.Tx {
	t.Helper()

	tx := bt.NewTx()
	require.NoError(t, tx.From(parent.TxIDChainHash().String(), 0, parent.Outputs[0].LockingScript.String(), parent.Outputs[0].Satoshis))
	tx.Inputs[0].UnlockingScript = bscript.NewFromBytes([]byte{})
	tx.Outputs = []*bt.Output{{Satoshis: satoshis, LockingScript: bscript.NewFromBytes([]byte{0x52})}}

	return tx
}

// A reset over blocks with no conflicting nodes has no forward WAL intents to complete, so it must
// not scan the pending intents once per block.
func TestReset_NoConflictingNodesSkipsPendingIntentScan(t *testing.T) {
	stp, store, _ := newResetWALTestProcessor(t)

	moveBack, moveForward := resetFixture()
	stp.InitCurrentBlockHeader(moveBack[0].Header)

	response := stp.Reset(moveBack[0].Header, moveBack, moveForward, false, func() error { return nil })
	require.NoError(t, response.Err)
	require.Equal(t, moveForward[1].Header.Hash(), stp.GetCurrentBlockHeader().Hash(), "reset must land on the last moveForward block")

	require.Zero(t, store.scans.Load(), "blocks without conflicting nodes must not scan the pending intents")
}

// A reset over a block with a conflicting node runs ProcessConflicting with deferred completion. The
// reset must still complete that block's WAL intent after the block is applied.
func TestReset_ConflictingNodeCompletesWALIntent(t *testing.T) {
	ctx := t.Context()
	stp, store, subtreeStore := newResetWALTestProcessor(t)

	parent := bt.NewTx()
	parentIn := &bt.Input{PreviousTxOutIndex: 0, PreviousTxSatoshis: 200000, SequenceNumber: 0xFFFFFFFF, UnlockingScript: bscript.NewFromBytes([]byte{})}
	_ = parentIn.PreviousTxIDAdd(&chainhash.Hash{9, 9, 9})
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

	st, err := subtreepkg.NewTreeByLeafCount(4)
	require.NoError(t, err)
	require.NoError(t, st.AddCoinbaseNode())
	require.NoError(t, st.AddNode(*winner.TxIDChainHash(), 1, 1))
	require.NoError(t, st.AddConflictingNode(*winner.TxIDChainHash()))

	subtreeBytes, err := st.Serialize()
	require.NoError(t, err)
	require.NoError(t, subtreeStore.Set(ctx, st.RootHash()[:], fileformat.FileTypeSubtree, subtreeBytes))

	moveBack, moveForward := resetFixture()
	moveForward[0].Subtrees = []*chainhash.Hash{st.RootHash()}
	stp.InitCurrentBlockHeader(moveBack[0].Header)

	response := stp.Reset(moveBack[0].Header, moveBack, moveForward, false, func() error { return nil })
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

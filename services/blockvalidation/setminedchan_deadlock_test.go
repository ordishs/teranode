// Package blockvalidation — regression tests for the setMinedChan deadlock (PR #828 review P0).
package blockvalidation

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestProcessBlockMinedNotSet_DoesNotBlockWhenBacklogExceedsBuffer reproduces the
// startup deadlock. processBlockMinedNotSet enqueues the entire GetBlocksMinedNotSet
// backlog onto setMinedChan, and that query has no SQL LIMIT. In start() this runs
// BEFORE the consumer worker is launched, so a backlog larger than the channel buffer
// blocks the send forever and start() never completes (the node never finishes booting
// block validation). The function must never block its caller.
func TestProcessBlockMinedNotSet_DoesNotBlockWhenBacklogExceedsBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nBits, _ := model.NewNBitFromString("2000ffff")
	// A single shared header is fine: distinctness is irrelevant here (no consumer,
	// no dedup), we only care that more blocks than the buffer get enqueued.
	header := &model.BlockHeader{
		Version:        1,
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      1,
		Bits:           *nBits,
		Nonce:          0,
	}

	const backlog = 5
	blocks := make([]*model.Block, backlog)
	for i := range blocks {
		blocks[i] = &model.Block{Header: header}
	}

	mockBC := &blockchain.Mock{}
	mockBC.On("GetBlocksMinedNotSet", mock.Anything).Return(blocks, nil)

	// Buffer smaller than the backlog, with NO consumer draining — exactly the startup
	// window before the setMinedChan worker is launched.
	bv := &BlockValidation{
		logger:           ulogger.TestLogger{},
		blockchainClient: mockBC,
		setMinedChan:     make(chan *chainhash.Hash, 2),
	}

	done := make(chan struct{})
	go func() {
		bv.processBlockMinedNotSet(ctx, nil)
		close(done)
	}()

	select {
	case <-done:
		// returned promptly — good
	case <-time.After(2 * time.Second):
		t.Fatal("processBlockMinedNotSet blocked when the backlog exceeded the setMinedChan buffer (startup deadlock)")
	}
}

// TestEnqueueSetMined_DoesNotBlockWhenChannelFull locks in the property the fix relies
// on: enqueuing a block for the setMined worker must never block the caller, even when
// the channel is full. The setMined worker is the sole drainer of setMinedChan, so a
// blocking send from the worker's own retry path — or from any producer while the worker
// is busy — would wedge mined finalization. No hash may be dropped.
func TestEnqueueSetMined_DoesNotBlockWhenChannelFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bv := &BlockValidation{
		logger:       ulogger.TestLogger{},
		setMinedChan: make(chan *chainhash.Hash, 1),
	}

	h1 := &chainhash.Hash{1}
	h2 := &chainhash.Hash{2}

	// Fill the single buffer slot so the channel is full.
	bv.setMinedChan <- h1

	done := make(chan struct{})
	go func() {
		bv.enqueueSetMined(ctx, h2)
		close(done)
	}()

	select {
	case <-done:
		// returned without blocking — good
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueSetMined blocked on a full setMinedChan")
	}

	// Both hashes are still delivered (no drops): the buffered one first, then the one
	// completed by enqueueSetMined once a slot frees.
	require.Equal(t, h1, <-bv.setMinedChan)
	require.Equal(t, h2, <-bv.setMinedChan)
}

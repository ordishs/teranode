package svp2p

import (
	"context"
	"io"
	"sync"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// orphanBlocks retains blocks that arrive before their parent, so they are
// ingested from the spool when the parent lands instead of being fetched from
// the network again.
//
// SVNode accepts an out-of-order block whose header it already holds straight
// into its block store (validation.cpp AcceptBlock) and never re-requests it.
// This port's bridge cannot ingest a block before its parent (legacy
// semantics, waitForPreviousBlockMined), and until now it refused the block
// and discarded the bytes — one wasted transfer per out-of-order arrival, which
// the parity harness measured at about five transfers per block on a
// three-peer download whose peers outran the ingest (phase-3 ledger residual 1,
// 2026-08-26). The spool is the svp2p TempStore, which had no consumer.
//
// The byte budget is the admission budget (legacy_blockPrefetchBufferBytes):
// bytes waiting for a parent are the same kind of pressure as bytes waiting for
// admission, and reusing the dial avoids a new settings key. Over budget the
// ingestor falls back to refusing the block, exactly as before.
type orphanBlocks struct {
	logger ulogger.Logger
	store  blob.Store
	budget int64

	mu       sync.Mutex
	bytes    int64
	byHash   map[chainhash.Hash]retainedBlock
	byParent map[chainhash.Hash][]chainhash.Hash

	replays sync.WaitGroup
}

type retainedBlock struct {
	header    *wire.BlockHeader
	txCount   uint64
	sizeBytes uint64
	peerAddr  string
	spooled   int64
}

func newOrphanBlocks(logger ulogger.Logger, store blob.Store, budget int64) *orphanBlocks {
	return &orphanBlocks{
		logger:   logger,
		store:    store,
		budget:   budget,
		byHash:   make(map[chainhash.Hash]retainedBlock),
		byParent: make(map[chainhash.Hash][]chainhash.Hash),
	}
}

// Len is the number of retained blocks.
func (o *orphanBlocks) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return len(o.byHash)
}

// Bytes is the spooled payload total.
func (o *orphanBlocks) Bytes() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.bytes
}

// Wait blocks until every replay in progress has finished. Test support.
func (o *orphanBlocks) Wait() { o.replays.Wait() }

// fits reserves budget for a block of the declared size; false means the
// caller must fall back to refusing the block.
func (o *orphanBlocks) fits(hash chainhash.Hash, sizeBytes uint64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	if _, dup := o.byHash[hash]; dup {
		return false
	}

	return o.bytes+int64(sizeBytes) <= o.budget //nolint:gosec // a block payload is bounded by MaxBlockPayload
}

// Retain spools the request's remaining stream into the store and indexes the
// block under its parent. The stream is fully consumed and closed either way.
func (o *orphanBlocks) Retain(ctx context.Context, req protocol.BlockIngestRequest) error {
	hash := req.Header.BlockHash()

	counted := &countingReader{r: req.TxReader}

	if err := o.store.SetFromReader(ctx, hash[:], fileformat.FileTypeBlock, counted); err != nil {
		return errors.NewStorageError("[svp2p] failed to spool orphan block %s", hash, err)
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	o.byHash[hash] = retainedBlock{
		header:    req.Header,
		txCount:   req.TxCount,
		sizeBytes: req.SizeBytes,
		peerAddr:  req.PeerAddr,
		spooled:   counted.n,
	}
	o.byParent[req.Header.PrevBlock] = append(o.byParent[req.Header.PrevBlock], hash)
	o.bytes += counted.n

	return nil
}

// take removes and returns the blocks retained under parent.
func (o *orphanBlocks) take(parent chainhash.Hash) []retainedBlock {
	o.mu.Lock()
	defer o.mu.Unlock()

	hashes := o.byParent[parent]
	delete(o.byParent, parent)

	out := make([]retainedBlock, 0, len(hashes))

	for _, h := range hashes {
		if rb, ok := o.byHash[h]; ok {
			out = append(out, rb)
			delete(o.byHash, h)
			o.bytes -= rb.spooled
		}
	}

	return out
}

// Replay ingests every block retained under parent, in the background so the
// caller's own ingest returns at once. Each replay runs through the ingestor's
// full path — pre-admission, the budget, the pipeline — and may itself land
// grandchildren.
func (o *orphanBlocks) Replay(ctx context.Context, parent chainhash.Hash, ingest func(context.Context, protocol.BlockIngestRequest) protocol.IngestOutcome) {
	children := o.take(parent)
	if len(children) == 0 {
		return
	}

	o.replays.Add(1)

	go func() {
		defer o.replays.Done()

		for _, child := range children {
			hash := child.header.BlockHash()

			reader, err := o.store.GetIoReader(ctx, hash[:], fileformat.FileTypeBlock)
			if err != nil {
				o.logger.Errorf("[svp2p] retained block %s lost from the spool, it will be fetched again: %v", hash, err)
				continue
			}

			outcome := ingest(ctx, protocol.BlockIngestRequest{
				Header:    child.header,
				TxCount:   child.txCount,
				SizeBytes: child.sizeBytes,
				TxReader:  reader,
				PeerAddr:  child.peerAddr,
			})

			if delErr := o.store.Del(ctx, hash[:], fileformat.FileTypeBlock); delErr != nil {
				o.logger.Debugf("[svp2p] spool entry %s not removed: %v", hash, delErr)
			}

			if outcome.Err != nil {
				// The scheduler already counts this block as received; a failure
				// here is logged at the level a lost download deserves.
				o.logger.Warnf("[svp2p] replay of retained block %s failed: %v", hash, outcome.Err)
			} else {
				o.logger.Infof("[svp2p] replayed retained block %s (%d bytes) after its parent %s landed", hash, child.spooled, parent)
			}
		}
	}()
}

type countingReader struct {
	r io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

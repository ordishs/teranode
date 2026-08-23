package svp2p

import (
	"sync"
	"time"

	"github.com/bsv-blockchain/go-batcher/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/batchermetrics"
)

// maxTxAnnounceBatch and txAnnounceBatchTimeout are legacy netsync's own
// tx-announce batcher constants, ported verbatim:
//
//   - maxTxAnnounceBatch mirrors maxRequestedTxns (netsync/manager.go:121,
//     "= wire.MaxInvPerMsg"), the batcher's size cap — a full batch flushes
//     immediately rather than waiting for the timeout.
//   - txAnnounceBatchTimeout mirrors the batcher's own construction
//     (netsync/manager.go:3212, "batcher.NewWithDeduplicationAndPool[...]
//     (maxRequestedTxns, 1*time.Second, ...)").
//
// The batcher's own deduplication window (go-batcher's
// NewWithDeduplicationAndPool: a fixed 1 minute, not configurable) is NOT a
// port of anything legacy-specific — it is go-batcher's own constant, the
// same library legacy already depends on for this exact batcher.
const (
	maxTxAnnounceBatch     = wire.MaxInvPerMsg
	txAnnounceBatchTimeout = 1 * time.Second
)

// txAnnouncer owns the tx-announce batcher and the Put-vs-Close race guard
// legacy carries alongside it (netsync/manager.go txAnnounceMu /
// txAnnounceClosed, "go-batcher v2.0.4 PANICS on Put-after-Close, and the
// txmeta Kafka listener ... is a fire-and-forget goroutine not joined by
// Stop()"). The same hazard applies here: bridge.StartTxMetaConsumer's
// Kafka callback runs on its own goroutine, tied to the Start ctx, and is
// not joined before Server.Stop calls close — the RLock/RWLock pairing
// below is what makes a Put racing a Close safe rather than a crash.
type txAnnouncer struct {
	mu      sync.RWMutex
	closed  bool
	batcher *batcher.BatcherWithDedup[protocol.TxHashAndFee]
}

// newTxAnnouncer builds the tx-announce batcher, wiring its flush directly
// to relay (in practice, *protocol.PeerManager.RelayTxs) — the same
// composition shape Server.go already uses for the blocks-final path
// (bridge.StartBlocksFinalConsumer's onFinal is bound to
// s.manager.RelayBlock). Kept as its own function so txrelay_test.go can
// construct one against a recording relay func without a live Server.
func newTxAnnouncer(logger ulogger.Logger, relay func([]protocol.TxHashAndFee)) *txAnnouncer {
	b := batcher.NewWithDeduplicationAndPool[protocol.TxHashAndFee](maxTxAnnounceBatch, txAnnounceBatchTimeout,
		func(batch []*protocol.TxHashAndFee) {
			items := make([]protocol.TxHashAndFee, len(batch))
			for i, item := range batch {
				items[i] = *item
			}

			relay(items)
		},
		true, // background: matches legacy's own construction (manager.go:3212)
		batcher.WithName("svp2p_tx_announce"),
		batcher.WithLogger(logger),
		batcher.WithMetrics(batchermetrics.Provider()),
	)

	return &txAnnouncer{batcher: b}
}

// put queues one tx for announcement, unless the announcer has already been
// closed — legacy's own announceTx (netsync/manager.go:3101-3107). Matches
// bridge.TxMetaHandler's signature exactly, so it can be passed straight to
// bridge.StartTxMetaConsumer as the onTx callback.
func (a *txAnnouncer) put(hash chainhash.Hash, fee, size uint64) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.closed {
		return
	}

	item := protocol.TxHashAndFee{TxHash: hash, Fee: fee, Size: size}
	a.batcher.Put(&item)
}

// close marks the announcer closed (so any Put racing this call, or arriving
// after it, becomes a no-op instead of a panic) and then drains the batcher
// under a bounded timeout — legacy's own closeTxAnnounceBatcher
// (netsync/manager.go:3110-3125). Taking the write lock first waits for any
// in-flight put holding the read lock, exactly as legacy's own comment
// describes for its RWMutex pair.
//
// Idempotent: a second call after closed is already true skips the drain,
// matching legacy's own alreadyClosed short-circuit.
func (a *txAnnouncer) close(logger ulogger.Logger) {
	a.mu.Lock()
	alreadyClosed := a.closed
	a.closed = true
	a.mu.Unlock()

	if alreadyClosed {
		return
	}

	util.DrainBatcher(logger, "svp2p_tx_announce", util.DefaultBatcherDrainTimeout, a.batcher.Close)
}

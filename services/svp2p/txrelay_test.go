package svp2p

import (
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// recordingRelay collects every RelayTxs-shaped call newTxAnnouncer's flush
// makes, so tests can assert positively on what actually reached it.
type recordingRelay struct {
	mu      sync.Mutex
	batches [][]protocol.TxHashAndFee
}

func (r *recordingRelay) relay(batch []protocol.TxHashAndFee) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.batches = append(r.batches, batch)
}

func (r *recordingRelay) all() []protocol.TxHashAndFee {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []protocol.TxHashAndFee
	for _, b := range r.batches {
		out = append(out, b...)
	}

	return out
}

// TestTxAnnouncerFlushesAfterTimeout pins the batcher's own timeout
// (txAnnounceBatchTimeout, ported verbatim from legacy's 1*time.Second): a
// single Put, with no other traffic to fill the batch, still reaches relay
// once the timeout fires — it does not wait forever for a full batch.
func TestTxAnnouncerFlushesAfterTimeout(t *testing.T) {
	relay := &recordingRelay{}
	a := newTxAnnouncer(ulogger.TestLogger{}, relay.relay)
	t.Cleanup(func() { a.close(ulogger.TestLogger{}) })

	hash := chainhash.Hash{0x01}
	a.put(hash, 500, 250)

	require.Eventually(t, func() bool {
		for _, tx := range relay.all() {
			if tx.TxHash == hash {
				return true
			}
		}

		return false
	}, 3*time.Second, 10*time.Millisecond, "a single queued tx must be flushed after the batcher's timeout")

	all := relay.all()
	require.Len(t, all, 1)
	require.Equal(t, uint64(500), all[0].Fee)
	require.Equal(t, uint64(250), all[0].Size)
}

// TestTxAnnouncerPutAfterCloseIsNoop pins the Put-vs-Close race guard ported
// from legacy's txAnnounceMu/txAnnounceClosed (netsync/manager.go:477-485,
// "go-batcher v2.0.4 PANICS on Put-after-Close"): once close has run, put
// must silently do nothing rather than panic on a closed batcher.
func TestTxAnnouncerPutAfterCloseIsNoop(t *testing.T) {
	relay := &recordingRelay{}
	a := newTxAnnouncer(ulogger.TestLogger{}, relay.relay)

	a.close(ulogger.TestLogger{})

	require.NotPanics(t, func() {
		a.put(chainhash.Hash{0x02}, 1, 1)
	})

	// Give any wrongly-still-alive batcher goroutine a moment, then confirm
	// nothing was relayed — the put must never have reached the batcher.
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, relay.all())
}

// TestTxAnnouncerCloseIsIdempotent mirrors legacy's own alreadyClosed
// short-circuit (netsync/manager.go closeTxAnnounceBatcher): a second close
// call must not panic or hang.
func TestTxAnnouncerCloseIsIdempotent(t *testing.T) {
	relay := &recordingRelay{}
	a := newTxAnnouncer(ulogger.TestLogger{}, relay.relay)

	require.NotPanics(t, func() {
		a.close(ulogger.TestLogger{})
		a.close(ulogger.TestLogger{})
	})
}

// TestTxAnnouncerCloseDrainsQueuedItems proves close() flushes whatever was
// already queued rather than discarding it — legacy's own
// util.DrainBatcher-based drain semantics.
func TestTxAnnouncerCloseDrainsQueuedItems(t *testing.T) {
	relay := &recordingRelay{}
	a := newTxAnnouncer(ulogger.TestLogger{}, relay.relay)

	hash := chainhash.Hash{0x03}
	a.put(hash, 1, 1)

	a.close(ulogger.TestLogger{})

	var found bool
	for _, tx := range relay.all() {
		if tx.TxHash == hash {
			found = true
		}
	}

	require.True(t, found, "close must drain an already-queued item into relay, not discard it")
}

package blockvalidation

// Tests for the per-block BLOCK_INCOMPLETE retry cap.
//
// A block whose transactions can never be blessed (missing parents, lost UTXO
// store partition) used to be retried forever by every block-processing worker
// while the chain tip stayed frozen behind it. The cap bounds that grinding:
// after IncompleteBlockMaxRetriesPerBlock failures the block enters a cooldown
// window (workers skip it at dequeue), a single manual_intervention_required
// escalation is emitted, and one self-requeue is scheduled for when the window
// expires so the block is retried without depending on a peer re-announcement.

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/jellydator/ttlcache/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// newIncompleteCapServer builds a Server with just the BLOCK_INCOMPLETE
// attempt-cap machinery wired.
func newIncompleteCapServer(t *testing.T, cap int) *Server {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.BlockValidation.IncompleteBlockMaxRetriesPerBlock = cap

	return &Server{
		settings: tSettings,
		logger:   ulogger.TestLogger{},
		blockIncompleteAttempts: ttlcache.New[chainhash.Hash, int](
			ttlcache.WithTTL[chainhash.Hash, int](incompleteBlockCooldown),
			ttlcache.WithDisableTouchOnHit[chainhash.Hash, int](),
		),
	}
}

func TestIncompleteBlockAttemptCap(t *testing.T) {
	u := newIncompleteCapServer(t, 3)

	h := chainhash.HashH([]byte("incomplete-blk-cap"))

	require.False(t, u.incompleteAttemptsExhausted(&h), "zero attempts is not exhausted")
	require.Equal(t, 1, u.recordIncompleteAttempt(&h))
	require.False(t, u.incompleteAttemptsExhausted(&h), "1/3 is below the cap")
	require.Equal(t, 2, u.recordIncompleteAttempt(&h))
	require.False(t, u.incompleteAttemptsExhausted(&h), "2/3 is below the cap")
	require.Equal(t, 3, u.recordIncompleteAttempt(&h))
	require.True(t, u.incompleteAttemptsExhausted(&h), "3/3 reaches the cap -> cooldown")

	// A different block has its own independent budget.
	other := chainhash.HashH([]byte("incomplete-blk-other"))
	require.False(t, u.incompleteAttemptsExhausted(&other))

	// Success clears the counter so a recovered block starts fresh.
	u.clearIncompleteAttempts(&h)
	require.False(t, u.incompleteAttemptsExhausted(&h), "cleared block is retriable again")

	// Cap disabled (<= 0) never exhausts, regardless of attempt count.
	u.settings.BlockValidation.IncompleteBlockMaxRetriesPerBlock = 0
	for range 10 {
		u.recordIncompleteAttempt(&h)
	}
	require.False(t, u.incompleteAttemptsExhausted(&h), "cap <= 0 disables the bound")
}

func TestIncompleteBlockAttemptCap_NilCacheSafe(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)
	u := &Server{settings: tSettings, logger: ulogger.TestLogger{}} // blockIncompleteAttempts == nil

	h := chainhash.HashH([]byte("incomplete-blk-nil"))
	require.NotPanics(t, func() {
		require.Equal(t, 0, u.recordIncompleteAttempt(&h))
		require.False(t, u.incompleteAttemptsExhausted(&h))
		u.clearIncompleteAttempts(&h)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		u.recordIncompleteBlockFailure(ctx, processBlockFound{hash: &h})
	})
}

// TestRecordIncompleteBlockFailure_EscalatesOnce verifies the escalation fires
// exactly once — at the failure that crosses the cap — and not again for
// failures racing in behind it (workers already past the enqueue gate).
func TestRecordIncompleteBlockFailure_EscalatesOnce(t *testing.T) {
	initPrometheusMetrics()

	u := newIncompleteCapServer(t, 2)
	u.blockPriorityQueue = NewBlockPriorityQueue(ulogger.TestLogger{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := chainhash.HashH([]byte("incomplete-blk-escalate"))
	bf := processBlockFound{hash: &h, baseURL: "http://peer-1", peerID: "peer-1"}

	retriesBefore := testutil.ToFloat64(prometheusBlockValidationIncompleteBlockRetries)
	escalationsBefore := testutil.ToFloat64(prometheusBlockValidationIncompleteBlockEscalations)

	u.recordIncompleteBlockFailure(ctx, bf)
	require.False(t, u.incompleteAttemptsExhausted(&h), "1/2 must not escalate")
	require.InDelta(t, escalationsBefore, testutil.ToFloat64(prometheusBlockValidationIncompleteBlockEscalations), 0.001)

	u.recordIncompleteBlockFailure(ctx, bf)
	require.True(t, u.incompleteAttemptsExhausted(&h), "2/2 reaches the cap")
	require.InDelta(t, escalationsBefore+1, testutil.ToFloat64(prometheusBlockValidationIncompleteBlockEscalations), 0.001)

	u.recordIncompleteBlockFailure(ctx, bf)
	u.recordIncompleteBlockFailure(ctx, bf)
	require.InDelta(t, escalationsBefore+1, testutil.ToFloat64(prometheusBlockValidationIncompleteBlockEscalations), 0.001, "failures past the cap must not re-escalate")

	require.InDelta(t, retriesBefore+4, testutil.ToFloat64(prometheusBlockValidationIncompleteBlockRetries), 0.001, "every failure counts toward the retry total")
}

// TestRecordIncompleteBlockFailure_RequeueCarriesSource verifies the
// cooldown-expiry self-requeue re-enters the priority queue with the failing
// announcement's ORIGINAL source. A stripped SourceTypeRetry marker is
// unfetchable: by requeue time the queue's alternative-source index for the
// hash is gone, so processBlockWithPriority would fail with "no sources
// available" and the retry would be a silent no-op.
func TestRecordIncompleteBlockFailure_RequeueCarriesSource(t *testing.T) {
	initPrometheusMetrics()

	u := newIncompleteCapServer(t, 1)
	u.blockPriorityQueue = NewBlockPriorityQueue(ulogger.TestLogger{})

	// Shrink the cooldown window so the AfterFunc fires within the test: the
	// requeue delay is the REMAINING window, which is bounded by the entry TTL.
	u.blockIncompleteAttempts = ttlcache.New[chainhash.Hash, int](
		ttlcache.WithTTL[chainhash.Hash, int](50*time.Millisecond),
		ttlcache.WithDisableTouchOnHit[chainhash.Hash, int](),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := chainhash.HashH([]byte("incomplete-blk-requeue"))
	bf := processBlockFound{hash: &h, baseURL: "http://origin-peer", peerID: "origin-peer"}

	u.recordIncompleteBlockFailure(ctx, bf)

	// Peek takes the queue lock (Len does not), so it is the race-safe way to
	// poll for the AfterFunc's requeue.
	require.Eventually(t, func() bool {
		_, _, ok := u.blockPriorityQueue.Peek()
		return ok
	}, 2*time.Second, 10*time.Millisecond, "cooldown expiry must requeue the block")

	queued, _, ok := u.blockPriorityQueue.Peek()
	require.True(t, ok)
	require.True(t, queued.hash.IsEqual(&h))
	require.Equal(t, "http://origin-peer", queued.baseURL, "requeue must carry the original source, not a bare retry marker")
	require.Equal(t, "origin-peer", queued.peerID)
}

// TestIncompleteBlockCooldownWindowAnchoredToFirstFailure mirrors the catchup
// cap's P1 regression: the cooldown window must run from the FIRST failure and
// not be extended by each subsequent one.
func TestIncompleteBlockCooldownWindowAnchoredToFirstFailure(t *testing.T) {
	u := newIncompleteCapServer(t, 3)

	h := chainhash.HashH([]byte("incomplete-blk-window"))

	require.Equal(t, 1, u.recordIncompleteAttempt(&h))
	firstExpiry := u.blockIncompleteAttempts.Get(h).ExpiresAt()

	time.Sleep(20 * time.Millisecond)

	require.Equal(t, 2, u.recordIncompleteAttempt(&h))
	secondExpiry := u.blockIncompleteAttempts.Get(h).ExpiresAt()

	require.WithinDuration(t, firstExpiry, secondExpiry, 5*time.Millisecond, "repeat failure must not extend the cooldown window")
}

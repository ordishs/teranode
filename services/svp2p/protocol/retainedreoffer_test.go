package protocol

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// retainThenAcceptIngestor retains the first copy of a block and accepts the
// second, which is the live sequence a retained block takes when its spooled
// replay fails and the block has to come across the wire again.
type retainThenAcceptIngestor struct {
	mu    sync.Mutex
	calls []chainhash.Hash
}

func (r *retainThenAcceptIngestor) WatchProgress(rd io.ReadCloser) IngestProgress {
	return newTestProgress(rd)
}

func (r *retainThenAcceptIngestor) Ingest(_ context.Context, req BlockIngestRequest) IngestOutcome {
	// The real composition consumes and releases the stream on every exit
	// path; without that the transport read loop stays parked.
	_, err := io.Copy(io.Discard, req.TxReader)

	if closeErr := req.TxReader.Close(); closeErr != nil && err == nil {
		err = closeErr
	}

	if err != nil {
		return IngestOutcome{Err: err}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, req.Header.BlockHash())

	if len(r.calls) == 1 {
		return IngestOutcome{Retained: true}
	}

	return IngestOutcome{}
}

func (r *retainThenAcceptIngestor) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.calls)
}

// TestManagerRefetchesARetainedBlockWhoseReplayFailed is the mainnet defect of
// 2026-08-30: block tip+1 was retained (so the scheduler recorded it as held),
// its spooled replay then failed on a local fault, and the block was never
// requested again — the node logged "no sync progress" every rotation window
// for four hours with the whole chain above it spooled.
//
// SVNode keeps a block that failed for a LOCAL reason wanted: net_processing.cpp
// leaves it in mapBlocksInFlight and re-requests it. The port's equivalent is
// that the failed replay reports the outcome with no sync peer — no peer
// answers for a replay — and the block goes back on the download schedule once
// its backoff window expires.
func TestManagerRefetchesARetainedBlockWhoseReplayFailed(t *testing.T) {
	const backoff = time.Second

	genesis := syncGenesis()
	chain := minedRun(genesis, 1, 11)
	hash := chain[0].BlockHash()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	ingestor := &retainThenAcceptIngestor{}
	m := syncTestManager(t, idx, ingestor)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	far := connectSyncPeer(t, m, genesis, chain)
	defer func() { _ = far.nc.Close() }()

	far.write(t, blockFor(chain[0]))

	require.Eventually(t, func() bool { return ingestor.count() == 1 },
		10*time.Second, 20*time.Millisecond, "the block never reached the ingestor")

	// A retained block is a completed download, so nothing asks for it again
	// while it sits in the spool.
	far.mustNotReceive(t, 300*time.Millisecond, wire.CmdGetData)

	// The replay fails on a local fault, with the admission backoff window the
	// ingest path reports. No peer is at fault and no peer holds the claim.
	_, disconnect := m.BlockDone(nil, hash, IngestOutcome{
		Err:            errors.NewServiceError("svp2p: test utxo store is unavailable"),
		TransientLocal: true,
		RetryAfter:     backoff,
	})
	require.NoError(t, disconnect, "a local fault must not condemn a peer")

	// The backoff is honoured: the same bytes must not be pulled straight back
	// across the wire to fail again.
	far.mustNotReceive(t, backoff/2, wire.CmdGetData)

	// Once the window expires the block is back on the download schedule.
	getData, ok := far.readUntil(t, wire.CmdGetData).(*wire.MsgGetData)
	require.True(t, ok)
	require.Len(t, getData.InvList, 1)
	require.Equal(t, hash, getData.InvList[0].Hash)

	far.write(t, blockFor(chain[0]))

	require.Eventually(t, func() bool { return ingestor.count() == 2 },
		10*time.Second, 20*time.Millisecond, "the re-fetched block never reached the ingestor")

	// The second ingest succeeds, so the scheduler holds the block again and
	// the download window advances past it.
	require.Eventually(t, func() bool {
		m.syncMu.Lock()
		defer m.syncMu.Unlock()

		_, held := m.blockDownloader.haveData[hash]

		return held && m.blockDownloader.BlocksInFlight() == 0
	}, 10*time.Second, 20*time.Millisecond, "the re-fetched block was not recorded as held")
}

// TestBlockRelease_ReanchorsTheWalkBelowTheReleasedBlock pins the mechanism the
// test above exercises through the live loop.
//
// FindNextBlocksToDownload advances pindexLastCommonBlock over every block it
// finds held and starts the next walk above it, so a block released AFTER the
// anchor passed it sits below every later walk. SVNode never meets the case:
// hasData() is set by AcceptBlock and never cleared. This port clears it — a
// retained block whose replay failed — so the release has to drop the anchor.
func TestBlockRelease_ReanchorsTheWalkBelowTheReleasedBlock(t *testing.T) {
	f := newDownloadFixture(t, 3)
	peer := f.peerAt(t, "1.2.3.4:8333", 3)
	activeTip := f.node(t, 0)

	held := f.node(t, 1).Hash

	require.True(t, f.bd.MarkBlockAsInFlight(peer, f.node(t, 1), testNow))
	require.True(t, f.bd.BlockReceived(peer, held, testNow))

	// The block is held, so the walk carries the anchor past it.
	require.Equal(t, []int32{2, 3}, f.requestedHeights(t, peer, activeTip, testNow))

	// The retained-block replay fails on a local fault. No peer holds the
	// claim, so the release names none.
	f.bd.BlockDeferred(nil, held, testNow, testNow+micros(time.Second))

	// The backoff window is honoured even though no peer was released.
	require.Equal(t, []int32{2, 3}, f.requestedHeights(t, peer, activeTip, testNow))

	// Once it expires the block is back on the schedule, below the anchor the
	// walk had already carried past it.
	require.Equal(t, []int32{1, 2, 3},
		f.requestedHeights(t, peer, activeTip, testNow+micros(2*time.Second)))
}

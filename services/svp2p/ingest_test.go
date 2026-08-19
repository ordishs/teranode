package svp2p

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/svp2p/bridge"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// stubBridge stands in for the relocated ingestion pipeline, which has its own
// coverage (Tasks 9, 10 and 13). What is under test here is the composition
// around it: the admission gate, the release discipline, and how a failure
// maps onto the outcome the peer loop acts on.
type stubBridge struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *stubBridge) IngestBlock(_ context.Context, _ *wire.BlockHeader, _ uint64, txReader io.Reader, _ string) error {
	s.mu.Lock()
	s.calls++
	err := s.err
	s.mu.Unlock()

	// The real IngestBlock releases the stream on every exit path.
	if closer, ok := txReader.(io.Closer); ok {
		_ = closer.Close()
	}

	return err
}

func (s *stubBridge) HeaderEvents() <-chan bridge.HeaderEvent { return nil }

func (s *stubBridge) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// countingStream is a block's transaction stream that records its releases, so
// a test can prove the transport read loop is never left parked.
type countingStream struct {
	io.Reader

	mu     sync.Mutex
	closes int
}

func (c *countingStream) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closes++

	return nil
}

func (c *countingStream) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closes
}

func newTestIngestor(t *testing.T, br bridge.Bridge) (*blockIngestor, *bridge.Admission) {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)
	require.Positive(t, tSettings.Legacy.BlockPrefetchBufferBytes, "the budget half of the gate must be on for this test")

	admission := bridge.NewAdmission(ulogger.TestLogger{}, tSettings)
	t.Cleanup(admission.Stop)

	return &blockIngestor{logger: ulogger.TestLogger{}, bridge: br, admission: admission}, admission
}

func testIngestRequest(header *wire.BlockHeader, stream io.ReadCloser) protocol.BlockIngestRequest {
	return protocol.BlockIngestRequest{
		Header:    header,
		TxCount:   1,
		SizeBytes: 1024,
		TxReader:  stream,
		PeerAddr:  "127.0.0.1:8333",
	}
}

func testIngestHeader() *wire.BlockHeader {
	zero := chainhash.Hash{}

	return wire.NewBlockHeader(1, &zero, &zero, 0x207fffff, 1)
}

func TestBlockIngestorAdmitsAndReleases(t *testing.T) {
	br := &stubBridge{}
	ingestor, admission := newTestIngestor(t, br)

	header := testIngestHeader()

	// Two sequential ingests of the same block: the second can only be
	// admitted if the first released both halves of the gate.
	for i := 0; i < 2; i++ {
		stream := &countingStream{Reader: bytes.NewReader(nil)}

		outcome := ingestor.Ingest(context.Background(), testIngestRequest(header, stream))

		require.NoError(t, outcome.Err)
		require.False(t, outcome.Duplicate)
		require.False(t, outcome.Rotate)
		require.False(t, outcome.TransientLocal)
		require.Equal(t, 1, stream.closeCount(), "the block stream must be released exactly once")
	}

	require.Equal(t, 2, br.callCount())
	require.Equal(t, int64(0), admission.Waiters())
}

func TestBlockIngestorReportsDuplicate(t *testing.T) {
	br := &stubBridge{}
	ingestor, admission := newTestIngestor(t, br)

	header := testIngestHeader()
	hash := header.BlockHash()

	// A copy of this block is already admitted and mid-ingest.
	weight, err := admission.Acquire(context.Background(), nil, hash, 1024)
	require.NoError(t, err)

	defer admission.Release(hash, weight)

	stream := &countingStream{Reader: bytes.NewReader(nil)}

	outcome := ingestor.Ingest(context.Background(), testIngestRequest(header, stream))

	require.True(t, outcome.Duplicate, "a second copy must be reported as a duplicate, not a failure of this peer")
	require.False(t, outcome.Rotate)
	require.ErrorIs(t, outcome.Err, bridge.ErrDuplicateBlockInFlight)
	require.Zero(t, br.callCount(), "a duplicate must never reach the pipeline")
	require.Equal(t, 1, stream.closeCount(), "a rejected block still has to release the stream")
}

func TestBlockIngestorBacksOffLocalFailures(t *testing.T) {
	br := &stubBridge{err: errors.NewServiceError("utxo store is unavailable")}
	ingestor, _ := newTestIngestor(t, br)

	header := testIngestHeader()

	first := &countingStream{Reader: bytes.NewReader(nil)}
	outcome := ingestor.Ingest(context.Background(), testIngestRequest(header, first))

	require.Error(t, outcome.Err)
	require.True(t, outcome.TransientLocal, "a local store fault must not be charged to the peer")
	require.False(t, outcome.Rotate)
	require.Equal(t, 1, br.callCount())

	// The failure is now inside its backoff window, so a re-delivery is
	// skipped cheaply instead of re-running the pipeline — and it is still a
	// local fault, so the peer keeps its stall clock refreshed.
	second := &countingStream{Reader: bytes.NewReader(nil)}
	outcome = ingestor.Ingest(context.Background(), testIngestRequest(header, second))

	require.Error(t, outcome.Err)
	require.True(t, outcome.TransientLocal)
	require.Equal(t, 1, br.callCount(), "a backed-off block must not reach the pipeline again")
	require.Equal(t, 1, second.closeCount(), "a skipped block still has to release the stream")
}

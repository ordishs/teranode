package svp2p

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

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

	// preAdmit is what the bounded pre-admission lookups answer, and
	// preAdmitErr what they fail with. preAdmitBlock, when non-nil, holds
	// those lookups until it is closed or the context ends, standing in for a
	// wedged blockchain client.
	preAdmit      bridge.PreAdmitResult
	preAdmitErr   error
	preAdmitBlock chan struct{}
	preAdmitCalls int
}

func (s *stubBridge) PreAdmit(ctx context.Context, _ *wire.BlockHeader) (bridge.PreAdmitResult, error) {
	s.mu.Lock()
	s.preAdmitCalls++
	block := s.preAdmitBlock
	result, err := s.preAdmit, s.preAdmitErr
	s.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return bridge.PreAdmitResult{}, ctx.Err()
		}
	}

	return result, err
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

// IngestTx and TxRejected are Task 14's additions to bridge.Bridge. Nothing
// in this file exercises them — this suite is about the block-ingest
// admission/release composition (see the type doc comment above); the tx
// adapter has its own suite in ingest_tx_test.go.
func (s *stubBridge) IngestTx(context.Context, []byte, string) (bridge.IngestTxResult, error) {
	panic("stubBridge: IngestTx is not exercised by this suite")
}

func (s *stubBridge) TxRejected(chainhash.Hash) bool {
	panic("stubBridge: TxRejected is not exercised by this suite")
}

// FetchBlock and FetchTx are Task 9's read-side additions to bridge.Bridge.
// Nothing in this file exercises them — this suite is about the admission
// gate and IngestBlock composition (see the comment above) — so the stub
// only needs to satisfy the interface.
func (s *stubBridge) FetchBlock(context.Context, *chainhash.Hash) (io.ReadCloser, uint64, error) {
	panic("stubBridge: FetchBlock is not exercised by this suite")
}

func (s *stubBridge) FetchTx(context.Context, *chainhash.Hash) ([]byte, error) {
	panic("stubBridge: FetchTx is not exercised by this suite")
}

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

func TestBlockIngestorSkipsBlocksWeAlreadyHold(t *testing.T) {
	br := &stubBridge{preAdmit: bridge.PreAdmitResult{Exists: true}}
	ingestor, _ := newTestIngestor(t, br)

	stream := &countingStream{Reader: bytes.NewReader(nil)}

	outcome := ingestor.Ingest(context.Background(), testIngestRequest(testIngestHeader(), stream))

	require.NoError(t, outcome.Err, "a block we already hold is a completed download, not a failure")
	require.False(t, outcome.TransientLocal)
	require.Zero(t, br.callCount(), "a block we already hold must not be re-ingested")
	require.Equal(t, 1, stream.closeCount())
}

func TestBlockIngestorHoldsBackOrphans(t *testing.T) {
	br := &stubBridge{preAdmit: bridge.PreAdmitResult{ParentMissing: true}}
	ingestor, _ := newTestIngestor(t, br)

	stream := &countingStream{Reader: bytes.NewReader(nil)}

	outcome := ingestor.Ingest(context.Background(), testIngestRequest(testIngestHeader(), stream))

	require.Error(t, outcome.Err)
	require.True(t, outcome.TransientLocal, "our chain being behind is not the peer's fault")
	require.False(t, outcome.Rotate)
	require.Zero(t, br.callCount(), "an orphan must not run the pipeline")
	require.Equal(t, 1, stream.closeCount())
}

// TestBlockIngestorRotatesOnPreAdmitDeadline pins which phase the pre-admission
// deadline covers: the blockchain lookups, and only those.
func TestBlockIngestorRotatesOnPreAdmitDeadline(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)
	tSettings.Legacy.PeerProcessingTimeout = 50 * time.Millisecond

	admission := bridge.NewAdmission(ulogger.TestLogger{}, tSettings)
	t.Cleanup(admission.Stop)

	// A wedged blockchain client: the lookups never answer.
	br := &stubBridge{preAdmitBlock: make(chan struct{})}
	defer close(br.preAdmitBlock)

	ingestor := &blockIngestor{logger: ulogger.TestLogger{}, bridge: br, admission: admission}

	stream := &countingStream{Reader: bytes.NewReader(nil)}

	outcome := ingestor.Ingest(context.Background(), testIngestRequest(testIngestHeader(), stream))

	require.Error(t, outcome.Err)
	require.True(t, outcome.Rotate, "a wedged lookup must rotate the sync peer rather than park the read loop")
	require.False(t, outcome.TransientLocal, "a rotation is not also a stall-clock refresh")
	require.Zero(t, br.callCount())
	require.Equal(t, 1, stream.closeCount())
}

// TestBlockIngestorDoesNotDeadlineTheBudgetWait is the other half of the same
// rule: a caller parked on the admission budget is waiting on OUR in-flight
// blocks, so it must not inherit the pre-admission deadline and must never
// rotate the delivering peer.
func TestBlockIngestorDoesNotDeadlineTheBudgetWait(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)
	tSettings.Legacy.PeerProcessingTimeout = 50 * time.Millisecond
	tSettings.Legacy.BlockPrefetchBufferBytes = 128 * 1024

	admission := bridge.NewAdmission(ulogger.TestLogger{}, tSettings)
	t.Cleanup(admission.Stop)

	// Fill the whole budget with another block, so this ingest parks.
	other := chainhash.Hash{0xaa}

	weight, err := admission.Acquire(context.Background(), nil, other, tSettings.Legacy.BlockPrefetchBufferBytes)
	require.NoError(t, err)

	br := &stubBridge{}
	ingestor := &blockIngestor{logger: ulogger.TestLogger{}, bridge: br, admission: admission}

	stream := &countingStream{Reader: bytes.NewReader(nil)}

	done := make(chan protocol.IngestOutcome, 1)
	go func() {
		done <- ingestor.Ingest(context.Background(), testIngestRequest(testIngestHeader(), stream))
	}()

	// Well past the pre-admission deadline: a budget wait must survive it.
	select {
	case outcome := <-done:
		t.Fatalf("the budget wait inherited the pre-admission deadline: %+v", outcome)
	case <-time.After(10 * tSettings.Legacy.PeerProcessingTimeout):
	}

	admission.Release(other, weight)

	select {
	case outcome := <-done:
		require.NoError(t, outcome.Err)
		require.False(t, outcome.Rotate, "our own backpressure must never rotate the delivering peer")
		require.Equal(t, 1, br.callCount())
	case <-time.After(5 * time.Second):
		t.Fatal("the ingest never resumed after the budget drained")
	}
}

// TestBlockIngestorFlagsPipelineRejectionAsPeerFault is the other end of the
// disconnect contract: the adapter is what decides a failure is the peer's,
// and the manager disconnects on that flag alone.
func TestBlockIngestorFlagsPipelineRejectionAsPeerFault(t *testing.T) {
	br := &stubBridge{err: errors.NewBlockInvalidError("svp2p: test block is invalid")}
	ingestor, _ := newTestIngestor(t, br)

	stream := &countingStream{Reader: bytes.NewReader(nil)}

	outcome := ingestor.Ingest(context.Background(), testIngestRequest(testIngestHeader(), stream))

	require.Error(t, outcome.Err)
	require.True(t, outcome.PeerFault, "a block the pipeline rejected is the peer's fault")
	require.False(t, outcome.TransientLocal)
	require.False(t, outcome.Rotate)
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

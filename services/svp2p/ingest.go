package svp2p

import (
	"context"
	"io"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/svp2p/bridge"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// blockIngestor is the adapter spec §4.4 calls for: protocol declares what it
// needs of block ingestion (protocol.BlockIngestor) and imports neither the
// bridge nor any Teranode client, and this type — in the service package that
// owns both halves — is the only place the two meet.
//
// It is also where the admission composition lives: acquire, ingest, release.
// Admission is deliberately not part of the Bridge interface, because
// IngestBlock never sees a block's byte size; only the transport does, from
// the declared payload length, and it arrives here as req.SizeBytes.
type blockIngestor struct {
	logger    ulogger.Logger
	bridge    bridge.Bridge
	admission *bridge.Admission
}

var _ protocol.BlockIngestor = (*blockIngestor)(nil)

// WatchProgress is bridge.NewProgressReader: the wrapper the peer loop's idle
// timer polls while a long ingest runs.
func (b *blockIngestor) WatchProgress(r io.ReadCloser) protocol.IngestProgress {
	return bridge.NewProgressReader(r)
}

// Ingest runs one block through the gate and the pipeline. Every exit path
// releases the transaction stream, because the transport read loop stays
// parked on that peer until it closes, and every path that reserved budget
// releases exactly the weight Acquire returned.
func (b *blockIngestor) Ingest(ctx context.Context, req protocol.BlockIngestRequest) protocol.IngestOutcome {
	hash := req.Header.BlockHash()

	// The backoff skip is a local fault by construction (a service error from
	// our own store), so it must refresh the peer's stall clock rather than
	// count against it.
	if err := b.admission.SkipForBackoff(hash); err != nil {
		return protocol.IngestOutcome{Err: b.release(req.TxReader, err), TransientLocal: true}
	}

	// The pre-admission phase is bounded so a wedged local round-trip cannot
	// strand a requested block: on its deadline the sync peer rotates.
	preAdmitCtx, cancel := b.admission.PreAdmitContext(ctx)
	defer cancel()

	weight, err := b.admission.Acquire(preAdmitCtx, req.Quit, hash, int64(req.SizeBytes)) //nolint:gosec // a block payload is bounded by MaxBlockPayload
	if err != nil {
		// Nothing was reserved on any of these paths, so nothing is released:
		// pairing Release with a failed Acquire would evict the dedup entry
		// belonging to the copy still being ingested.
		if errors.Is(err, bridge.ErrDuplicateBlockInFlight) {
			return protocol.IngestOutcome{Err: b.release(req.TxReader, err), Duplicate: true}
		}

		return protocol.IngestOutcome{
			Err:    b.release(req.TxReader, err),
			Rotate: bridge.PreAdmitTimedOut(preAdmitCtx),
		}
	}

	// Released with the same weight Acquire returned, and only for a block it
	// actually admitted.
	defer b.admission.Release(hash, weight)

	// IngestBlock owns the stream from here: it releases it on every one of
	// its own exit paths.
	if err := b.bridge.IngestBlock(ctx, req.Header, req.TxCount, req.TxReader, req.PeerAddr); err != nil {
		if errors.IsTransientLocalError(err) {
			backoff := b.admission.RecordFailure(hash)
			b.logger.Warnf("[svp2p] block %s failed on a local fault, backing off for %s: %v", hash, backoff, err)

			return protocol.IngestOutcome{Err: err, TransientLocal: true}
		}

		return protocol.IngestOutcome{Err: err}
	}

	b.admission.ClearFailure(hash)

	return protocol.IngestOutcome{}
}

// release closes a stream the pipeline never took ownership of, and keeps the
// original failure as the reported one.
func (b *blockIngestor) release(r io.Closer, cause error) error {
	if err := r.Close(); err != nil {
		b.logger.Debugf("[svp2p] block stream closed with: %v", err)
	}

	return cause
}

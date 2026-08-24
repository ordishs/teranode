package svp2p

import (
	"bytes"
	"context"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
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

// The read side needs no adapter type of its own: bridge.Bridge already
// declares FetchBlock and FetchTx with exactly the signatures protocol asks
// for, so the bridge satisfies the narrow interface directly. This assertion
// is the seam spec §4.4 requires — the one place the two halves are named
// together — and it fails to compile the day either side drifts. The
// composition itself is one assignment in Server.startSync, beside the
// ingestor.
var _ protocol.BlockTxFetcher = (bridge.Bridge)(nil)

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

	// ONLY the pre-admission lookups read this deadline
	// (services/legacy/peer_server.go OnBlock, PR 1281): they are
	// sub-millisecond on a healthy node, so the deadline fires only on a
	// genuinely wedged blockchain client, and a wedged one must rotate the
	// sync peer instead of parking the transport read loop for ever.
	preAdmitCtx, cancel := b.admission.PreAdmitContext(ctx)
	defer cancel()

	result, err := b.bridge.PreAdmit(preAdmitCtx, req.Header)
	if err != nil {
		// A deadline means our own lookup path is wedged, which strands a
		// block the scheduler requested; legacy answers that by rotating the
		// sync peer. A parent cancellation (shutdown, peer teardown) is not a
		// timeout — the block is simply dropped. Neither is the peer's fault,
		// so neither may disconnect it.
		return protocol.IngestOutcome{
			Err:            b.release(req.TxReader, err),
			Rotate:         bridge.PreAdmitTimedOut(preAdmitCtx),
			TransientLocal: !bridge.PreAdmitTimedOut(preAdmitCtx),
		}
	}

	if result.Exists {
		b.logger.Debugf("[svp2p] block %s already exists, not admitting it", hash)

		// We hold the block, so the download is complete as far as the
		// scheduler is concerned. Nothing was reserved and nothing ran.
		return protocol.IngestOutcome{Err: b.release(req.TxReader, nil)}
	}

	if result.ParentMissing {
		// Our own validation is behind the header index. The scheduler requests
		// blocks in chain order, so the parent is already in flight or being
		// validated; re-offer this one rather than running a pipeline that would
		// fail on it, and never charge the peer for it.
		//
		// ParentMissing is what stops the re-offer becoming a re-download on the
		// next tick, every tick, until the parent lands: the scheduler holds the
		// block back instead. TransientLocal stays set alongside it, because
		// this IS our own fault and the delivering peer's stall clock must still
		// be refreshed.
		return protocol.IngestOutcome{
			Err: b.release(req.TxReader, errors.NewServiceUnavailableError(
				"[svp2p] block %s cannot be admitted yet: its parent %s is not in our chain", hash, req.Header.PrevBlock)),
			ParentMissing:  true,
			TransientLocal: true,
		}
	}

	// The budget wait deliberately keeps ctx and Quit, with no deadline: a
	// caller parked here is waiting on OUR in-flight blocks to drain, which is
	// backpressure we created, never a fault of the delivering peer.
	weight, err := b.admission.Acquire(ctx, req.Quit, hash, int64(req.SizeBytes)) //nolint:gosec // a block payload is bounded by MaxBlockPayload
	if err != nil {
		// Nothing was reserved on any of these paths, so nothing is released:
		// pairing Release with a failed Acquire would evict the dedup entry
		// belonging to the copy still being ingested.
		if errors.Is(err, bridge.ErrDuplicateBlockInFlight) {
			return protocol.IngestOutcome{Err: b.release(req.TxReader, err), Duplicate: true}
		}

		// The only other outcomes are ctx cancellation and peer teardown,
		// both local.
		return protocol.IngestOutcome{Err: b.release(req.TxReader, err), TransientLocal: true}
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

		// Everything the pipeline rejects that is not a local fault is the
		// peer's: a block that fails validation, or a payload that fails to
		// decode.
		return protocol.IngestOutcome{Err: err, PeerFault: true}
	}

	b.admission.ClearFailure(hash)

	return protocol.IngestOutcome{}
}

// release closes a stream the pipeline never took ownership of, and keeps the
// original failure as the reported one. A nil cause stays nil: the close of a
// stream we deliberately did not ingest is not itself a failure to report.
func (b *blockIngestor) release(r io.Closer, cause error) error {
	if err := r.Close(); err != nil {
		b.logger.Debugf("[svp2p] block stream closed with: %v", err)
	}

	return cause
}

// txIngestor is the tx-side counterpart to blockIngestor: the one place
// protocol.TxIngestor (spec §4.4) and bridge.Bridge meet for Task 14. It also
// owns the composition Task 13's announce seam needs — announce matches
// txAnnouncer.put's signature exactly (txrelay.go), so Server.startSync
// passes that method straight through with no adapter-specific shape
// leaking into it.
type txIngestor struct {
	bridge bridge.Bridge

	// announce feeds an accepted tx into the tx announcement relay. In
	// production this is *txAnnouncer.put, wired at the one place a
	// *txIngestor is built, Server.startSync (Server.go). There is no
	// separate newTxIngestor constructor — an earlier version of this
	// comment claimed one that never existed (review round 1, Minor 4);
	// Ingest nil-guards this field regardless, so a *txIngestor built
	// without it (a test, most likely) cannot panic rather than relying on
	// that claim.
	announce func(hash chainhash.Hash, fee, size uint64)
}

var _ protocol.TxIngestor = (*txIngestor)(nil)

// Ingest serializes msg and runs it through bridge.IngestTx, then feeds the
// announce seam for an accepted tx. Every other outcome (orphan, rejected,
// already-rejected) announces nothing.
//
// Latency divergence, disclosed rather than silent (review round 1, Minor
// 9): legacy's handleTxMsg announces an accepted peer-sourced tx
// immediately, in the same call (netsync/manager.go:1305,
// AnnounceNewTransactions). Here, announce is txAnnouncer.put
// (services/svp2p/txrelay.go), which queues into the same 1 second batcher
// Task 13's Kafka-sourced path already flushes through — so a peer-sourced
// accepted tx can sit up to txAnnounceBatchTimeout before it is relayed
// onward, where legacy relays it the instant Validate returns. Almost
// certainly intended, since both producers sharing one batcher (and one
// canRelay gate — see txAnnouncer.put) is the whole point of the seam this
// task was asked to feed, but it is a real, measurable behavior change from
// legacy's own timing and is recorded here rather than assumed obvious.
func (t *txIngestor) Ingest(ctx context.Context, msg *wire.MsgTx, peerAddr string) protocol.TxIngestOutcome {
	var buf bytes.Buffer
	if err := msg.Serialize(&buf); err != nil {
		return protocol.TxIngestOutcome{
			Err: errors.NewProcessingError("[svp2p] failed to serialize inbound tx from %s", peerAddr, err),
		}
	}

	result, err := t.bridge.IngestTx(ctx, buf.Bytes(), peerAddr)
	if err != nil {
		return protocol.TxIngestOutcome{Err: err}
	}

	if result.Accepted && t.announce != nil {
		t.announce(result.TxHash, result.Fee, result.Size)
	}

	// Orphans released by this tx's acceptance (Task 15's orphan pool,
	// bridge/orphans.go) feed the identical announce seam: they are
	// accepted transactions too, just discovered a step later than the
	// tx that unblocked them.
	if t.announce != nil {
		for _, released := range result.ReleasedOrphans {
			t.announce(released.TxHash, released.Fee, released.Size)
		}
	}

	return protocol.TxIngestOutcome{
		Accepted: result.Accepted,
		Orphan:   result.Orphan,
		Reject:   result.Reject,
	}
}

// Rejected delegates to bridge.Bridge.TxRejected — Task 16's seam (see that
// method's own doc comment in ingest_tx.go).
func (t *txIngestor) Rejected(hash chainhash.Hash) bool {
	return t.bridge.TxRejected(hash)
}

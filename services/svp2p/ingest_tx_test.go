package svp2p

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/svp2p/bridge"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/stretchr/testify/require"
)

// recordingTxBridge is txIngestor's own stub of bridge.Bridge: nothing here
// exercises block ingestion (that's ingest_test.go's stubBridge), only
// IngestTx and TxRejected, plus a record of exactly what bytes/peerAddr
// IngestTx was called with, so the adapter's own serialization can be
// checked.
type recordingTxBridge struct {
	stubBridge

	mu       sync.Mutex
	gotBytes []byte
	gotPeer  string

	result bridge.IngestTxResult
	err    error

	rejectedArg    chainhash.Hash
	rejectedResult bool
}

func (b *recordingTxBridge) IngestTx(_ context.Context, txBytes []byte, peerAddr string) (bridge.IngestTxResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.gotBytes = append([]byte(nil), txBytes...)
	b.gotPeer = peerAddr

	return b.result, b.err
}

func (b *recordingTxBridge) TxRejected(hash chainhash.Hash) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.rejectedArg = hash

	return b.rejectedResult
}

// announceRecorder stands in for txAnnouncer.put (txrelay.go): it has the
// identical signature so the adapter can be pointed at it directly, and it
// records what it was called with.
type announceRecorder struct {
	mu    sync.Mutex
	calls []struct {
		hash chainhash.Hash
		fee  uint64
		size uint64
	}
}

func (a *announceRecorder) put(hash chainhash.Hash, fee, size uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.calls = append(a.calls, struct {
		hash chainhash.Hash
		fee  uint64
		size uint64
	}{hash, fee, size})
}

func (a *announceRecorder) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.calls)
}

func TestTxIngestor_SerializesTxAndCallsBridge(t *testing.T) {
	br := &recordingTxBridge{result: bridge.IngestTxResult{Accepted: true}}
	announce := &announceRecorder{}

	ing := &txIngestor{bridge: br, announce: announce.put}

	tx := wire.NewMsgTx(1)

	outcome := ing.Ingest(t.Context(), tx, "peer1:8333")
	require.NoError(t, outcome.Err)

	var want bytes.Buffer
	require.NoError(t, tx.Serialize(&want))

	require.Equal(t, want.Bytes(), br.gotBytes, "the adapter must hand IngestTx exactly what tx.Serialize produces")
	require.Equal(t, "peer1:8333", br.gotPeer)
}

func TestTxIngestor_AcceptedTxFeedsAnnounceSeam(t *testing.T) {
	hash := chainhash.HashH([]byte("accepted"))
	br := &recordingTxBridge{result: bridge.IngestTxResult{
		Accepted: true,
		TxHash:   hash,
		Fee:      777,
		Size:     321,
	}}
	announce := &announceRecorder{}

	ing := &txIngestor{bridge: br, announce: announce.put}

	outcome := ing.Ingest(t.Context(), wire.NewMsgTx(1), "peer1:8333")
	require.True(t, outcome.Accepted)

	require.Equal(t, 1, announce.callCount())
	require.Equal(t, hash, announce.calls[0].hash)
	require.Equal(t, uint64(777), announce.calls[0].fee)
	require.Equal(t, uint64(321), announce.calls[0].size)
}

func TestTxIngestor_OrphanAndRejectDoNotAnnounce(t *testing.T) {
	t.Run("orphan", func(t *testing.T) {
		br := &recordingTxBridge{result: bridge.IngestTxResult{Orphan: true}}
		announce := &announceRecorder{}

		ing := &txIngestor{bridge: br, announce: announce.put}

		outcome := ing.Ingest(t.Context(), wire.NewMsgTx(1), "peer1:8333")
		require.True(t, outcome.Orphan)
		require.False(t, outcome.Accepted)
		require.Zero(t, announce.callCount())
	})

	t.Run("rejected", func(t *testing.T) {
		reject := wire.NewMsgReject(wire.CmdTx, wire.RejectInvalid, "rejected")
		br := &recordingTxBridge{result: bridge.IngestTxResult{Reject: reject}}
		announce := &announceRecorder{}

		ing := &txIngestor{bridge: br, announce: announce.put}

		outcome := ing.Ingest(t.Context(), wire.NewMsgTx(1), "peer1:8333")
		require.Equal(t, reject, outcome.Reject)
		require.False(t, outcome.Accepted)
		require.Zero(t, announce.callCount())
	})
}

func TestTxIngestor_BridgeErrorSurfacesAsOutcomeErr(t *testing.T) {
	br := &recordingTxBridge{err: errors.NewProcessingError("decode failed")}
	announce := &announceRecorder{}

	ing := &txIngestor{bridge: br, announce: announce.put}

	outcome := ing.Ingest(t.Context(), wire.NewMsgTx(1), "peer1:8333")
	require.Error(t, outcome.Err)
	require.Zero(t, announce.callCount())
}

func TestTxIngestor_RejectedDelegatesToBridge(t *testing.T) {
	br := &recordingTxBridge{rejectedResult: true}
	ing := &txIngestor{bridge: br}

	hash := chainhash.HashH([]byte("some-tx"))
	require.True(t, ing.Rejected(hash))
	require.Equal(t, hash, br.rejectedArg)
}

// TestTxIngestor_AcceptedTxWithNilAnnounceDoesNotPanic pins the nil-guard
// review round 1's Minor 4 asked for: a *txIngestor built without its
// announce field (a test, most likely, since Server.go's one production
// site always sets it) must not panic when Ingest accepts a tx.
func TestTxIngestor_AcceptedTxWithNilAnnounceDoesNotPanic(t *testing.T) {
	br := &recordingTxBridge{result: bridge.IngestTxResult{Accepted: true}}
	ing := &txIngestor{bridge: br}

	var outcome protocol.TxIngestOutcome

	require.NotPanics(t, func() {
		outcome = ing.Ingest(t.Context(), wire.NewMsgTx(1), "peer1:8333")
	})
	require.True(t, outcome.Accepted)
}

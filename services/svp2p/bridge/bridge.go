// Package bridge is the Teranode-side half of the svp2p rewrite (spec §7):
// it owns the relocated block-ingestion pipeline (handle_block.go,
// subtree_partition.go — moved here with logic intact from the legacy
// netsync package) and the eight Teranode service dependencies that
// pipeline needs. `protocol` (services/svp2p/protocol) never imports this
// package directly; per spec §4.4 it sees bridge through its own narrow,
// locally-declared interface, which the Bridge interface here is shaped to
// satisfy.
package bridge

import (
	"context"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/services/blockassembly"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockvalidation"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation"
	"github.com/bsv-blockchain/teranode/services/validator"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// Bridge grows one phase at a time (Decision 2, svp2p Phase 2 plan): a
// method is added only in the phase that implements it, never early as an
// unreachable no-op. Phase 2 shipped PreAdmit, IngestBlock and HeaderEvents;
// Phase 3 (this task) adds the two read-side methods a getdata answerer
// needs, FetchBlock and FetchTx. IngestTx is still unimplemented — it is
// Task 14's.
type Bridge interface {
	// PreAdmit runs the two blockchain lookups IngestBlock makes before it
	// touches the block payload — does this block already exist, and is its
	// parent in our chain — so the caller can bound them on its own context.
	// Legacy did exactly that (services/legacy/peer_server.go OnBlock, PR
	// 1281): the pre-admission phase is deadline-bounded because a wedged
	// blockchain client would otherwise park the transport read loop, while
	// the admission budget wait deliberately is NOT bounded, being our own
	// backpressure. IngestBlock still repeats both lookups, so it stays
	// correct when called on its own; they are sub-millisecond on a healthy
	// node.
	PreAdmit(ctx context.Context, header *wire.BlockHeader) (PreAdmitResult, error)

	// IngestBlock runs a received block through the relocated netsync
	// ingestion pipeline: existence check, parent-height validation,
	// block-assembly readiness wait, then createTxMap -> prepareSubtrees ->
	// extendTransactions -> createUtxos -> createSubtrees -> writeSubtree ->
	// ProcessBlock. header and txCount come from the wire message envelope;
	// txReader streams the block's transactions (transport hands bridge a
	// bounded reader instead of a materialized block — spec §5). peerAddr
	// identifies the sending peer for logging only; the pipeline has no
	// other use for it (no peer-liveness or per-peer state lives in bridge).
	IngestBlock(ctx context.Context, header *wire.BlockHeader, txCount uint64, txReader io.Reader, peerAddr string) error

	// HeaderEvents delivers tip-change notifications sourced from the
	// blockchain service's subscription (spec §4.4). The channel is never
	// closed by Bridge.
	HeaderEvents() <-chan HeaderEvent

	// FetchBlock streams a block's legacy-wire bytes (header + varint(txCount)
	// + transactions) from the asset service, and reports the declared length
	// the caller (Task 10) is to write into the wire message header before
	// streaming the body. See fetch.go for why the length comes from the
	// blockchain service rather than the HTTP response.
	FetchBlock(ctx context.Context, hash *chainhash.Hash) (io.ReadCloser, uint64, error)

	// FetchTx returns a transaction's serialized bytes from the UTXO store.
	// A missing or not-fully-retained transaction is reported as a typed
	// not-found error (errors.ErrTxNotFound) rather than legacy's silence —
	// see fetch.go.
	FetchTx(ctx context.Context, hash *chainhash.Hash) ([]byte, error)
}

// PreAdmitResult is what the bounded pre-admission lookups found. Both fields
// are answers, not failures: an error from PreAdmit means the lookup itself
// did not complete.
type PreAdmitResult struct {
	// Exists reports that we already hold this block, so there is nothing to
	// ingest and nothing to charge against the admission budget.
	Exists bool

	// ParentMissing reports that the block's parent is not in our chain yet.
	// Under svp2p the download scheduler only ever requests blocks in chain
	// order, so this means our own validation is behind the header index, not
	// that the peer sent something it should not have.
	ParentMissing bool
}

// HeaderEvent is a tip-change notification. It is kept deliberately minimal
// (Phase 2 width): enough for a consumer to know the chain advanced and to
// what, without carrying anything IngestBlock's caller already has cheaper
// access to (e.g. the full header or block).
type HeaderEvent struct {
	// Hash is the new tip's block hash.
	Hash chainhash.Hash
	// Height is the new tip's height.
	Height uint32
}

// svp2pBridge is the concrete Bridge. It stores the eight dependencies
// startSvp2pService injects (daemon/daemon_services.go:1103-1180, mirroring
// legacy.New's parameter list) plus logger/settings, and backs the relocated
// pipeline in handle_block.go / subtree_partition.go — those files reference
// these exact field names via the "sm" receiver, unchanged from netsync's
// SyncManager except for the receiver's type.
//
// It has two entry points into that pipeline. HandleBlockDirect takes a fully
// decoded *wire.MsgBlock, which is the shape the relocated netsync code
// arrived in. IngestBlock (ingest.go) is the streaming entry the transport
// uses: it feeds the same pipeline from a reader, decoding one transaction at
// a time, so a multi-GB block is never materialized in memory — the reason the
// streaming interface exists at all (see the decode-arena-release comments
// throughout handle_block.go). Both run the same checks and the same pipeline
// stages; only the transaction source differs.
type svp2pBridge struct {
	logger      ulogger.Logger
	settings    *settings.Settings
	chainParams *chaincfg.Params

	blockchainClient  blockchain.ClientI
	validationClient  validator.Interface
	subtreeStore      blob.Store
	tempStore         blob.Store
	utxoStore         utxo.Store
	subtreeValidation subtreevalidation.Interface
	blockValidation   blockvalidation.Interface
	blockAssembly     blockassembly.ClientI

	headerEvents chan HeaderEvent
}

// New constructs the bridge with its eight injected Teranode dependencies,
// in the same order startSvp2pService will fetch and pass them
// (daemon/daemon_services.go:1103-1180, matching legacy.New's signature).
// tempStore is stored but has no consumer yet: the relocated pipeline
// (handle_block.go, subtree_partition.go) never reads it — legacy's netsync
// package doesn't either, it is used elsewhere in the legacy service.
func New(
	logger ulogger.Logger,
	tSettings *settings.Settings,
	blockchainClient blockchain.ClientI,
	validationClient validator.Interface,
	subtreeStore blob.Store,
	tempStore blob.Store,
	utxoStore utxo.Store,
	subtreeValidation subtreevalidation.Interface,
	blockValidation blockvalidation.Interface,
	blockAssemblyClient *blockassembly.Client,
) *svp2pBridge {
	initPrometheusMetrics()

	return &svp2pBridge{
		logger:            logger,
		settings:          tSettings,
		chainParams:       tSettings.ChainCfgParams,
		blockchainClient:  blockchainClient,
		validationClient:  validationClient,
		subtreeStore:      subtreeStore,
		tempStore:         tempStore,
		utxoStore:         utxoStore,
		subtreeValidation: subtreeValidation,
		blockValidation:   blockValidation,
		blockAssembly:     blockAssemblyClient,
		headerEvents:      make(chan HeaderEvent, 16),
	}
}

// HeaderEvents returns the bridge's tip-change notification channel. It is on
// the interface because Decision 2 fixed the Phase 2 width there, not because
// Phase 2 uses it: block sync pulls headers from peers, so nothing in this
// phase needs telling that our own tip moved, and nothing publishes into the
// channel. The channel is real rather than nil — a consumer can range over it
// today without an adapter — but it stays silent until the Phase 3 relay work
// wires the blockchain-service subscription (spec §4.4) that feeds it.
func (b *svp2pBridge) HeaderEvents() <-chan HeaderEvent {
	return b.headerEvents
}

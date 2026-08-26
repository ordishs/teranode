// Package parity is the svp2p parity harness: it drives the legacy service and
// the svp2p service from the same scripted peer and compares what each did.
// Design: docs/superpowers/specs/2026-08-26-svp2p-parity-harness-design.md.
package parity

import (
	"context"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/legacy"
	"github.com/bsv-blockchain/teranode/services/p2p"
	"github.com/bsv-blockchain/teranode/services/svp2p"
	"github.com/bsv-blockchain/teranode/services/svp2p/svp2ptest"
	"github.com/bsv-blockchain/teranode/services/validator"
	"github.com/bsv-blockchain/teranode/settings"
	blobmemory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	blockchain_store "github.com/bsv-blockchain/teranode/stores/blockchain"
	utxosql "github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// Impl names the service under test.
type Impl string

const (
	// Legacy is services/legacy, the oracle.
	Legacy Impl = "legacy"
	// Svp2p is services/svp2p, the port.
	Svp2p Impl = "svp2p"
)

// service is the part of both servers the harness drives.
type service interface {
	Init(ctx context.Context) error
	Start(ctx context.Context, readyCh chan<- struct{}) error
	Stop(ctx context.Context) error
}

// nodeUnderTest is one running node: the implementation named by Impl on top
// of the real Teranode pipeline (block assembly, subtree validation, block
// validation, validator) over in-memory stores.
type nodeUnderTest struct {
	Impl     Impl
	Logger   *svp2ptest.RecordingLogger
	Settings *settings.Settings

	blockchainStore blockchain_store.Store
	svc             service
	cancel          context.CancelFunc
}

var nodeCounter atomic.Uint64

// Clocks the scenarios lean on. Both services share the legacy_* settings by
// design, so the settings tweaks below reach both; the svp2p tick and rotation
// window are instance fields and are narrowed through SetSyncClocks.
const (
	syncTick         = 100 * time.Millisecond
	maxLastBlockTime = 3 * time.Second
)

// newNode builds and starts a node of the given implementation that dials
// connectPeers. Every dependency is the real code over in-memory stores; only
// the implementation flag differs between the two legs of a scenario.
func newNode(t *testing.T, impl Impl, connectPeers []string, tweaks ...func(*settings.Settings)) *nodeUnderTest {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	logger := &svp2ptest.RecordingLogger{}

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.Context = fmt.Sprintf("%s-parity-%s-%d", tSettings.Context, impl, nodeCounter.Add(1))
	tSettings.Legacy.ListenAddresses = []string{"127.0.0.1:0"}
	tSettings.Legacy.GRPCListenAddress = svp2ptest.FreePort(t)
	tSettings.Legacy.WorkingDir = t.TempDir()
	tSettings.Legacy.ConnectPeers = connectPeers
	tSettings.GRPCAdminAPIKey = "test-admin-key"
	tSettings.BlockAssembly.GRPCListenAddress = svp2ptest.FreePort(t)
	tSettings.BlockAssembly.GRPCAddress = tSettings.BlockAssembly.GRPCListenAddress
	tSettings.SubtreeValidation.GRPCListenAddress = svp2ptest.FreePort(t)
	tSettings.SubtreeValidation.GRPCAddress = tSettings.SubtreeValidation.GRPCListenAddress
	tSettings.BlockValidation.GRPCListenAddress = svp2ptest.FreePort(t)
	tSettings.BlockValidation.GRPCAddress = tSettings.BlockValidation.GRPCListenAddress
	tSettings.BlockValidation.PeriodicProcessingInterval = 200 * time.Millisecond

	// legacy Server.Init refuses to start without an asset address; these
	// scenarios never serve a block, so the address only has to parse.
	if tSettings.Asset.HTTPAddress == "" {
		tSettings.Asset.HTTPAddress = "http://127.0.0.1:1/api/v1"
	}

	for _, tweak := range tweaks {
		tweak(tSettings)
	}

	blockchainStore, err := blockchain_store.NewStore(logger, &url.URL{Scheme: "sqlitememory"}, tSettings)
	require.NoError(t, err)

	blockchainClient, err := blockchain.NewLocalClient(logger, tSettings, blockchainStore, nil, nil)
	require.NoError(t, err)

	utxoStoreURL, err := url.Parse("sqlitememory:///parity")
	require.NoError(t, err)

	tSettings.UtxoStore.UtxoStore = utxoStoreURL

	utxoStore, err := utxosql.New(ctx, logger, tSettings, utxoStoreURL)
	require.NoError(t, err)

	subtreeStore := blobmemory.New()
	tempStore := blobmemory.New()
	txStore := blobmemory.New()

	blockAssemblyClient := svp2ptest.StartBlockAssembly(ctx, t, logger, tSettings, txStore, subtreeStore, utxoStore, blockchainClient)

	validatorClient, err := validator.New(ctx, logger, tSettings, utxoStore, nil, nil, nil, blockAssemblyClient, blockchainClient)
	require.NoError(t, err)

	subtreeValidationClient := svp2ptest.StartSubtreeValidation(ctx, t, tSettings.Context, logger, tSettings, subtreeStore,
		txStore, utxoStore, validatorClient, blockchainClient)

	blockValidationClient := svp2ptest.StartBlockValidation(ctx, t, tSettings.Context, logger, tSettings, subtreeStore, txStore,
		utxoStore, validatorClient, blockchainClient, blockAssemblyClient)

	var svc service

	switch impl {
	case Legacy:
		// legacy newServer takes the process-wide p2p ban list, which is built
		// once from settings and backed by the blockchain store URL; every
		// legacy node in this process gets a fresh in-memory one.
		p2p.ResetBanListSingleton()

		banStoreURL, urlErr := url.Parse("sqlitememory://")
		require.NoError(t, urlErr)

		tSettings.BlockChain.StoreURL = banStoreURL

		svc = legacy.New(logger, tSettings, blockchainClient, validatorClient, subtreeStore, tempStore, utxoStore,
			subtreeValidationClient, blockValidationClient, blockAssemblyClient)
	case Svp2p:
		srv := svp2p.NewWithDeps(logger, tSettings, blockchainClient, svp2p.Deps{
			ValidationClient:  validatorClient,
			SubtreeStore:      subtreeStore,
			TempStore:         tempStore,
			UtxoStore:         utxoStore,
			SubtreeValidation: subtreeValidationClient,
			BlockValidation:   blockValidationClient,
			BlockAssembly:     blockAssemblyClient,
		})
		srv.SetSyncClocks(syncTick, maxLastBlockTime)
		svc = srv
	default:
		t.Fatalf("unknown impl %q", impl)
	}

	require.NoError(t, svc.Init(ctx))

	readyCh := make(chan struct{})

	go func() { _ = svc.Start(ctx, readyCh) }()

	select {
	case <-readyCh:
	case <-time.After(30 * time.Second):
		logger.Dump(t)
		t.Fatalf("%s did not become ready", impl)
	}

	n := &nodeUnderTest{Impl: impl, Logger: logger, Settings: tSettings, blockchainStore: blockchainStore, svc: svc, cancel: cancel}

	t.Cleanup(n.Stop)

	return n
}

// Stop tears the node down. Safe to call twice.
func (n *nodeUnderTest) Stop() {
	if n.cancel == nil {
		return
	}

	n.cancel()
	n.cancel = nil

	_ = n.svc.Stop(context.Background())
}

// BestHeight is the node's active chain height.
func (n *nodeUnderTest) BestHeight(t *testing.T) uint32 {
	t.Helper()

	_, meta, err := n.blockchainStore.GetBestBlockHeader(context.Background())
	require.NoError(t, err)

	return meta.Height
}

// WaitForHeight polls until the active chain reaches h, dumping the node's
// log on timeout.
func (n *nodeUnderTest) WaitForHeight(t *testing.T, h uint32, timeout time.Duration) {
	t.Helper()

	n.WaitFor(t, func() bool { return n.BestHeight(t) == h }, timeout,
		fmt.Sprintf("%s never reached height %d", n.Impl, h))
}

// WaitFor polls cond until it holds or timeout passes.
func (n *nodeUnderTest) WaitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	n.Logger.Dump(t)
	t.Fatalf("%s (height now %d)", what, n.BestHeight(t))
}

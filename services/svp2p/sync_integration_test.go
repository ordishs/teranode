package svp2p

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockvalidation"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/services/validator"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	blobmemory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	blockchain_store "github.com/bsv-blockchain/teranode/stores/blockchain"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	utxosql "github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Loggers
// ---------------------------------------------------------------------------

// recordingLogger keeps every formatted Warnf and Infof line so a test can
// assert on a decision the production code already logs, without adding any
// new production surface. It is the same technique protocol/manager_test.go
// uses for the self-connection disconnect reason.
type recordingLogger struct {
	ulogger.TestLogger

	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) record(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recordingLogger) Warnf(format string, args ...interface{}) { l.record(format, args...) }

func (l *recordingLogger) Infof(format string, args ...interface{}) { l.record(format, args...) }

func (l *recordingLogger) Errorf(format string, args ...interface{}) { l.record(format, args...) }

func (l *recordingLogger) Debugf(format string, args ...interface{}) { l.record(format, args...) }

// dump prints what the node logged, so a failing leg is diagnosable from the
// test output alone.
func (l *recordingLogger) dump(t *testing.T) {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()

	for _, line := range l.lines {
		t.Log(line)
	}
}

func (l *recordingLogger) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}

	return false
}

// ---------------------------------------------------------------------------
// Chain fixture: a run of regtest coinbase-only blocks the real pipeline
// accepts (valid proof of work under the regtest limit, BIP34 coinbase, merkle
// root = the coinbase txid, which is what a single-transaction block's root is).
// ---------------------------------------------------------------------------

type fixtureChain struct {
	headers []*wire.BlockHeader // headers[i] is at height i+1
	blocks  map[chainhash.Hash]*wire.MsgBlock
	heights map[chainhash.Hash]int32 // includes genesis at height 0
}

func (c *fixtureChain) tip() chainhash.Hash { return c.headers[len(c.headers)-1].BlockHash() }

func buildFixtureChain(t *testing.T, tSettings *settings.Settings, count int) *fixtureChain {
	t.Helper()

	privKey, err := bec.NewPrivateKey()
	require.NoError(t, err)

	address, err := bscript.NewAddressFromPublicKey(privKey.PubKey(), true)
	require.NoError(t, err)

	genesis := tSettings.ChainCfgParams.GenesisBlock.Header
	genesisHash := genesis.BlockHash()

	chain := &fixtureChain{
		blocks:  make(map[chainhash.Hash]*wire.MsgBlock, count),
		heights: map[chainhash.Hash]int32{genesisHash: 0},
	}

	bits, err := model.NewNBitFromString(fmt.Sprintf("%08x", genesis.Bits))
	require.NoError(t, err)

	prevHash := genesisHash
	baseTime := time.Now().Add(-2 * time.Hour).Unix()

	for i := 0; i < count; i++ {
		height := uint32(i + 1) //nolint:gosec // test heights are small

		coinbase, cbErr := model.CreateCoinbase(height, 50e8, "svp2p sync test", []string{address.AddressString})
		require.NoError(t, cbErr)

		merkleRoot := coinbase.TxIDChainHash()
		prev := prevHash

		modelHeader := &model.BlockHeader{
			Version:        0x20000000,
			HashPrevBlock:  &prev,
			HashMerkleRoot: merkleRoot,
			Timestamp:      uint32(baseTime + int64(i)*600), //nolint:gosec // test timestamps are in range
			Bits:           *bits,
		}

		for {
			ok, _, _ := modelHeader.HasMetTargetDifficulty()
			if ok {
				break
			}

			modelHeader.Nonce++
		}

		wireHeader := &wire.BlockHeader{}
		require.NoError(t, wireHeader.Deserialize(bytes.NewReader(modelHeader.Bytes())))

		coinbaseWire := wire.NewMsgTx(1)
		require.NoError(t, coinbaseWire.Deserialize(bytes.NewReader(coinbase.Bytes())))

		block := wire.NewMsgBlock(wireHeader)
		require.NoError(t, block.AddTransaction(coinbaseWire))

		hash := wireHeader.BlockHash()
		require.Equal(t, *modelHeader.Hash(), hash, "the wire header must round-trip the mined model header")

		chain.headers = append(chain.headers, wireHeader)
		chain.blocks[hash] = block
		chain.heights[hash] = int32(height) //nolint:gosec // test heights are small

		prevHash = hash
	}

	return chain
}

// ---------------------------------------------------------------------------
// Scripted serving peer: raw go-wire over TCP. It listens, so the svp2p server
// reaches it through Legacy.ConnectPeers exactly as it reaches a real node.
// ---------------------------------------------------------------------------

type scriptedServingPeer struct {
	t     *testing.T
	chain *fixtureChain
	net   wire.BitcoinNet
	addr  string

	mu         sync.Mutex
	ln         net.Listener
	conns      []net.Conn
	served     int
	serveLimit int // negative means "serve everything"
	closed     bool
}

// newScriptedServingPeer reserves an address and, when listen is true, starts
// serving on it immediately. A peer created with listen false holds the
// address only; Listen starts it later, which is how the stall test brings a
// second peer up after the first has already stalled.
func newScriptedServingPeer(t *testing.T, chain *fixtureChain, netMagic wire.BitcoinNet, serveLimit int, listen bool) *scriptedServingPeer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()

	p := &scriptedServingPeer{
		t:          t,
		chain:      chain,
		net:        netMagic,
		addr:       addr,
		serveLimit: serveLimit,
	}

	require.NoError(t, ln.Close())

	if listen {
		p.Listen()
	}

	t.Cleanup(p.Close)

	return p
}

func (p *scriptedServingPeer) Listen() {
	p.t.Helper()

	ln, err := net.Listen("tcp", p.addr)
	require.NoError(p.t, err)

	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()

	go p.acceptLoop(ln)
}

func (p *scriptedServingPeer) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = conn.Close()

			return
		}

		p.conns = append(p.conns, conn)
		p.mu.Unlock()

		go p.serve(conn)
	}
}

// Close stops the listener and drops every connection, which is how the test
// makes a stalling peer go away for good.
func (p *scriptedServingPeer) Close() {
	p.mu.Lock()

	if p.closed {
		p.mu.Unlock()
		return
	}

	p.closed = true
	ln := p.ln
	conns := p.conns
	p.conns = nil
	p.ln = nil
	p.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}

	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (p *scriptedServingPeer) servedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.served
}

// claimServe reports whether this peer will answer one more getdata entry.
func (p *scriptedServingPeer) claimServe() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.serveLimit >= 0 && p.served >= p.serveLimit {
		return false
	}

	p.served++

	return true
}

func (p *scriptedServingPeer) write(conn net.Conn, msg wire.Message) error {
	_, err := wire.WriteMessageWithEncodingN(conn, msg, wire.ProtocolVersion, p.net, wire.BaseEncoding)
	return err
}

func (p *scriptedServingPeer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	local := wire.NewNetAddressIPPort(net.ParseIP("127.0.0.1"), 0, wire.SFNodeNetwork)
	remote := wire.NewNetAddressIPPort(net.ParseIP("127.0.0.1"), 0, wire.SFNodeNetwork)

	for {
		_, msg, _, err := wire.ReadMessageWithEncodingN(conn, wire.ProtocolVersion, p.net, wire.BaseEncoding)
		if err != nil {
			return
		}

		switch m := msg.(type) {
		case *wire.MsgVersion:
			// The inbound side answers with its own version, then verack and
			// protoconf, which is the order net_processing.cpp uses.
			version := wire.NewMsgVersion(local, remote, uint64(time.Now().UnixNano()), int32(len(p.chain.headers))) //nolint:gosec // fixture height is small
			version.UserAgent = "/svp2p-scripted-peer:1.0/"
			version.Services = wire.SFNodeNetwork

			if p.write(conn, version) != nil {
				return
			}

			if p.write(conn, wire.NewMsgVerAck()) != nil {
				return
			}

			if p.write(conn, wire.NewMsgProtoconf(wire.DefaultMaxRecvPayloadLength, true)) != nil {
				return
			}

		case *wire.MsgPing:
			if p.write(conn, wire.NewMsgPong(m.Nonce)) != nil {
				return
			}

		case *wire.MsgGetHeaders:
			if p.write(conn, p.headersFor(m)) != nil {
				return
			}

		case *wire.MsgGetData:
			for _, inv := range m.InvList {
				if inv == nil || inv.Type != wire.InvTypeBlock {
					continue
				}

				block, known := p.chain.blocks[inv.Hash]
				if !known {
					continue
				}

				if !p.claimServe() {
					// The stall: the peer keeps the connection up and simply
					// stops answering for the blocks it was asked for.
					continue
				}

				if p.write(conn, block) != nil {
					return
				}
			}
		}
	}
}

// headersFor answers a getheaders from the first locator hash this peer knows,
// which is the same rule a real node applies.
func (p *scriptedServingPeer) headersFor(msg *wire.MsgGetHeaders) *wire.MsgHeaders {
	start := int32(0)

	for _, hash := range msg.BlockLocatorHashes {
		if hash == nil {
			continue
		}

		if height, known := p.chain.heights[*hash]; known {
			start = height
			break
		}
	}

	headers := wire.NewMsgHeaders()

	for i := start; i < int32(len(p.chain.headers)); i++ {
		if len(headers.Headers) == wire.MaxBlockHeadersPerMsg {
			break
		}

		header := p.chain.headers[i]

		_ = headers.AddBlockHeader(header)

		if msg.HashStop != (chainhash.Hash{}) && header.BlockHash() == msg.HashStop {
			break
		}
	}

	return headers
}

// ---------------------------------------------------------------------------
// Harness: a real svp2p Server with the real ingestion pipeline behind it.
// ---------------------------------------------------------------------------

type syncHarness struct {
	logger          *recordingLogger
	blockchainStore blockchain_store.Store
	server          *Server
}

// syncHarnessCounter keeps each harness's settings context unique.
// util.GetListener caches gRPC listeners by (context, service name), so two
// harnesses sharing a context would share a listener.
var syncHarnessCounter atomic.Uint64

func newSyncHarness(t *testing.T, name string, connectPeers []string, maxLastBlockTime time.Duration) *syncHarness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	logger := &recordingLogger{}

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.Context = fmt.Sprintf("%s-svp2psync-%s-%d", tSettings.Context, name, syncHarnessCounter.Add(1))
	tSettings.Legacy.ListenAddresses = []string{"127.0.0.1:0"}
	tSettings.Legacy.GRPCListenAddress = freePort(t)
	tSettings.Legacy.WorkingDir = t.TempDir()
	tSettings.Legacy.ConnectPeers = connectPeers
	tSettings.GRPCAdminAPIKey = "test-admin-key"
	tSettings.BlockAssembly.GRPCListenAddress = freePort(t)
	tSettings.BlockAssembly.GRPCAddress = tSettings.BlockAssembly.GRPCListenAddress
	tSettings.SubtreeValidation.GRPCListenAddress = freePort(t)
	tSettings.SubtreeValidation.GRPCAddress = tSettings.SubtreeValidation.GRPCListenAddress
	tSettings.BlockValidation.GRPCListenAddress = freePort(t)
	tSettings.BlockValidation.GRPCAddress = tSettings.BlockValidation.GRPCListenAddress

	// blockchain.LocalClient answers SetBlockSubtreesSet straight from the
	// store and publishes no BlockSubtreesSet notification, which is what
	// normally drives block validation's setMined worker. Its periodic
	// mined-not-set sweep is the other, equally real, trigger, so the sweep
	// interval is shortened rather than the mined flag being written by hand:
	// the bridge's waitForPreviousBlockMined is a genuine gate on this path and
	// it must be a real setMined that opens it.
	tSettings.BlockValidation.PeriodicProcessingInterval = 200 * time.Millisecond

	blockchainStore, err := blockchain_store.NewStore(logger, &url.URL{Scheme: "sqlitememory"}, tSettings)
	require.NoError(t, err)

	blockchainClient, err := blockchain.NewLocalClient(logger, tSettings, blockchainStore, nil, nil)
	require.NoError(t, err)

	utxoStoreURL, err := url.Parse("sqlitememory:///svp2p")
	require.NoError(t, err)

	tSettings.UtxoStore.UtxoStore = utxoStoreURL

	utxoStore, err := utxosql.New(ctx, logger, tSettings, utxoStoreURL)
	require.NoError(t, err)

	subtreeStore := blobmemory.New()
	tempStore := blobmemory.New()
	txStore := blobmemory.New()

	// The three Teranode services the ingestion path reaches over gRPC run
	// here for real, in process, on their own loopback ports — the same
	// constructors and clients daemon_services.go uses. Nothing about the
	// pipeline is stubbed: what is removed is the network between processes.
	blockAssemblyClient := startBlockAssembly(ctx, t, logger, tSettings, txStore, subtreeStore, utxoStore, blockchainClient)

	validatorClient, err := validator.New(ctx, logger, tSettings, utxoStore, nil, nil, nil, blockAssemblyClient, blockchainClient)
	require.NoError(t, err)

	subtreeValidationClient := startSubtreeValidation(ctx, t, tSettings.Context, logger, tSettings, subtreeStore,
		txStore, utxoStore, validatorClient, blockchainClient)

	blockValidationClient := startBlockValidation(ctx, t, tSettings.Context, logger, tSettings, subtreeStore, txStore,
		utxoStore, validatorClient, blockchainClient, blockAssemblyClient)

	server := NewWithDeps(logger, tSettings, blockchainClient, Deps{
		ValidationClient:  validatorClient,
		SubtreeStore:      subtreeStore,
		TempStore:         tempStore,
		UtxoStore:         utxoStore,
		SubtreeValidation: subtreeValidationClient,
		BlockValidation:   blockValidationClient,
		BlockAssembly:     blockAssemblyClient,
	})

	server.syncTick = 100 * time.Millisecond
	server.maxLastBlockTime = maxLastBlockTime

	// Registered last so it runs FIRST on teardown: every service above was
	// started against this context and its Stop only returns once the context
	// is cancelled.
	t.Cleanup(cancel)

	return &syncHarness{
		logger:          logger,
		blockchainStore: blockchainStore,
		server:          server,
	}
}

// start brings the svp2p service up and registers its teardown.
func (h *syncHarness) start(t *testing.T) {
	t.Helper()

	cancel := startServer(t, h.server)

	t.Cleanup(func() {
		cancel()
		_ = h.server.Stop(context.Background())
	})
}

// waitForHeight polls the node's own blockchain store until it reaches height,
// and dumps the node's log before failing so the reason is visible.
func (h *syncHarness) waitForHeight(t *testing.T, height uint32, timeout time.Duration, what string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if _, got := h.bestBlock(t); got == height {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	_, got := h.bestBlock(t)

	h.logger.dump(t)
	t.Fatalf("%s: the blockchain store reached height %d, wanted %d", what, got, height)
}

// waitFor polls cond and dumps the node's log before failing.
func (h *syncHarness) waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	h.logger.dump(t)
	t.Fatal(what)
}

// bestBlock reads the node's own blockchain store, which is the exit-criterion
// witness: nothing but a completed ingest through ProcessBlock puts a block there.
func (h *syncHarness) bestBlock(t *testing.T) (chainhash.Hash, uint32) {
	t.Helper()

	header, meta, err := h.blockchainStore.GetBestBlockHeader(context.Background())
	if err != nil {
		return chainhash.Hash{}, 0
	}

	return *header.Hash(), meta.Height
}

// startBlockAssembly runs the real block assembly service in-process on its own
// gRPC port. Deps.BlockAssembly is a *blockassembly.Client, so the ingestion
// path's WaitForBlockAssemblyReady gate can only be satisfied honestly by a
// service that actually answers GetBlockAssemblyState.
func startBlockAssembly(ctx context.Context, t *testing.T, logger ulogger.Logger, tSettings *settings.Settings,
	txStore, subtreeStore blob.Store, utxoStore utxo.Store, blockchainClient blockchain.ClientI) *blockassembly.Client {
	t.Helper()

	ba := blockassembly.New(logger, tSettings, txStore, utxoStore, subtreeStore, blockchainClient)
	require.NoError(t, ba.Init(ctx))

	readyCh := make(chan struct{})

	go func() { _ = ba.Start(ctx, readyCh) }()

	select {
	case <-readyCh:
	case <-time.After(30 * time.Second):
		t.Fatal("block assembly did not become ready")
	}

	client, err := blockassembly.NewClient(ctx, logger, tSettings)
	require.NoError(t, err)

	return client
}

// inMemoryConsumer builds a real KafkaConsumerGroup over the in-memory broker,
// which is what the repo's testing rules ask for instead of a Kafka mock.
func inMemoryConsumer(t *testing.T, logger ulogger.Logger, topic, group string) *kafka.KafkaConsumerGroup {
	t.Helper()

	consumer, err := kafka.NewKafkaConsumerGroup(kafka.KafkaConsumerConfig{
		Logger:          logger,
		URL:             &url.URL{Scheme: "memory", Host: "svp2p-sync-test", Path: "/" + topic},
		Topic:           topic,
		Partitions:      1,
		ConsumerGroupID: group,
	})
	require.NoError(t, err)

	return consumer
}

// startSubtreeValidation runs the real subtree validation service in-process
// and returns the real gRPC client for it, which is what Deps.SubtreeValidation
// carries in the daemon.
func startSubtreeValidation(ctx context.Context, t *testing.T, name string, logger ulogger.Logger,
	tSettings *settings.Settings, subtreeStore, txStore blob.Store, utxoStore utxo.Store,
	validatorClient validator.Interface, blockchainClient blockchain.ClientI) subtreevalidation.Interface {
	t.Helper()

	server, err := subtreevalidation.New(ctx, logger, tSettings, subtreeStore, txStore, utxoStore, validatorClient,
		blockchainClient,
		inMemoryConsumer(t, logger, "subtree-"+name, "svp2p-sync-subtree-"+name),
		inMemoryConsumer(t, logger, "txmeta-"+name, "svp2p-sync-txmeta-"+name),
		nil, nil)
	require.NoError(t, err)
	require.NoError(t, server.Init(ctx))

	readyCh := make(chan struct{})

	go func() { _ = server.Start(ctx, readyCh) }()

	waitReady(t, readyCh, "subtree validation")

	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	client, err := subtreevalidation.NewClient(ctx, logger, tSettings, "svp2p-sync-test")
	require.NoError(t, err)

	return client
}

// startBlockValidation runs the real block validation service in-process and
// returns the real gRPC client for it. It must start after subtree validation:
// blockvalidation Server.Init dials that service.
func startBlockValidation(ctx context.Context, t *testing.T, name string, logger ulogger.Logger,
	tSettings *settings.Settings, subtreeStore, txStore blob.Store, utxoStore utxo.Store,
	validatorClient validator.Interface, blockchainClient blockchain.ClientI,
	blockAssemblyClient *blockassembly.Client) blockvalidation.Interface {
	t.Helper()

	server := blockvalidation.New(logger, tSettings, subtreeStore, txStore, utxoStore, validatorClient,
		blockchainClient, inMemoryConsumer(t, logger, "blocks-"+name, "svp2p-sync-blocks-"+name),
		blockAssemblyClient, nil)
	require.NoError(t, server.Init(ctx))

	readyCh := make(chan struct{})

	go func() { _ = server.Start(ctx, readyCh) }()

	waitReady(t, readyCh, "block validation")

	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	client, err := blockvalidation.NewClient(ctx, logger, tSettings, "svp2p-sync-test")
	require.NoError(t, err)

	return client
}

func waitReady(t *testing.T, readyCh <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-readyCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not become ready", what)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

const syncTestChainLength = 20

// TestIntegrationHeadersFirstSyncFromScriptedPeer is the in-repo proxy for the
// phase's "syncs testnet end to end" exit: a node that starts at genesis pulls
// a whole chain from one peer through headers-first sync, block download,
// streaming ingest and ProcessBlock, and ends with that chain in its own
// blockchain store.
func TestIntegrationHeadersFirstSyncFromScriptedPeer(t *testing.T) {
	require.Greater(t, syncTestChainLength, protocol.MaxBlocksInTransitPerPeer,
		"the chain must be longer than one getdata batch, or the second scheduling round is never exercised")

	tSettings := test.CreateBaseTestSettings(t)
	chain := buildFixtureChain(t, tSettings, syncTestChainLength)

	peer := newScriptedServingPeer(t, chain, tSettings.ChainCfgParams.Net, -1, true)

	h := newSyncHarness(t, "happy", []string{peer.addr}, 0)
	h.start(t)

	h.waitForHeight(t, uint32(syncTestChainLength), 60*time.Second, "headers-first sync")

	hash, height := h.bestBlock(t)
	require.Equal(t, uint32(syncTestChainLength), height)
	require.Equal(t, chain.tip(), hash, "the node's best block must be the scripted chain's tip")
}

// TestIntegrationSyncPeerRotationRecoversFromAStalledPeer covers the
// adversarial leg: the serving peer answers headers and half the blocks, then
// stops answering getdata while staying connected. Only the sync-peer rotation
// (blockdownload.go CheckStall -> StallActionRotateSyncPeer) can catch that
// peer, because nothing is ever in flight at the download window's edge, so
// DetectStalling's nStallingSince clock never starts.
func TestIntegrationSyncPeerRotationRecoversFromAStalledPeer(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)
	chain := buildFixtureChain(t, tSettings, syncTestChainLength)

	stalled := newScriptedServingPeer(t, chain, tSettings.ChainCfgParams.Net, syncTestChainLength/2, true)
	replacement := newScriptedServingPeer(t, chain, tSettings.ChainCfgParams.Net, -1, false)

	h := newSyncHarness(t, "stall", []string{stalled.addr, replacement.addr}, 3*time.Second)
	h.start(t)

	// The stalling peer delivers its half first, and then nothing.
	h.waitFor(t, func() bool { return stalled.servedCount() == syncTestChainLength/2 },
		60*time.Second, "the stalling peer never delivered its half of the chain")

	h.waitFor(t, func() bool {
		_, height := h.bestBlock(t)
		return height == uint32(syncTestChainLength/2)
	}, 60*time.Second, "the half the stalling peer did deliver never reached the blockchain store")

	// The rotation is the mechanism under test: manager.go syncTickOnce logs
	// exactly this line when BlockDownloader.CheckStall returns
	// StallActionRotateSyncPeer.
	h.waitFor(t, func() bool {
		return h.logger.contains("rotating the sync peer") && h.logger.contains("no sync progress")
	},
		60*time.Second, "the sync peer was never rotated for making no progress")

	// The rotation releases the sync slot and the peer's downloads without
	// disconnecting it. DetectStalling's disconnect never fires here: with the
	// whole remaining chain inside the download window, no peer is ever held at
	// the window's edge, so nStallingSince never starts.
	require.Equal(t, int32(1), h.server.manager.ConnectedCount(),
		"a rotation must leave the rotated peer connected")
	require.False(t, h.logger.contains("stalling block download"),
		"the stall must be caught by the rotation, not by the block-stalling disconnect")

	// The stalling peer then goes away, which is what releases the blocks it
	// re-claimed while it was still the only candidate on offer.
	stalled.Close()

	replacement.Listen()

	h.waitForHeight(t, uint32(syncTestChainLength), 120*time.Second, "sync after the replacement peer connected")

	hash, _ := h.bestBlock(t)
	require.Equal(t, chain.tip(), hash)
	require.Positive(t, replacement.servedCount(), "the replacement peer must have served the rest of the chain")
}

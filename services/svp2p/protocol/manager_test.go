package protocol

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

func managerSettings() *settings.Settings {
	return &settings.Settings{
		ChainCfgParams: &chaincfg.MainNetParams,
		Legacy: settings.LegacySettings{
			PeerIdleTimeout:    125 * time.Second,
			AllowBlockPriority: true,
		},
	}
}

func newTestManager(t *testing.T, banList *BanList) *PeerManager {
	t.Helper()

	if banList == nil {
		var err error

		banList, err = NewBanList("")
		require.NoError(t, err)
	}

	return NewPeerManager(ulogger.TestLogger{}, managerSettings(), banList)
}

// dialScripted connects to addr and acts as the outbound side of the
// handshake: it sends version first, then completes the exchange.
func dialScripted(t *testing.T, addr string) *scriptedPeer {
	t.Helper()

	nc, err := net.Dial("tcp", addr)
	require.NoError(t, err)

	return &scriptedPeer{nc: nc}
}

func (s *scriptedPeer) completeOutboundHandshake(t *testing.T) {
	t.Helper()

	s.completeOutboundHandshakeAs(t, remoteVersion(4321))
}

// completeOutboundHandshakeAs is completeOutboundHandshake with the version
// message spelled out, so a test can advertise service flags — the sync
// candidate rules read them (headersync.go isSyncCandidate).
func (s *scriptedPeer) completeOutboundHandshakeAs(t *testing.T, version *wire.MsgVersion) {
	t.Helper()

	s.write(t, version)

	sawVerack := false
	for !sawVerack {
		switch s.read(t).(type) {
		case *wire.MsgVerAck:
			sawVerack = true
		case *wire.MsgVersion, *wire.MsgProtoconf:
		}
	}

	s.write(t, wire.NewMsgVerAck())
}

func TestManagerAcceptsInboundHandshake(t *testing.T) {
	m := newTestManager(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	addrs := m.ListenAddrs()
	require.Len(t, addrs, 1)

	far := dialScripted(t, addrs[0])
	far.completeOutboundHandshake(t)

	require.Eventually(t, func() bool { return m.ConnectedCount() == 1 }, 5*time.Second, 50*time.Millisecond)

	snaps := m.Snapshots()
	require.Len(t, snaps, 1)
	require.True(t, snaps[0].Inbound)
	require.Equal(t, "/sv:1.1.0/", snaps[0].UserAgent)

	require.NoError(t, far.nc.Close())
	require.Eventually(t, func() bool { return m.ConnectedCount() == 0 }, 5*time.Second, 50*time.Millisecond)
}

func TestManagerRejectsBannedInbound(t *testing.T) {
	banList, err := NewBanList("")
	require.NoError(t, err)
	require.NoError(t, banList.Add("127.0.0.1", time.Now().Add(time.Hour)))

	m := newTestManager(t, banList)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	nc, err := net.Dial("tcp", m.ListenAddrs()[0])
	require.NoError(t, err)

	defer func() { _ = nc.Close() }()

	// net.cpp CConnman::AcceptConnection: banned peers are dropped before
	// any protocol traffic. Expect the socket to close without data.
	require.NoError(t, nc.SetReadDeadline(time.Now().Add(5*time.Second)))

	buf := make([]byte, 1)
	_, err = nc.Read(buf)
	require.Error(t, err)
	require.Equal(t, int32(0), m.ConnectedCount())
}

func TestManagerDialsConfiguredPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer func() { _ = ln.Close() }()

	tSettings := managerSettings()
	tSettings.Legacy.ConnectPeers = []string{ln.Addr().String()}

	banList, err := NewBanList("")
	require.NoError(t, err)

	m := NewPeerManager(ulogger.TestLogger{}, tSettings, banList)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, nil))

	defer func() { require.NoError(t, m.Stop()) }()

	nc, err := ln.Accept()
	require.NoError(t, err)

	far := &scriptedPeer{nc: nc}
	require.IsType(t, &wire.MsgVersion{}, far.read(t)) // outbound dialer sends version first

	require.NoError(t, nc.Close())
}

// captureLogger records Infof lines so a test can assert on disconnect
// reasons without any new production surface: PeerManager already logs
// each peer's terminal error via Infof (manager.go runPeer).
type captureLogger struct {
	ulogger.TestLogger
	mu   sync.Mutex
	logs []string
}

func (l *captureLogger) Infof(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.logs = append(l.logs, fmt.Sprintf(format, args...))
}

func (l *captureLogger) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, line := range l.logs {
		if strings.Contains(line, substr) {
			return true
		}
	}

	return false
}

// establishedCount counts peers whose handshake reached Established,
// using PeerManager's existing private peer registry and Peer's existing
// public Established() channel — no new production surface.
func establishedCount(m *PeerManager) int {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))

	for p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()

	n := 0

	for _, p := range peers {
		select {
		case <-p.Established():
			n++
		default:
		}
	}

	return n
}

// Ledger ruling 2026-08-18: the plan's "dialer must not redial" assertion
// was a defect, superseded by SVNode fidelity (spec §4.3). net.cpp
// CConnman::CheckIncomingNonce / net_processing.cpp ProcessVersionMessage
// detect a self-connect only on the accepting (inbound) side, and that
// side disconnects immediately without ever pushing its own version
// reply — so the outbound (-connect) dialer only ever sees a plain
// socket close, never ErrSelfConnection, and keeps redialing on its
// normal backoff. That matches real SVNode/bitcoind behavior for a
// self-pointed -connect misconfiguration and is not a defect this task
// fixes. What this task guarantees: a self-connection never reaches
// Established, on either end, no matter how many times the dialer retries.
func TestManagerDetectsSelfConnection(t *testing.T) {
	// Reserve a free port, then reuse the same address as both this
	// manager's listener and its configured outbound peer, so the dialer
	// connects back to itself: a real self-connect, where the outbound and
	// inbound ends of the loopback carry different per-connection nonces.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	tSettings := managerSettings()
	tSettings.Legacy.ConnectPeers = []string{addr}

	banList, err := NewBanList("")
	require.NoError(t, err)

	logger := &captureLogger{}
	m := NewPeerManager(logger, tSettings, banList)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{addr}))
	defer func() { require.NoError(t, m.Stop()) }()

	// The inbound side's registry check kills the session inside onVersion,
	// before any reply — confirms detection actually fired rather than the
	// dialer simply never having reached the listener.
	require.Eventually(t, func() bool { return logger.contains("svp2p: connected to self") },
		5*time.Second, 50*time.Millisecond, "expected the inbound side to log an ErrSelfConnection disconnect")

	// The behavioral contract: no self-connection ever reaches Established,
	// on either end, across at least two full redial windows — including
	// the redials the dialer keeps making per the ruling above.
	require.Never(t, func() bool { return establishedCount(m) != 0 }, 2*dialRetryBase, 100*time.Millisecond)
}

// recordingIngestor is a fake of protocol's own narrow ingest interface
// (peer.go BlockIngestor), not of the bridge: spec §4.4 requires this package
// to be testable without a Teranode stack, and the real bridge composition
// behind the interface is covered by Tasks 9, 10 and 13. It records what the
// peer loop handed it, in arrival order.
type recordingIngestor struct {
	// outcome is what every Ingest reports. The zero value is success.
	outcome IngestOutcome

	mu       sync.Mutex
	ingested []chainhash.Hash
	sizes    []uint64
	txBytes  []int64
}

func (r *recordingIngestor) WatchProgress(rd io.ReadCloser) IngestProgress {
	return newTestProgress(rd)
}

func (r *recordingIngestor) Ingest(_ context.Context, req BlockIngestRequest) IngestOutcome {
	// The real composition (bridge.IngestBlock) consumes the stream and
	// releases it on every exit path; do the same, or the transport read loop
	// stays parked and no second block ever arrives.
	n, err := io.Copy(io.Discard, req.TxReader)

	if closeErr := req.TxReader.Close(); closeErr != nil && err == nil {
		err = closeErr
	}

	if err != nil {
		return IngestOutcome{Err: err}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.ingested = append(r.ingested, req.Header.BlockHash())
	r.sizes = append(r.sizes, req.SizeBytes)
	r.txBytes = append(r.txBytes, n)

	return r.outcome
}

func (r *recordingIngestor) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.ingested)
}

// envelope returns the declared payload size and the transaction bytes the
// ingest actually read for call i.
func (r *recordingIngestor) envelope(i int) (uint64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.sizes[i], r.txBytes[i]
}

func (r *recordingIngestor) hashes() []chainhash.Hash {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]chainhash.Hash(nil), r.ingested...)
}

// testProgress is the IngestProgress the fake hands back, with the same
// contract bridge.ProgressReader carries: the stamp is seeded at construction
// so a watcher never sees a zero time.
type testProgress struct {
	r io.ReadCloser

	mu   sync.Mutex
	read uint64
	last time.Time
}

func newTestProgress(r io.ReadCloser) *testProgress {
	return &testProgress{r: r, last: time.Now()}
}

func (p *testProgress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)

	if n > 0 {
		p.mu.Lock()
		p.read += uint64(n) //nolint:gosec // n is non-negative
		p.last = time.Now()
		p.mu.Unlock()
	}

	return n, err
}

func (p *testProgress) Close() error { return p.r.Close() }

func (p *testProgress) BytesRead() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.read
}

func (p *testProgress) LastProgress() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.last
}

// blockFor builds a wire block for header with one syntactically valid
// transaction, which is all the streaming transport needs: it decodes the
// header and the transaction count and hands the rest to the consumer.
func blockFor(header *wire.BlockHeader) *wire.MsgBlock {
	block := wire.NewMsgBlock(header)

	tx := wire.NewMsgTx(1)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{}, 0xffffffff), []byte{0x51}))
	tx.AddTxOut(wire.NewTxOut(0, []byte{0x51}))

	_ = block.AddTransaction(tx)

	return block
}

// syncTestManager builds a manager whose chain params mine cheaply (the
// regtest powLimit on mainnet magic, so the non-regtest sync-candidate branch
// still runs) with block sync wired to ingestor.
func syncTestManager(t *testing.T, idx *HeaderIndex, ingestor BlockIngestor) *PeerManager {
	t.Helper()

	tSettings := managerSettings()
	tSettings.ChainCfgParams = syncTestParams(nil)

	banList, err := NewBanList("")
	require.NoError(t, err)

	m := NewPeerManager(ulogger.TestLogger{}, tSettings, banList)

	require.NoError(t, m.ConfigureSync(SyncConfig{
		Index:        idx,
		Ingestor:     ingestor,
		TickInterval: 20 * time.Millisecond,
	}))

	return m
}

// connectSyncPeer runs a scripted peer up to the point where we have asked it
// for every block in chain: handshake, our getheaders, its headers reply, and
// our getdata. It asserts the request path on the way through.
func connectSyncPeer(t *testing.T, m *PeerManager, genesis *wire.BlockHeader, chain []*wire.BlockHeader) *scriptedPeer {
	t.Helper()

	far := dialScripted(t, m.ListenAddrs()[0])

	version := remoteVersion(4321)
	version.Services = wire.SFNodeNetwork
	far.completeOutboundHandshakeAs(t, version)

	// net_processing.cpp SendMessages: the initial getheaders goes out as soon
	// as the handshake finishes, from a locator on our own tip.
	getHeaders, ok := far.readUntil(t, wire.CmdGetHeaders).(*wire.MsgGetHeaders)
	require.True(t, ok)
	require.Len(t, getHeaders.BlockLocatorHashes, 1)
	require.Equal(t, genesis.BlockHash(), *getHeaders.BlockLocatorHashes[0])

	headers := wire.NewMsgHeaders()
	for _, header := range chain {
		require.NoError(t, headers.AddBlockHeader(header))
	}

	far.write(t, headers)

	// The scheduler is the only thing that may request a block (Task 6), and
	// it asks for exactly what it just learned about, in chain order.
	getData, ok := far.readUntil(t, wire.CmdGetData).(*wire.MsgGetData)
	require.True(t, ok)
	require.Len(t, getData.InvList, len(chain))

	for i, inv := range getData.InvList {
		require.Equal(t, wire.InvTypeBlock, inv.Type)
		require.Equal(t, chain[i].BlockHash(), inv.Hash, "getdata entry %d", i)
	}

	return far
}

// TestManagerDrivesHeadersFirstBlockSync is the whole Phase 2 path through the
// live peer loop: handshake, our getheaders, the peer's headers, our getdata
// for exactly those blocks, the blocks themselves, and one ingest per block in
// chain order — with every download released afterwards.
func TestManagerDrivesHeadersFirstBlockSync(t *testing.T) {
	genesis := syncGenesis()
	chain := minedRun(genesis, 3, 1)

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	ingestor := &recordingIngestor{}
	m := syncTestManager(t, idx, ingestor)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	far := connectSyncPeer(t, m, genesis, chain)
	defer func() { _ = far.nc.Close() }()

	for _, header := range chain {
		far.write(t, blockFor(header))
	}

	require.Eventually(t, func() bool { return ingestor.count() == len(chain) },
		10*time.Second, 20*time.Millisecond, "not every block reached the ingest interface")

	// The envelope the transport hands the ingestor is the block's own: the
	// declared payload length (the honest admission weight) and the
	// transactions after the 80 byte header and the count varint.
	for i, header := range chain {
		payload := int64(blockFor(header).SerializeSize())
		size, txBytes := ingestor.envelope(i)

		require.Equal(t, uint64(payload), size, "block %d: SizeBytes must be the declared payload length", i)
		require.Equal(t, payload-80-1, txBytes, "block %d: the ingest reads the payload past the header and the tx count", i)
	}

	want := make([]chainhash.Hash, 0, len(chain))
	for _, header := range chain {
		want = append(want, header.BlockHash())
	}

	require.Equal(t, want, ingestor.hashes(), "blocks must be ingested in the order they arrived")

	// Every ingest reported completion to the scheduler: nothing is left in
	// flight and all three blocks are recorded as held (BlockReceived).
	require.Eventually(t, func() bool {
		m.syncMu.Lock()
		defer m.syncMu.Unlock()

		return m.blockDownloader.BlocksInFlight() == 0 && len(m.blockDownloader.haveData) == len(chain)
	}, 10*time.Second, 20*time.Millisecond, "block downloads were not released on ingest completion")
}

// TestManagerDisconnectsAPeerWhoseBlockIsRejected carries
// services/legacy/peer_server.go shouldDisconnectOnBlockErr: a block the
// pipeline rejected is the peer's fault, and the peer goes — otherwise the
// same bad block is re-offered to the same peer for ever.
func TestManagerDisconnectsAPeerWhoseBlockIsRejected(t *testing.T) {
	genesis := syncGenesis()
	chain := minedRun(genesis, 1, 3)

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	ingestor := &recordingIngestor{
		outcome: IngestOutcome{Err: errors.New(errors.ERR_BLOCK_INVALID, "svp2p: test block is invalid")},
	}

	m := syncTestManager(t, idx, ingestor)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	far := connectSyncPeer(t, m, genesis, chain)
	defer func() { _ = far.nc.Close() }()

	far.write(t, blockFor(chain[0]))

	require.Eventually(t, func() bool { return m.ConnectedCount() == 0 },
		10*time.Second, 20*time.Millisecond, "a peer serving a rejected block must be disconnected")
}

// TestManagerKeepsAPeerAfterALocalFault is the other half of the same rule: a
// transient LOCAL condition is ours, not the peer's, so the peer stays and the
// block is simply re-offered.
func TestManagerKeepsAPeerAfterALocalFault(t *testing.T) {
	genesis := syncGenesis()
	chain := minedRun(genesis, 1, 4)

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	ingestor := &recordingIngestor{
		outcome: IngestOutcome{
			Err:            errors.NewServiceError("svp2p: test utxo store is unavailable"),
			TransientLocal: true,
		},
	}

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

	// The block goes back on offer, so the scheduler asks for it again — and
	// the peer that delivered it is still there to ask.
	getData, ok := far.readUntil(t, wire.CmdGetData).(*wire.MsgGetData)
	require.True(t, ok)
	require.Len(t, getData.InvList, 1)
	require.Equal(t, chain[0].BlockHash(), getData.InvList[0].Hash)

	require.Equal(t, int32(1), m.ConnectedCount(), "a local fault must not cost the peer its connection")
}

// TestManagerDisconnectsAnUnrequestedBlock carries
// services/legacy/peer_server.go OnBlock: an unrequested block is refused
// before it can consume admission budget, so a peer cannot flood the shared
// budget and starve the real sync peer.
func TestManagerDisconnectsAnUnrequestedBlock(t *testing.T) {
	genesis := syncGenesis()
	chain := minedRun(genesis, 1, 5)

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	ingestor := &recordingIngestor{}
	m := syncTestManager(t, idx, ingestor)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	far := dialScripted(t, m.ListenAddrs()[0])
	defer func() { _ = far.nc.Close() }()

	version := remoteVersion(4321)
	version.Services = wire.SFNodeNetwork
	far.completeOutboundHandshakeAs(t, version)

	require.Eventually(t, func() bool { return m.ConnectedCount() == 1 }, 5*time.Second, 20*time.Millisecond)

	// Nothing has been requested from this peer: this block is unsolicited.
	far.write(t, blockFor(chain[0]))

	require.Eventually(t, func() bool { return m.ConnectedCount() == 0 },
		10*time.Second, 20*time.Millisecond, "an unrequested block must cost the peer its connection")

	require.Zero(t, ingestor.count(), "an unrequested block must never reach the ingest path")
}

// TestManagerDuplicateReleasesTheHolderRecord covers the exact leak sequence a
// rotation opens: the sync peer A is rotated while block X is in flight from
// it, the scheduler re-offers X to peer B, B is refused by the admission dedup
// because A is still ingesting X, and A then completes. Without releasing B's
// record on the duplicate report, X stays in flight from B for ever and is
// never re-offered.
func TestManagerDuplicateReleasesTheHolderRecord(t *testing.T) {
	genesis := syncGenesis()
	chain := minedRun(genesis, 1, 6)

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	connected, err := idx.AddHeader(chain[0])
	require.NoError(t, err)
	require.True(t, connected)

	m := syncTestManager(t, idx, &recordingIngestor{})

	block, ok := idx.Lookup(chain[0].BlockHash())
	require.True(t, ok)

	peerA := NewSyncPeer("1.1.1.1:8333", wire.SFNodeNetwork, newPeerSyncState())
	peerB := NewSyncPeer("2.2.2.2:8333", wire.SFNodeNetwork, newPeerSyncState())

	m.syncMu.Lock()
	require.True(t, m.blockDownloader.MarkBlockAsInFlight(peerA, block))

	// The rotation releases A's downloads while its ingest is still running.
	m.blockDownloader.PeerDisconnected(peerA)

	// The scheduler re-offers the block to B.
	require.True(t, m.blockDownloader.MarkBlockAsInFlight(peerB, block))
	m.syncMu.Unlock()

	// B is refused by the admission dedup: A still holds the admission slot.
	require.NoError(t, m.BlockDone(peerB, block.Hash, IngestOutcome{
		Duplicate: true,
		Err:       errors.NewServiceError("duplicate block already in flight"),
	}))

	// A's ingest completes.
	require.NoError(t, m.BlockDone(peerA, block.Hash, IngestOutcome{}))

	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	require.False(t, m.blockDownloader.IsInFlight(block.Hash),
		"the duplicate must release the record of the peer that will not deliver")
	require.Equal(t, 0, peerB.State.nBlocksInFlight)
	require.Contains(t, m.blockDownloader.haveData, block.Hash,
		"the copy that completed still counts as held")
}

// TestManagerAdvertisesHeaderIndexHeight covers the Phase 1 placeholder this
// task replaces: net_processing.cpp PushNodeVersion sends
// nNodeStartingHeight, which is now the header index tip.
func TestManagerAdvertisesHeaderIndexHeight(t *testing.T) {
	genesis := syncGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	for _, header := range minedRun(genesis, 4, 2) {
		connected, addErr := idx.AddHeader(header)
		require.NoError(t, addErr)
		require.True(t, connected)
	}

	m := syncTestManager(t, idx, &recordingIngestor{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	defer func() { require.NoError(t, m.Stop()) }()

	far := dialScripted(t, m.ListenAddrs()[0])
	defer func() { _ = far.nc.Close() }()

	far.write(t, remoteVersion(4321))

	ourVersion, ok := far.readUntil(t, wire.CmdVersion).(*wire.MsgVersion)
	require.True(t, ok)
	require.Equal(t, int32(4), ourVersion.LastBlock)
}

func TestManagerStopClosesEverything(t *testing.T) {
	m := newTestManager(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, m.Start(ctx, []string{"127.0.0.1:0"}))

	addr := m.ListenAddrs()[0]

	far := dialScripted(t, addr)
	far.completeOutboundHandshake(t)
	require.Eventually(t, func() bool { return m.ConnectedCount() == 1 }, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, m.Stop())
	require.Equal(t, int32(0), m.ConnectedCount())

	_, err := net.DialTimeout("tcp", addr, time.Second)
	require.Error(t, err)
}

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
func syncTestManager(t *testing.T, idx *HeaderIndex, ingestor BlockIngestor, checkpoints ...chaincfg.Checkpoint) *PeerManager {
	t.Helper()

	tSettings := managerSettings()
	tSettings.ChainCfgParams = syncTestParams(checkpoints)

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

	// What the adapter reports for a block the pipeline rejected: the peer is
	// at fault, and only that flag disconnects (see
	// TestManagerBlockDoneDisconnectsOnlyOnPeerFault).
	ingestor := &recordingIngestor{
		outcome: IngestOutcome{
			Err:       errors.New(errors.ERR_BLOCK_INVALID, "svp2p: test block is invalid"),
			PeerFault: true,
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
	require.True(t, m.blockDownloader.MarkBlockAsInFlight(peerA, block, testNow))

	// The rotation releases A's downloads while its ingest is still running.
	m.blockDownloader.PeerDisconnected(peerA)

	// The scheduler re-offers the block to B.
	require.True(t, m.blockDownloader.MarkBlockAsInFlight(peerB, block, testNow))
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

// TestManagerRotationNeverDisconnects pins the rotation contract from the
// outside: a pre-admission timeout releases the sync slot and this peer's
// downloads, and the peer stays connected. Disconnecting it as well would
// drive that same release a second time through peerGone.
func TestManagerRotationNeverDisconnects(t *testing.T) {
	genesis := syncGenesis()
	chain := minedRun(genesis, 1, 7)

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	ingestor := &recordingIngestor{
		outcome: IngestOutcome{
			Err:    errors.New(errors.ERR_ERROR, "svp2p: test pre-admission timed out"),
			Rotate: true,
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

	require.Never(t, func() bool { return m.ConnectedCount() == 0 }, time.Second, 20*time.Millisecond,
		"a rotation must not disconnect the peer it rotated")
}

// TestManagerBlockDoneDisconnectsOnlyOnPeerFault is the same contract at the
// dispatcher: the verdict reads PeerFault, and nothing else.
func TestManagerBlockDoneDisconnectsOnlyOnPeerFault(t *testing.T) {
	genesis := syncGenesis()
	chain := minedRun(genesis, 1, 8)

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	connected, err := idx.AddHeader(chain[0])
	require.NoError(t, err)
	require.True(t, connected)

	m := syncTestManager(t, idx, &recordingIngestor{})

	block, ok := idx.Lookup(chain[0].BlockHash())
	require.True(t, ok)

	fault := errors.New(errors.ERR_ERROR, "svp2p: test failure")

	for _, tc := range []struct {
		name    string
		outcome IngestOutcome
		drops   bool
	}{
		{name: "pre-admission timeout", outcome: IngestOutcome{Err: fault, Rotate: true}},
		{name: "transient local", outcome: IngestOutcome{Err: fault, TransientLocal: true}},
		{name: "unclassified", outcome: IngestOutcome{Err: fault}},
		{name: "peer fault", outcome: IngestOutcome{Err: fault, PeerFault: true}, drops: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := NewSyncPeer("1.2.3.4:8333", wire.SFNodeNetwork, newPeerSyncState())

			m.syncMu.Lock()
			require.True(t, m.blockDownloader.MarkBlockAsInFlight(peer, block, testNow))
			m.syncMu.Unlock()

			err := m.BlockDone(peer, block.Hash, tc.outcome)

			if tc.drops {
				require.Error(t, err, "a block the pipeline rejected must cost the peer its connection")
				return
			}

			require.NoError(t, err, "only a peer fault may disconnect")
		})
	}
}

// TestManagerDropsAnUnsolicitedStreamWithoutReadingIt covers the refusal path's
// cost: a peer that declares a huge payload and then goes silent must not be
// able to hold the peer loop, which is the goroutine servicing the idle timer
// and shutdown.
func TestManagerDropsAnUnsolicitedStreamWithoutReadingIt(t *testing.T) {
	genesis := syncGenesis()
	chain := minedRun(genesis, 1, 9)

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

	// Nothing was requested from this peer, and the frame it sends declares
	// 8 MiB of payload but carries only the block header and the transaction
	// count. Reading the rest would never return.
	far.writeStalledBlockFrame(t, chain[0], 8<<20)

	require.Eventually(t, func() bool { return m.ConnectedCount() == 0 }, 5*time.Second, 20*time.Millisecond,
		"an unsolicited stream must be dropped unread, not drained on the peer loop")

	require.Zero(t, ingestor.count())
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

// rotationFixture drives the manager's sync tick one pass at a time, with an
// explicit clock and no live connections. A rotation and the download pass that
// follows it happen inside a single syncPass call, so the only way to observe
// what the rotated peer was asked for on that pass is to hold the pass still.
//
// The peers here are real *Peer values over an unread net.Pipe. Nothing is ever
// sent to them: syncPass returns the send list rather than sending it.
type rotationFixture struct {
	m       *PeerManager
	idx     *HeaderIndex
	genesis *wire.BlockHeader
	chain   []*wire.BlockHeader
	handles []peerHandle
}

func newRotationFixture(t *testing.T, height int, checkpoints ...chaincfg.Checkpoint) *rotationFixture {
	t.Helper()

	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	return newRotationFixtureFor(t, syncTestManager(t, idx, &recordingIngestor{}, checkpoints...), idx, genesis, height)
}

// newRegtestRotationFixture is newRotationFixture on the regression network with
// the localhost restriction lifted, which is the one configuration where
// isSyncCandidate (headersync.go) accepts a peer on nothing at all: no service
// flag, and not even its address. That is what makes the handshake guard
// load-bearing there. On every other network a peer that has not sent its
// version has advertised no services, so isSyncCandidate refuses it anyway and
// the hole never opens.
func newRegtestRotationFixture(t *testing.T, height int) *rotationFixture {
	t.Helper()

	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	params := *syncTestParams(nil)
	params.Net = wire.RegTestNet

	tSettings := managerSettings()
	tSettings.ChainCfgParams = &params

	banList, err := NewBanList("")
	require.NoError(t, err)

	m := NewPeerManager(ulogger.TestLogger{}, tSettings, banList)

	require.NoError(t, m.ConfigureSync(SyncConfig{
		Index:                            idx,
		Ingestor:                         &recordingIngestor{},
		TickInterval:                     20 * time.Millisecond,
		AllowSyncCandidateFromLocalPeers: true,
	}))

	return newRotationFixtureFor(t, m, idx, genesis, height)
}

func newRotationFixtureFor(t *testing.T, m *PeerManager, idx *HeaderIndex, genesis *wire.BlockHeader, height int) *rotationFixture {
	t.Helper()

	// The chain is built AFTER ConfigureSync sampled the index tip, so our own
	// active tip stays at genesis and every header above it is a block still to
	// download.
	chain := buildChain(t, idx, &nonceCounter{}, genesis, height)

	f := &rotationFixture{m: m, idx: idx, genesis: genesis, chain: chain}

	for _, addr := range []string{"1.1.1.1:8333", "2.2.2.2:8333"} {
		peer, far := newTestPeer(t, time.Minute, time.Minute)

		t.Cleanup(func() { _ = far.nc.Close() })

		// established stands for what runPeer's handshake would have set: these
		// peers are never run, and syncPass skips a peer that has not finished
		// its version exchange.
		f.handles = append(f.handles, peerHandle{peer: peer, sync: fullNodePeer(addr), established: true})
	}

	return f
}

// node returns the indexed header at height h, where 0 is genesis.
func (f *rotationFixture) node(t *testing.T, h int) HeaderNode {
	t.Helper()

	hash := f.genesis.BlockHash()
	if h > 0 {
		hash = f.chain[h-1].BlockHash()
	}

	n, ok := f.idx.Lookup(hash)
	require.True(t, ok)

	return n
}

// setup runs sync-state mutations under the mutex the machines are caller-locked
// against, so a test seeds state the same way the manager would.
//
// Do NOT call pass from inside fn: syncPass takes the same mutex and it is not
// reentrant.
func (f *rotationFixture) setup(fn func()) {
	f.m.syncMu.Lock()
	defer f.m.syncMu.Unlock()

	fn()
}

// pass runs one sync tick at the given clock, with no peer ingesting.
func (f *rotationFixture) pass(now int64) (out []outgoing, disconnect []*Peer) {
	return f.m.syncPass(f.handles, make([]IngestSnapshot, len(f.handles)), now)
}

// getHeadersTo returns every getheaders one pass produced for peer, which is
// how a test observes the header-sync eligibility sweep: PeerEstablished
// answers an eligible peer with exactly that message and an ineligible one with
// nothing.
func getHeadersTo(out []outgoing, peer *Peer) []*wire.MsgGetHeaders {
	msgs := make([]*wire.MsgGetHeaders, 0)

	for _, o := range out {
		if o.peer != peer {
			continue
		}

		for _, msg := range o.msgs {
			if getHeaders, ok := msg.(*wire.MsgGetHeaders); ok {
				msgs = append(msgs, getHeaders)
			}
		}
	}

	return msgs
}

// getDataTo returns every block hash one pass asked peer for.
func getDataTo(out []outgoing, peer *Peer) []chainhash.Hash {
	hashes := make([]chainhash.Hash, 0)

	for _, o := range out {
		if o.peer != peer {
			continue
		}

		for _, msg := range o.msgs {
			getData, ok := msg.(*wire.MsgGetData)
			if !ok {
				continue
			}

			for _, inv := range getData.InvList {
				hashes = append(hashes, inv.Hash)
			}
		}
	}

	return hashes
}

// TestSyncPass_RotationDoesNotReHandBlocksToTheRotatedPeer is the manager-level
// pin on mechanism (a) of the rotation finding: the rotate branch must skip the
// rest of that peer's pass. Without the skip, SendGetDataBlocks hands the peer
// we just judged non-progressing another MaxBlocksInTransitPerPeer blocks on
// the SAME pass, because clearPeer nils pindexLastCommonBlock but keeps
// pindexBestKnownBlock and the window simply bootstraps again.
func TestSyncPass_RotationDoesNotReHandBlocksToTheRotatedPeer(t *testing.T) {
	f := newRotationFixture(t, 40)

	rotating, spare := f.handles[0], f.handles[1]
	tip := f.node(t, len(f.chain))

	held := make([]chainhash.Hash, 0, 3)

	f.setup(func() {
		require.Len(t, f.m.headerSync.PeerEstablished(rotating.sync), 1)
		require.True(t, rotating.sync.State.fSyncStarted)

		rotating.sync.State.pindexBestKnownBlock = &tip

		for h := 1; h <= 3; h++ {
			node := f.node(t, h)
			require.True(t, f.m.blockDownloader.MarkBlockAsInFlight(rotating.sync, node, testNow))

			held = append(held, node.Hash)
		}

		// A progress clock that has already run past the rotation window.
		rotating.sync.State.nLastProgressTime = testNow - micros(2*MaxLastBlockTime)

		// A second peer on the same chain, eligible for both the slot and the
		// blocks the rotation frees.
		spare.sync.State.pindexBestKnownBlock = &tip
	})

	out, disconnect := f.pass(testNow)

	require.Empty(t, disconnect, "a rotation must not disconnect anyone")

	require.Empty(t, getDataTo(out, rotating.peer),
		"a peer just judged non-progressing must not be re-handed blocks on the same pass")

	// The fixture fixes the handle order, so the other peer is reached on this
	// same pass. Production ranges a map, so a rotation the map order puts last
	// re-offers on the next tick instead; both are within the contract.
	asked := getDataTo(out, spare.peer)
	require.Len(t, asked, MaxBlocksInTransitPerPeer)
	require.Subset(t, asked, held, "the blocks the rotation freed must be re-offered to another peer")

	f.setup(func() {
		require.False(t, rotating.sync.State.fSyncStarted, "the sync slot must be released")
		require.Equal(t, 0, rotating.sync.State.nBlocksInFlight)
		require.Nil(t, rotating.sync.State.pindexLastCommonBlock,
			"the rotated peer must stay schedulable from scratch")

		for _, hash := range held {
			require.True(t, f.m.blockDownloader.IsInFlightFrom(spare.sync, hash),
				"the freed block must now be in flight from the other peer")
		}
	})
}

// TestSyncPass_ASingleCandidateNodeTakesTheSlotBackOnTheNextTick pins the one
// case the post-rotation note on CheckStall has to carve out: a node with a
// single candidate must not stop syncing for ever because that candidate
// rotated. The per-pass eligibility sweep is what hands the slot back, and it
// hands it back on the NEXT tick, because the sweep runs ahead of the stall
// check for the same peer — the SendMessages order (SendBlockSync at
// net_processing.cpp:5865, DetectStalling at :5881).
//
// The pass that rotates therefore asks the peer for NOTHING: not blocks, which
// is Task 3's skip, and not headers either.
func TestSyncPass_ASingleCandidateNodeTakesTheSlotBackOnTheNextTick(t *testing.T) {
	f := newRotationFixture(t, 40)

	only := f.handles[0]
	f.handles = f.handles[:1]

	tip := f.node(t, len(f.chain))

	f.setup(func() {
		require.Len(t, f.m.headerSync.PeerEstablished(only.sync), 1)

		only.sync.State.pindexBestKnownBlock = &tip
		require.True(t, f.m.blockDownloader.MarkBlockAsInFlight(only.sync, f.node(t, 1), testNow))

		only.sync.State.nLastProgressTime = testNow - micros(2*MaxLastBlockTime)
	})

	out, disconnect := f.pass(testNow)

	require.Empty(t, disconnect)
	require.Empty(t, getDataTo(out, only.peer),
		"the skip must hold even when the rotated peer is the only candidate")
	require.Empty(t, out,
		"a peer that rotated on this pass must not be asked for anything on it")

	f.setup(func() {
		require.False(t, only.sync.State.fSyncStarted,
			"the rotation must release the slot, and nothing on this pass may hand it back")
	})

	out, disconnect = f.pass(testNow + micros(time.Second))

	require.Empty(t, disconnect)
	require.Len(t, out, 1)
	require.Equal(t, only.peer, out[0].peer)
	require.Len(t, getHeadersTo(out, only.peer), 1,
		"the next tick's sweep gives the only candidate the sync slot back")
	require.Len(t, getDataTo(out, only.peer), MaxBlocksInTransitPerPeer,
		"and the rotation left it schedulable, so it is a download peer again too")

	f.setup(func() {
		require.True(t, only.sync.State.fSyncStarted,
			"the only candidate takes the sync slot back")
	})
}

// TestSyncPass_SweepsHeaderSyncEligibilityOnEveryTick pins the per-pass shape of
// the header-sync eligibility check. net_processing.cpp SendMessages calls
// SendBlockSync for EVERY peer on EVERY pass (net_processing.cpp:5865, the
// function at :5180-5222), so a peer that becomes eligible later starts header
// sync without any event reaching the node. Before this, PeerEstablished was
// reached only from the handshake and from a rotation or a peerGone.
//
// It also pins the Phase 2 Task 5 round invariant the sweep must not break:
// while a headers-first round runs, no second peer gains fSyncStarted, so
// roundAnchorHeight and the ErrHeadersNoProgress terminator stay
// single-sourced.
func TestSyncPass_SweepsHeaderSyncEligibilityOnEveryTick(t *testing.T) {
	// A checkpoint far above our tip keeps the headers-first round running for
	// the whole test.
	cpHash := chainhash.Hash{0xC0}

	f := newRotationFixture(t, 40, chaincfg.Checkpoint{Height: 100000, Hash: &cpHash})

	first, second := f.handles[0], f.handles[1]

	// Nothing has been established: no handshake, no rotation, no peerGone. The
	// sweep is the only thing that can start header sync here.
	f.setup(func() {
		require.False(t, first.sync.State.fSyncStarted)
		require.False(t, second.sync.State.fSyncStarted)
	})

	out, disconnect := f.pass(testNow)

	require.Empty(t, disconnect)
	require.Len(t, getHeadersTo(out, first.peer), 1,
		"the sweep must start header sync with an eligible peer on the tick alone")
	require.Empty(t, getHeadersTo(out, second.peer),
		"a headers-first round is single-slot: the second peer must not be asked for headers")

	f.setup(func() {
		require.True(t, f.m.headerSync.IsHeadersFirstMode())
		require.True(t, first.sync.State.fSyncStarted)
		require.False(t, second.sync.State.fSyncStarted,
			"no second peer may gain fSyncStarted while a headers-first round runs")
	})

	// The sweep is idempotent for a peer that already holds the slot: it must
	// not re-issue the initial getheaders on every tick.
	out, disconnect = f.pass(testNow + micros(time.Second))

	require.Empty(t, disconnect)
	require.Empty(t, getHeadersTo(out, first.peer))
	require.Empty(t, getHeadersTo(out, second.peer))

	f.setup(func() {
		require.True(t, first.sync.State.fSyncStarted)
		require.False(t, second.sync.State.fSyncStarted)
	})
}

// TestSyncPass_HeaderSyncBreadthRecoversAfterEveryPeerRotates is the regression
// the per-pass sweep exists to close, and it is the cost Task 4 booked into this
// task at PeerEstablished.
//
// Past the final checkpoint the near-tip relaxation lets several peers hold
// fSyncStarted at once, and headers-first mode is off there, so CheckStall's
// header-progress refresh is unreachable and nLastProgressTime moves only when a
// block is delivered. Most rotation windows deliver no block, so EVERY header
// peer rotates on the same pass. With the eligibility check reachable only from
// an event, the one election that followed refilled exactly one slot and the node
// fell to a single header peer until peers reconnected.
//
// The pass that rotates them still hands nothing back — the sweep runs ahead of
// the stall check for the same peer, so a peer that rotated on this pass cannot
// take the slot back on it.
func TestSyncPass_HeaderSyncBreadthRecoversAfterEveryPeerRotates(t *testing.T) {
	// No checkpoints: this is the steady state past the final one, where
	// headersFirstMode is false and the 24 hour relaxation is the only gate.
	f := newRotationFixture(t, 40)

	a, b := f.handles[0], f.handles[1]
	tip := f.node(t, len(f.chain))

	f.setup(func() {
		require.Len(t, f.m.headerSync.PeerEstablished(a.sync), 1)
		require.Len(t, f.m.headerSync.PeerEstablished(b.sync), 1,
			"the 24 hour near-tip relaxation admits a second header peer")
		require.False(t, f.m.headerSync.IsHeadersFirstMode())

		for _, h := range f.handles {
			h.sync.State.pindexBestKnownBlock = &tip
			h.sync.State.nLastProgressTime = testNow - micros(2*MaxLastBlockTime)
		}
	})

	out, disconnect := f.pass(testNow)

	require.Empty(t, disconnect, "a rotation must not disconnect anyone")
	require.Empty(t, getHeadersTo(out, a.peer),
		"a peer that rotated on this pass must not take the slot back on it")
	require.Empty(t, getHeadersTo(out, b.peer),
		"a peer that rotated on this pass must not take the slot back on it")

	f.setup(func() {
		require.False(t, a.sync.State.fSyncStarted)
		require.False(t, b.sync.State.fSyncStarted)
	})

	// The next tick restores the breadth, with no event of any kind.
	out, disconnect = f.pass(testNow + micros(time.Second))

	require.Empty(t, disconnect)
	require.Len(t, getHeadersTo(out, a.peer), 1)
	require.Len(t, getHeadersTo(out, b.peer), 1,
		"header sync must restart with EVERY eligible peer, not just the one an election reached")

	f.setup(func() {
		require.True(t, a.sync.State.fSyncStarted)
		require.True(t, b.sync.State.fSyncStarted)
	})
}

// TestSyncPass_ADisconnectUnwindsTheSlotTheSweepJustGranted pins the hazard the
// SendMessages pass order carries, and the recovery that answers it.
//
// The sweep runs BEFORE the stall check for the same peer (SendBlockSync at
// net_processing.cpp:5865, DetectStalling at :5881), so a peer holding no slot
// but heading the download window is granted the slot and THEN disconnected on
// the same pass. While it holds that slot every peer after it on the pass is
// refused, because the single-slot guard in PeerEstablished reads the mode as it
// stands. SVNode carries the same order and the same hazard, so this port keeps
// it.
//
// What makes it safe is that the disconnect unwinds every piece of state the
// sweep seeded: runPeer drives peerGone once Run returns, and
// HeaderSync.PeerDisconnected reaches releaseSyncPeer, whose early return cannot
// fire here — the sweep has just set fSyncStarted and the disconnect clause in
// CheckStall mutates nothing. This test walks that path.
func TestSyncPass_ADisconnectUnwindsTheSlotTheSweepJustGranted(t *testing.T) {
	// A checkpoint far above our tip is what makes the sweep seed the round
	// state, so the unwind has the most to undo.
	cpHash := chainhash.Hash{0xC0}

	f := newRotationFixture(t, 40, chaincfg.Checkpoint{Height: 100000, Hash: &cpHash})

	stalled, bystander := f.handles[0], f.handles[1]
	tip := f.node(t, len(f.chain))

	f.setup(func() {
		// The state a rotation leaves behind: connected, no sync slot, and now
		// heading the download window.
		require.False(t, stalled.sync.State.fSyncStarted)

		stalled.sync.State.pindexBestKnownBlock = &tip
		stalled.sync.State.nStallingSince = testNow

		require.True(t, f.m.blockDownloader.MarkBlockAsInFlight(stalled.sync, f.node(t, 1), testNow))

		bystander.sync.State.pindexBestKnownBlock = &tip
	})

	_, disconnect := f.pass(testNow + micros(BlockStallingTimeout) + 1)

	require.Equal(t, []*Peer{stalled.peer}, disconnect)

	// THE HAZARD, pinned rather than argued: the doomed peer holds the slot and
	// the round for the rest of this pass, and the peer after it is refused.
	f.setup(func() {
		require.True(t, stalled.sync.State.fSyncStarted,
			"the sweep grants the slot before the stall check judges the peer")
		require.Equal(t, 1, f.m.headerSync.nSyncStarted)
		require.True(t, f.m.headerSync.IsHeadersFirstMode())
		require.NotZero(t, f.m.headerSync.roundAnchorHeight)
		require.NotNil(t, f.m.headerSync.nextCheckpoint)

		require.False(t, bystander.sync.State.fSyncStarted,
			"the doomed peer's slot refuses every peer after it on this pass")
	})

	// THE RECOVERY: what runPeer does once Run returns.
	f.m.peerGone(stalled.sync)

	f.setup(func() {
		require.False(t, stalled.sync.State.fSyncStarted, "the slot must come back")
		require.Zero(t, f.m.headerSync.nSyncStarted)
		require.False(t, f.m.headerSync.IsHeadersFirstMode(), "the round must not outlive the peer driving it")
		require.Zero(t, f.m.headerSync.roundAnchorHeight)
		require.Equal(t, &cpHash, f.m.headerSync.nextCheckpoint.Hash, "resetHeaderState must re-seed the checkpoint")

		require.Zero(t, stalled.sync.State.nBlocksInFlight)
		require.Zero(t, stalled.sync.State.nStallingSince)
		require.Nil(t, stalled.sync.State.pindexLastCommonBlock)
		require.False(t, f.m.blockDownloader.IsInFlight(f.node(t, 1).Hash),
			"the disconnected peer's downloads must go back on offer")
	})
}

// TestSyncPass_SkipsAPeerThatHasNotFinishedItsHandshake pins the guard
// net_processing.cpp SendMessages carries before it does anything at all for a
// peer (:5835-5837, "Don't send anything until the version handshake is
// complete"). runPeer puts a peer in the registry BEFORE it runs the handshake,
// so a pass can reach one that is not ready, and the eligibility sweep would
// otherwise give it the sync slot and a getheaders before its verack.
//
// The fixture is regtest with the localhost restriction lifted, because that is
// where isSyncCandidate refuses nothing — see newRegtestRotationFixture.
func TestSyncPass_SkipsAPeerThatHasNotFinishedItsHandshake(t *testing.T) {
	f := newRegtestRotationFixture(t, 40)

	f.handles[0].established = false

	waiting, ready := f.handles[0], f.handles[1]
	tip := f.node(t, len(f.chain))

	f.setup(func() {
		// Both peers announced the same chain and neither holds the slot, so the
		// handshake is the only thing that can separate them on this pass.
		waiting.sync.State.pindexBestKnownBlock = &tip
		ready.sync.State.pindexBestKnownBlock = &tip
	})

	out, disconnect := f.pass(testNow)

	require.Empty(t, disconnect)

	require.Empty(t, getHeadersTo(out, waiting.peer),
		"a peer that has not finished its handshake must not be given the sync slot")
	require.Empty(t, getDataTo(out, waiting.peer),
		"and must not be asked for blocks either")

	f.setup(func() {
		require.False(t, waiting.sync.State.fSyncStarted,
			"nothing pre-handshake may reach the sync machines")
		require.Zero(t, waiting.sync.State.nBlocksInFlight)
	})

	// The other peer, identical but for its handshake, takes both passes on that
	// same tick. That is what shows the skip is the handshake and nothing else.
	require.Len(t, getHeadersTo(out, ready.peer), 1)
	require.Len(t, getDataTo(out, ready.peer), MaxBlocksInTransitPerPeer)
}

// TestPeerGone_DoesNotElectAPeerThatHasNotFinishedItsHandshake pins the same
// SendMessages guard (:5835-5837) on the EVENT path. peerGone and BlockDone's
// rotation both offer the freed slot through electLocked, and both read the same
// registry, which holds a peer from before its handshake runs. The two differ
// only in which peer they exclude, and the guard is orthogonal to that, so one
// of them covers both.
func TestPeerGone_DoesNotElectAPeerThatHasNotFinishedItsHandshake(t *testing.T) {
	f := newRegtestRotationFixture(t, 3)

	// A peer in the connection registry that has not completed its handshake,
	// which is exactly the state runPeer registers it in.
	candidate := f.handles[0]
	registerPeer(f.m, candidate.peer, candidate.sync)

	require.False(t, handshakeComplete(candidate.peer), "test setup: the peer must still be handshaking")

	// The peer holding the slot then goes away, which is what frees it.
	gone := fullNodePeer("3.3.3.3:8333")

	f.setup(func() {
		require.Len(t, f.m.headerSync.PeerEstablished(gone), 1)
	})

	f.m.peerGone(gone)

	f.setup(func() {
		require.False(t, candidate.sync.State.fSyncStarted,
			"a peer that has not finished its handshake must not be elected the sync peer")
		require.Zero(t, f.m.headerSync.nSyncStarted)

		// The control: the candidate is eligible in every respect EXCEPT its
		// handshake, so the guard is what refused it and not one of
		// PeerEstablished's own rules.
		require.Len(t, f.m.headerSync.PeerEstablished(candidate.sync), 1)
	})
}

// TestSyncPass_SchedulesBlocksFromEveryPromotedPeerInOneTick is the
// manager-level end of all-peer block scheduling, driven through the seam Task 4
// built: two peers park an announcement they can do nothing with, the index
// grows from our own chain, promoteBlockAvailabilityLocked resolves both parked
// hashes, and ONE sync tick then asks both of them for blocks.
//
// Neither peer ever held the sync slot when its hash was parked, which is the
// point: what makes a peer schedulable is pindexBestKnownBlock, not
// fSyncStarted.
func TestSyncPass_SchedulesBlocksFromEveryPromotedPeerInOneTick(t *testing.T) {
	genesis := testGenesis()

	idx, err := NewHeaderIndex(genesis)
	require.NoError(t, err)

	m := syncTestManager(t, idx, &recordingIngestor{})

	// Built AFTER ConfigureSync sampled the index tip and left unindexed, so our
	// own active chain stays at genesis and every header here is a block still to
	// download.
	nonces := &nonceCounter{}
	chain := make([]*wire.BlockHeader, 0, 2*MaxBlocksInTransitPerPeer)

	prev := genesis
	for i := 0; i < 2*MaxBlocksInTransitPerPeer; i++ {
		header := childOf(prev, nonces.take())
		chain = append(chain, header)
		prev = header
	}

	announced := chain[len(chain)-1].BlockHash()

	handles := make([]peerHandle, 0, 2)

	for _, addr := range []string{"1.1.1.1:8333", "2.2.2.2:8333"} {
		peer, far := newTestPeer(t, time.Minute, time.Minute)

		t.Cleanup(func() { _ = far.nc.Close() })

		syncPeer := fullNodePeer(addr)
		registerPeer(m, peer, syncPeer)

		// An inv for a block whose header we do not hold parks the hash, which is
		// the state a peer that never held the sync slot comes to rest in.
		msgs, invErr := m.Inv(syncPeer, invMsg(t, wire.InvTypeBlock, announced))
		require.NoError(t, invErr)
		require.Len(t, msgs, 1, "an unknown block inv asks for the headers that place it")

		handles = append(handles, peerHandle{peer: peer, sync: syncPeer, established: true})
	}

	m.syncMu.Lock()

	for i, h := range handles {
		require.Nil(t, h.sync.State.pindexBestKnownBlock, "peer %d: a parked hash is not availability yet", i)
	}

	m.syncMu.Unlock()

	orphans, err := m.AddHeaders(chain)
	require.NoError(t, err)
	require.Empty(t, orphans)

	m.syncMu.Lock()

	for i, h := range handles {
		require.NotNil(t, h.sync.State.pindexBestKnownBlock, "peer %d: the promotion sweep must resolve the parked hash", i)
		require.Equal(t, announced, h.sync.State.pindexBestKnownBlock.Hash)
		require.False(t, h.sync.State.fSyncStarted, "peer %d: neither peer has held the sync slot", i)
	}

	m.syncMu.Unlock()

	out, disconnect := m.syncPass(handles, make([]IngestSnapshot, len(handles)), testNow)

	require.Empty(t, disconnect)

	batches := [][]chainhash.Hash{getDataTo(out, handles[0].peer), getDataTo(out, handles[1].peer)}

	holders := make(map[chainhash.Hash]int, 2*MaxBlocksInTransitPerPeer)

	for i, batch := range batches {
		require.Len(t, batch, MaxBlocksInTransitPerPeer,
			"peer %d must be asked for its full share of the window on the same pass", i)

		for _, hash := range batch {
			previous, dup := holders[hash]
			require.False(t, dup, "block %s was requested from peer %d and peer %d", hash, previous, i)

			holders[hash] = i
		}
	}

	for _, header := range chain {
		require.Contains(t, holders, header.BlockHash(),
			"the two batches together must cover the whole chain neither peer has delivered")
	}
}

// TestSyncPass_ARotatedPeerThatStallsTheWindowStillGoes is mechanism (b): a
// rotated peer stays connected and keeps no sync slot, so its own rotation
// clause can never fire again. What still governs it is DetectStalling, which
// CheckStall runs BEFORE the fSyncStarted early-return. Reverse those two and a
// rotated-but-connected peer could never be released again.
func TestSyncPass_ARotatedPeerThatStallsTheWindowStillGoes(t *testing.T) {
	f := newRotationFixture(t, 40)

	stalled := f.handles[0]
	tip := f.node(t, len(f.chain))

	f.setup(func() {
		// The state a rotation leaves behind: connected, no sync slot, and now
		// heading the download window.
		require.False(t, stalled.sync.State.fSyncStarted)

		stalled.sync.State.pindexBestKnownBlock = &tip
		stalled.sync.State.nStallingSince = testNow
	})

	_, disconnect := f.pass(testNow + micros(BlockStallingTimeout) + 1)

	require.Equal(t, []*Peer{stalled.peer}, disconnect,
		"the staller rule must still reach a peer that holds no sync slot")
}

// TestSyncPass_ReHandedBlocksToASilentRotatedPeerAreReleasedAgain is the
// CROSS-TASK test for mechanism (c), the per-block download timeout, which is
// NOT carried yet.
//
// It pins the whole recovery chain as it stands today, and the chain is long.
// A rotated peer is a plain download peer, so a later tick hands it up to
// MaxBlocksInTransitPerPeer blocks. It stays silent, so it never gives them
// back and never asks for more: 16 in flight is where a silent peer stops, for
// ever. Nothing then reaches those 16 blocks until ANOTHER peer downloads the
// whole rest of the download window and still cannot move, which is the only
// thing that names the silent peer the staller and starts its clock. Only then
// does BlockStallingTimeout run, and only the disconnect that follows releases
// the blocks. With no second eligible peer, none of it ever happens.
//
// So the real cost is not the three ticks below. It is the download time for up
// to BlockDownloadWindow blocks — during which our own chain tip cannot advance
// past the hole those 16 blocks leave — plus BlockStallingTimeout, plus a
// disconnect. The ticks here are only the shape of it.
//
// WHEN THE PER-BLOCK DOWNLOAD TIMEOUT LANDS, EXTEND THIS TEST: the blocks must
// come back from the silent peer on the timeout alone — no window drain, no
// second peer to name it the staller, and no disconnect. Keep the staller path
// below as the backstop it is.
func TestSyncPass_ReHandedBlocksToASilentRotatedPeerAreReleasedAgain(t *testing.T) {
	const height = BlockDownloadWindow + 6

	f := newRotationFixture(t, height)

	silent, other := f.handles[0], f.handles[1]
	tip := f.node(t, height)

	f.setup(func() {
		require.Len(t, f.m.headerSync.PeerEstablished(silent.sync), 1)

		silent.sync.State.pindexBestKnownBlock = &tip
		require.True(t, f.m.blockDownloader.MarkBlockAsInFlight(silent.sync, f.node(t, 1), testNow))

		silent.sync.State.nLastProgressTime = testNow - micros(2*MaxLastBlockTime)
	})

	// Tick 1: the rotation. The other peer has announced no chain yet, so the
	// freed block goes nowhere — and the rotated peer is asked for nothing.
	out, disconnect := f.pass(testNow)

	require.Empty(t, disconnect)
	require.Empty(t, getDataTo(out, silent.peer))

	f.setup(func() {
		// A later tick hands the rotated peer blocks again: it is a plain
		// download peer now, and nothing about the rotation stops the scheduler
		// choosing it. This is where a silent peer comes to rest —
		// SendGetDataBlocks is the only caller of MarkBlockAsInFlight, it asks
		// for at most MaxBlocksInTransitPerPeer minus what is already in
		// flight, and nothing decrements that count until a block arrives.
		for h := 1; h <= MaxBlocksInTransitPerPeer; h++ {
			require.True(t, f.m.blockDownloader.MarkBlockAsInFlight(silent.sync, f.node(t, h), testNow))
		}

		other.sync.State.pindexBestKnownBlock = &tip

		// The other peer then downloads the whole rest of the window, 16 blocks
		// per tick, while the silent peer holds the head of it. That is the
		// real precondition for naming a staller, and it is the expensive part
		// of this recovery: our own tip cannot advance past the hole at height
		// 1 for any of it. Requesting and delivering them here in one loop is
		// the same sequence, minus the wire.
		for h := MaxBlocksInTransitPerPeer + 1; h <= BlockDownloadWindow; h++ {
			node := f.node(t, h)

			require.True(t, f.m.blockDownloader.MarkBlockAsInFlight(other.sync, node, testNow))
			require.True(t, f.m.blockDownloader.BlockReceived(other.sync, node.Hash, testNow))
		}
	})

	// Tick 2: the other peer has drained the window and still cannot move,
	// because the silent peer holds its head. THAT names the silent peer the
	// staller and starts its clock.
	second := testNow + micros(time.Second)

	out, disconnect = f.pass(second)

	require.Empty(t, disconnect)
	require.Empty(t, getDataTo(out, other.peer), "the window is held shut by the silent peer's 16 blocks")
	require.Equal(t, second, silent.sync.State.nStallingSince, "the peer holding the window head starts stalling")

	// Tick 3: the stall timeout expires. This is the expensive recovery, and it
	// is no longer the only one: the ticks here are seconds apart, so the
	// per-block download timeout has not come due. The test beside this one
	// drives that path instead.
	third := second + micros(BlockStallingTimeout) + 1

	_, disconnect = f.pass(third)

	require.Equal(t, []*Peer{silent.peer}, disconnect)

	// The disconnect the manager then performs is what releases them.
	f.m.peerGone(silent.sync)
	f.handles = f.handles[1:]

	out, _ = f.pass(third + micros(time.Second))

	require.Len(t, getDataTo(out, other.peer), MaxBlocksInTransitPerPeer,
		"the blocks the silent peer was re-handed must reach another peer in the end")
}

// registerPeer puts a peer in the connection registry the way runPeer does, so
// a manager-level test exercises the sweeps that read every connected peer's
// CNodeState entry. The *Peer itself is never run: these tests drive the
// dispatch methods directly and assert on sync state.
func registerPeer(m *PeerManager, peer *Peer, syncPeer *SyncPeer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.peers[peer] = syncPeer
}

// TestManagerPromotesBlockAvailabilityOnIndexGrowth is the manager-level pin on
// the eager promotion sweep. Phase 2 Task 5's N2 fix made a non-sync peer
// availability-only during a headers-first round, so its announcement parks in
// hashLastUnknownBlock. Before this sweep, only FindNextBlocksToDownload's own
// processBlockAvailability call could resolve that hash, which the download
// walk reaches on none of its early returns. The peer therefore stayed
// unschedulable while the round indexed the very chain it had announced.
func TestManagerPromotesBlockAvailabilityOnIndexGrowth(t *testing.T) {
	t.Run("a bystander parked during a round is promoted when the round indexes its hash", func(t *testing.T) {
		genesis := syncGenesis()

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		// A checkpoint far above our tip keeps the headers-first round running
		// for the whole test, which is what makes the bystander
		// availability-only.
		cpHash := chainhash.Hash{0xC0}
		m := syncTestManager(t, idx, &recordingIngestor{},
			chaincfg.Checkpoint{Height: 100000, Hash: &cpHash})

		holderPeer, holderFar := newTestPeer(t, time.Minute, time.Minute)
		bystanderPeer, bystanderFar := newTestPeer(t, time.Minute, time.Minute)

		t.Cleanup(func() {
			_ = holderFar.nc.Close()
			_ = bystanderFar.nc.Close()
		})

		holder := fullNodePeer("1.1.1.1:8333")
		bystander := fullNodePeer("2.2.2.2:8333")

		registerPeer(m, holderPeer, holder)
		registerPeer(m, bystanderPeer, bystander)

		require.Len(t, m.Established(holder, wire.SFNodeNetwork), 1)

		m.syncMu.Lock()
		require.True(t, m.headerSync.IsHeadersFirstMode())
		require.False(t, bystander.State.fSyncStarted, "the round is single-slot")
		m.syncMu.Unlock()

		batch := minedRun(genesis, 3, 4200)
		last := batch[len(batch)-1].BlockHash()

		// The bystander announces a chain we do not hold. Its batch is ignored
		// and the hash parks.
		msgs, score, err := m.Headers(bystander, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, score)
		require.Nil(t, msgs)

		m.syncMu.Lock()
		require.Equal(t, last, bystander.State.hashLastUnknownBlock)
		require.Nil(t, bystander.State.pindexBestKnownBlock, "a parked hash is not availability yet")
		m.syncMu.Unlock()

		// The round indexes exactly that chain. The bystander sends nothing
		// more: this call carries no traffic from it at all.
		_, score, err = m.Headers(holder, headersMsg(batch))
		require.NoError(t, err)
		require.Zero(t, score)

		m.syncMu.Lock()
		defer m.syncMu.Unlock()

		require.NotNil(t, bystander.State.pindexBestKnownBlock,
			"a parked hash must resolve the moment the index grows past it")
		require.Equal(t, last, bystander.State.pindexBestKnownBlock.Hash)
		require.Equal(t, int32(3), bystander.State.pindexBestKnownBlock.Height)
		require.Equal(t, chainhash.Hash{}, bystander.State.hashLastUnknownBlock)
	})

	t.Run("AddHeaders from our own chain promotes a parked announcement", func(t *testing.T) {
		genesis := syncGenesis()

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		m := syncTestManager(t, idx, &recordingIngestor{})

		peer, far := newTestPeer(t, time.Minute, time.Minute)
		t.Cleanup(func() { _ = far.nc.Close() })

		syncPeer := fullNodePeer("3.3.3.3:8333")
		registerPeer(m, peer, syncPeer)

		chain := minedRun(genesis, 2, 5100)
		last := chain[len(chain)-1].BlockHash()

		// An inv for a block whose header we do not hold parks the hash.
		msgs, err := m.Inv(syncPeer, invMsg(t, wire.InvTypeBlock, last))
		require.NoError(t, err)
		require.Len(t, msgs, 1, "an unknown block inv asks for the headers that place it")

		m.syncMu.Lock()
		require.Equal(t, last, syncPeer.State.hashLastUnknownBlock)
		require.Nil(t, syncPeer.State.pindexBestKnownBlock)
		m.syncMu.Unlock()

		// The blockchain subscription indexes that chain, and nothing else
		// happens.
		orphans, err := m.AddHeaders(chain)
		require.NoError(t, err)
		require.Empty(t, orphans)

		m.syncMu.Lock()
		defer m.syncMu.Unlock()

		require.NotNil(t, syncPeer.State.pindexBestKnownBlock)
		require.Equal(t, last, syncPeer.State.pindexBestKnownBlock.Hash)
		require.Equal(t, int32(2), syncPeer.State.pindexBestKnownBlock.Height)
		require.Equal(t, chainhash.Hash{}, syncPeer.State.hashLastUnknownBlock)
	})

	t.Run("a hash the index still does not hold stays parked", func(t *testing.T) {
		genesis := syncGenesis()

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		m := syncTestManager(t, idx, &recordingIngestor{})

		peer, far := newTestPeer(t, time.Minute, time.Minute)
		t.Cleanup(func() { _ = far.nc.Close() })

		syncPeer := fullNodePeer("4.4.4.4:8333")
		registerPeer(m, peer, syncPeer)

		unknown := chainhash.Hash{0xAB}

		_, err = m.Inv(syncPeer, invMsg(t, wire.InvTypeBlock, unknown))
		require.NoError(t, err)

		// The index grows, but not past the parked hash.
		orphans, err := m.AddHeaders(minedRun(genesis, 2, 6100))
		require.NoError(t, err)
		require.Empty(t, orphans)

		m.syncMu.Lock()
		defer m.syncMu.Unlock()

		require.Equal(t, unknown, syncPeer.State.hashLastUnknownBlock,
			"the sweep must not clear a hash it could not resolve")
		require.Nil(t, syncPeer.State.pindexBestKnownBlock,
			"the sweep must not invent availability the peer never announced")
	})
}

// TestSyncPass_TimesOutASilentRotatedPeerAndRehomesItsBlocks is the cheap
// recovery Task 6 adds, and the counterpart to
// TestSyncPass_ReHandedBlocksToASilentRotatedPeerAreReleasedAgain above, which
// pins the expensive path this replaces.
//
// Same starting position: a peer loses the sync slot to the rotation, stays
// connected, and the scheduler hands it blocks again on a later tick because a
// rotated peer is a plain download peer. What differs is what it takes to get
// those blocks back. The staller rule needs another peer to drain the entire
// download window first and then be blocked by this peer's blocks, which needs
// a second eligible peer and leaves our own tip stuck behind the hole for all
// of it. The per-block timeout needs neither: the front block's own clock runs
// out, the peer is disconnected, and its blocks go back on offer.
func TestSyncPass_TimesOutASilentRotatedPeerAndRehomesItsBlocks(t *testing.T) {
	const height = BlockDownloadWindow + 6

	f := newRotationFixture(t, height)

	silent, other := f.handles[0], f.handles[1]
	tip := f.node(t, height)

	f.setup(func() {
		require.Len(t, f.m.headerSync.PeerEstablished(silent.sync), 1)

		silent.sync.State.pindexBestKnownBlock = &tip
		silent.sync.State.nLastProgressTime = testNow - micros(2*MaxLastBlockTime)
	})

	// Tick 1: the rotation. It clears fSyncStarted, which is what puts this
	// peer beyond the reach of the rotation clause from here on.
	_, disconnect := f.pass(testNow)

	require.Empty(t, disconnect)
	require.False(t, silent.sync.State.fSyncStarted, "the rotation released the sync slot")

	second := testNow + micros(time.Second)

	f.setup(func() {
		// The scheduler hands the rotated peer a full batch on a later tick.
		for h := 1; h <= MaxBlocksInTransitPerPeer; h++ {
			require.True(t, f.m.blockDownloader.MarkBlockAsInFlight(silent.sync, f.node(t, h), second))
		}

		require.Equal(t, second, silent.sync.State.nDownloadingSince)

		other.sync.State.pindexBestKnownBlock = &tip
	})

	// Tick 2 gives the other peer the window above the hole, so both peers are
	// downloading when the timeout is judged. That is the case the per-peer term
	// of the formula exists for.
	out, disconnect := f.pass(second)

	require.Empty(t, disconnect)

	taken := getDataTo(out, other.peer)
	require.NotEmpty(t, taken, "the other peer takes the window above the hole")

	timedOut := second + micros(20*time.Minute)

	f.setup(func() {
		// The healthy peer delivers what it was asked for. This is what
		// separates the two peers: an empty queue has no front block, so the
		// timeout cannot reach it however long the tick clock has run. Without
		// this the case would prove nothing — a peer sitting on blocks IS what
		// the timeout is for, and both peers would go.
		for _, hash := range taken {
			require.True(t, f.m.blockDownloader.BlockReceived(other.sync, hash, timedOut-micros(time.Second)))
		}

		require.Empty(t, other.sync.State.vBlocksInFlight)
	})

	// The timeout expires for the silent peer alone. Nothing named it a staller,
	// and no peer had to drain the whole window to make this happen: with the
	// other peer's queue empty, the window is the bare steady-state base of one
	// block interval, which is 10 minutes.

	out, disconnect = f.pass(timedOut)

	require.Equal(t, []*Peer{silent.peer}, disconnect, "the front block's clock is what disconnects it")
	require.Equal(t, int64(0), silent.sync.State.nStallingSince, "no staller rule was involved")

	// The same pass hands the healthy peer the next slice of the window, which
	// fills its in-flight cap. Delivering it is what leaves room for the freed
	// blocks below — MaxBlocksInTransitPerPeer is a hard gate on the getdata
	// pass, so a peer already holding sixteen is offered nothing whatever came
	// free.
	f.setup(func() {
		for _, hash := range getDataTo(out, other.peer) {
			require.True(t, f.m.blockDownloader.BlockReceived(other.sync, hash, timedOut))
		}
	})

	// The disconnect the manager then performs releases the blocks.
	hole := f.node(t, 1)
	require.True(t, f.m.blockDownloader.IsInFlight(hole.Hash), "still held by the peer being dropped")

	f.m.peerGone(silent.sync)
	f.handles = f.handles[1:]

	require.False(t, f.m.blockDownloader.IsInFlight(hole.Hash), "the disconnect put it back on offer")

	out, _ = f.pass(timedOut + micros(time.Second))

	require.Subset(t, getDataTo(out, other.peer), []chainhash.Hash{hole.Hash},
		"the block at the head of the hole must reach the surviving peer on its next walk")
}

// TestConfigureSync_CarriesTheBlockDownloadTimeoutSettings walks the plumbing
// the three DetectStalling percentages travel: settings.Legacy through
// SyncConfig into the downloader that reads them. The behavior they produce is
// covered by TestCheckStall_HonoursTheConfiguredTimeoutWindow; what this pins is
// that an operator's value arrives at all, and that leaving one unset does not
// silently become a zero-length download window.
func TestConfigureSync_CarriesTheBlockDownloadTimeoutSettings(t *testing.T) {
	newManager := func(t *testing.T, cfg SyncConfig) *BlockDownloader {
		t.Helper()

		genesis := testGenesis()

		idx, err := NewHeaderIndex(genesis)
		require.NoError(t, err)

		tSettings := managerSettings()
		tSettings.ChainCfgParams = syncTestParams(nil)

		banList, err := NewBanList("")
		require.NoError(t, err)

		m := NewPeerManager(ulogger.TestLogger{}, tSettings, banList)

		cfg.Index = idx
		cfg.Ingestor = &recordingIngestor{}
		cfg.TickInterval = 20 * time.Millisecond

		require.NoError(t, m.ConfigureSync(cfg))

		return m.blockDownloader
	}

	t.Run("configured values reach the downloader", func(t *testing.T) {
		bd := newManager(t, SyncConfig{
			BlockDownloadTimeoutBasePercent:    250,
			BlockDownloadTimeoutBaseIBDPercent: 1500,
			BlockDownloadTimeoutPerPeerPercent: 75,
		})

		require.Equal(t, int64(250), bd.timeoutBasePercent)
		require.Equal(t, int64(1500), bd.timeoutBaseIBDPercent)
		require.Equal(t, int64(75), bd.timeoutPerPeerPercent)
	})

	t.Run("unset keeps the SVNode defaults, never zero", func(t *testing.T) {
		bd := newManager(t, SyncConfig{})

		require.Equal(t, BlockDownloadTimeoutBase, bd.timeoutBasePercent)
		require.Equal(t, BlockDownloadTimeoutBaseIBD, bd.timeoutBaseIBDPercent)
		require.Equal(t, BlockDownloadTimeoutPerPeer, bd.timeoutPerPeerPercent)
	})

	t.Run("a negative value is refused like an unset one", func(t *testing.T) {
		bd := newManager(t, SyncConfig{BlockDownloadTimeoutBasePercent: -1})

		require.Equal(t, BlockDownloadTimeoutBase, bd.timeoutBasePercent)
	})
}

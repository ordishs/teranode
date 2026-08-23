package protocol

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/svp2p/transport"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
)

const (
	// UserAgent identifies this node on the wire.
	UserAgent = "/teranode-svp2p:0.1.0/"

	// pingInterval mirrors SVNode PING_INTERVAL (net.h: 2 * 60).
	pingInterval = 2 * time.Minute

	// banScoreThreshold mirrors SVNode DEFAULT_BANSCORE_THRESHOLD (100).
	banScoreThreshold = 100

	// dialRetryBase and dialRetryMax bound the outbound reconnect backoff,
	// matching the retry semantics the bsvd connmgr gave the old service
	// (DefaultRetryDuration 5s, capped).
	dialRetryBase = 5 * time.Second
	dialRetryMax  = 5 * time.Minute

	// maxSentNonces bounds the self-connection nonce registry, mirroring
	// bsvd's sentNonces mruNonceMap size (50).
	maxSentNonces = 50

	sendBudgetBytes = 10 * 1024 * 1024
	recvQueueLen    = 128
	writeTimeout    = 30 * time.Second

	// defaultSyncTick is how often the manager runs the send pass the
	// machines are clockless for: the net_processing.cpp SendMessages block
	// pass (SendGetDataBlocks) and the DetectStalling check. SVNode runs
	// SendMessages on its 100 ms message loop; one second is enough here,
	// because a peer holds up to MAX_BLOCKS_IN_TRANSIT_PER_PEER blocks and
	// both stall windows are measured in tens of seconds.
	defaultSyncTick = time.Second
)

// SyncConfig wires the block-sync machines into the manager. Index is
// mandatory: without it the manager cannot answer a version message with a
// real starting height. Ingestor is optional — without it the manager keeps
// the index and runs the Phase 1 shape (handshake and ping only), because
// requesting blocks nothing can ingest would only burn bandwidth.
type SyncConfig struct {
	// Index is the shared header index. The manager takes every WRITE to it
	// under its own sync-state mutex; see the mutex's note.
	Index *HeaderIndex

	// Ingestor is the Teranode-side block ingestion path, or nil when the
	// bridge dependencies are not injected yet.
	Ingestor BlockIngestor

	// Fetcher is the Teranode-side read path a getdata is answered from, or
	// nil when the bridge dependencies are not injected yet. Unlike Ingestor
	// it gates nothing else: a manager with no fetcher still syncs, it just
	// answers no getdata.
	Fetcher BlockTxFetcher

	// DisableCheckpoints mirrors legacy netsync Config.DisableCheckpoints.
	DisableCheckpoints bool

	// AllowSyncCandidateFromLocalPeers mirrors
	// settings.Legacy.AllowSyncCandidateFromLocalPeers.
	AllowSyncCandidateFromLocalPeers bool

	// TickInterval overrides defaultSyncTick.
	TickInterval time.Duration

	// MaxLastBlockTime narrows the sync-peer rotation window from the
	// MaxLastBlockTime constant. Zero keeps the constant. It exists so an
	// integration test can observe a rotation in seconds instead of the three
	// real minutes the production window costs.
	MaxLastBlockTime time.Duration

	// BlockDownloadTimeoutBasePercent, BlockDownloadTimeoutBaseIBDPercent and
	// BlockDownloadTimeoutPerPeerPercent carry the three DetectStalling
	// percentages from settings.Legacy. Anything at or below zero keeps the
	// SVNode default the downloader is built with, so a caller that leaves them
	// unset gets SVNode's behavior rather than a zero-length timeout.
	BlockDownloadTimeoutBasePercent    int64
	BlockDownloadTimeoutBaseIBDPercent int64
	BlockDownloadTimeoutPerPeerPercent int64

	// BlockDownloadSlowFetchTimeout and BlockDownloadMaxParallelFetch carry the
	// parallel-fetch fuse and cap from settings.Legacy. Zero or less keeps the
	// SVNode default, as above. A cap of 1 is the way to turn racing off, since
	// one stalled holder already reaches it.
	BlockDownloadSlowFetchTimeout time.Duration
	BlockDownloadMaxParallelFetch int
}

// PeerManager owns listeners, the outbound dialer, the peer registry, and the
// node-wide sync state. It is the net.cpp CConnman counterpart, and the owner
// of this package's port of cs_main.
type PeerManager struct {
	logger    ulogger.Logger
	tSettings *settings.Settings
	banList   *BanList

	// mu guards the connection registry only: peers, listeners, nonces and
	// the started flag.
	mu        sync.Mutex
	peers     map[*Peer]*SyncPeer
	listeners []net.Listener
	nonces    []uint64
	started   bool

	// syncMu is this package's cs_main: ONE mutex over the whole sync-state
	// graph — HeaderSync, BlockDownloader, every peerSyncState, activeTip,
	// and every WRITE to headerIndex. Those machines are all caller-locked by
	// design (see their locking notes), and headersync's Lookup-before-
	// AddHeader reads stale state if any writer skips it, which is why
	// Server's blockchain-subscription goroutine reaches the index through
	// AddHeaders below rather than calling HeaderIndex.AddHeader itself.
	// Index READS need only the index's own lock.
	//
	// Lock order in this package is peer lock, then manager lock: nothing may
	// call a Peer method (Info, Disconnect) or Conn.Send while holding a
	// manager lock, or a future refactor deadlocks. mu and syncMu are never
	// held at the same time — every path that needs both snapshots the
	// registry under mu first, then works under syncMu.
	syncMu          sync.Mutex
	headerIndex     *HeaderIndex
	headerSync      *HeaderSync
	blockDownloader *BlockDownloader
	serving         *Serving
	ingestor        BlockIngestor
	fetcher         BlockTxFetcher
	// activeTip is our own best chain tip (the chainActive counterpart),
	// fed from the blockchain service and always a header present in the
	// index — the download scheduler cannot place a tip it cannot look up.
	activeTip HeaderNode
	syncTick  time.Duration

	quit chan struct{}
	wg   sync.WaitGroup
}

func NewPeerManager(logger ulogger.Logger, tSettings *settings.Settings, banList *BanList) *PeerManager {
	return &PeerManager{
		logger:    logger,
		tSettings: tSettings,
		banList:   banList,
		peers:     make(map[*Peer]*SyncPeer),
		quit:      make(chan struct{}),
		syncTick:  defaultSyncTick,
	}
}

// ConfigureSync gives the manager the shared header index and, when an
// ingestor is supplied, builds the headers-first and block-download machines
// that drive block sync. It must be called before Start.
func (m *PeerManager) ConfigureSync(cfg SyncConfig) error {
	if cfg.Index == nil {
		return errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: sync config carries no header index")
	}

	m.mu.Lock()
	started := m.started
	m.mu.Unlock()

	if started {
		return errors.New(errors.ERR_SERVICE_ERROR, "svp2p: sync must be configured before the peer manager starts")
	}

	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	m.headerIndex = cfg.Index

	if cfg.TickInterval > 0 {
		m.syncTick = cfg.TickInterval
	}

	if cfg.Ingestor == nil {
		return nil
	}

	headerSync, err := NewHeaderSync(HeaderSyncConfig{
		Index:                            cfg.Index,
		Params:                           m.tSettings.ChainCfgParams,
		DisableCheckpoints:               cfg.DisableCheckpoints,
		AllowSyncCandidateFromLocalPeers: cfg.AllowSyncCandidateFromLocalPeers,
	})
	if err != nil {
		return err
	}

	blockDownloader, err := NewBlockDownloader(cfg.Index, headerSync)
	if err != nil {
		return err
	}

	if cfg.MaxLastBlockTime > 0 {
		blockDownloader.maxLastBlockTime = cfg.MaxLastBlockTime
	}

	// A zero here means "not configured", not "no timeout": a zero base would
	// disconnect every peer holding a block on the first check.
	if cfg.BlockDownloadTimeoutBasePercent > 0 {
		blockDownloader.timeoutBasePercent = cfg.BlockDownloadTimeoutBasePercent
	}

	if cfg.BlockDownloadTimeoutBaseIBDPercent > 0 {
		blockDownloader.timeoutBaseIBDPercent = cfg.BlockDownloadTimeoutBaseIBDPercent
	}

	// Zero means unset for this one too, so the per-peer compensation cannot be
	// turned off from settings the way SVNode's own flag allows. That is the
	// price of the zero-means-unset convention this struct already uses for
	// TickInterval and MaxLastBlockTime, and turning the compensation off is not
	// something an operator has a reason to want: it only ever widens a window,
	// and only for peers whose blocks we asked for.
	if cfg.BlockDownloadTimeoutPerPeerPercent > 0 {
		blockDownloader.timeoutPerPeerPercent = cfg.BlockDownloadTimeoutPerPeerPercent
	}

	if cfg.BlockDownloadSlowFetchTimeout > 0 {
		blockDownloader.slowFetchTimeout = cfg.BlockDownloadSlowFetchTimeout
	}

	if cfg.BlockDownloadMaxParallelFetch > 0 {
		blockDownloader.maxParallelFetch = cfg.BlockDownloadMaxParallelFetch
	}

	serving, err := NewServing(cfg.Index, headerSync)
	if err != nil {
		return err
	}

	m.headerSync = headerSync
	m.blockDownloader = blockDownloader
	m.serving = serving
	m.ingestor = cfg.Ingestor
	m.fetcher = cfg.Fetcher

	// Until the blockchain service reports a tip, our own chain is whatever
	// hydration put in the index.
	tipHash, _ := cfg.Index.Tip()
	if tip, ok := cfg.Index.Lookup(tipHash); ok {
		m.activeTip = tip
	}

	return nil
}

// SyncEnabled reports whether block sync is wired.
func (m *PeerManager) SyncEnabled() bool {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	return m.headerSync != nil
}

// AddHeaders links headers into the shared header index under the sync-state
// mutex, and returns those whose parent was not known for the caller to log.
// It is how the blockchain-subscription goroutine writes to the index without
// racing the headers-first machine's Lookup-before-AddHeader sequence. Pass
// headers oldest first, so each header's parent is already indexed.
//
// THIS PATH RUNS NO HEADER VALIDATION, deliberately. It is the trusted half of
// the two ways a header reaches the index: Server hydration and the blockchain
// subscription feed it headers Teranode's own blockchain service already
// validated and stored, so re-checking them here would be checking our own
// chain against itself. A peer's headers take the other half,
// HeaderSync.OnHeaders, which applies CheckBlockHeader, the checkpoint fence,
// the contextual difficulty rule and the timestamp cap before anything is
// inserted (see headersync.go acceptHeader).
//
// The distinction is load-bearing for the contextual difficulty check, which
// needs the parent's 147-block window. Hydration walks the store forward from
// the index tip and would otherwise have to satisfy that window while it is
// still building it. It matters for the timestamp cap too: a header our own
// store already holds must not be refused because THIS node's clock is skewed,
// which is exactly what the too-far-in-the-future rule would do to it. It also
// means a header this method inserts is NOT evidence that a peer could have got
// the same header past OnHeaders.
//
// Anything else that grows the index must go through OnHeaders, or the gates
// there stop bounding what a peer can make us store.
func (m *PeerManager) AddHeaders(headers []*wire.BlockHeader) ([]*wire.BlockHeader, error) {
	// Snapshotted before syncMu is taken: the two mutexes are never held
	// together (see the note on syncMu).
	handles := m.peerHandles()

	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	// Deferred after the unlock, so it runs BEFORE it — including on the
	// mid-batch error return, where the headers accepted so far stay indexed
	// and may already have resolved somebody's parked hash. It is a no-op when
	// the index is not configured.
	defer m.promoteBlockAvailabilityLocked(handles)

	if m.headerIndex == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: header index is not configured")
	}

	var orphans []*wire.BlockHeader

	for _, header := range headers {
		connected, err := m.headerIndex.AddHeader(header)
		if err != nil {
			return orphans, err
		}

		if !connected {
			orphans = append(orphans, header)
		}
	}

	return orphans, nil
}

// promoteBlockAvailabilityLocked resolves every peer's parked
// hashLastUnknownBlock against the header index, and is called on both paths
// that grow the index.
//
// net_processing.cpp runs ProcessBlockAvailability (net_processing.cpp:355) on
// every peer on every SendMessages pass, so a parked hash is resolved by the
// next pass whatever put the header in mapBlockIndex. THE DIVERGENCE IS THAT
// THIS PORT MAKES IT EVENT-DRIVEN INSTEAD OF PER-PASS: the sweep runs when the
// index grows rather than on a timer.
//
// The port needs it because it cannot inherit the C++ safety net. Phase 2 Task
// 5's N2 fix made a non-sync peer availability-only during a headers-first
// round (see the divergence note in headersync.go OnHeaders), so its
// announcement parks. The only other caller of processBlockAvailability is
// FindNextBlocksToDownload, which the download walk reaches on none of its
// early returns — a peer with no pindexBestKnownBlock is exactly a peer that
// walk gives up on. So a parked hash could otherwise sit unresolved while the
// round indexed the very chain it named, and the peer stayed unschedulable.
// Running it on index growth resolves the hash at the earliest moment it CAN
// resolve, which is strictly sooner than any pass-driven sweep.
//
// Cost is one HeaderIndex.Lookup per PARKED peer per batch — not per header:
// both callers sweep once, after the whole batch is indexed.
// processBlockAvailability returns on its zero-hash test for every peer with
// nothing parked, so an idle peer costs one comparison. Nothing here blocks,
// which is what lets it run under syncMu.
//
// Requires syncMu. handles must be snapshotted by the caller BEFORE it takes
// syncMu, because mu and syncMu are never held together.
func (m *PeerManager) promoteBlockAvailabilityLocked(handles []peerHandle) {
	if m.headerIndex == nil {
		return
	}

	for _, h := range handles {
		if h.sync == nil || h.sync.State == nil {
			continue
		}

		h.sync.State.processBlockAvailability(m.headerIndex)
	}
}

// SetActiveTip records our own best chain tip, the chainActive counterpart the
// download scheduler walks against. It reports false when the hash is not in
// the index, in which case the previous tip stands: FindNextBlocksToDownload
// refuses to schedule against a tip it cannot place, so a wrong answer here
// would stall download rather than misdirect it.
func (m *PeerManager) SetActiveTip(hash chainhash.Hash) bool {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.headerIndex == nil {
		return false
	}

	tip, ok := m.headerIndex.Lookup(hash)
	if !ok {
		return false
	}

	m.activeTip = tip

	return true
}

// startingHeight is net_processing.cpp PushNodeVersion's nNodeStartingHeight:
// the height we advertise in our own version message.
func (m *PeerManager) startingHeight() int32 {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.headerIndex == nil {
		return 0
	}

	_, height := m.headerIndex.Tip()

	return height
}

func (m *PeerManager) Start(ctx context.Context, listenAddresses []string) error {
	m.mu.Lock()

	if m.started {
		m.mu.Unlock()
		return errors.New(errors.ERR_SERVICE_ERROR, "svp2p: peer manager already started")
	}

	m.started = true

	for _, addr := range listenAddresses {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			m.mu.Unlock()
			_ = m.Stop()

			return errors.New(errors.ERR_SERVICE_ERROR, "svp2p: cannot listen on %s", addr, err)
		}

		m.listeners = append(m.listeners, ln)
	}

	listeners := append([]net.Listener(nil), m.listeners...)
	m.mu.Unlock()

	for _, ln := range listeners {
		m.wg.Add(1)

		go func(ln net.Listener) {
			defer m.wg.Done()
			m.acceptLoop(ctx, ln)
		}(ln)
	}

	for _, addr := range m.tSettings.Legacy.ConnectPeers {
		m.wg.Add(1)

		go func(addr string) {
			defer m.wg.Done()
			m.dialLoop(ctx, addr)
		}(addr)
	}

	if m.SyncEnabled() {
		m.wg.Add(1)

		go func() {
			defer m.wg.Done()
			m.syncLoop(ctx)
		}()
	}

	return nil
}

func (m *PeerManager) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			select {
			case <-m.quit:
			case <-ctx.Done():
			default:
				m.logger.Errorf("[svp2p] accept failed on %s: %v", ln.Addr(), err)
			}

			return
		}

		// net.cpp CConnman::AcceptConnection: drop banned peers before any
		// protocol traffic.
		if m.banList.IsBanned(nc.RemoteAddr().String()) {
			m.logger.Infof("[svp2p] rejected banned inbound peer %s", nc.RemoteAddr())

			_ = nc.Close()

			continue
		}

		m.wg.Add(1)

		go func() {
			defer m.wg.Done()

			_ = m.runPeer(ctx, nc, true)
		}()
	}
}

func (m *PeerManager) dialLoop(ctx context.Context, addr string) {
	delay := dialRetryBase

	for {
		if m.banList.IsBanned(addr) {
			m.logger.Infof("[svp2p] not dialing banned peer %s", addr)
			return
		}

		nc, err := net.DialTimeout("tcp", addr, 30*time.Second)
		if err == nil {
			runErr := m.runPeer(ctx, nc, false)

			// net.cpp: never redial a peer that proved to be ourselves.
			if errors.Is(runErr, ErrSelfConnection) {
				m.logger.Warnf("[svp2p] %s is ourselves, not redialing", addr)
				return
			}

			// bsvd connmgr semantics: a completed connection resets backoff.
			delay = dialRetryBase
		} else {
			m.logger.Debugf("[svp2p] dial %s failed: %v", addr, err)
		}

		select {
		case <-time.After(delay):
		case <-m.quit:
			return
		case <-ctx.Done():
			return
		}

		delay *= 2
		if delay > dialRetryMax {
			delay = dialRetryMax
		}
	}
}

func (m *PeerManager) runPeer(ctx context.Context, nc net.Conn, inbound bool) error {
	conn := transport.New(nc, transport.Config{
		Net:             m.tSettings.ChainCfgParams.Net,
		ProtocolVersion: wire.ProtocolVersion,
		SendBudgetBytes: sendBudgetBytes,
		RecvQueueLen:    recvQueueLen,
		WriteTimeout:    writeTimeout,
	})

	syncPeer := NewSyncPeer(nc.RemoteAddr().String(), 0, newPeerSyncState())

	var (
		dispatch syncDispatcher
		ingestor BlockIngestor
		fetcher  BlockTxFetcher
	)

	if m.SyncEnabled() {
		dispatch = m

		m.syncMu.Lock()
		ingestor = m.ingestor
		fetcher = m.fetcher
		m.syncMu.Unlock()
	}

	peer := NewPeer(PeerConfig{
		Handshake: HandshakeConfig{
			Inbound:   inbound,
			Nonce:     m.newNonce(),
			UserAgent: UserAgent,
			// net_processing.cpp PushNodeVersion: nNodeStartingHeight, which
			// is the height of the header index tip.
			StartingHeight:       m.startingHeight(),
			MaxRecvPayloadLength: wire.DefaultMaxRecvPayloadLength,
			AllowBlockPriority:   m.tSettings.Legacy.AllowBlockPriority,
			LocalAddr:            netAddressOf(nc.LocalAddr()),
			RemoteAddr:           netAddressOf(nc.RemoteAddr()),
			CheckIncomingNonce:   m.hasSentNonce,
		},
		Conn:         conn,
		Logger:       m.logger,
		IdleTimeout:  m.tSettings.Legacy.PeerIdleTimeout,
		PingInterval: pingInterval,
		BanThreshold: banScoreThreshold,
		Sync:         dispatch,
		SyncPeer:     syncPeer,
		Ingestor:     ingestor,
		Fetcher:      fetcher,
	})

	m.mu.Lock()
	m.peers[peer] = syncPeer
	m.mu.Unlock()

	err := peer.Run(ctx)

	m.mu.Lock()
	delete(m.peers, peer)
	m.mu.Unlock()

	// net_processing.cpp FinalizeNode, both halves: the sync slot and every
	// block this peer was downloading go back on offer. This is the single
	// release point for a peer that goes away, however it went away —
	// a stall disconnect, a machine's disconnect error, or a dead socket.
	m.peerGone(syncPeer)

	m.logger.Infof("[svp2p] peer %s done: %v", nc.RemoteAddr(), err)

	return err
}

// peerGone is net_processing.cpp FinalizeNode. A rotation must NOT come here:
// CheckStall's rotate branch has already released the slot and the peer's
// downloads, and the peer stays connected.
func (m *PeerManager) peerGone(syncPeer *SyncPeer) {
	m.syncMu.Lock()

	if m.headerSync == nil {
		m.syncMu.Unlock()
		return
	}

	m.blockDownloader.PeerDisconnected(syncPeer)
	m.headerSync.PeerDisconnected(syncPeer)
	m.syncMu.Unlock()

	// The slot this peer held is free; hand it to another candidate rather
	// than waiting for one to connect.
	m.electSyncPeer(syncPeer)
}

// peerHandle pairs a connected peer with its CNodeState entry. Everything
// that needs both the registry and the sync state snapshots handles under mu
// first, so the two mutexes are never held together.
type peerHandle struct {
	peer *Peer
	sync *SyncPeer

	// established is net_processing.cpp's fSuccessfullyConnected, which
	// SendMessages tests before it does anything at all for a peer
	// (net_processing.cpp:5835-5837, "Don't send anything until the version
	// handshake is complete"). runPeer puts a peer in the registry BEFORE it runs
	// the handshake, so a pass can reach one that is not ready.
	//
	// It is sampled here because everything a pass needs about a peer is sampled
	// before syncMu is taken. The read is a non-blocking select on a channel the
	// peer loop closes exactly once, so it takes no peer lock and cannot block.
	established bool
}

func (m *PeerManager) peerHandles() []peerHandle {
	m.mu.Lock()
	defer m.mu.Unlock()

	handles := make([]peerHandle, 0, len(m.peers))
	for peer, syncPeer := range m.peers {
		handles = append(handles, peerHandle{peer: peer, sync: syncPeer, established: handshakeComplete(peer)})
	}

	return handles
}

// handshakeComplete reports whether peer finished its version/verack exchange,
// without waiting for it.
func handshakeComplete(peer *Peer) bool {
	select {
	case <-peer.Established():
		return true
	default:
		return false
	}
}

// outgoing is one peer's share of a send pass, collected under the sync-state
// mutex and sent after it is released: Conn.Send can block on a backed-up
// writer, and no lock may be held across it.
type outgoing struct {
	peer *Peer
	msgs []wire.Message
}

func sendAll(out []outgoing) {
	for _, o := range out {
		o.peer.send(o.msgs)
	}
}

// Established is the syncDispatcher half of net_processing.cpp SendMessages'
// initial getheaders: a peer finished its handshake, so consider starting
// header sync with it.
func (m *PeerManager) Established(syncPeer *SyncPeer, services wire.ServiceFlag) []wire.Message {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.headerSync == nil || syncPeer == nil {
		return nil
	}

	// The sync-candidate rules read the services the peer advertised, which
	// are only known once its version message has arrived.
	syncPeer.Services = services

	return m.headerSync.PeerEstablished(syncPeer)
}

// Headers dispatches the NetMsgType::HEADERS event.
func (m *PeerManager) Headers(syncPeer *SyncPeer, msg *wire.MsgHeaders) ([]wire.Message, int, error) {
	// Snapshotted before syncMu is taken: the two mutexes are never held
	// together (see the note on syncMu).
	handles := m.peerHandles()

	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.headerSync == nil {
		return nil, 0, nil
	}

	msgs, score, err := m.headerSync.OnHeaders(syncPeer, msg)

	// This batch may have grown the index, which is what can resolve another
	// peer's parked announcement. It runs on the error path too: OnHeaders
	// keeps the headers it accepted before the refusal that stopped the batch,
	// exactly as validation.cpp does, so the index may have grown even when
	// this peer is about to be disconnected.
	m.promoteBlockAvailabilityLocked(handles)

	return msgs, score, err
}

// Inv dispatches the NetMsgType::INV event.
func (m *PeerManager) Inv(syncPeer *SyncPeer, msg *wire.MsgInv) ([]wire.Message, error) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.blockDownloader == nil {
		return nil, nil
	}

	return m.blockDownloader.OnInv(syncPeer, msg)
}

// GetHeaders dispatches the NetMsgType::GETHEADERS event. The refusal is
// logged here rather than in the machine, which holds no logger, and the log
// line is the one the legacy service wrote (services/legacy/peer_server.go:1585).
func (m *PeerManager) GetHeaders(syncPeer *SyncPeer, msg *wire.MsgGetHeaders) []wire.Message {
	m.syncMu.Lock()

	if m.serving == nil || syncPeer == nil {
		m.syncMu.Unlock()
		return nil
	}

	out, refused := m.serving.OnGetHeaders(syncPeer, m.activeTip, msg)

	m.syncMu.Unlock()

	// Logged with no lock held: the package lock order forbids reaching a peer
	// while a manager lock is taken, and a logger sink is outside this package.
	if refused {
		m.logger.Debugf("[svp2p] ignoring getheaders from %s: node is syncing in headers-first mode", syncPeer.Addr)
	}

	return out
}

// GetBlocks dispatches the NetMsgType::GETBLOCKS event.
func (m *PeerManager) GetBlocks(syncPeer *SyncPeer, msg *wire.MsgGetBlocks) []wire.Message {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.serving == nil || syncPeer == nil {
		return nil
	}

	return m.serving.OnGetBlocks(syncPeer, m.activeTip, msg)
}

// GetData dispatches the NetMsgType::GETDATA event. It only classifies the
// request; the fetches and the sends run on the peer's serve goroutine, with
// no lock held (getdata.go serveGetData).
func (m *PeerManager) GetData(syncPeer *SyncPeer, msg *wire.MsgGetData) []getDataItem {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.serving == nil || syncPeer == nil {
		return nil
	}

	return m.serving.OnGetData(msg)
}

// ContinueInv dispatches the getdata continuation, after a block has gone out.
func (m *PeerManager) ContinueInv(syncPeer *SyncPeer, hash chainhash.Hash) []wire.Message {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.serving == nil || syncPeer == nil {
		return nil
	}

	return m.serving.ContinueInv(syncPeer, m.activeTip, hash)
}

// BlockExpected reports whether hash is in flight from this peer, which is
// what makes an inbound block solicited.
func (m *PeerManager) BlockExpected(syncPeer *SyncPeer, hash chainhash.Hash) bool {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.blockDownloader == nil {
		return false
	}

	return m.blockDownloader.IsInFlightFrom(syncPeer, hash)
}

// BlockDone reports one block's ingest outcome to the download scheduler:
// in-flight is cleared, the peer's progress clock is fed, and a pre-admission
// timeout rotates the sync peer. A non-nil return means the peer must be
// disconnected, which the caller does with no lock held.
func (m *PeerManager) BlockDone(syncPeer *SyncPeer, hash chainhash.Hash, outcome IngestOutcome) error {
	now := time.Now().UnixMicro()

	var (
		rotate     bool
		disconnect error
	)

	m.syncMu.Lock()

	if m.blockDownloader == nil {
		m.syncMu.Unlock()
		return nil
	}

	switch {
	case outcome.Duplicate:
		// Another copy of this hash won the admission race and owns the
		// have-data record, so BlockFailed must not be used here — it would
		// delete a record that copy may already have written. What must go is
		// THIS peer's own in-flight claim: without it a block whose holder
		// changed under a rotation stays in flight for ever and is never
		// re-offered. BlockNotDelivered is guarded, so it does nothing when
		// the holder is someone else.
		m.blockDownloader.BlockNotDelivered(syncPeer, hash, now)

		m.logger.Debugf("[svp2p] block %s was already in flight: %v", hash, outcome.Err)

	case outcome.Err == nil:
		m.blockDownloader.BlockReceived(syncPeer, hash, now)

	case outcome.ParentMissing:
		// The block is wanted and the peer delivered it; our own chain is just
		// not ready for it. Release the claim, hold the block back from the next
		// walk, and refresh the peer's stall clock as any other local fault
		// does. Deliberately NOT the default branch below: nothing here is a
		// rotation, a disconnect or a warning.
		m.blockDownloader.BlockParentMissing(syncPeer, hash, now)

		if syncPeer != nil && syncPeer.State != nil {
			syncPeer.State.nLastProgressTime = now
		}

		// Debug, not Warn. This is an expected consequence of asking several
		// peers for blocks at once, and at Warn it buried the ingest failures
		// that are not expected: 476 of these against 36 real ingests on the
		// two-peer integration leg.
		m.logger.Debugf("[svp2p] block %s is waiting for its parent: %v", hash, outcome.Err)

	default:
		// The block is back on offer to any peer, including this one.
		m.blockDownloader.BlockFailed(syncPeer, hash, now)

		if outcome.TransientLocal && syncPeer != nil && syncPeer.State != nil {
			// Admission.SkipForBackoff's caller contract: our own store
			// faulted, and the peer did deliver, so refresh its stall clock
			// instead of rotating it. Legacy netsync manager.go:1668-1670
			// refreshed lastBlockTime on exactly this path.
			syncPeer.State.nLastProgressTime = now
		}

		rotate = outcome.Rotate

		// services/legacy/peer_server.go shouldDisconnectOnBlockErr: a block
		// the pipeline rejected is the peer's fault and the peer goes, so the
		// sync peer actually rotates instead of the same bad block being
		// re-offered to the same peer for ever.
		//
		// Read from PeerFault alone, never from the absence of
		// TransientLocal. A rotation is a LOCAL fault that has already
		// released the slot and this peer's downloads; disconnecting it as
		// well would drive that release a second time through peerGone, which
		// is exactly the double-finalize the rotation contract forbids. An
		// unclassified failure disconnects nobody either — the block goes back
		// on offer and the stall rules deal with a peer that keeps failing.
		if outcome.PeerFault {
			disconnect = errors.New(errors.ERR_NETWORK_PEER_MALICIOUS,
				"svp2p: block %s was rejected", hash, outcome.Err)
		}

		m.logger.Warnf("[svp2p] block %s ingest failed: %v", hash, outcome.Err)
	}

	if rotate {
		// A wedged local round-trip stranded a requested block. This is the
		// same release pair CheckStall's rotate branch performs: the peer
		// stays connected, only the slot and its downloads go.
		m.blockDownloader.PeerDisconnected(syncPeer)
		m.headerSync.SyncPeerTimedOut(syncPeer)
	}

	m.syncMu.Unlock()

	if rotate {
		m.logger.Warnf("[svp2p] rotating the sync peer after a pre-admission timeout on block %s", hash)
		m.electSyncPeer(syncPeer)
	}

	return disconnect
}

// electSyncPeer offers the free sync slot to the first eligible candidate,
// skipping exclude (the peer that just lost it). HeaderSync.PeerEstablished
// is the election: it returns the initial getheaders only for a peer that may
// take the slot. Map iteration makes "first" arbitrary, which matches
// net_processing.cpp taking the first eligible candidate that reaches
// SendMessages rather than ranking them.
func (m *PeerManager) electSyncPeer(exclude *SyncPeer) {
	handles := m.peerHandles()

	var out []outgoing

	m.syncMu.Lock()

	if m.headerSync != nil {
		out = m.electLocked(handles, exclude)
	}

	m.syncMu.Unlock()

	sendAll(out)
}

// electLocked runs the election under the sync-state mutex. It prefers a peer
// other than exclude, and falls back to exclude itself when no one else is
// eligible: legacy netsync updateSyncPeer re-runs startSync over every peer,
// the freed one included, so a single-peer node keeps syncing rather than
// stopping for ever. PeerEstablished leaves a peer it refuses untouched, so
// the second pass costs only the refusals of the first.
//
// It answers the two EVENT paths only — peerGone and BlockDone's rotation —
// which have no SVNode counterpart at all: SendMessages simply reaches the peers
// again on its next pass. Both fill the freed slot without waiting for a tick,
// so they are kept. syncPass does NOT use it: the per-pass eligibility sweep
// there is the port of SendBlockSync, and it offers the slot to every eligible
// peer rather than to the first one an election reaches.
func (m *PeerManager) electLocked(handles []peerHandle, exclude *SyncPeer) []outgoing {
	for _, allowExcluded := range []bool{false, true} {
		for _, h := range handles {
			// The same guard syncPass carries, and for the same reason: this
			// election reads the connection registry, which holds a peer from
			// before its handshake runs, and net_processing.cpp SendMessages does
			// nothing at all for such a peer (:5835-5837). Both event paths that
			// reach here — peerGone and BlockDone's rotation — could otherwise
			// give it the sync slot and a getheaders before its verack, wherever
			// isSyncCandidate does not refuse it on the services flag (see the
			// regtest branch of headersync.go isSyncCandidate).
			if !h.established {
				continue
			}

			if !allowExcluded && h.sync == exclude {
				continue
			}

			if msgs := m.headerSync.PeerEstablished(h.sync); len(msgs) > 0 {
				return []outgoing{{peer: h.peer, msgs: msgs}}
			}
		}

		if exclude == nil {
			break
		}
	}

	return nil
}

// syncLoop drives the two passes the machines are deliberately clockless for:
// net_processing.cpp SendMessages' block-download pass and its stall checks.
// Time enters the machines only here, as a parameter.
func (m *PeerManager) syncLoop(ctx context.Context) {
	ticker := time.NewTicker(m.syncTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.quit:
			return
		case <-ticker.C:
			m.syncTickOnce()
		}
	}
}

func (m *PeerManager) syncTickOnce() {
	handles := m.peerHandles()
	now := time.Now().UnixMicro()

	// Sampled before the sync-state mutex is taken: IngestSnapshot takes the
	// PEER lock, and the package lock order forbids reaching for a peer lock
	// while holding a manager one.
	ingests := make([]IngestSnapshot, len(handles))
	for i, h := range handles {
		ingests[i] = h.peer.IngestSnapshot()
	}

	out, disconnect := m.syncPass(handles, ingests, now)

	for _, peer := range disconnect {
		m.logger.Warnf("[svp2p] disconnecting %s: stalling block download", peer.Info().Addr)
		peer.Disconnect("stalling block download")
	}

	sendAll(out)
}

// syncPass is the locked half of one tick: the header-sync eligibility sweep,
// the stall check and the block-download pass, run for every peer in that order.
// It RETURNS the work instead of doing it, because neither Conn.Send nor
// Peer.Disconnect may run under syncMu — both can block on a peer. That is the
// collect-then-act shape every sync path here keeps; syncTickOnce acts on the
// two lists after the unlock.
//
// THE CALLER MUST NOT HOLD syncMu. This method takes it itself, unlike the
// *Locked helpers in this file, and syncMu is not reentrant. That cuts against
// the convention here, so any test or caller that seeds sync state under the
// lock must release it before calling this.
//
// ingests must be index-aligned with handles, and is sampled by the caller
// before syncMu is taken: IngestSnapshot takes the PEER lock, and the package
// lock order forbids reaching for a peer lock while holding a manager one.
func (m *PeerManager) syncPass(handles []peerHandle, ingests []IngestSnapshot, now int64) (out []outgoing, disconnect []*Peer) {
	m.syncMu.Lock()

	if m.blockDownloader == nil {
		m.syncMu.Unlock()
		return nil, nil
	}

	activeTip := m.activeTip

	for i, h := range handles {
		// net_processing.cpp SendMessages does nothing at all for a peer whose
		// handshake has not finished (:5835-5837), and the sweep below is why that
		// guard has to be carried. Outside regtest a handshaking peer has
		// advertised no services yet, so isSyncCandidate refuses it on its own.
		// The regtest branch refuses far less: it reads the address alone, and
		// nothing whatsoever once AllowSyncCandidateFromLocalPeers is set. So
		// without this, a peer could take the sync slot — and be sent a
		// getheaders — before its verack. It is also what peer.go dispatchSync
		// states for the inbound direction: nothing pre-handshake may reach the
		// sync machines. electLocked carries the same guard for the event paths.
		if !h.established {
			continue
		}

		// THE ORDER OF THE THREE PASSES BELOW IS net_processing.cpp
		// SendMessages' own (net_processing.cpp:5829-5897), which runs them for
		// one peer at a time: SendBlockSync at :5865, DetectStalling at :5881,
		// and SendGetDataBlocks at :5888. The eligibility sweep therefore comes
		// FIRST, ahead of both the stall check and the getdata pass for the same
		// peer.
		//
		// That order is what makes the rotate branch below hold without a second
		// exclusion rule: a peer that rotates on this pass had its sweep before
		// the rotation, and nothing sweeps it again, so it cannot take the slot
		// back on the pass that took it away.
		//
		// The sweep is the per-pass half of net_processing.cpp SendBlockSync
		// (net_processing.cpp:5180-5222). PeerEstablished was previously reached
		// only from PeerManager.Established (the handshake) and from electLocked
		// (a rotation or a peerGone), so a peer that became eligible later never
		// started header sync — see the note at PeerEstablished for what that
		// cost past the final checkpoint. Its per-tick cost is one bool read for a
		// peer that already holds the slot; for any other peer one
		// isSyncCandidate, plus — while somebody else holds the slot outside a
		// headers-first round — one header-index tip read, one lookup and one
		// clock read, which is the near-tip test.
		//
		// WHAT THE SWEEP COSTS IN STEADY STATE, past the final checkpoint, because
		// it is not free and it is a divergence from SVNode. The 24 hour near-tip
		// relaxation admits every eligible peer there, headersFirstMode is off so
		// CheckStall's header-progress refresh is unreachable, and
		// nLastProgressTime moves only on a delivered block. Mainnet blocks are
		// about ten minutes apart against a 180 second rotation window, so most
		// windows deliver nothing and EVERY header peer rotates, then the next
		// tick's sweep re-admits all of them — for ever. Each cycle costs one
		// getheaders per peer, one clearPeer per peer, which nils
		// pindexLastCommonBlock and so buys a fresh lastCommonAncestor walk on the
		// next download pass, and one resetHeaderState. SVNode pays none of it: it
		// has no sync-peer rotation at all, and keeps a sync peer until it
		// disconnects or its blocks time out. The rotation is the licensed legacy
		// netsync deviation documented at CheckStall; the churn is what it costs
		// once every eligible peer is swept rather than one being elected.
		//
		// Nothing here blocks, which is what lets it run under syncMu: any
		// getheaders it produces joins out and is sent by syncTickOnce after the
		// unlock, the collect-then-act shape this file keeps throughout.
		msgs := m.headerSync.PeerEstablished(h.sync)

		switch m.blockDownloader.CheckStall(h.sync, ingests[i], now) {
		case StallActionDisconnect:
			// The caller disconnects; runPeer then drives FinalizeNode, which is
			// what releases the slot and the peer's downloads. CheckStall itself
			// mutated nothing on this branch.
			//
			// THE SWEEP ABOVE MAY HAVE, THOUGH, and this is the hazard the source
			// pass order carries. A peer holding no slot but heading the download
			// window is granted the slot by the sweep and disconnected by this
			// clause on the SAME pass, so as the first sync peer it has just
			// seeded fSyncStarted, nSyncStarted, nextCheckpoint, headersFirstMode
			// and roundAnchorHeight — for a peer that is about to go. While it
			// holds that slot every peer after it on this pass is refused, because
			// PeerEstablished's single-slot guard reads the mode as it stands.
			//
			// The order is not ours to change: SVNode runs SendBlockSync (:5865)
			// before DetectStalling (:5881) and carries the same hazard.
			//
			// WHAT MAKES IT SAFE IS THE UNWIND, and the whole of it runs:
			// Peer.Disconnect closes the connection, so Peer.Run returns and
			// runPeer reaches peerGone unconditionally. peerGone calls
			// BlockDownloader.PeerDisconnected, which releases the peer's
			// downloads, and HeaderSync.PeerDisconnected, which reaches
			// releaseSyncPeer. That clears fSyncStarted, decrements nSyncStarted
			// and runs resetHeaderState, which clears headersFirstMode and
			// roundAnchorHeight and re-seeds nextCheckpoint from the current tip.
			// releaseSyncPeer's own !fSyncStarted early return cannot fire here:
			// the sweep has just set the flag and nothing between clears it.
			// TestSyncPass_ADisconnectUnwindsTheSlotTheSweepJustGranted walks that
			// path and asserts each piece.
			//
			// Whatever the sweep produced for this peer is dropped with it.
			// syncTickOnce disconnects before it sends, so the message would
			// reach a closed peer; C++ has already pushed its getheaders by this
			// point, but there PushMessage precedes the fDisconnect flag. It is
			// the same reasoning OnInv states for its own error path: messages
			// queued for a peer we are about to drop are not worth sending.
			disconnect = append(disconnect, h.peer)

			continue

		case StallActionRotateSyncPeer:
			// The slot and the peer's in-flight blocks are ALREADY released
			// and the peer stays connected, so it must not be disconnected or
			// finalized here — only left to be swept again on a later tick.
			m.logger.Warnf("[svp2p] rotating the sync peer %s: no sync progress", h.sync.Addr)

			// This branch runs no getdata pass, so the rest of this peer's tick is
			// skipped. Without the skip, SendGetDataBlocks hands the peer we have
			// just judged non-progressing another MaxBlocksInTransitPerPeer blocks
			// on THIS pass: clearPeer nils pindexLastCommonBlock but keeps
			// pindexBestKnownBlock, so FindNextBlocksToDownload bootstraps the
			// window from our own tip again and refills it.
			//
			// The seam is a composition of two models, and neither model's
			// safety net came across with it. SVNode runs a per-peer download
			// loop and relies on a per-block download timeout to take blocks
			// back from a peer that does not deliver. legacy netsync rotates
			// the sync peer alone and hands blocks to nobody else. Composed
			// here, the rotated peer keeps being re-handed blocks, while its
			// own rotation clause can no longer fire for it, because
			// SyncPeerTimedOut cleared fSyncStarted. What DOES still govern it
			// from here is stated at CheckStall.
			//
			// The skip costs this peer its own chance to NAME a staller on
			// this pass, since that clock is started from inside
			// SendGetDataBlocks. Every other peer on the pass still names one,
			// and the next tick gives this peer the chance back.
			//
			// THE SWEEP ABOVE CANNOT HAVE PRODUCED ANYTHING FOR THIS PEER, and
			// that coupling is what makes "the rotating peer is asked for nothing"
			// hold rather than the weaker "asked for no blocks". The rotation
			// clause only ever judges a peer with fSyncStarted set — CheckStall's
			// !fSyncStarted early return sits above it — and PeerEstablished
			// returns nil for exactly that peer. So msgs is empty here, and the
			// send list below gets no entry for this peer. Anything that ever
			// lets PeerEstablished answer an fSyncStarted peer breaks that, and
			// TestSyncPass_ASingleCandidateNodeTakesTheSlotBackOnTheNextTick is
			// what would catch it.
			//
			// There is no continue in this branch: the switch itself is the skip,
			// because only StallActionNone runs the getdata pass.

		case StallActionNone:
			msgs = append(msgs, m.blockDownloader.SendGetDataBlocks(h.sync, activeTip, now)...)
		}

		if len(msgs) > 0 {
			out = append(out, outgoing{peer: h.peer, msgs: msgs})
		}
	}

	m.syncMu.Unlock()

	return out, disconnect
}

func (m *PeerManager) newNonce() uint64 {
	nonce := randNonce()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.nonces = append(m.nonces, nonce)
	if len(m.nonces) > maxSentNonces {
		m.nonces = m.nonces[len(m.nonces)-maxSentNonces:]
	}

	return nonce
}

// hasSentNonce mirrors net.cpp CConnman::CheckIncomingNonce: true if this
// node itself generated the given nonce for one of its own connections
// (inbound or outbound), meaning an incoming VERSION carrying it proves a
// self-connect.
func (m *PeerManager) hasSentNonce(nonce uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, n := range m.nonces {
		if n == nonce {
			return true
		}
	}

	return false
}

func (m *PeerManager) ListenAddrs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	addrs := make([]string, 0, len(m.listeners))
	for _, ln := range m.listeners {
		addrs = append(addrs, ln.Addr().String())
	}

	return addrs
}

func (m *PeerManager) ConnectedCount() int32 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return int32(len(m.peers)) //nolint:gosec // peer count is small
}

func (m *PeerManager) Snapshots() []PeerSnapshot {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))

	for p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()

	snaps := make([]PeerSnapshot, 0, len(peers))
	for _, p := range peers {
		snaps = append(snaps, p.Info())
	}

	return snaps
}

func (m *PeerManager) DisconnectHost(host string) {
	m.mu.Lock()
	peers := make([]*Peer, 0, len(m.peers))

	for p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()

	for _, p := range peers {
		peerHost, _, err := net.SplitHostPort(p.Info().Addr)
		if err != nil {
			peerHost = p.Info().Addr
		}

		if peerHost == host {
			p.Disconnect("banned by operator")
		}
	}
}

func (m *PeerManager) Stop() error {
	m.mu.Lock()

	select {
	case <-m.quit:
	default:
		close(m.quit)
	}

	listeners := m.listeners
	m.listeners = nil

	peers := make([]*Peer, 0, len(m.peers))
	for p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()

	for _, ln := range listeners {
		_ = ln.Close()
	}

	for _, p := range peers {
		p.Disconnect("shutting down")
	}

	m.wg.Wait()

	return nil
}

func netAddressOf(addr net.Addr) *wire.NetAddress {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return wire.NewNetAddressIPPort(nil, 0, 0)
	}

	return wire.NewNetAddress(tcpAddr, 0)
}

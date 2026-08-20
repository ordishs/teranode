// Package svp2p is the rewrite of the legacy P2P bridge service, modeled on
// the bitcoin-sv reference implementation (net.cpp / net_processing.cpp)
// per docs/superpowers/specs/2026-08-18-svp2p-rewrite-design.md.
// Phase 1 scope: transport, handshake, peer management, and the peer_api
// gRPC surface. Block/tx ingestion (bridge) arrives in Phase 2.
package svp2p

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockvalidation"
	"github.com/bsv-blockchain/teranode/services/legacy/peer_api"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation"
	"github.com/bsv-blockchain/teranode/services/svp2p/bridge"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/services/validator"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/health"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// defaultHeaderBatchSize bounds each forward-walk fetch during header index
// hydration and resync to the wire protocol's own headers-per-message cap.
const defaultHeaderBatchSize = uint32(wire.MaxBlockHeadersPerMsg)

// Deps carries the Teranode service dependencies the block-ingestion bridge
// needs, in the same set legacy.New takes (daemon/daemon_services.go). They
// are optional on this Server: the daemon always injects them via
// NewWithDeps, but a caller that constructs the Server through New (tests,
// or any depless caller) still gets a service that runs without block sync
// rather than with a faked pipeline behind it.
type Deps struct {
	ValidationClient  validator.Interface
	SubtreeStore      blob.Store
	TempStore         blob.Store
	UtxoStore         utxo.Store
	SubtreeValidation subtreevalidation.Interface
	BlockValidation   blockvalidation.Interface
	BlockAssembly     *blockassembly.Client
}

// complete reports whether every dependency the bridge needs is present. A
// partial set is not usable: the ingestion pipeline calls all of them.
func (d Deps) complete() bool {
	return d.ValidationClient != nil &&
		d.SubtreeStore != nil &&
		d.TempStore != nil &&
		d.UtxoStore != nil &&
		d.SubtreeValidation != nil &&
		d.BlockValidation != nil &&
		d.BlockAssembly != nil
}

// Server is the Teranode service shell for svp2p. It matches the service
// manager contract of services/legacy/Server.go so the daemon can host
// either service behind the same lifecycle.
type Server struct {
	peer_api.UnsafePeerServiceServer

	logger           ulogger.Logger
	settings         *settings.Settings
	blockchainClient blockchain.ClientI
	deps             Deps

	listenAddresses []string
	banList         *protocol.BanList
	manager         *protocol.PeerManager
	admission       *bridge.Admission

	headerIndexMu sync.RWMutex
	headerIndex   *protocol.HeaderIndex

	// headerBatchSize is an instance field, not a package const, purely so
	// tests can shrink it on their own *Server to force a multi-batch
	// forward walk without touching shared state.
	headerBatchSize uint32

	// syncTick and maxLastBlockTime are instance fields for the same reason:
	// the end-to-end sync test drives the periodic send pass and the sync-peer
	// rotation on its own *Server without waiting out the production windows.
	// Zero on both means "keep the protocol package's own default".
	syncTick         time.Duration
	maxLastBlockTime time.Duration
}

func New(logger ulogger.Logger, tSettings *settings.Settings, blockchainClient blockchain.ClientI) *Server {
	return &Server{
		logger:           logger,
		settings:         tSettings,
		blockchainClient: blockchainClient,
		headerBatchSize:  defaultHeaderBatchSize,
	}
}

// NewWithDeps is New plus the ingestion dependencies, which is what enables
// block sync. The daemon uses this constructor exclusively.
func NewWithDeps(logger ulogger.Logger, tSettings *settings.Settings, blockchainClient blockchain.ClientI, deps Deps) *Server {
	srv := New(logger, tSettings, blockchainClient)
	srv.deps = deps

	return srv
}

func (s *Server) Health(ctx context.Context, checkLiveness bool) (int, string, error) {
	if checkLiveness {
		return http.StatusOK, "OK", nil
	}

	checks := make([]health.Check, 0, 2)

	if s.blockchainClient != nil {
		checks = append(checks, health.Check{Name: "BlockchainClient", Check: s.blockchainClient.Health})
		checks = append(checks, health.Check{Name: "FSM", Check: blockchain.CheckFSM(s.blockchainClient)})
	}

	return health.CheckAll(ctx, checkLiveness, checks)
}

func (s *Server) Init(_ context.Context) error {
	wire.SetLimits(4000000000)

	s.listenAddresses = s.settings.Legacy.ListenAddresses
	if len(s.listenAddresses) == 0 {
		ip, err := outboundIP()
		if err != nil {
			return err
		}

		s.listenAddresses = []string{net.JoinHostPort(ip.String(), s.settings.ChainCfgParams.DefaultPort)}
	}

	banListPath := ""
	if s.settings.Legacy.WorkingDir != "" {
		banListPath = filepath.Join(s.settings.Legacy.WorkingDir, "banlist.json")
	}

	banList, err := protocol.NewBanList(banListPath)
	if err != nil {
		return err
	}

	s.banList = banList
	s.manager = protocol.NewPeerManager(s.logger, s.settings, banList)

	return nil
}

func (s *Server) Start(ctx context.Context, readyCh chan<- struct{}) error {
	var readyClosed bool

	closeReady := func() {
		if !readyClosed {
			readyClosed = true

			close(readyCh)
		}
	}
	defer closeReady()

	if err := s.blockchainClient.WaitUntilFSMTransitionFromIdleState(ctx); err != nil {
		if errors.IsContextError(err) {
			s.logger.Infof("[svp2p] shutting down during FSM wait")
			return err
		}

		s.logger.Errorf("[svp2p] failed to wait for FSM transition from IDLE state: %s", err)

		return err
	}

	if err := s.startSync(ctx); err != nil {
		if errors.IsContextError(err) {
			s.logger.Infof("[svp2p] shutting down during header index hydration")
			return err
		}

		s.logger.Errorf("[svp2p] failed to start block sync: %s", err)

		return err
	}

	// Hydrating before subscribing cannot miss a block that lands in between:
	// both Client and LocalClient replay their last/current best-block
	// notification to a subscriber immediately upon Subscribe, so the first
	// notification received here always re-confirms (rather than skips)
	// whatever the store's best header was at subscribe time.
	headerNotifications, err := s.blockchainClient.Subscribe(ctx, blockchain.SubscriberSVP2P)
	if err != nil {
		s.logger.Errorf("[svp2p] failed to subscribe to blockchain notifications: %s", err)
		return err
	}

	go s.runHeaderIndexSubscription(ctx, headerNotifications)

	if err := s.manager.Start(ctx, s.listenAddresses); err != nil {
		return err
	}

	s.logger.Infof("[svp2p] peer manager started, listening on %v", s.manager.ListenAddrs())

	apiKey := s.settings.GRPCAdminAPIKey
	if apiKey == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return errors.New(errors.ERR_SERVICE_NOT_STARTED, "svp2p: failed to generate API key", err)
		}

		apiKey = hex.EncodeToString(key)
		s.logger.Infof("[svp2p] generated admin API key: %s", apiKey)
	}

	authOptions := &util.AuthOptions{
		APIKey: apiKey,
		ProtectedMethods: map[string]bool{
			"/peer_api.PeerService/BanPeer":   true,
			"/peer_api.PeerService/UnbanPeer": true,
		},
	}

	// This blocks until the gRPC server stops.
	if err := util.StartGRPCServer(ctx, s.logger, s.settings, "svp2p", s.settings.Legacy.GRPCListenAddress, func(server *grpc.Server) {
		peer_api.RegisterPeerServiceServer(server, s)
		closeReady()
	}, authOptions); err != nil {
		return errors.New(errors.ERR_SERVICE_NOT_STARTED, "svp2p: cannot start gRPC server", err)
	}

	return nil
}

func (s *Server) Stop(_ context.Context) error {
	// The header index subscription goroutine (runHeaderIndexSubscription)
	// is ctx-owned: it exits on the Start ctx's cancellation, which the
	// daemon triggers before calling Stop. Nothing to join here.
	//
	// Peers are joined FIRST: PeerManager.Stop waits for every peer goroutine,
	// and a peer may still be running an ingest that goes through Admission.
	// Releasing Admission's eviction goroutine before those callers are gone
	// would stop the failure map underneath a live ingest.
	if s.manager != nil {
		if err := s.manager.Stop(); err != nil {
			return err
		}
	}

	// Admission is not ctx-owned: its failure map runs a background eviction
	// goroutine that only Stop releases.
	if s.admission != nil {
		s.admission.Stop()
		s.admission = nil
	}

	return nil
}

// HeaderIndex returns the server's header index, or nil before Start has
// hydrated it. Safe for concurrent use.
func (s *Server) HeaderIndex() *protocol.HeaderIndex {
	s.headerIndexMu.RLock()
	defer s.headerIndexMu.RUnlock()

	return s.headerIndex
}

func (s *Server) setHeaderIndex(idx *protocol.HeaderIndex) {
	s.headerIndexMu.Lock()
	s.headerIndex = idx
	s.headerIndexMu.Unlock()
}

// startSync builds the header index, hands it and the block-ingestion path to
// the peer manager, and hydrates the index. The order matters: the manager
// must own the index before the first header is written to it, because every
// write goes through the manager's shared sync-state mutex.
func (s *Server) startSync(ctx context.Context) error {
	if err := s.hydrateHeaderIndex(ctx); err != nil {
		return err
	}

	ingestor, err := s.newBlockIngestor()
	if err != nil {
		return err
	}

	if err := s.manager.ConfigureSync(protocol.SyncConfig{
		Index:                            s.HeaderIndex(),
		Ingestor:                         ingestor,
		AllowSyncCandidateFromLocalPeers: s.settings.Legacy.AllowSyncCandidateFromLocalPeers,
		TickInterval:                     s.syncTick,
		MaxLastBlockTime:                 s.maxLastBlockTime,

		BlockDownloadTimeoutBasePercent:    s.settings.Legacy.BlockDownloadTimeoutBasePercent,
		BlockDownloadTimeoutBaseIBDPercent: s.settings.Legacy.BlockDownloadTimeoutBaseIBDPercent,
		BlockDownloadTimeoutPerPeerPercent: s.settings.Legacy.BlockDownloadTimeoutPerPeerPercent,
	}); err != nil {
		return err
	}

	return s.syncHeaderIndex(ctx)
}

// newBlockIngestor constructs the real bridge and its admission gate from the
// injected dependencies, or returns nil when they are not injected. A nil
// ingestor leaves the manager with the header index and no block sync: the
// service keeps serving the peer API and following the chain, and asks no peer
// for a block it has nothing to ingest with. The daemon always injects the
// dependencies; only a depless caller (New instead of NewWithDeps) hits this.
func (s *Server) newBlockIngestor() (protocol.BlockIngestor, error) {
	if !s.deps.complete() {
		s.logger.Warnf("[svp2p] block sync disabled: the block ingestion dependencies are not injected")
		return nil, nil
	}

	s.admission = bridge.NewAdmission(s.logger, s.settings)

	return &blockIngestor{
		logger: s.logger,
		bridge: bridge.New(
			s.logger,
			s.settings,
			s.blockchainClient,
			s.deps.ValidationClient,
			s.deps.SubtreeStore,
			s.deps.TempStore,
			s.deps.UtxoStore,
			s.deps.SubtreeValidation,
			s.deps.BlockValidation,
			s.deps.BlockAssembly,
		),
		admission: s.admission,
	}, nil
}

// hydrateHeaderIndex builds a fresh, genesis-rooted header index from the
// blockchain service. The blockchain service stays the authoritative header
// store (spec §11): this index is discarded and rebuilt on every startup.
func (s *Server) hydrateHeaderIndex(ctx context.Context) error {
	genesisHeaders, _, err := s.blockchainClient.GetBlockHeadersFromHeight(ctx, 0, 1)
	if err != nil {
		return errors.NewServiceError("svp2p: failed to fetch genesis header", err)
	}

	if len(genesisHeaders) == 0 {
		return errors.NewServiceError("svp2p: blockchain store returned no genesis header")
	}

	idx, err := protocol.NewHeaderIndex(genesisHeaders[0].ToWireBlockHeader())
	if err != nil {
		return err
	}

	s.setHeaderIndex(idx)

	return nil
}

// syncHeaderIndex walks the header index forward from its current tip to the
// blockchain service's best header. A bounded-batch forward walk covers
// straightforward chain growth cheaply, including a full genesis replay on
// first hydration. That walk alone cannot follow a reorg, though: the
// blockchain service stays authoritative and can switch its best chain to
// one whose ancestors sit at heights the forward walk already passed (a
// competing branch that only recently became taller), leaving those headers
// unfetched and the new best unconnected. reconcileHeaderIndex covers that
// case afterward. Both passes are called once during hydration and again on
// every subscription notification, so both are no-ops once the index has
// caught up.
func (s *Server) syncHeaderIndex(ctx context.Context) error {
	idx := s.HeaderIndex()
	if idx == nil {
		return errors.NewServiceError("svp2p: header index sync called before hydration")
	}

	best, bestMeta, err := s.blockchainClient.GetBestBlockHeader(ctx)
	if err != nil {
		return errors.NewServiceError("svp2p: failed to get best block header", err)
	}

	if err := s.forwardWalkHeaderIndex(ctx, idx, bestMeta.Height); err != nil {
		return err
	}

	if err := s.reconcileHeaderIndex(ctx, idx, best); err != nil {
		return err
	}

	// The download scheduler walks against our own chain tip (the chainActive
	// counterpart), and it must be a header the index holds — which the two
	// passes above have just guaranteed for this one.
	if !s.manager.SetActiveTip(*best.Hash()) {
		s.logger.Warnf("[svp2p] best header %s is not in the header index; block download keeps the previous tip", best.Hash())
	}

	return nil
}

// addHeaders links headers into the index through the peer manager, which
// holds the shared sync-state mutex while it writes. Nothing may call
// HeaderIndex.AddHeader directly: the headers-first machine reads the index
// and then writes to it under that mutex, so a write outside it makes the
// machine act on stale state.
func (s *Server) addHeaders(headers []*wire.BlockHeader, pass string) error {
	orphans, err := s.manager.AddHeaders(headers)

	for _, orphan := range orphans {
		hash := orphan.BlockHash()
		s.logger.Warnf("[svp2p] orphan header %s while syncing header index (%s)", hash, pass)
	}

	return err
}

// forwardWalkHeaderIndex fetches headers in bounded, height-ordered batches
// from the index's current tip up to targetHeight.
//
// GetBlockHeadersFromHeight ranges over height, not row count — its query is
// `WHERE height >= $1 AND height < $1+limit` (see
// stores/blockchain/sql/GetBlockHeadersFromHeight.go), and it returns ALL
// headers at each height, including competing-fork headers, preallocating
// for up to 2*limit rows. So the next fetch must start at height+limit
// regardless of how many rows came back, and each batch is returned in
// descending height order, so it's walked oldest-first here so a header's
// parent is always already indexed when AddHeader sees it.
func (s *Server) forwardWalkHeaderIndex(ctx context.Context, idx *protocol.HeaderIndex, targetHeight uint32) error {
	_, tipHeight := idx.Tip()

	for height := uint32(tipHeight) + 1; height <= targetHeight; { //nolint:gosec // header heights are non-negative
		limit := targetHeight - height + 1
		if limit > s.headerBatchSize {
			limit = s.headerBatchSize
		}

		headers, _, err := s.blockchainClient.GetBlockHeadersFromHeight(ctx, height, limit)
		if err != nil {
			return errors.NewServiceError("svp2p: failed to fetch headers from height %d", height, err)
		}

		batch := make([]*wire.BlockHeader, 0, len(headers))
		for i := len(headers) - 1; i >= 0; i-- {
			batch = append(batch, headers[i].ToWireBlockHeader())
		}

		if err := s.addHeaders(batch, fmt.Sprintf("forward walk from height %d", height)); err != nil {
			return err
		}

		height += limit
	}

	return nil
}

// reconcileHeaderIndex recovers from a reorg the forward walk could not
// follow: if best is not yet indexed, its ancestors were left behind on a
// branch that stopped being best before the forward walk reached their
// heights. It walks backward one header at a time via GetBlockHeader,
// starting at best, until it reaches a header already in idx — a walk
// guaranteed to terminate because genesis is always present from hydration
// — then adds the recovered segment forward so every ancestor connects.
func (s *Server) reconcileHeaderIndex(ctx context.Context, idx *protocol.HeaderIndex, best *model.BlockHeader) error {
	if _, ok := idx.Lookup(*best.Hash()); ok {
		return nil
	}

	missing := []*model.BlockHeader{best}
	current := best

	for current.HashPrevBlock != nil {
		parent, _, err := s.blockchainClient.GetBlockHeader(ctx, current.HashPrevBlock)
		if err != nil {
			return errors.NewServiceError("svp2p: failed to fetch header %s while reconciling a reorg", current.HashPrevBlock, err)
		}

		if _, ok := idx.Lookup(*parent.Hash()); ok {
			break
		}

		missing = append(missing, parent)
		current = parent
	}

	recovered := make([]*wire.BlockHeader, 0, len(missing))
	for i := len(missing) - 1; i >= 0; i-- {
		recovered = append(recovered, missing[i].ToWireBlockHeader())
	}

	return s.addHeaders(recovered, "reorg reconciliation")
}

// runHeaderIndexSubscription keeps the header index current from the
// blockchain service's own notifications. It exits when ctx is cancelled,
// the same shutdown signal Start passes to the rest of the service.
func (s *Server) runHeaderIndexSubscription(ctx context.Context, notifications <-chan *blockchain.Notification) {
	for {
		select {
		case <-ctx.Done():
			s.logger.Infof("[svp2p] header index subscription shutting down")
			return
		case notification, ok := <-notifications:
			if !ok {
				// Channel closed, exit the listener.
				return
			}

			if notification == nil || notification.Type != model.NotificationType_Block {
				continue
			}

			if err := s.syncHeaderIndex(ctx); err != nil {
				if errors.IsContextError(err) {
					return
				}

				s.logger.Errorf("[svp2p] failed to sync header index: %s", err)
			}
		}
	}
}

func (s *Server) GetPeerCount(_ context.Context, _ *emptypb.Empty) (*peer_api.GetPeerCountResponse, error) {
	return &peer_api.GetPeerCountResponse{Count: s.manager.ConnectedCount()}, nil
}

func (s *Server) GetPeers(_ context.Context, _ *emptypb.Empty) (*peer_api.GetPeersResponse, error) {
	resp := &peer_api.GetPeersResponse{}

	for _, snap := range s.manager.Snapshots() {
		resp.Peers = append(resp.Peers, &peer_api.Peer{
			Addr:           snap.Addr,
			Inbound:        snap.Inbound,
			SubVer:         snap.UserAgent,
			Version:        snap.ProtocolVersion,
			StartingHeight: snap.StartingHeight,
			BytesSent:      snap.BytesSent,
			BytesReceived:  snap.BytesReceived,
			ConnTime:       snap.ConnectedAt.Unix(),
			LastRecv:       snap.LastRecv.Unix(),
			Banscore:       int32(snap.MisbehaviorScore), //nolint:gosec // bounded by ban threshold
		})
	}

	return resp, nil
}

func (s *Server) BanPeer(_ context.Context, req *peer_api.BanPeerRequest) (*peer_api.BanPeerResponse, error) {
	host := banHost(req.Addr)

	if err := s.banList.Add(host, time.Unix(req.Until, 0)); err != nil {
		return nil, err
	}

	s.manager.DisconnectHost(host)
	s.logger.Infof("[svp2p] banned %s until %d", host, req.Until)

	return &peer_api.BanPeerResponse{Ok: true}, nil
}

func (s *Server) UnbanPeer(_ context.Context, req *peer_api.UnbanPeerRequest) (*peer_api.UnbanPeerResponse, error) {
	if err := s.banList.Remove(banHost(req.Addr)); err != nil {
		return nil, err
	}

	return &peer_api.UnbanPeerResponse{Ok: true}, nil
}

func (s *Server) IsBanned(_ context.Context, req *peer_api.IsBannedRequest) (*peer_api.IsBannedResponse, error) {
	target := banHost(req.IpOrSubnet)

	if _, _, err := net.ParseCIDR(target); err == nil {
		for _, entry := range s.banList.List() {
			if entry.Host == target {
				return &peer_api.IsBannedResponse{IsBanned: true}, nil
			}
		}

		return &peer_api.IsBannedResponse{IsBanned: false}, nil
	}

	return &peer_api.IsBannedResponse{IsBanned: s.banList.IsBanned(target)}, nil
}

func (s *Server) ListBanned(_ context.Context, _ *emptypb.Empty) (*peer_api.ListBannedResponse, error) {
	entries := s.banList.List()

	banned := make([]string, 0, len(entries))
	for _, e := range entries {
		banned = append(banned, e.Host)
	}

	return &peer_api.ListBannedResponse{Banned: banned}, nil
}

func (s *Server) ClearBanned(_ context.Context, _ *emptypb.Empty) (*peer_api.ClearBannedResponse, error) {
	if err := s.banList.Clear(); err != nil {
		return nil, err
	}

	return &peer_api.ClearBannedResponse{Ok: true}, nil
}

// banHost strips a port from ip:port input; ban entries are host-level.
func banHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}

	return addr
}

// outboundIP mirrors legacy GetOutboundIP: the default listen address when
// none is configured.
func outboundIP() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, errors.New(errors.ERR_SERVICE_ERROR, "svp2p: failed to get outbound IP", err)
	}

	defer func() { _ = conn.Close() }()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, errors.New(errors.ERR_SERVICE_ERROR, "svp2p: unexpected local address type")
	}

	return localAddr.IP, nil
}

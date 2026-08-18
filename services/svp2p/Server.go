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
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/legacy/peer_api"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/health"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// headerHydrateBatch bounds each forward-walk fetch during header index
// hydration and resync to the wire protocol's own headers-per-message cap.
const headerHydrateBatch = wire.MaxBlockHeadersPerMsg

// Server is the Teranode service shell for svp2p. It matches the service
// manager contract of services/legacy/Server.go so the daemon can host
// either service behind the same lifecycle.
type Server struct {
	peer_api.UnsafePeerServiceServer

	logger           ulogger.Logger
	settings         *settings.Settings
	blockchainClient blockchain.ClientI

	listenAddresses []string
	banList         *protocol.BanList
	manager         *protocol.PeerManager

	headerIndexMu sync.RWMutex
	headerIndex   *protocol.HeaderIndex
}

func New(logger ulogger.Logger, tSettings *settings.Settings, blockchainClient blockchain.ClientI) *Server {
	return &Server{
		logger:           logger,
		settings:         tSettings,
		blockchainClient: blockchainClient,
	}
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

	if err := s.hydrateHeaderIndex(ctx); err != nil {
		if errors.IsContextError(err) {
			s.logger.Infof("[svp2p] shutting down during header index hydration")
			return err
		}

		s.logger.Errorf("[svp2p] failed to hydrate header index: %s", err)

		return err
	}

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
	if s.manager == nil {
		return nil
	}

	return s.manager.Stop()
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

	return s.syncHeaderIndex(ctx)
}

// syncHeaderIndex walks the header index forward from its current tip to the
// blockchain service's best header, in batches bounded by headerHydrateBatch.
// It runs once during hydration and again on every subscription
// notification, so it is a no-op once the index has caught up.
func (s *Server) syncHeaderIndex(ctx context.Context) error {
	idx := s.HeaderIndex()
	if idx == nil {
		return errors.NewServiceError("svp2p: header index sync called before hydration")
	}

	_, bestMeta, err := s.blockchainClient.GetBestBlockHeader(ctx)
	if err != nil {
		return errors.NewServiceError("svp2p: failed to get best block header", err)
	}

	_, tipHeight := idx.Tip()

	for height := uint32(tipHeight) + 1; height <= bestMeta.Height; { //nolint:gosec // header heights are non-negative
		limit := bestMeta.Height - height + 1
		if limit > headerHydrateBatch {
			limit = headerHydrateBatch
		}

		headers, _, err := s.blockchainClient.GetBlockHeadersFromHeight(ctx, height, limit)
		if err != nil {
			return errors.NewServiceError("svp2p: failed to fetch headers from height %d", height, err)
		}

		if len(headers) == 0 {
			break
		}

		// GetBlockHeadersFromHeight returns its batch in descending height
		// order; AddHeader needs each parent already indexed, so walk the
		// batch from oldest to newest.
		for i := len(headers) - 1; i >= 0; i-- {
			h := headers[i]

			connected, err := idx.AddHeader(h.ToWireBlockHeader())
			if err != nil {
				return err
			}

			if !connected {
				s.logger.Warnf("[svp2p] orphan header %s while syncing header index at height %d", h.Hash(), height)
			}
		}

		height += uint32(len(headers))
	}

	return nil
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
		case notification := <-notifications:
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

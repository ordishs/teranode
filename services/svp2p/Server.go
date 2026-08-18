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
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
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

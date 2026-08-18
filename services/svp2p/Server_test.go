package svp2p

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/legacy/peer_api"
	blockchain_store "github.com/bsv-blockchain/teranode/stores/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

func freePort(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	return addr
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.Legacy.ListenAddresses = []string{"127.0.0.1:0"}
	tSettings.Legacy.GRPCListenAddress = freePort(t)
	tSettings.Legacy.WorkingDir = t.TempDir()
	tSettings.GRPCAdminAPIKey = "test-admin-key"

	store, err := blockchain_store.NewStore(ulogger.TestLogger{}, &url.URL{Scheme: "sqlitememory"}, tSettings)
	require.NoError(t, err)

	blockchainClient, err := blockchain.NewLocalClient(ulogger.TestLogger{}, tSettings, store, nil, nil)
	require.NoError(t, err)

	return New(ulogger.TestLogger{}, tSettings, blockchainClient), tSettings.Legacy.GRPCListenAddress
}

func startServer(t *testing.T, srv *Server) context.CancelFunc {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	require.NoError(t, srv.Init(ctx))

	readyCh := make(chan struct{})

	go func() { _ = srv.Start(ctx, readyCh) }()

	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not become ready")
	}

	return cancel
}

func dialPeerAPI(t *testing.T, addr string) peer_api.PeerServiceClient {
	t.Helper()

	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	t.Cleanup(func() { _ = cc.Close() })

	return peer_api.NewPeerServiceClient(cc)
}

func TestServerStartServesGRPC(t *testing.T) {
	srv, grpcAddr := newTestServer(t)

	cancel := startServer(t, srv)
	defer cancel()

	defer func() { require.NoError(t, srv.Stop(context.Background())) }()

	client := dialPeerAPI(t, grpcAddr)

	resp, err := client.GetPeerCount(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, int32(0), resp.Count)

	peers, err := client.GetPeers(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	require.Empty(t, peers.Peers)
}

func TestServerBanUnbanRoundTrip(t *testing.T) {
	srv, _ := newTestServer(t)

	cancel := startServer(t, srv)
	defer cancel()

	defer func() { require.NoError(t, srv.Stop(context.Background())) }()

	ctx := context.Background()

	_, err := srv.BanPeer(ctx, &peer_api.BanPeerRequest{Addr: "1.2.3.4", Until: time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)

	banned, err := srv.IsBanned(ctx, &peer_api.IsBannedRequest{IpOrSubnet: "1.2.3.4"})
	require.NoError(t, err)
	require.True(t, banned.IsBanned)

	list, err := srv.ListBanned(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.Contains(t, list.Banned, "1.2.3.4")

	_, err = srv.UnbanPeer(ctx, &peer_api.UnbanPeerRequest{Addr: "1.2.3.4"})
	require.NoError(t, err)

	banned, err = srv.IsBanned(ctx, &peer_api.IsBannedRequest{IpOrSubnet: "1.2.3.4"})
	require.NoError(t, err)
	require.False(t, banned.IsBanned)
}

func TestServerClearBanned(t *testing.T) {
	srv, _ := newTestServer(t)

	cancel := startServer(t, srv)
	defer cancel()

	defer func() { require.NoError(t, srv.Stop(context.Background())) }()

	ctx := context.Background()

	_, err := srv.BanPeer(ctx, &peer_api.BanPeerRequest{Addr: "10.0.0.0/8", Until: time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)

	_, err = srv.ClearBanned(ctx, &emptypb.Empty{})
	require.NoError(t, err)

	list, err := srv.ListBanned(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.Empty(t, list.Banned)
}

func TestServerStopIsClean(t *testing.T) {
	srv, grpcAddr := newTestServer(t)

	cancel := startServer(t, srv)
	defer cancel()

	require.NoError(t, srv.Stop(context.Background()))
	cancel()

	require.Eventually(t, func() bool {
		_, err := net.DialTimeout("tcp", grpcAddr, time.Second)
		return err != nil
	}, 10*time.Second, 100*time.Millisecond, "gRPC port still open after Stop")
}

func TestServerHealth(t *testing.T) {
	srv, _ := newTestServer(t)

	code, msg, err := srv.Health(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, 200, code)
	require.Equal(t, "OK", msg)
}

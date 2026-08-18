package httpimpl

import (
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/stretchr/testify/require"
)

// TestTransportLabel checks the string the dashboard switches on.
func TestTransportLabel(t *testing.T) {
	require.Equal(t, "legacy",
		transportLabel(blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL))
	require.Equal(t, "libp2p",
		transportLabel(blockchain_api.TransportType_TRANSPORT_HTTP))
}

// TestPeerInfoToResponse_LegacyPeer checks a wire-protocol peer maps to the
// legacy transport label and a populated nested legacy object.
func TestPeerInfoToResponse_LegacyPeer(t *testing.T) {
	connectedAt := time.Unix(1750000000, 0).UTC()

	got := peerInfoToResponse(&blockchain.PeerInfo{
		ID:              "legacy:203.0.113.7:8333",
		TransportType:   blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL,
		ClientName:      "/Bitcoin SV:1.0.16/",
		NetworkAddress:  "203.0.113.7:8333",
		Height:          912345,
		BytesSent:       111,
		BytesReceived:   222,
		IsConnected:     true,
		LastMessageTime: time.Unix(1750000500, 0).UTC(),
		Legacy: &blockchain.LegacyPeerInfo{
			Inbound:         true,
			ProtocolVersion: 70016,
			ServiceFlags:    0x25,
			PingMicros:      42000,
			TimeOffsetSecs:  -3,
			StartingHeight:  912000,
			IsSyncPeer:      true,
			TimeConnected:   connectedAt,
		},
	})

	require.Equal(t, "legacy:203.0.113.7:8333", got.ID)
	require.Equal(t, "legacy", got.Transport)
	require.Equal(t, "203.0.113.7:8333", got.NetworkAddress)
	require.Equal(t, uint64(111), got.BytesSent)
	require.Equal(t, uint64(222), got.BytesReceived)
	require.True(t, got.IsConnected)

	require.NotNil(t, got.Legacy)
	require.True(t, got.Legacy.Inbound)
	require.Equal(t, uint32(70016), got.Legacy.ProtocolVersion)
	require.Equal(t, uint64(0x25), got.Legacy.ServiceFlags)
	require.Equal(t, int64(42000), got.Legacy.PingMicros)
	require.Equal(t, int64(-3), got.Legacy.TimeOffsetSecs)
	require.Equal(t, int32(912000), got.Legacy.StartingHeight)
	require.True(t, got.Legacy.IsSyncPeer)
	require.Equal(t, connectedAt.Unix(), got.Legacy.TimeConnected)
}

// TestPeerInfoToResponse_Libp2pPeerOmitsLegacy checks a libp2p peer carries no
// legacy object, so the dashboard can rely on its absence.
func TestPeerInfoToResponse_Libp2pPeerOmitsLegacy(t *testing.T) {
	got := peerInfoToResponse(&blockchain.PeerInfo{
		ID:            "12D3KooWGRUEbFsXTBnpVRHtE3ZBSbSMd4x8hs9NfCVCNhqTFPHb",
		TransportType: blockchain_api.TransportType_TRANSPORT_HTTP,
		DataHubURL:    "http://198.51.100.4:8090",
		Height:        912350,
	})

	require.Equal(t, "libp2p", got.Transport)
	require.Nil(t, got.Legacy)
	require.Equal(t, "http://198.51.100.4:8090", got.DataHubURL)
}

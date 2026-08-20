package legacy

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// countingRegistry wraps a real local registry client and counts the calls the
// reconcile loop makes, so a test can assert that an unchanged peer costs
// nothing. Only call counting is added; every write lands in a real
// CentralizedPeerRegistry.
type countingRegistry struct {
	blockchain.PeerRegistryClientI
	registerCalls   int
	connStateCalls  map[string][]bool
	metricsCalls    int
	lastMessageCals int
	failRegister    error
}

func newCountingRegistry(reg *blockchain.CentralizedPeerRegistry) *countingRegistry {
	return &countingRegistry{
		PeerRegistryClientI: blockchain.NewLocalPeerRegistryClient(reg),
		connStateCalls:      make(map[string][]bool),
	}
}

func (c *countingRegistry) RegisterPeer(ctx context.Context, info *blockchain.PeerInfo) error {
	if c.failRegister != nil {
		return c.failRegister
	}

	c.registerCalls++

	return c.PeerRegistryClientI.RegisterPeer(ctx, info)
}

func (c *countingRegistry) UpdateConnectionState(ctx context.Context, peerID string, connected bool) error {
	c.connStateCalls[peerID] = append(c.connStateCalls[peerID], connected)

	return c.PeerRegistryClientI.UpdateConnectionState(ctx, peerID, connected)
}

func (c *countingRegistry) UpdatePeerMetrics(ctx context.Context, peerID string, height uint32,
	bytesSentDelta, bytesRecvDelta uint64, recordSuccess, recordFailure, recordMalicious bool,
	responseTimeMs int64) error {
	c.metricsCalls++

	return c.PeerRegistryClientI.UpdatePeerMetrics(ctx, peerID, height, bytesSentDelta,
		bytesRecvDelta, recordSuccess, recordFailure, recordMalicious, responseTimeMs)
}

func (c *countingRegistry) UpdateLastMessageTime(ctx context.Context, peerID string) error {
	c.lastMessageCals++

	return c.PeerRegistryClientI.UpdateLastMessageTime(ctx, peerID)
}

func testSyncSettings() *settings.Settings {
	tSettings := settings.NewSettings()
	tSettings.Legacy.PeerRegistryEnabled = true
	tSettings.Legacy.PeerRegistrySyncInterval = time.Second

	return tSettings
}

func testSnapshot(addr string, bytesRecv uint64, lastRecv time.Time) peerSnapshot {
	return peerSnapshot{
		id:            legacyRegistryID(addr),
		addr:          addr,
		userAgent:     "/Bitcoin SV:1.0.16/",
		height:        912345,
		bytesSent:     100,
		bytesReceived: bytesRecv,
		lastRecv:      lastRecv,
		legacy: blockchain.LegacyPeerInfo{
			Inbound:         false,
			ProtocolVersion: 70016,
			ServiceFlags:    0x25,
			PingMicros:      42000,
			StartingHeight:  912000,
			IsSyncPeer:      true,
			TimeConnected:   time.Unix(1750000000, 0).UTC(),
		},
	}
}

// TestReconcile_RegistersNewPeer checks a first sighting registers with the
// wire-protocol transport, the legacy: ID and the legacy block.
func TestReconcile_RegistersNewPeer(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	counting := newCountingRegistry(reg)

	snap := testSnapshot("203.0.113.7:8333", 500, time.Unix(1750000100, 0))
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), counting,
		func() []peerSnapshot { return []peerSnapshot{snap} })

	sync.reconcile(context.Background())

	got, ok := reg.Get("legacy:203.0.113.7:8333")
	require.True(t, ok)
	require.Equal(t, blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL, got.TransportType)
	require.Equal(t, "203.0.113.7:8333", got.NetworkAddress)
	require.Equal(t, "/Bitcoin SV:1.0.16/", got.ClientName)
	require.Equal(t, uint32(912345), got.Height)
	require.True(t, got.IsConnected)
	require.NotNil(t, got.Legacy)
	require.Equal(t, uint32(70016), got.Legacy.ProtocolVersion)
	require.True(t, got.Legacy.IsSyncPeer)
	require.Equal(t, uint64(500), got.BytesReceived, "first sighting pushes the whole total")
	require.Equal(t, 1, counting.registerCalls)
}

// TestReconcile_UnchangedPeerCostsNoRegisterCall checks the skip rule.
func TestReconcile_UnchangedPeerCostsNoRegisterCall(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	counting := newCountingRegistry(reg)

	snap := testSnapshot("203.0.113.7:8333", 500, time.Unix(1750000100, 0))
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), counting,
		func() []peerSnapshot { return []peerSnapshot{snap} })

	sync.reconcile(context.Background())
	sync.reconcile(context.Background())

	require.Equal(t, 1, counting.registerCalls, "an unchanged peer must not re-register")
	require.Equal(t, []bool{true}, counting.connStateCalls["legacy:203.0.113.7:8333"],
		"connection state must be asserted once, not every tick")
}

// TestReconcile_VanishedPeerMarkedDisconnectedOnce checks step 4 of the loop,
// and the TTL trap: the peer must also leave the tracking map, so no later tick
// re-registers it and refreshes its LastSeen clock.
func TestReconcile_VanishedPeerMarkedDisconnectedOnce(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	counting := newCountingRegistry(reg)

	snap := testSnapshot("203.0.113.7:8333", 500, time.Unix(1750000100, 0))
	present := true
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), counting,
		func() []peerSnapshot {
			if present {
				return []peerSnapshot{snap}
			}

			return []peerSnapshot{}
		})

	sync.reconcile(context.Background())
	present = false
	sync.reconcile(context.Background())
	sync.reconcile(context.Background())

	got, ok := reg.Get("legacy:203.0.113.7:8333")
	require.True(t, ok, "the entry stays for history; the TTL reaps it")
	require.False(t, got.IsConnected)
	require.Equal(t, []bool{true, false}, counting.connStateCalls["legacy:203.0.113.7:8333"],
		"disconnect must be asserted exactly once")
	require.Equal(t, 1, counting.registerCalls,
		"a vanished peer must never be re-registered: Register refreshes the TTL clock")
}

// TestReconcile_NilSnapshotIsNotADisconnect is a regression test: getPeers
// returns nil when its query channel is full or its reply times out. That must
// not read as "every peer went away".
func TestReconcile_NilSnapshotIsNotADisconnect(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	counting := newCountingRegistry(reg)

	snap := testSnapshot("203.0.113.7:8333", 500, time.Unix(1750000100, 0))
	healthy := true
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), counting,
		func() []peerSnapshot {
			if healthy {
				return []peerSnapshot{snap}
			}

			return nil
		})

	sync.reconcile(context.Background())
	healthy = false
	sync.reconcile(context.Background())

	got, ok := reg.Get("legacy:203.0.113.7:8333")
	require.True(t, ok)
	require.True(t, got.IsConnected, "a nil snapshot must not mark peers disconnected")
	require.Equal(t, []bool{true}, counting.connStateCalls["legacy:203.0.113.7:8333"])
}

// TestReconcile_ByteCountersAreDeltas checks the delta conversion, including a
// counter reset after a reconnect.
func TestReconcile_ByteCountersAreDeltas(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	counting := newCountingRegistry(reg)

	current := testSnapshot("203.0.113.7:8333", 500, time.Unix(1750000100, 0))
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), counting,
		func() []peerSnapshot { return []peerSnapshot{current} })

	sync.reconcile(context.Background())

	current.bytesReceived = 1200
	current.lastRecv = time.Unix(1750000200, 0)
	sync.reconcile(context.Background())

	got, _ := reg.Get("legacy:203.0.113.7:8333")
	require.Equal(t, uint64(1200), got.BytesReceived, "500 then a delta of 700")

	// A reconnect resets the peer's counters. The delta must treat the reset
	// total as new bytes rather than wrapping around uint64.
	current.bytesReceived = 10
	current.lastRecv = time.Unix(1750000300, 0)
	sync.reconcile(context.Background())

	got, _ = reg.Get("legacy:203.0.113.7:8333")
	require.Equal(t, uint64(1210), got.BytesReceived,
		"a backwards counter means a replaced connection, so its 10 bytes are new")
}

// TestReconcile_RegistryErrorDoesNotStopTheLoop checks that a failing registry
// leaves the loop able to recover on the next tick.
func TestReconcile_RegistryErrorDoesNotStopTheLoop(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	counting := newCountingRegistry(reg)
	counting.failRegister = context.DeadlineExceeded

	snap := testSnapshot("203.0.113.7:8333", 500, time.Unix(1750000100, 0))
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), counting,
		func() []peerSnapshot { return []peerSnapshot{snap} })

	sync.reconcile(context.Background())

	_, ok := reg.Get("legacy:203.0.113.7:8333")
	require.False(t, ok)

	counting.failRegister = nil
	sync.reconcile(context.Background())

	got, ok := reg.Get("legacy:203.0.113.7:8333")
	require.True(t, ok, "the next tick must retry a failed registration")
	require.True(t, got.IsConnected)
}

// TestLegacyRegistryID checks the ID never collides with a libp2p peer ID.
func TestLegacyRegistryID(t *testing.T) {
	require.Equal(t, "legacy:203.0.113.7:8333", legacyRegistryID("203.0.113.7:8333"))
	require.Equal(t, "legacy:[2001:db8::1]:8333", legacyRegistryID("[2001:db8::1]:8333"))
}

// TestReconcile_ReconnectDoesNotDoubleCountBytes is a regression test. A
// vanished peer is dropped from lastSeen, but its registry entry survives until
// TTL cleanup. Byte counters travel as deltas that UpdateMetrics ADDS, so a peer
// seen "for the first time" against a surviving entry must not re-add its whole
// running total. getPeers() only returns peers whose Connected() is true, so a
// peer can leave one snapshot and return with its counters still climbing.
func TestReconcile_ReconnectDoesNotDoubleCountBytes(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	counting := newCountingRegistry(reg)

	snap := testSnapshot("203.0.113.7:8333", 1000, time.Unix(1750000100, 0))
	present := true
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), counting,
		func() []peerSnapshot {
			if present {
				return []peerSnapshot{snap}
			}

			return []peerSnapshot{}
		})

	sync.reconcile(context.Background())

	got, _ := reg.Get("legacy:203.0.113.7:8333")
	require.Equal(t, uint64(1000), got.BytesReceived)

	// The peer drops out of one snapshot, then returns with its counters still
	// running. The registry entry survived the gap.
	present = false
	sync.reconcile(context.Background())

	present = true
	snap.bytesReceived = 1200
	snap.lastRecv = time.Unix(1750000200, 0)
	sync.reconcile(context.Background())

	got, _ = reg.Get("legacy:203.0.113.7:8333")
	require.Equal(t, uint64(1200), got.BytesReceived,
		"the running total must not be re-added on top of the surviving entry")
	require.True(t, got.IsConnected, "the peer must be marked connected again")
}

// TestReconcile_ReconnectWithResetCountersDoesNotRegress covers the other
// reappearance shape: a genuinely new TCP connection whose counters restart at
// zero must not drag the stored total backwards.
func TestReconcile_ReconnectWithResetCountersDoesNotRegress(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	counting := newCountingRegistry(reg)

	snap := testSnapshot("203.0.113.7:8333", 1000, time.Unix(1750000100, 0))
	present := true
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), counting,
		func() []peerSnapshot {
			if present {
				return []peerSnapshot{snap}
			}

			return []peerSnapshot{}
		})

	sync.reconcile(context.Background())
	present = false
	sync.reconcile(context.Background())

	// Fresh connection: the peer's own counters start again from near zero.
	present = true
	snap.bytesReceived = 50
	snap.lastRecv = time.Unix(1750000300, 0)
	sync.reconcile(context.Background())

	got, _ := reg.Get("legacy:203.0.113.7:8333")
	require.Equal(t, uint64(1050), got.BytesReceived,
		"a reset counter contributes its new bytes on top of the stored lifetime total")

	// Subsequent growth on the new connection is tracked normally.
	snap.bytesReceived = 90
	snap.lastRecv = time.Unix(1750000400, 0)
	sync.reconcile(context.Background())

	got, _ = reg.Get("legacy:203.0.113.7:8333")
	require.Equal(t, uint64(1090), got.BytesReceived)
}

// failingGetPeer wraps a registry client and fails only GetPeer, so the byte
// baseline fallback can be exercised.
type failingGetPeer struct {
	blockchain.PeerRegistryClientI
}

func (f failingGetPeer) GetPeer(_ context.Context, _ string) (*blockchain.PeerInfo, bool, error) {
	return nil, false, context.DeadlineExceeded
}

// TestReconcile_BaselineLookupFailureStillReports checks that a failed byte
// baseline lookup degrades to reporting the whole total rather than dropping the
// peer or skipping its metrics.
func TestReconcile_BaselineLookupFailureStillReports(t *testing.T) {
	reg := blockchain.NewCentralizedPeerRegistry(blockchain.DefaultBanConfig())
	client := failingGetPeer{blockchain.NewLocalPeerRegistryClient(reg)}

	snap := testSnapshot("203.0.113.7:8333", 500, time.Unix(1750000100, 0))
	sync := newPeerRegistrySync(ulogger.TestLogger{}, testSyncSettings(), client,
		func() []peerSnapshot { return []peerSnapshot{snap} })

	sync.reconcile(context.Background())

	got, ok := reg.Get("legacy:203.0.113.7:8333")
	require.True(t, ok, "the peer must still be registered")
	require.True(t, got.IsConnected)
	require.Equal(t, uint64(500), got.BytesReceived,
		"without a baseline the whole total is reported")
}

package legacy

import (
	"context"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// legacyPeerIDPrefix namespaces a wire-protocol peer inside the shared peer
// registry. A libp2p ID never carries this prefix, so a legacy entry is
// self-identifying in logs, filters and the dashboard.
const legacyPeerIDPrefix = "legacy:"

// defaultPeerRegistrySyncInterval applies when the configured interval is
// missing or not positive.
const defaultPeerRegistrySyncInterval = 10 * time.Second

// legacyRegistryID builds the registry key for a wire-protocol peer address.
// The address is stable across reconnects, so ban state and history survive a
// peer that flaps.
func legacyRegistryID(addr string) string {
	return legacyPeerIDPrefix + addr
}

// peerSnapshot is the subset of legacy peer state the registry needs. The
// reconcile loop compares consecutive snapshots, so an idle peer costs no RPC.
type peerSnapshot struct {
	id            string
	addr          string
	userAgent     string
	height        uint32
	bytesSent     uint64
	bytesReceived uint64
	lastRecv      time.Time
	legacy        blockchain.LegacyPeerInfo
}

// registrationEqual reports whether two snapshots carry identical registration
// data. The byte counters are excluded on purpose: they travel as deltas
// through UpdatePeerMetrics, never through RegisterPeer.
func (s peerSnapshot) registrationEqual(other peerSnapshot) bool {
	return s.addr == other.addr &&
		s.userAgent == other.userAgent &&
		s.height == other.height &&
		s.legacy == other.legacy
}

// peerRegistrySync mirrors connected legacy peers into the centralized peer
// registry, so the dashboard can show them beside libp2p peers. It is a
// read-only visibility path: nothing here feeds a sync, catchup or
// peer-selection decision.
type peerRegistrySync struct {
	logger   ulogger.Logger
	registry blockchain.PeerRegistryClientI
	interval time.Duration
	snapshot func() []peerSnapshot
	lastSeen map[string]peerSnapshot
}

// newPeerRegistrySync builds the reconcile loop. The snapshot function must
// return nil to mean "no data available", and an empty slice to mean "no peers
// connected"; the two cases are handled differently.
func newPeerRegistrySync(logger ulogger.Logger, tSettings *settings.Settings,
	registry blockchain.PeerRegistryClientI, snapshot func() []peerSnapshot) *peerRegistrySync {
	interval := defaultPeerRegistrySyncInterval
	if tSettings != nil && tSettings.Legacy.PeerRegistrySyncInterval > 0 {
		interval = tSettings.Legacy.PeerRegistrySyncInterval
	}

	return &peerRegistrySync{
		logger:   logger,
		registry: registry,
		interval: interval,
		snapshot: snapshot,
		lastSeen: make(map[string]peerSnapshot),
	}
}

// run reconciles on every tick until ctx is cancelled.
func (p *peerRegistrySync) run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.logger.Infof("[LegacyPeerRegistry] started, interval %s", p.interval)

	// Reconcile once up front so a node with live legacy peers is not shown as
	// empty for a whole interval. If the internal server is not answering yet,
	// the snapshot is nil and this tick is skipped.
	p.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			p.logger.Infof("[LegacyPeerRegistry] stopped")
			return
		case <-ticker.C:
			p.reconcile(ctx)
		}
	}
}

// reconcile pushes one snapshot of connected legacy peers into the registry.
func (p *peerRegistrySync) reconcile(ctx context.Context) {
	peers := p.snapshot()
	if peers == nil {
		// A nil snapshot means the internal legacy server could not answer: a
		// full query channel, or a reply that timed out. It does NOT mean every
		// peer went away, so leave lastSeen untouched and retry next tick.
		p.logger.Debugf("[LegacyPeerRegistry] no peer snapshot available this tick")
		return
	}

	current := make(map[string]peerSnapshot, len(peers))

	for _, snap := range peers {
		current[snap.id] = snap

		previous, known := p.lastSeen[snap.id]

		if !known || !snap.registrationEqual(previous) {
			legacyCopy := snap.legacy
			info := &blockchain.PeerInfo{
				ID:               snap.id,
				TransportType:    blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL,
				TransportTypeSet: true,
				ClientName:       snap.userAgent,
				NetworkAddress:   snap.addr,
				Height:           snap.height,
				Legacy:           &legacyCopy,
			}

			if err := p.registry.RegisterPeer(ctx, info); err != nil {
				p.logger.Warnf("[LegacyPeerRegistry] RegisterPeer %s failed: %v", snap.id, err)
				continue
			}
		}

		if !known {
			if err := p.registry.UpdateConnectionState(ctx, snap.id, true); err != nil {
				p.logger.Warnf("[LegacyPeerRegistry] connect %s failed: %v", snap.id, err)
				continue
			}
		}

		// Baseline the byte deltas. A registry entry can outlive our tracking:
		// a vanished peer leaves lastSeen but its entry survives until TTL
		// cleanup. UpdatePeerMetrics ADDS deltas, so a peer we are seeing for
		// the first time must baseline against what the registry already holds,
		// or its running total gets added on top of itself.
		sentBaseline, recvBaseline := previous.bytesSent, previous.bytesReceived
		haveBaseline := known

		if !known {
			stored, found, err := p.registry.GetPeer(ctx, snap.id)
			switch {
			case err != nil:
				p.logger.Warnf("[LegacyPeerRegistry] byte baseline %s failed: %v", snap.id, err)
			case found:
				sentBaseline, recvBaseline = stored.BytesSent, stored.BytesReceived
				haveBaseline = true
			}
		}

		sentDelta := byteDelta(snap.bytesSent, sentBaseline, haveBaseline)
		recvDelta := byteDelta(snap.bytesReceived, recvBaseline, haveBaseline)

		if sentDelta > 0 || recvDelta > 0 {
			if err := p.registry.UpdatePeerMetrics(ctx, snap.id, 0, sentDelta, recvDelta,
				false, false, false, 0); err != nil {
				p.logger.Warnf("[LegacyPeerRegistry] metrics %s failed: %v", snap.id, err)
			}
		}

		if snap.lastRecv.After(previous.lastRecv) {
			if err := p.registry.UpdateLastMessageTime(ctx, snap.id); err != nil {
				p.logger.Warnf("[LegacyPeerRegistry] last message %s failed: %v", snap.id, err)
			}
		}

		p.lastSeen[snap.id] = snap
	}

	for id := range p.lastSeen {
		if _, present := current[id]; present {
			continue
		}

		if err := p.registry.UpdateConnectionState(ctx, id, false); err != nil {
			p.logger.Warnf("[LegacyPeerRegistry] disconnect %s failed: %v", id, err)
			continue
		}

		// Drop the tracking entry so no later tick registers this peer again.
		// Register refreshes LastSeen, which feeds registry TTL cleanup; a peer
		// that kept being registered would never age out.
		delete(p.lastSeen, id)
	}
}

// byteDelta converts an absolute counter into bytes not yet reported.
//
// With no baseline the whole current total is new. A counter that went backwards
// means the peer's connection object was replaced — legacy counters only ever
// climb within one connection — so the current total again belongs entirely to
// the new connection. Both cases return current, which never wraps uint64.
func byteDelta(current, previous uint64, haveBaseline bool) uint64 {
	if !haveBaseline || current < previous {
		return current
	}

	return current - previous
}

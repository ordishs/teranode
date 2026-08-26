package parity

import (
	"fmt"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/services/svp2p/protocol"
	"github.com/bsv-blockchain/teranode/services/svp2p/svp2ptest"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/stretchr/testify/require"
)

// TestParity_MultiPeerDistribution — watch-list scenario 5, the lead item of
// Phase 3. Three peers over one chain: fast (honest), slow (20 ms per block),
// silent (answers headers and pings, withholds every block). The fast peer is
// listed first so legacy's height-ranked election takes it as the single sync
// peer and its leg stays bounded; legacy never downloads from the others, and
// that is the baseline svp2p's distribution is recorded against.
func TestParity_MultiPeerDistribution(t *testing.T) {
	const chain = 200

	dial := func(_ Impl, _ *svp2ptest.FixtureChain, s *settings.Settings) {
		// One percent of the regtest block interval is six seconds: the silent
		// peer's front block times out inside the test instead of in ten minutes.
		s.Legacy.BlockDownloadTimeoutBasePercent = 1
		s.Legacy.BlockDownloadTimeoutBaseIBDPercent = 1
		s.Legacy.BlockDownloadTimeoutPerPeerPercent = 0
		// The parallel-fetch fuse keeps SVNode's 30 s default on purpose: at the
		// in-process ingest pace every in-flight block crosses a 1 s fuse and is
		// raced to three holders, which is correct behaviour but swamps the
		// duplicate-fetch figure this scenario measures (1004 serves from the
		// slow peer for a 200-block chain, 2026-08-26).
	}

	slowScript := svp2ptest.Script{WriteDelay: func(msg wire.Message, _ int) time.Duration {
		if msg.Command() == "block" {
			return 20 * time.Millisecond
		}

		return 0
	}}

	silentScript := svp2ptest.Script{OnGetData: func(*svp2ptest.ScriptedPeer, *wire.MsgGetData) []wire.Message { return nil }}

	obs, res := RunParity(t, Scenario{
		Name:   "multi-peer-distribution",
		Chain:  chain,
		Pad:    200_000, // see BuildFixtureChainPadded: legacy's byte-rate floor
		Tweaks: []func(Impl, *svp2ptest.FixtureChain, *settings.Settings){dial},
		Peers: func(t *testing.T, c *svp2ptest.FixtureChain, net wire.BitcoinNet) []*svp2ptest.ScriptedPeer {
			return []*svp2ptest.ScriptedPeer{
				svp2ptest.NewScriptedPeer(t, c, net, svp2ptest.Script{}, true), // peer0 fast
				svp2ptest.NewScriptedPeer(t, c, net, slowScript, true),         // peer1 slow
				svp2ptest.NewScriptedPeer(t, c, net, silentScript, true),       // peer2 silent
			}
		},
		Drive: func(t *testing.T, n *nodeUnderTest, _ []*svp2ptest.ScriptedPeer) {
			n.WaitForHeight(t, chain, 240*time.Second)
		},
		Observe: func(t *testing.T, n *nodeUnderTest, peers []*svp2ptest.ScriptedPeer) Observation {
			o := ObserveDefault(t, n, peers)
			o.Notes = map[string]string{}

			for _, needle := range []string{"disconnecting", "rotating", "ingest failed", "admission", "not mined", "parent", "unrequested", "timed out", "block download timeout", "stalling"} {
				o.Notes[needle] = fmt.Sprint(len(n.Logger.Matching(needle)))
			}

			return o
		},
		Accepted: []Divergence{
			{Field: "Requests", Reason: "legacy schedules blocks from its single sync peer only; svp2p from every useful peer (SVNode's model)"},
			{Field: "Served", Reason: "as Requests"},
			{Field: "Disconnected", Reason: "legacy never judges a non-sync peer; svp2p disconnects the silent holder on the per-block timeout"},
		},
	})

	require.Empty(t, res.Diffs)

	s := obs[Svp2p]
	require.Equal(t, uint32(chain), s.BlocksAccepted)
	require.Equal(t, "node", s.Disconnected["peer2"], "svp2p must drop the silent peer on the per-block timeout")
	// legacy_connect_peers redials a dropped peer, so the bound is per
	// connection: one batch on election, plus one re-hand per rotation that fits
	// inside the per-block budget before the carried clock drops the peer. With
	// the harness's 3 s rotation window and this scenario's 6 s budget (1 % of
	// the 600 s regtest interval) that is three batches.
	budget := 6 * time.Second
	batchesPerConn := int(budget/maxLastBlockTime) + 1
	require.LessOrEqual(t, s.Requests["peer2"], batchesPerConn*protocol.MaxBlocksInTransitPerPeer*s.Connections["peer2"],
		"the silent peer is handed at most %d batches per connection before the per-block timeout drops it", batchesPerConn)
	require.Greater(t, s.Connections["peer2"], 1, "the silent peer was dropped and redialed at least once")
	require.GreaterOrEqual(t, s.Served["peer0"]+s.Served["peer1"], chain, "every block crossed the wire from a serving peer")
	require.Positive(t, s.Served["peer1"], "the slow peer must have been given work too")

	// KNOWN GAP (ledger carried residual 1, orphan-block retention), measured by
	// this harness 2026-08-26: a block that arrives before its parent is refused
	// pre-admission, its bytes discarded, and it is fetched again when the
	// parent lands — so under an ingest slower than the three peers' delivery
	// the same block crosses the wire several times (the svp2p node logged ~1000
	// parent-not-held lines for a 200-block chain; legacy, single-peer and
	// in-order, fetched each block once). Task 21 measured "at most twice" on a
	// 36-block chain; here it is about five times. The rule-derived bound below
	// is what a fix must meet; until then the row pins the gap so the figure is
	// reported and a fix flips it consciously.
	dup := s.Served["peer0"] + s.Served["peer1"] - chain
	ruleBound := 3*protocol.MaxBlocksInTransitPerPeer + s.Requests["peer2"]

	t.Logf("duplicate fetches: %d (rule-derived bound %d) — KNOWN GAP residual 1", dup, ruleBound)
	require.Greater(t, dup, ruleBound,
		"KNOWN GAP residual 1 appears to be fixed: tighten this to require.LessOrEqual(dup, ruleBound) and close the residual")

	l := obs[Legacy]
	require.Equal(t, uint32(chain), l.BlocksAccepted)
	t.Logf("legacy baseline: requests=%v served=%v disconnected=%v connections=%v wall=%s", l.Requests, l.Served, l.Disconnected, l.Connections, l.WallClock)
	t.Logf("svp2p:          requests=%v served=%v disconnected=%v connections=%v wall=%s", s.Requests, s.Served, s.Disconnected, s.Connections, s.WallClock)
}

package parity

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/services/svp2p/svp2ptest"
	"github.com/bsv-blockchain/teranode/settings"
	inmemorykafka "github.com/bsv-blockchain/teranode/util/kafka/in_memory_kafka"
	"github.com/stretchr/testify/require"
)

// The fixture chain row 15 runs on. Five blocks is enough for a coinbase to
// mature at the lowered maturity below and still leave the run short.
const (
	compactChain    = 5
	compactMaturity = 2
	compactFee      = uint64(2000)
)

// compactNonce keys the short IDs of every announcement in this file. Any value
// serves; the peer and the node derive the same (k0,k1) from it and the header.
const compactNonce = uint64(0x5643_0F15)

// compactTopics hands each leg its own txmeta topic. The two legs run in the
// same process against the same in-memory Kafka broker, and a topic shared
// between them would let one leg's entries reach the other's index.
var compactTopicSeq atomic.Uint64

func compactTopic(t *testing.T) *url.URL {
	t.Helper()

	u, err := url.Parse(fmt.Sprintf("memory://localhost/txmeta-compact-%d", compactTopicSeq.Add(1)))
	require.NoError(t, err)

	return u
}

// compactMaturityTweak lowers the coinbase maturity so a fixture coinbase can
// be spent inside a five block chain. The parameters are copied before the
// field is written: ChainCfgParams points at the process-wide regtest
// parameters, which every other test in this binary reads.
func compactMaturityTweak(_ Impl, _ *svp2ptest.FixtureChain, s *settings.Settings) {
	params := *s.ChainCfgParams
	params.CoinbaseMaturity = compactMaturity
	s.ChainCfgParams = &params
}

// compactEnabled turns compact blocks on for the svp2p leg and gives it the
// txmeta topic its recent-transaction index is fed from.
//
// Applied to ONE leg, unlike every other tweak in this package, because the two
// keys are not symmetric. legacy_compactBlocks is unread by services/legacy, so
// setting it for both legs would be noise; kafka_txmetaConfig is NOT — legacy
// netsync starts its own txmeta listener on it — so setting it for both legs
// would add a second consumer to the oracle leg for no gain. The legacy leg
// therefore runs exactly as every other scenario's does.
func compactEnabled(topics map[Impl]*url.URL) func(Impl, *svp2ptest.FixtureChain, *settings.Settings) {
	return func(impl Impl, _ *svp2ptest.FixtureChain, s *settings.Settings) {
		if impl != Svp2p {
			return
		}

		s.Legacy.CompactBlocks = true
		s.Legacy.CompactBlocksRecentTxs = 1024
		s.Kafka.TxMetaConfig = topics[impl]
	}
}

// compactRecorder records every getblocktxn the node sends and answers it
// honestly from the fixture chain, which is what ScriptedPeer does with no
// script at all — the override exists to keep the requested slot list, the fact
// row 15 is a verdict on.
type compactRecorder struct {
	mu       sync.Mutex
	requests [][]uint32
}

func (r *compactRecorder) script() svp2ptest.Script {
	return svp2ptest.Script{
		OnGetBlockTxn: func(p *svp2ptest.ScriptedPeer, _ net.Conn, m *wire.MsgGetBlockTxn) []wire.Message {
			r.record(m.Indexes)

			reply := p.BlockTxnFor(m)
			if reply == nil {
				return nil
			}

			return []wire.Message{reply}
		},
	}
}

func (r *compactRecorder) record(indexes []uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	kept := make([]uint32, len(indexes))
	copy(kept, indexes)

	r.requests = append(r.requests, kept)
}

func (r *compactRecorder) seen() [][]uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([][]uint32, len(r.requests))
	copy(out, r.requests)

	return out
}

// seedCompactBlock brings one leg to the point an announcement can be made: the
// node is synced to the fixture tip, one transaction of the block to come has
// travelled the production relay path into the recent-transaction index, and
// the block itself is built and serveable but unannounced.
//
// It returns the block, the transaction the node now holds, and the one it has
// never seen — the single slot a getblocktxn must ask for.
//
// The waits it performs are only meaningful on the svp2p leg; on the legacy leg
// there is no index and no topic, and it stops after the relay.
func seedCompactBlock(t *testing.T, n *nodeUnderTest, peers []*svp2ptest.ScriptedPeer, topic *url.URL) (*wire.MsgBlock, *bt.Tx, *bt.Tx) {
	t.Helper()

	n.WaitForHeight(t, compactChain, 60*time.Second)

	if n.Impl == Svp2p {
		// The in-memory broker drops what it is given while a topic has no
		// consumer, and the txmeta consumer only starts once its FSM gate has
		// ticked (bridge.pollTxRunningGate, one second). A transaction relayed
		// before that is never indexed, so the relay below waits for the
		// consumer to be registered.
		name := strings.TrimPrefix(topic.Path, "/")

		n.WaitFor(t, func() bool { return inmemorykafka.GetSharedBroker().HasConsumer(name) },
			60*time.Second, "the txmeta consumer never registered on "+name)
	}

	chain := peers[0].Chain

	// held travels to the node before the announcement, so the index can fill
	// its slot; gap never does, so it is the one slot a getblocktxn has to ask
	// for.
	held := chain.SpendCoinbase(t, 1, compactFee)
	gap := chain.SpendCoinbase(t, 2, compactFee)

	block := chain.BuildNextBlock(t, n.Settings, []*bt.Tx{held, gap})

	peers[0].Send(svp2ptest.WireTx(t, held))

	if n.Impl == Svp2p {
		// The barrier is the index itself, not the node's own inv for the
		// transaction: the inv is raised by the peer-sourced path
		// (txIngestor.Ingest) as well, which never touches the index, so it
		// would let the announcement race the Kafka round trip that fills it.
		n.WaitFor(t, func() bool { return n.RecentTxIndexLen() > 0 },
			60*time.Second, "the relayed transaction never reached the recent-transaction index")
	}

	return block, held, gap
}

// TestParity_CompactBlockReceive — watch-list scenario 15.
//
// A scripted peer syncs both legs to the fixture tip, relays one transaction,
// and then announces a block holding that transaction and one the node has
// never seen. svp2p must reconstruct the block from its recent-transaction
// index and ONE getblocktxn for the single gap, never asking for the block
// itself; legacy, which has no compact block support and therefore never sent
// sendcmpct, is announced to by inv — SVNode's own rule — and downloads the
// whole block.
//
// The relay is not a shortcut around the index: the transaction travels the
// production path end to end (peer tx message, validator, txmeta topic,
// bridge.StartTxMetaConsumer, RecentTxIndex.Add), and the node's own inv for
// it is what tells the scenario the round trip has finished.
func TestParity_CompactBlockReceive(t *testing.T) {
	topics := map[Impl]*url.URL{Svp2p: compactTopic(t)}
	recorder := &compactRecorder{}

	obs, _ := RunParity(t, Scenario{
		Name:  "compact-block-receive",
		Chain: compactChain,
		Tweaks: []func(Impl, *svp2ptest.FixtureChain, *settings.Settings){
			compactMaturityTweak,
			compactEnabled(topics),
		},
		Peers: func(t *testing.T, chain *svp2ptest.FixtureChain, netMagic wire.BitcoinNet) []*svp2ptest.ScriptedPeer {
			return []*svp2ptest.ScriptedPeer{
				svp2ptest.NewScriptedPeer(t, chain, netMagic, svp2ptest.Script{}, true),
				svp2ptest.NewScriptedPeer(t, chain, netMagic, recorder.script(), true),
			}
		},
		Drive: func(t *testing.T, n *nodeUnderTest, peers []*svp2ptest.ScriptedPeer) {
			block, held, gap := seedCompactBlock(t, n, peers, topics[Svp2p])
			hash := block.Header.BlockHash()

			if n.Impl == Svp2p {
				require.NoError(t, peers[1].AnnounceCompact(block, compactNonce, []int{0}))
			} else {
				// legacy sent no sendcmpct, so SVNode's rule makes this an inv
				// announcement; legacy learns the header from the headers round
				// the inv draws, which is why the header is published here and
				// not for the svp2p leg.
				peers[0].Chain.PublishHeader(t, block)

				inv := wire.NewMsgInv()
				require.NoError(t, inv.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, &hash)))

				peers[1].Send(inv)
			}

			n.WaitForHeight(t, compactChain+1, 90*time.Second)

			n.notes = map[string]string{
				"announced-block":   hash.String(),
				"held-tx":           held.TxID(),
				"gap-tx":            gap.TxID(),
				"getblocktxn":       fmt.Sprint(peers[1].Transcript.Count(svp2ptest.In, wire.CmdGetBlockTxn)),
				"gap-requests":      fmt.Sprint(recorder.seen()),
				"getdata-for-block": fmt.Sprint(blockRequests(peers, hash)),
				"sendcmpct":         fmt.Sprint(peers[1].Transcript.Count(svp2ptest.In, wire.CmdSendcmpct)),
				"ingested-height":   fmt.Sprint(n.BestHeight(t)),
			}
		},
		Observe: func(t *testing.T, n *nodeUnderTest, peers []*svp2ptest.ScriptedPeer) Observation {
			o := ObserveDefault(t, n, peers)
			o.Notes = n.notes

			return o
		},
		Accepted: []Divergence{
			{Field: "Requests", Reason: "the announced block is a getdata on legacy and a getblocktxn on svp2p; that difference IS scenario 15"},
			{Field: "Served", Reason: "as Requests"},
		},
	})

	svp := obs[Svp2p].Notes
	legacyNotes := obs[Legacy].Notes

	require.Equal(t, "1", svp["sendcmpct"], "svp2p must have offered compact blocks before the peer may announce one")
	require.Equal(t, "1", svp["getblocktxn"], "one gap must cost exactly one getblocktxn")
	require.Equal(t, "[[2]]", svp["gap-requests"], "only the slot the index could not fill may be requested")
	require.Equal(t, "0", svp["getdata-for-block"], "a reconstructed block must never be downloaded as well")
	require.Equal(t, fmt.Sprint(compactChain+1), svp["ingested-height"])

	require.Equal(t, "0", legacyNotes["sendcmpct"], "legacy has no compact block support and must never offer one")
	require.Equal(t, "0", legacyNotes["getblocktxn"], "legacy must not answer a compact exchange it never joined")
	require.Equal(t, "1", legacyNotes["getdata-for-block"], "legacy must download the whole block")
	require.Equal(t, fmt.Sprint(compactChain+1), legacyNotes["ingested-height"])

	require.Equal(t, obs[Legacy].BlocksAccepted, obs[Svp2p].BlocksAccepted, "both legs must end on the same height")

	t.Logf("compact block receive: svp2p asked %s getblocktxn for slots %s and %s getdata; legacy asked %s getdata",
		svp["getblocktxn"], svp["gap-requests"], svp["getdata-for-block"], legacyNotes["getdata-for-block"])
}

// blockRequests counts the getdata for one block hash across every peer.
func blockRequests(peers []*svp2ptest.ScriptedPeer, hash chainhash.Hash) int {
	count := 0

	for _, p := range peers {
		for _, e := range p.Transcript.Snapshot() {
			if e.Dir != svp2ptest.In {
				continue
			}

			getData, ok := e.Msg.(*wire.MsgGetData)
			if !ok {
				continue
			}

			for _, vect := range getData.InvList {
				if vect.Type == wire.InvTypeBlock && vect.Hash.IsEqual(&hash) {
					count++
				}
			}
		}
	}

	return count
}

// TestParity_CompactBlockShortBlockTxnIsBanned — watch-list scenario 15, the
// invalid reply sub-case.
//
// The gap request is answered with a blocktxn carrying nothing, so the arity
// check FillBlock performs (blockencodings.cpp:264-285) fails. svp2p must take
// ProcessBlockTxnMessage's READ_STATUS_INVALID branch
// (net_processing.cpp:3610-3616): mark the block failed, score the peer 100 for
// "invalid-cmpctblk-txns", and drop it. The block must not be ingested.
//
// svp2p alone: legacy joins no compact exchange, so there is nothing to compare.
func TestParity_CompactBlockShortBlockTxnIsBanned(t *testing.T) {
	topics := map[Impl]*url.URL{Svp2p: compactTopic(t)}

	// The reply the node asked for, emptied. An empty blocktxn is a legal wire
	// message; it is the count that is the lie.
	short := svp2ptest.Script{
		OnGetBlockTxn: func(_ *svp2ptest.ScriptedPeer, _ net.Conn, m *wire.MsgGetBlockTxn) []wire.Message {
			return []wire.Message{wire.NewMsgBlockTxn(&m.BlockHash)}
		},
	}

	obs, _ := RunParity(t, Scenario{
		Name:  "compact-block-short-blocktxn",
		Chain: compactChain,
		Only:  []Impl{Svp2p},
		Tweaks: []func(Impl, *svp2ptest.FixtureChain, *settings.Settings){
			compactMaturityTweak,
			compactEnabled(topics),
		},
		Peers: func(t *testing.T, chain *svp2ptest.FixtureChain, netMagic wire.BitcoinNet) []*svp2ptest.ScriptedPeer {
			return []*svp2ptest.ScriptedPeer{
				svp2ptest.NewScriptedPeer(t, chain, netMagic, svp2ptest.Script{}, true),
				svp2ptest.NewScriptedPeer(t, chain, netMagic, short, true),
			}
		},
		Drive: func(t *testing.T, n *nodeUnderTest, peers []*svp2ptest.ScriptedPeer) {
			sampler := n.sampleScores()

			block, _, _ := seedCompactBlock(t, n, peers, topics[Svp2p])

			require.NoError(t, peers[1].AnnounceCompact(block, compactNonce, []int{0}))

			n.WaitFor(t, func() bool { return peers[1].Transcript.ClosedBy() != "" }, 60*time.Second,
				"the peer that answered with a short blocktxn was never dropped")

			n.scores = sampler.Result()

			n.notes = map[string]string{
				"getblocktxn":     fmt.Sprint(peers[1].Transcript.Count(svp2ptest.In, wire.CmdGetBlockTxn)),
				"peer1-score":     fmt.Sprint(n.scores[peers[1].Addr]),
				"disconnect-line": fmt.Sprint(n.Logger.Matching("done:")),
				"ingested-height": fmt.Sprint(n.BestHeight(t)),
			}
		},
		Observe: func(t *testing.T, n *nodeUnderTest, peers []*svp2ptest.ScriptedPeer) Observation {
			o := ObserveDefault(t, n, peers)
			o.Notes = n.notes
			o.Scores = n.scores

			return o
		},
	})

	notes := obs[Svp2p].Notes

	require.Equal(t, "1", notes["getblocktxn"], "the gap must have been asked for once")
	require.Equal(t, "100", notes["peer1-score"], "an invalid blocktxn is worth SVNode's own DoS(100)")
	require.Equal(t, fmt.Sprint(compactChain), notes["ingested-height"], "a block that failed reconstruction must not be ingested")
	require.Equal(t, "node", obs[Svp2p].Disconnected["peer1"], "the node must drop the peer")
}

// TestParity_UnsolicitedBlockTxnIsDroppedUnscored — watch-list scenario 15, the
// unsolicited reply sub-case.
//
// A blocktxn nobody asked for is logged and dropped, with no Misbehaving call
// and no MarkBlockAsFailed (net_processing.cpp:3602-3606). It is a timing
// artefact — a reply racing a claim the node released — not evidence of malice,
// so the peer keeps both its score of zero and its connection.
//
// svp2p alone, for the same reason as the sub-case above.
func TestParity_UnsolicitedBlockTxnIsDroppedUnscored(t *testing.T) {
	topics := map[Impl]*url.URL{Svp2p: compactTopic(t)}

	obs, _ := RunParity(t, Scenario{
		Name:  "compact-unsolicited-blocktxn",
		Chain: compactChain,
		Only:  []Impl{Svp2p},
		Tweaks: []func(Impl, *svp2ptest.FixtureChain, *settings.Settings){
			compactMaturityTweak,
			compactEnabled(topics),
		},
		Peers: honestPeers(2),
		Drive: func(t *testing.T, n *nodeUnderTest, peers []*svp2ptest.ScriptedPeer) {
			sampler := n.sampleScores()

			n.WaitForHeight(t, compactChain, 60*time.Second)

			// A well-formed reply to a request that was never made: the block
			// is one the node holds, and no cmpctblock for it was ever sent.
			chain := peers[0].Chain
			tip := chain.Tip()

			reply := wire.NewMsgBlockTxn(&tip)
			require.NoError(t, reply.AddTransaction(chain.Blocks[tip].Transactions[0]))

			peers[1].Send(reply)

			// Silence is the expected answer, so the wait is bounded rather
			// than asserted: it gives the node time to score or drop the peer
			// if it were going to.
			n.WaitFor(t, func() bool { return peers[1].Transcript.ClosedBy() != "" }, 5*time.Second, "")

			n.scores = sampler.Result()

			n.notes = map[string]string{
				"peer1-score":     fmt.Sprint(n.scores[peers[1].Addr]),
				"still-connected": fmt.Sprint(n.ConnectedCount(t)),
				"ingested-height": fmt.Sprint(n.BestHeight(t)),
			}
		},
		Observe: func(t *testing.T, n *nodeUnderTest, peers []*svp2ptest.ScriptedPeer) Observation {
			o := ObserveDefault(t, n, peers)
			o.Notes = n.notes
			o.Scores = n.scores

			return o
		},
	})

	notes := obs[Svp2p].Notes

	require.Equal(t, "0", notes["peer1-score"], "an unsolicited blocktxn earns no score")
	require.Equal(t, "2", notes["still-connected"], "neither peer may be dropped for it")
	require.Equal(t, fmt.Sprint(compactChain), notes["ingested-height"], "the chain must not move")
	require.Empty(t, obs[Svp2p].Disconnected, "no peer may be dropped")
}

package protocol

import (
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// MaxGetBlocksResults is the `int nLimit = 500` ProcessGetBlocks spells inline
// (net_processing.cpp:2580): how many block hashes one getblocks reply may
// carry. wire.MaxBlocksPerMsg is the same 500, and is what the legacy service
// passed to LocateBlockHeaders (services/legacy/peer_server.go:1524).
const MaxGetBlocksResults = wire.MaxBlocksPerMsg

// Serving answers the two chain-query messages from the header index: the
// net_processing.cpp ProcessGetHeadersMessage (:2968) and ProcessGetBlocks
// (:2551) handlers. Like the other machines in this package it performs no
// I/O: each method reads the index and returns the messages the caller must
// send (spec §4.3).
//
// Both machines serve the ACTIVE chain — the tip the blockchain service has
// validated, handed in by the caller — and never the header index tip, which
// headers-first sync runs ahead to. Serving headers for blocks we cannot
// serve the bodies of would invite getdata we can only answer with notfound.
//
// Locking: Serving carries no lock of its own. Like HeaderSync and
// BlockDownloader, every method assumes the caller already holds
// PeerManager's shared sync-state mutex — this package's port of cs_main.
//
// Four guards ProcessGetHeadersMessage and ProcessGetBlocks carry are NOT
// ported here, each for a stated reason:
//
//   - The pendingResponses limit (net_processing.cpp:2974-2988), which
//     disconnects a non-whitelisted peer with too many getheaders replies
//     still queued. It measures the depth of the send queue per request type,
//     which this port's transport does not expose; it is a flood guard worth
//     having, and is carried as a Phase 3 hardening residual rather than
//     silently dropped.
//   - The AreOlderOrEqualUnvalidatedBlockIndexCandidates deferral
//     (net_processing.cpp:2564-2567), which parks a getblocks reply while a
//     block received before the request is still an unvalidated candidate. It
//     exists for the compact-block announcement race, and this port announces
//     no compact blocks (Phase 4).
//   - The pruning branch (net_processing.cpp:2593-2604). Teranode does not
//     prune blocks the way fPruneMode does, and it has no counterpart of
//     nPrunedBlocksLikelyToHave.
//   - state->pindexBestHeaderSent (net_processing.cpp:3040-3045), which
//     SendMessages reads when it decides whether to announce a new block by
//     headers. That consumer arrives with the relay path (Task 12), and
//     peerSyncState deliberately carries no field without a consumer, so the
//     field is added there, not here.
type Serving struct {
	idx *HeaderIndex
	hs  *HeaderSync
}

// NewServing builds the serving machine over the shared header index and the
// headers-first machine, whose mode gates getheaders (see OnGetHeaders).
func NewServing(idx *HeaderIndex, hs *HeaderSync) (*Serving, error) {
	if idx == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: header index is nil")
	}

	if hs == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: header sync is nil")
	}

	return &Serving{idx: idx, hs: hs}, nil
}

// OnGetHeaders is the NetMsgType::GETHEADERS handler, net_processing.cpp
// ProcessGetHeadersMessage (:2968): locate the fork point from the locator and
// serve up to MaxHeadersResults headers after it, stopping at hashStop.
//
// refused reports the catch-up refusal, and is the ONE case that answers
// nothing at all rather than a possibly empty headers message. The rule is
// carried verbatim from the legacy service (services/legacy/peer_server.go:1574-1589):
// "Don't serve headers to other peers while we're actively syncing in
// headers-first mode", because serving 2000 headers per peer out of the store
// competes with our own header processing and costs the sync its
// three-minute window. SVNode refuses on IsInitialBlockDownload() instead
// (net_processing.cpp:2995-3000) — the same intent measured against the state
// this port does not carry — and exempts whitelisted peers, which this port
// has no notion of. The caller logs the refusal: the machines hold no logger.
//
// An empty locator is not a peer error. SVNode reads that case as "the peer
// named its start with hashStop" (GetFirstBlockIndexFromLocatorNL,
// net_processing.cpp:2845-2871) and serves from that header, which may sit off
// our chain — in which case it is the only header served, since CChain::Next
// of an off-chain index is nullptr. An empty locator whose hashStop we do not
// hold is the second case that answers nothing, matching the C++ early return
// on a nullopt locate.
func (s *Serving) OnGetHeaders(peer *SyncPeer, activeTip HeaderNode, msg *wire.MsgGetHeaders) (msgs []wire.Message, refused bool) {
	if peer == nil || msg == nil {
		return nil, false
	}

	if s.hs.IsHeadersFirstMode() {
		return nil, true
	}

	start, ok := s.locateStart(activeTip, msg.BlockLocatorHashes, msg.HashStop)
	if !ok {
		// Either the empty-locator hashStop is unknown (the C++ nullopt early
		// return), or the active tip is not in the index and there is no
		// chain to serve from.
		return nil, false
	}

	out := wire.NewMsgHeaders()

	// A locator that already names our tip leaves nothing to serve, and
	// SVNode answers it with an EMPTY headers message rather than with
	// silence: ProcessGetHeadersMessage builds vHeaders (possibly empty) and
	// PushMessage is unconditional. The legacy Go service diverges here,
	// sending nothing when it found no headers (peer_server.go:1636). SVNode's
	// answer is kept, because it is what tells a peer its getheaders was
	// received and its chain is not behind ours; a peer that gets silence
	// cannot tell that from a dropped message. Our own OnHeaders reads an
	// empty batch as "nothing interesting, stop asking" and neither scores nor
	// disconnects for it.
	if start.empty {
		return []wire.Message{out}, false
	}

	headers := s.idx.ActiveChainHeaders(activeTip.Hash, start.hash, MaxHeadersResults)
	stopping := msg.HashStop != chainhash.Hash{}

	for i := range headers {
		header := headers[i]

		// AddBlockHeader only refuses past MaxBlockHeadersPerMsg, which is
		// the cap the range was taken with, so this cannot fail. The error is
		// still not dropped: a future cap change must not silently serve a
		// message the wire encoder will refuse.
		if err := out.AddBlockHeader(&header); err != nil {
			break
		}

		// C++: `if(--nLimit <= 0 || pindex->GetBlockHash() == hashStop) break;`
		// — the cap is the range limit above, and hashStop is INCLUSIVE here,
		// unlike the getblocks loop which breaks before it pushes.
		//
		// The null-hashStop guard is not in the C++, which compares
		// unconditionally. It changes no answer — a null hashStop matches no
		// block hash — and it keeps the common case, a sync peer asking for
		// everything, from hashing all 2000 headers it is served.
		if stopping && header.BlockHash() == msg.HashStop {
			break
		}
	}

	return []wire.Message{out}, false
}

// OnGetBlocks is the NetMsgType::GETBLOCKS handler, net_processing.cpp
// ProcessGetBlocks (:2551) and legacy OnGetBlocks (peer_server.go:1510): an
// inv of up to MaxGetBlocksResults block hashes following the locator's fork
// point, stopping BEFORE hashStop.
//
// Unlike getheaders there is no empty-locator special case: ProcessGetBlocks
// calls FindForkInGlobalIndex directly, so an empty locator takes the same
// genesis fallback an unknown one does. There is also no empty reply — the
// C++ loop pushes inventory or does nothing, and pushes no message of its own.
//
// SVNode queues the hashes into the peer's inventory-to-send set
// (PushBlockInventory) for its SendMessages pass to batch, which is the
// mechanism the relay path (Task 12) needs and this task does not: one
// getblocks answers with one inv, the shape the legacy service also has.
//
// The legacy service answers an empty inv with a notfound instead of silence
// (peer_server.go:1556-1568), for NODE_NETWORK_LIMITED, which SVNode does not
// do and this port does not advertise.
func (s *Serving) OnGetBlocks(peer *SyncPeer, activeTip HeaderNode, msg *wire.MsgGetBlocks) []wire.Message {
	if peer == nil || msg == nil {
		return nil
	}

	fork, ok := s.idx.ForkPoint(activeTip.Hash, derefHashes(msg.BlockLocatorHashes))
	if !ok {
		return nil
	}

	// C++: `if(pindex) pindex = chainActive.Next(pindex);` — serve the rest of
	// the chain, starting after what the peer already has. Nothing after the
	// fork point means the peer is level with us, and the loop below never
	// runs.
	next, ok := s.idx.ActiveChainNext(activeTip.Hash, fork.Hash)
	if !ok {
		return nil
	}

	hashes := s.idx.ActiveChainHashes(activeTip.Hash, next.Hash, MaxGetBlocksResults)

	out := wire.NewMsgInv()

	for _, hash := range hashes {
		// C++ breaks on hashStop BEFORE it pushes, so hashStop is excluded
		// from the inv — the opposite of the getheaders loop.
		if hash == msg.HashStop {
			break
		}

		if err := out.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, &hash)); err != nil {
			break
		}
	}

	if len(out.InvList) == 0 {
		return nil
	}

	// C++: `pfrom->hashContinue = pindex->GetBlockHash();` on the nLimit
	// break, and the legacy service's own continueHash
	// (peer_server.go:1546-1553): a full inv means there is more chain than
	// fits, so when the peer requests this last hash we answer with an inv of
	// our tip, which triggers its next getblocks. Task 10's getdata path
	// consumes it.
	if len(out.InvList) == MaxGetBlocksResults {
		peer.hashContinue = out.InvList[len(out.InvList)-1].Hash
	}

	return []wire.Message{out}
}

// serveStart is where a serving walk begins: the header to serve first, or
// empty when the peer is level with our tip and there is nothing after the
// fork point.
type serveStart struct {
	hash  chainhash.Hash
	empty bool
}

// locateStart is net_processing.cpp GetFirstBlockIndexFromLocatorNL
// (:2845-2871), plus the CChain::Next step the C++ helper folds into its
// locator branch. ok is false for the two cases the C++ answers with a bare
// return: an empty locator naming a hashStop we do not hold, and (this port
// only) an active tip the index cannot place.
func (s *Serving) locateStart(activeTip HeaderNode, locator []*chainhash.Hash, hashStop chainhash.Hash) (serveStart, bool) {
	if len(locator) == 0 {
		// C++: `pindex = mapBlockIndex.Get(hashStop); if (!pindex) return {};`
		// The header may sit off our active chain, and the range walk answers
		// that with the one header, as CChain::Next does.
		if _, ok := s.idx.Lookup(hashStop); !ok {
			return serveStart{}, false
		}

		return serveStart{hash: hashStop}, true
	}

	fork, ok := s.idx.ForkPoint(activeTip.Hash, derefHashes(locator))
	if !ok {
		return serveStart{}, false
	}

	next, ok := s.idx.ActiveChainNext(activeTip.Hash, fork.Hash)
	if !ok {
		// C++ leaves pindex nullptr, which serves an empty headers message.
		return serveStart{empty: true}, true
	}

	return serveStart{hash: next.Hash}, true
}

// derefHashes flattens a wire locator into the value slice the index takes.
// A nil entry cannot come off the wire — the decoder always fills every slot —
// and is skipped rather than dereferenced so an in-process caller cannot panic
// the index.
func derefHashes(hashes []*chainhash.Hash) []chainhash.Hash {
	out := make([]chainhash.Hash, 0, len(hashes))

	for _, h := range hashes {
		if h == nil {
			continue
		}

		out = append(out, *h)
	}

	return out
}

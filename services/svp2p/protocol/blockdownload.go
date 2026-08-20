package protocol

import (
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-wire"
	"github.com/bsv-blockchain/teranode/errors"
)

// BlockDownloadWindow mirrors validation.h DEFAULT_BLOCK_DOWNLOAD_WINDOW
// (1024): how far past the last block we have in common with a peer we are
// willing to schedule downloads. SVNode makes it configurable with
// -blockdownloadwindow; Phase 2 carries the default as a constant.
const BlockDownloadWindow = 1024

// MaxBlocksInTransitPerPeer mirrors validation.h
// MAX_BLOCKS_IN_TRANSIT_PER_PEER (16): the number of blocks we may have in
// flight from one peer at a time.
const MaxBlocksInTransitPerPeer = 16

// BlockStallingTimeout mirrors validation.h DEFAULT_BLOCK_STALLING_TIMEOUT
// (10 seconds): how long a peer may hold the head of the download window
// before we disconnect it. SVNode makes it configurable with
// -blockstallingtimeout; Phase 2 carries the default as a constant.
const BlockStallingTimeout = 10 * time.Second

// MaxLastBlockTime carries legacy netsync manager.go maxLastBlockTime
// (60 * 3 seconds): how long the sync peer may make no progress before
// Teranode rotates it. This is not an SVNode rule — see the source note on
// CheckStall.
const MaxLastBlockTime = 180 * time.Second

// BlockDownloadTimeoutBase mirrors validation.h
// DEFAULT_BLOCK_DOWNLOAD_TIMEOUT_BASE (100): the per-block download timeout in
// the steady state, as a PERCENTAGE of the block interval. SVNode makes it
// configurable with -blockdownloadtimeoutbase; this port carries the default.
const BlockDownloadTimeoutBase int64 = 100

// BlockDownloadTimeoutBaseIBD mirrors validation.h
// DEFAULT_BLOCK_DOWNLOAD_TIMEOUT_BASE_IBD (600): the same timeout during
// initial block download, where a peer is forgiven six block intervals rather
// than one because the whole chain is in flight rather than one new tip.
const BlockDownloadTimeoutBaseIBD int64 = 600

// BlockDownloadTimeoutPerPeer mirrors validation.h
// DEFAULT_BLOCK_DOWNLOAD_TIMEOUT_PER_PEER (50): the extra allowance, again as a
// percentage of the block interval, granted for every OTHER peer we are
// downloading blocks from. C++: "We compensate for other peers to prevent
// killing off peers due to our own downstream link being saturated."
const BlockDownloadTimeoutPerPeer int64 = 50

// percentOfBlockIntervalMicros converts the percentage-of-block-interval unit
// the three constants above are expressed in into microseconds, given a block
// interval in seconds. It is the C++ comment's own arithmetic
// (net_processing.cpp:5652-5654): "to get seconds we must multiply by 1000000
// and divide by 100 which is equivalent to multiply by 10000".
const percentOfBlockIntervalMicros = 10000

// MaxBlockDownloadTime carries services/legacy/peer/peer.go
// MaxBlockDownloadTime (30 minutes): the wall-clock ceiling on how long a
// single block download may hold off the sync-peer rotation on the strength of
// its own throughput. Past it the peer rotates however fast it is delivering,
// so a peer cannot dribble bytes just above the rate floor for ever and hold
// the single sync slot.
const MaxBlockDownloadTime = 30 * time.Minute

// MinBlockDownloadBytesPerSec carries services/legacy/peer/peer.go
// minBlockDownloadBytesPerSec (51200), which is also the legacy
// -minsyncpeernetworkspeed default (services/legacy/config.go
// defaultMinSyncPeerNetworkSpeed). It is the rate a block ingest must sustain
// to count as progress for the rotation clock.
const MinBlockDownloadBytesPerSec = 51200

// microsPerSecond mirrors the net_processing.cpp MICROS_PER_SECOND used by
// every GetTimeMicros comparison this file ports.
const microsPerSecond = int64(time.Second / time.Microsecond)

// IngestSnapshot is what the caller observed about the block this peer is
// currently ingesting, sampled before the sync-state mutex is taken. It is the
// counterpart of legacy netsync's per-tick association read-byte sample
// (manager.go syncPeerState.assocReadBytes), which is what lets the rotation
// clock tell a fat block still streaming in from a peer that went quiet.
//
// The zero value means no ingest is running, which is what every caller that
// does not observe ingests passes.
type IngestSnapshot struct {
	// Active reports that a block ingest is in progress for this peer.
	Active bool

	// StartedMicros is when that ingest began, in microseconds since the Unix
	// epoch. It is what MaxBlockDownloadTime is measured against.
	StartedMicros int64

	// BytesRead is the payload bytes the ingest has taken off the stream so
	// far. It is monotonic within one ingest and restarts at zero for the
	// next.
	BytesRead uint64
}

// StallAction is what a periodic stall check decided about one peer. The
// machine performs no teardown itself: it reports what the caller must do, the
// same contract HeaderSync.OnHeaders uses for its disconnect errors.
type StallAction int

const (
	// StallActionNone means the peer is healthy; do nothing.
	StallActionNone StallAction = iota

	// StallActionDisconnect means the peer stalled block download past
	// BlockStallingTimeout. The caller must disconnect it and then call
	// PeerDisconnected on both this machine and HeaderSync.
	StallActionDisconnect

	// StallActionRotateSyncPeer means the sync peer made no progress for
	// MaxLastBlockTime. The sync slot and the peer's in-flight blocks are
	// already released when this returns; the peer stays connected and the
	// caller must choose a new sync peer (HeaderSync.PeerEstablished on a
	// candidate).
	StallActionRotateSyncPeer
)

func (a StallAction) String() string {
	switch a {
	case StallActionNone:
		return "none"
	case StallActionDisconnect:
		return "disconnect"
	case StallActionRotateSyncPeer:
		return "rotate-sync-peer"
	default:
		return "unknown"
	}
}

// inFlightHolder is one entry of BlockDownloadTracker::InFlightBlock: a peer we
// have asked for a block, and when we asked it. The per-holder clock is what the
// slow-fetch trigger measures, and it is distinct from peerSyncState's
// nDownloadingSince, which measures the head of one peer's whole queue.
type inFlightHolder struct {
	peer  *SyncPeer
	since int64
}

// BlockDownloader is the net_processing.cpp block download scheduler: the
// FindNextBlocksToDownload window walk, the BlockDownloadTracker in-flight
// map, the INV handler's block half, and the stall rules. It performs no I/O
// and reads no clock: every event returns the messages the caller must send
// plus a decision, and every timestamp arrives as a parameter in microseconds
// since the Unix epoch, the unit SVNode's GetTimeMicros uses.
//
// Locking: BlockDownloader carries no lock of its own. Like peerSyncState and
// HeaderSync, every method assumes the caller already holds PeerManager's
// shared sync-state mutex — this package's port of cs_main. Lock order in this
// package is peer lock, then manager lock.
type BlockDownloader struct {
	idx *HeaderIndex
	hs  *HeaderSync

	// inFlight is the BlockDownloadTracker mMapBlocksInFlight port: every block
	// we have asked for, mapped to every peer we asked, in the order we asked
	// them.
	//
	// THE ORDER IS LOAD-BEARING, which is why this is a slice and not a set. C++
	// holds a multimap, whose equal_range walks the holders of one hash in
	// insertion order, and GetPeerForBlock (block_download_tracker.cpp:232-244)
	// returns the FIRST of them. That first holder is the one the download walk
	// records as `waitingfor`, and therefore the one it may name as the staller.
	// A Go map carries no order at all, so a set would make staller naming
	// depend on hash iteration order.
	//
	// Phase 3 up to Task 6 held ONE peer per block and called it a decision, on
	// the grounds that racing needs a per-peer bandwidth meter this port did not
	// have. Task 6 built the meter — IngestSnapshot reaches CheckStall for every
	// peer on every tick — so Task 6b ported the racing (see the parallel-fetch
	// branch in FindNextBlocksToDownload). Racing is also what makes the
	// unconditional download timeout affordable: SVNode can discard a partial
	// block because the block still arrives from someone else.
	inFlight map[chainhash.Hash][]inFlightHolder

	// haveData ports CBlockIndex::getStatus().hasData(): the blocks whose full
	// data we hold, mapped to their height so the watermark prune below can
	// find them. SVNode reads this from the block index; this machine has no
	// block store, so it records what it was told arrived through BlockReceived.
	//
	// A block we hold from an earlier run and never saw arrive this session is
	// therefore re-requested; that costs one redundant download, which SVNode
	// also tolerates elsewhere.
	//
	// Entries are dropped by height watermark, not by the download walk: see
	// pruneHaveData for why the walk cannot do it and what the height rule
	// costs.
	haveData map[chainhash.Hash]int32

	// haveDataWatermark is the highest active-chain tip height haveData has
	// been pruned against. It keeps the prune O(1) while our chain stands
	// still, which is every call but the ones that follow a new block.
	haveDataWatermark int32

	// lastTipHash is the header index tip at the previous stall check. It is
	// how CheckStall observes headers-first progress without reading a clock
	// or being told about it — see the source note there.
	//
	// It is the tip hash rather than its height because the index now selects
	// the tip by cumulative work: a heavier branch can take the tip while
	// standing shorter than the branch it displaced, and a height watermark
	// would read that fall as no progress. AddHeader replaces the tip only on
	// strictly more work, so any change of this hash is an advance.
	lastTipHash chainhash.Hash

	// txInvsReceived counts tx inventory announcements. Decision 1 defers the
	// whole tx path to Phase 3, so Phase 2 counts and logs them and does
	// nothing else. Guarded by the caller's sync-state mutex like every other
	// field here, so it needs no atomic.
	txInvsReceived uint64

	// maxLastBlockTime is the rotation window CheckStall measures against. It
	// is an instance field rather than the MaxLastBlockTime constant used
	// directly so an integration test can shrink it on its own downloader and
	// observe a rotation without waiting out three real minutes. It is seeded
	// with MaxLastBlockTime and is only ever narrowed, never widened, by the
	// one caller that sets it (PeerManager.ConfigureSync).
	maxLastBlockTime time.Duration

	// timeoutBasePercent, timeoutBaseIBDPercent and timeoutPerPeerPercent are
	// the three DetectStalling percentages, held per instance for the reason
	// SVNode holds them in config rather than in validation.h: an operator whose
	// blocks outgrow the default window has to be able to widen it. They are
	// seeded with the SVNode defaults and overridden from settings by
	// PeerManager.ConfigureSync. See the clause in CheckStall for why this
	// timeout, alone among the rules here, is worth a dial.
	timeoutBasePercent    int64
	timeoutBaseIBDPercent int64
	timeoutPerPeerPercent int64
}

// NewBlockDownloader builds a downloader over the header index and the
// headers-first machine that shares it. The HeaderSync reference is what lets
// a rotation release the sync slot in the same step it releases the peer's
// downloads.
func NewBlockDownloader(idx *HeaderIndex, hs *HeaderSync) (*BlockDownloader, error) {
	if idx == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: header index is nil")
	}

	if hs == nil {
		return nil, errors.New(errors.ERR_INVALID_ARGUMENT, "svp2p: header sync is nil")
	}

	tipHash, _ := idx.Tip()

	return &BlockDownloader{
		idx:              idx,
		hs:               hs,
		inFlight:         make(map[chainhash.Hash][]inFlightHolder),
		haveData:         make(map[chainhash.Hash]int32),
		lastTipHash:      tipHash,
		maxLastBlockTime: MaxLastBlockTime,

		timeoutBasePercent:    BlockDownloadTimeoutBase,
		timeoutBaseIBDPercent: BlockDownloadTimeoutBaseIBD,
		timeoutPerPeerPercent: BlockDownloadTimeoutPerPeer,
	}, nil
}

// pruneHaveData drops every recorded block at or below the active chain tip,
// because from that height down our own chain is the record of what we hold and
// the download walk never looks below its last-common block anyway.
//
// The prune has to key on height rather than on chain membership. The obvious
// rule — drop a block once the walk sees our chain covering it — never fires in
// the steady state: the walk starts above the last-common block and every
// "nothing interesting" return happens before it. That left one entry per
// received block for the lifetime of the process, roughly one per block of a
// mainnet IBD.
//
// The cost of the height rule is that a block on an abandoned fork below the
// tip is dropped too, so if a peer offers that fork again we download the block
// a second time. That is the same redundant-download class the field note
// already accepts, and it is bounded by how much forking a peer can cause,
// whereas the leak it replaces was not bounded at all.
func (bd *BlockDownloader) pruneHaveData(activeTipHeight int32) {
	if activeTipHeight <= bd.haveDataWatermark {
		return
	}

	bd.haveDataWatermark = activeTipHeight

	for hash, height := range bd.haveData {
		if height <= activeTipHeight {
			delete(bd.haveData, hash)
		}
	}
}

// FindNextBlocksToDownload is the net_processing.cpp function of the same name:
// "Update pindexLastCommonBlock and add not-in-flight missing successors to
// vBlocks, until it has at most count entries." activeTip is our own best chain
// tip, the chainActive counterpart, which the caller reads from Teranode's
// blockchain service. staller is the peer whose in-flight blocks are the only
// reason this peer cannot fetch anything, the C++ nodeStaller out-parameter.
//
// THE CONTRACT IS MULTI-PEER: EVERY peer that can serve blocks gets its own walk
// over the same window on every sync tick. net_processing.cpp SendMessages drives
// SendGetDataBlocks per peer (net_processing.cpp:5865 reaching :5662-5701), and
// PeerManager.syncPass does the same. What keeps the walks apart is the walk
// itself, not a rule above it: a block another peer already holds is skipped
// through the bd.inFlight test below, so peer B resumes at the first block peer A
// did not take. pindexLastCommonBlock is per-peer state (peerSyncState), so each
// peer's window also advances on its own deliveries alone.
//
// WHAT MAKES A PEER SCHEDULABLE IS pindexBestKnownBlock, NOT THE SYNC SLOT. Phase
// 2 could rely on exactly one peer having a useful pindexBestKnownBlock during
// IBD, because a non-sync peer's announcement parked in hashLastUnknownBlock
// until headers-first ended. Phase 3 Task 4 removed that:
// PeerManager.promoteBlockAvailabilityLocked sweeps every peer's parked hash
// through processBlockAvailability whenever the index grows, independently of
// fSyncStarted, so a peer that has never held the sync slot is a download
// candidate. A peer with nothing announced still takes the "nothing interesting"
// return below and costs one map lookup.
func (bd *BlockDownloader) FindNextBlocksToDownload(peer *SyncPeer, activeTip HeaderNode, count int) (blocks []HeaderNode, staller *SyncPeer) {
	// Before any of the early returns below: our chain moving on is what makes
	// recorded blocks droppable, and most calls take one of those returns.
	bd.pruneHaveData(activeTip.Height)

	if peer == nil || peer.State == nil || count <= 0 {
		return nil, nil
	}

	state := peer.State

	// "Make sure pindexBestKnownBlock is up to date, we'll need it."
	state.processBlockAvailability(bd.idx)

	best := state.pindexBestKnownBlock
	if best == nil {
		// "This peer has nothing interesting."
		return nil, nil
	}

	// net_processing.cpp:362-368:
	//
	//   else if (auto chainWork = state->pindexBestKnownBlock->GetChainWork();
	//       chainWork < nMinimumChainWork ||
	//       chainWork < chainActive.Tip()->GetChainWork())
	//
	// Phase 2 compared Height here because the header index carried no work;
	// Phase 3 Task 1 restored the work compare with the index field.
	//
	// The nMinimumChainWork half is dropped: it is a chain parameter
	// (chainparams.cpp consensus.nMinimumChainWork) and go-chaincfg v1.6.0,
	// the version this module pins, has no field for it — chaincfg.Params
	// carries PowLimit, PowLimitBits and Checkpoints but nothing equivalent.
	// The gate it provides is an IBD guard against wasting download slots on
	// a peer advertising a chain nobody could have mined; the checkpoint
	// fence in HeaderSync covers the same ground for the branches a peer can
	// actually get us to accept. Restore this if go-chaincfg gains the
	// parameter.
	if chainWorkOf(*best).Cmp(chainWorkOf(activeTip)) < 0 {
		// "This peer has nothing interesting."
		return nil, nil
	}

	if state.pindexLastCommonBlock == nil {
		// "Bootstrap quickly by guessing a parent of our best tip is the
		// forking point. Guessing wrong in either direction is not a problem."
		height := best.Height
		if activeTip.Height < height {
			height = activeTip.Height
		}

		anchor, ok := bd.idx.Ancestor(activeTip.Hash, height)
		if !ok {
			return nil, nil
		}

		state.pindexLastCommonBlock = &anchor
	}

	// "If the peer reorganized, our previous pindexLastCommonBlock may not be
	// an ancestor of its current tip anymore. Go back enough to fix that."
	common, ok := lastCommonAncestor(bd.idx, *state.pindexLastCommonBlock, *best)
	if !ok {
		return nil, nil
	}

	state.pindexLastCommonBlock = &common

	if common.Hash.IsEqual(&best.Hash) {
		return nil, nil
	}

	// The chainActive.Contains(pindex) test, made O(1). Every block the walk
	// below visits is on the peer's branch, so it is on our active chain
	// exactly when it is at or below the height where the two branches part.
	//
	// A failure here means activeTip is not a header this index holds, so we
	// cannot tell which blocks are already ours. Carrying on would treat every
	// one of them as missing and re-request the whole branch, so refuse instead
	// — the same answer the bootstrap above gives when it cannot place
	// activeTip.
	fork, forkOK := lastCommonAncestor(bd.idx, activeTip, *best)
	if !forkOK {
		return nil, nil
	}

	forkHeight := fork.Height

	// "Never fetch further than the best block we know the peer has, or more
	// than BLOCK_DOWNLOAD_WINDOW + 1 beyond the last linked block we have in
	// common with this peer. The +1 is so we can detect stalling, namely if we
	// would be able to download that next block if the window were 1 larger."
	windowEnd := common.Height + BlockDownloadWindow

	maxHeight := best.Height
	if windowEnd+1 < maxHeight {
		maxHeight = windowEnd + 1
	}

	branch, ok := bd.branchBetween(*best, common.Height+1, maxHeight)
	if !ok {
		return nil, nil
	}

	var (
		waitingFor *SyncPeer
		// contiguous ports the GetChainTx() guard on the pindexLastCommonBlock
		// advance. C++ reads nChainTx, which is non-zero only once every
		// ancestor has data, so the last-common block never jumps over a hole
		// in the download. Tracking the unbroken run from the start of the walk
		// is the same rule: the walk starts at the block after the current
		// last-common, which we have by definition.
		contiguous = true
	)

	for i := range branch {
		node := branch[i]
		inActiveChain := node.Height <= forkHeight

		_, have := bd.haveData[node.Hash]

		// "update pindexLastCommonBlock as long as all ancestors are already
		// downloaded, or if it's already part of our chain (and therefore don't
		// need it even if pruned)."
		if have || inActiveChain {
			if contiguous {
				advanced := node
				state.pindexLastCommonBlock = &advanced
			}

			continue
		}

		contiguous = false

		if holders := bd.inFlight[node.Hash]; len(holders) > 0 {
			// "This is the first already-in-flight block."
			if waitingFor == nil {
				waitingFor = holders[0].peer
			}

			continue
		}

		// The C++ FetchBlock lambda, inlined. SVNode also drops blocks above
		// chainActive.Height() + GetBlockDownloadLowerWindow(), a disk-usage
		// limiter on a separate config knob; it is not carried here.
		if node.Height > windowEnd {
			// "We reached the end of the window."
			if len(blocks) == 0 && waitingFor != nil && waitingFor != peer {
				// "We aren't able to fetch anything, but we would be if the
				// download window was one larger."
				staller = waitingFor
			}

			break
		}

		blocks = append(blocks, node)

		if len(blocks) == count {
			break
		}
	}

	return blocks, staller
}

// branchBetween returns the nodes on best's branch at heights from..to
// inclusive, in ascending height order.
//
// C++ walks this range with CBlockIndex::GetAncestor in chunks of 128 and then
// follows pprev backwards inside each chunk, because its skiplist makes one
// GetAncestor cost about a hundred pointer steps. HeaderIndex.Ancestor is a
// plain pprev walk with no skiplist (see its own note), so calling it once per
// height would be O(window x depth) — quadratic in the depth of the chain. One
// Ancestor call to the top of the range plus ParentHash steps down through it
// is O(depth + window) instead, and takes the index lock once for the long walk
// rather than once per step.
func (bd *BlockDownloader) branchBetween(best HeaderNode, from, to int32) ([]HeaderNode, bool) {
	if to < from {
		return nil, false
	}

	node, ok := bd.idx.Ancestor(best.Hash, to)
	if !ok {
		return nil, false
	}

	branch := make([]HeaderNode, 0, to-from+1)

	for {
		branch = append(branch, node)

		if node.Height == from {
			break
		}

		parent, found := bd.idx.Lookup(node.ParentHash)
		if !found {
			// Defensive: unreachable given the genesis-rooted invariant on
			// HeaderIndex, since from is at least 1 here.
			return nil, false
		}

		node = parent
	}

	for i, j := 0, len(branch)-1; i < j; i, j = i+1, j-1 {
		branch[i], branch[j] = branch[j], branch[i]
	}

	return branch, true
}

// lastCommonAncestor is chain.cpp LastCommonAncestor: walk the higher of the
// two nodes down to the other's height, then step both back together until
// they meet. The genesis-rooted invariant on HeaderIndex guarantees they meet.
func lastCommonAncestor(idx *HeaderIndex, a, b HeaderNode) (HeaderNode, bool) {
	var ok bool

	if a.Height > b.Height {
		if a, ok = idx.Ancestor(a.Hash, b.Height); !ok {
			return HeaderNode{}, false
		}
	} else if b.Height > a.Height {
		if b, ok = idx.Ancestor(b.Hash, a.Height); !ok {
			return HeaderNode{}, false
		}
	}

	for !a.Hash.IsEqual(&b.Hash) {
		if a.Height == 0 {
			// Defensive: unreachable, both chains terminate at the same
			// genesis node.
			return HeaderNode{}, false
		}

		if a, ok = idx.Lookup(a.ParentHash); !ok {
			return HeaderNode{}, false
		}

		if b, ok = idx.Lookup(b.ParentHash); !ok {
			return HeaderNode{}, false
		}
	}

	return a, true
}

// SendGetDataBlocks is the net_processing.cpp SendMessages helper of the same
// name: schedule up to MAX_BLOCKS_IN_TRANSIT_PER_PEER blocks from this peer,
// mark them in flight, and start the stall clock of whatever peer is holding
// the window shut. The caller drives it for each peer on its send pass.
func (bd *BlockDownloader) SendGetDataBlocks(peer *SyncPeer, activeTip HeaderNode, nowMicros int64) []wire.Message {
	if peer == nil || peer.State == nil {
		return nil
	}

	// C++ gates on !pto->fClient && (fFetch || !IsInitialBlockDownload()) —
	// "this peer can serve us blocks". isSyncCandidate is the reading of that
	// predicate this port already made for headers-first sync (Phase 2 Task 5),
	// including the regtest exception, so it is reused rather than restated.
	if !bd.hs.isSyncCandidate(peer) {
		return nil
	}

	state := peer.State
	if state.nBlocksInFlight >= MaxBlocksInTransitPerPeer {
		return nil
	}

	blocks, staller := bd.FindNextBlocksToDownload(peer, activeTip, MaxBlocksInTransitPerPeer-state.nBlocksInFlight)

	getData := wire.NewMsgGetDataSizeHint(uint(len(blocks)))

	for i := range blocks {
		hash := blocks[i].Hash

		if err := getData.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, &hash)); err != nil {
			// Unreachable at this size: the batch is capped at 16 and the
			// message limit is MaxInvPerMsg. Stop rather than mark a block in
			// flight we are not going to ask for.
			break
		}

		// net_processing.cpp: "Requesting block %s (%d) peer=%d".
		bd.MarkBlockAsInFlight(peer, blocks[i], nowMicros)
	}

	// Checked after the marking, as C++ does: staller is only ever set when the
	// batch came back empty, so nBlocksInFlight is unchanged in that case.
	if state.nBlocksInFlight == 0 && staller != nil && staller.State != nil && staller.State.nStallingSince == 0 {
		// net_processing.cpp: "Stall started (current speed %d) peer=%d".
		staller.State.nStallingSince = nowMicros
	}

	if len(getData.InvList) == 0 {
		return nil
	}

	return []wire.Message{getData}
}

// MarkBlockAsInFlight is BlockDownloadTracker::MarkBlockAsInFlight. It reports
// false when the block is already in flight, the C++ short-circuit for the same
// block from the same node — extended to any node, since this port fetches each
// block from exactly one peer whatever the tick's peer count (see the inFlight
// field note).
func (bd *BlockDownloader) MarkBlockAsInFlight(peer *SyncPeer, block HeaderNode, nowMicros int64) bool {
	if peer == nil || peer.State == nil {
		return false
	}

	if bd.IsInFlightFrom(peer, block.Hash) {
		return false
	}

	bd.inFlight[block.Hash] = append(bd.inFlight[block.Hash], inFlightHolder{peer: peer, since: nowMicros})
	peer.State.vBlocksInFlight = append(peer.State.vBlocksInFlight, block.Hash)
	peer.State.nBlocksInFlight++

	if peer.State.nBlocksInFlight == 1 {
		// block_download_tracker.cpp:46-50: "We're starting a block download
		// (batch) from this peer."
		peer.State.nDownloadingSince = nowMicros
	}

	return true
}

// BlockReceived is BlockDownloadTracker::MarkBlockAsReceived: the block's data
// arrived from peer and we now hold it. It reports whether the block was
// actually in flight from that peer. The caller must use BlockFailed instead
// when the download failed or the block was rejected, so the data is not
// recorded as held.
func (bd *BlockDownloader) BlockReceived(peer *SyncPeer, hash chainhash.Hash, nowMicros int64) bool {
	// The height is what makes the entry prunable later. A block whose header
	// this index does not hold is not recorded at all: the download walk only
	// ever visits indexed headers, so the entry could never be read, and
	// without a height it could never be dropped either.
	if node, known := bd.idx.Lookup(hash); known {
		bd.haveData[hash] = node.Height
	}

	if peer != nil && peer.State != nil {
		// legacy netsync manager.go handleBlockMsg: a delivered block refreshes
		// the sync peer's rotation clock. See the source note on CheckStall.
		peer.State.nLastProgressTime = nowMicros
	}

	return bd.removeFromFlight(peer, hash, nowMicros)
}

// BlockFailed is BlockDownloadTracker::MarkBlockAsFailed: the download was
// cancelled, timed out, or the block was rejected. The block goes back on
// offer to any peer, including this one.
func (bd *BlockDownloader) BlockFailed(peer *SyncPeer, hash chainhash.Hash, nowMicros int64) bool {
	delete(bd.haveData, hash)

	return bd.removeFromFlight(peer, hash, nowMicros)
}

// removeFromFlight is BlockDownloadTracker::removeFromBlockMapNL. It only fires
// for the peer the block was requested from, matching the C++ getBlockFromNodeNL
// lookup by node.
func (bd *BlockDownloader) removeFromFlight(peer *SyncPeer, hash chainhash.Hash, nowMicros int64) bool {
	holders := bd.inFlight[hash]

	at := -1

	for i := range holders {
		if holders[i].peer == peer {
			at = i
			break
		}
	}

	if at < 0 {
		return false
	}

	if len(holders) == 1 {
		delete(bd.inFlight, hash)
	} else {
		bd.inFlight[hash] = append(holders[:at], holders[at+1:]...)
	}

	if peer.State != nil {
		state := peer.State

		for i, queued := range state.vBlocksInFlight {
			if queued != hash {
				continue
			}

			if i == 0 && nowMicros > state.nDownloadingSince {
				// block_download_tracker.cpp:311-315: "First block on the
				// queue was received, update the start download time for the
				// next one" — std::max, so a clock reading older than the one
				// already held cannot lengthen the next block's window.
				state.nDownloadingSince = nowMicros
			}

			state.vBlocksInFlight = append(state.vBlocksInFlight[:i], state.vBlocksInFlight[i+1:]...)

			break
		}

		if state.nBlocksInFlight > 0 {
			state.nBlocksInFlight--
		}

		state.nStallingSince = 0
	}

	return true
}

// IsInFlight is BlockDownloadTracker::IsInFlight. Like every method here it
// reads shared state and requires the caller to already hold PeerManager's
// sync-state mutex; it is a plain map read, not a synchronized one.
func (bd *BlockDownloader) IsInFlight(hash chainhash.Hash) bool {
	return len(bd.inFlight[hash]) > 0
}

// IsInFlightFrom is BlockDownloadTracker::IsInFlight narrowed to one peer,
// the lookup behind legacy netsync's BlockRequested check
// (services/legacy/peer_server.go OnBlock, PR 1190): a block we did not ask
// THIS peer for is unsolicited, and must be refused before it can consume the
// admission budget.
func (bd *BlockDownloader) IsInFlightFrom(peer *SyncPeer, hash chainhash.Hash) bool {
	for _, holder := range bd.inFlight[hash] {
		if holder.peer == peer {
			return true
		}
	}

	return false
}

// BlockNotDelivered releases a block this peer will NOT deliver after all,
// without recording it as held. It is removeFromFlight's guard on its own:
// nothing happens unless this peer is the recorded holder.
//
// It exists for the duplicate-admission case, which BlockFailed cannot serve.
// Both would re-offer the block, but BlockFailed also DELETES the have-data
// record — and the copy that won the admission race may already have completed
// and recorded it. Losing that record would re-download a block we hold.
func (bd *BlockDownloader) BlockNotDelivered(peer *SyncPeer, hash chainhash.Hash, nowMicros int64) bool {
	return bd.removeFromFlight(peer, hash, nowMicros)
}

// BlocksInFlight reports how many blocks are in flight across all peers.
// Requires the caller to hold PeerManager's sync-state mutex.
func (bd *BlockDownloader) BlocksInFlight() int { return len(bd.inFlight) }

// TxInvsReceived reports how many tx inventory announcements have arrived.
// Decision 1 defers the tx path to Phase 3; this is the counter that stands in
// for it until then. Requires the caller to hold PeerManager's sync-state
// mutex: the counter is deliberately not atomic, like every other field here.
func (bd *BlockDownloader) TxInvsReceived() uint64 { return bd.txInvsReceived }

// PeerDisconnected is BlockDownloadTracker::ClearPeer, the block-download half
// of net_processing.cpp FinalizeNode: everything the peer was downloading goes
// back on offer and its download state resets. The caller must also call
// HeaderSync.PeerDisconnected, which releases the sync slot.
func (bd *BlockDownloader) PeerDisconnected(peer *SyncPeer) {
	bd.clearPeer(peer)
}

// clearPeer releases every block in flight from peer and resets its download
// bookkeeping. It is also legacy netsync manager.go clearRequestedState, which
// the sync-peer rotation runs before choosing another peer.
func (bd *BlockDownloader) clearPeer(peer *SyncPeer) {
	if peer == nil {
		return
	}

	// Only this peer's claims go; a block another peer is also fetching stays in
	// flight from that peer. C++ ClearPeer walks the same multimap the same way.
	for hash, holders := range bd.inFlight {
		kept := holders[:0]

		for _, holder := range holders {
			if holder.peer != peer {
				kept = append(kept, holder)
			}
		}

		if len(kept) == 0 {
			delete(bd.inFlight, hash)
			continue
		}

		bd.inFlight[hash] = kept
	}

	if peer.State != nil {
		peer.State.nBlocksInFlight = 0
		peer.State.vBlocksInFlight = nil
		peer.State.nDownloadingSince = 0
		peer.State.nStallingSince = 0
		peer.State.pindexLastCommonBlock = nil
	}
}

// OnInv is the net_processing.cpp NetMsgType::INV event. It returns the
// messages to send back and an error only when the peer must be disconnected.
//
// Block invs update availability and, for a block whose header we do not have,
// ask for headers rather than the block itself: "since headers-announcements
// are now the primary method of announcement on the network, and since, in the
// case that a node fell back to inv we probably have a reorg which we should
// get the headers for first, we now only provide a getheaders response here.
// When we receive the headers, we will then ask for the blocks we need."
//
// Tx invs are counted and nothing else — Decision 1 defers the tx path to
// Phase 3.
//
// On error the message list is nil, never partial. C++ scores the bad entry and
// carries on through the rest of the batch, but it is setting fDisconnect while
// it does so; here the error means the caller must drop the peer, and messages
// queued for a peer we are about to drop are not worth sending. Availability
// updates made for the entries already processed do stand — they are state, not
// output, and they were valid when they were made.
func (bd *BlockDownloader) OnInv(peer *SyncPeer, msg *wire.MsgInv) ([]wire.Message, error) {
	if peer == nil || peer.State == nil || msg == nil {
		return nil, nil
	}

	var (
		msgs      []wire.Message
		requested map[chainhash.Hash]struct{}
	)

	for _, inv := range msg.InvList {
		if inv == nil {
			continue
		}

		switch inv.Type {
		case wire.InvTypeBlock:
			// net_processing.cpp: "got block inv: %s %s peer=%d".
			peer.State.updateBlockAvailability(bd.idx, inv.Hash)

			// AlreadyHave(MSG_BLOCK) is IsBlockKnown, a mapBlockIndex lookup:
			// a block we already have a header for needs no getheaders.
			if _, known := bd.idx.Lookup(inv.Hash); known {
				continue
			}

			if bd.IsInFlight(inv.Hash) {
				continue
			}

			// A narrow deviation: C++ answers every inv entry on its own, so a
			// hash repeated inside one message draws one getheaders per copy.
			// Nothing about our state changes between them, so the copies are
			// identical and wasted. One per message is enough.
			if _, dup := requested[inv.Hash]; dup {
				continue
			}

			if requested == nil {
				requested = make(map[chainhash.Hash]struct{})
			}

			requested[inv.Hash] = struct{}{}

			// net_processing.cpp: "getheaders (%d) %s to peer=%d", where the
			// height is that of the best header the locator was built from.
			msgs = append(msgs, bd.getHeadersFor(inv.Hash))

		case wire.InvTypeTx:
			// net_processing.cpp: "got txn inv: %s %s txnsrc peer=%d". Phase 2
			// stops here (Decision 1).
			bd.txInvsReceived++

		default:
			// net_processing.cpp: "Got invalid inv type %d from peer=%d" →
			// pfrom->fDisconnect = true. C++ finishes the loop first; there is
			// nothing worth doing for a peer we are about to drop, so this
			// returns straight away and discards what it had queued.
			return nil, errors.New(errors.ERR_NETWORK_PEER_MALICIOUS,
				"svp2p: unsupported inv type %d", uint32(inv.Type), ErrProtocolViolation)
		}
	}

	return msgs, nil
}

// getHeadersFor builds the inv-answering getheaders: a locator from our best
// header, stopping at the announced hash. This is chainActive.GetLocator with
// mapBlockIndex.GetBestHeader, and unlike HeaderSync's getheaders it never
// carries a checkpoint hashStop — the stop here is the block the peer just
// announced.
func (bd *BlockDownloader) getHeadersFor(stop chainhash.Hash) *wire.MsgGetHeaders {
	msg := wire.NewMsgGetHeaders()
	msg.ProtocolVersion = wire.ProtocolVersion

	locator := bd.idx.Locator()
	for i := range locator {
		hash := locator[i]
		msg.BlockLocatorHashes = append(msg.BlockLocatorHashes, &hash)
	}

	msg.HashStop = stop

	return msg
}

// CheckStall is the periodic timer event, driven per peer by the caller. It
// reads no clock: nowMicros arrives as a parameter. It carries two rules from
// two different sources.
//
// The first is net_processing.cpp DetectStalling: a peer whose nStallingSince
// clock has run past BlockStallingTimeout is holding the head of the download
// window and is disconnected. C++ additionally re-arms that clock instead of
// disconnecting when the peer is still delivering bytes at a healthy rate
// (IsBlockDownloadStallingFromPeer); this port has no per-peer bandwidth meter,
// so it always takes the disconnect branch — the branch C++ takes for a peer
// delivering nothing. The bound on that gap is narrow: nStallingSince is only
// ever set on a peer that is blocking the window, and any block it delivers
// clears it (see removeFromFlight).
//
// The second is the Teranode sync-peer rotation, carried from legacy netsync
// manager.go handleCheckSyncPeer and maxLastBlockTime (PR 1067). It is a
// licensed deviation from SVNode, which has no such rule: SVNode keeps a sync
// peer until it disconnects or its in-flight blocks time out. It also closes a
// gap the SVNode rule leaves open here. During headers-first sync no block need
// ever be in flight, so nStallingSince never starts and DetectStalling can
// never fire; a peer that simply stops answering getheaders would hold the
// single sync slot for ever. The rotation catches it, because its progress
// clock is not refreshed by anything the silent peer does.
//
// Progress means one of two things, and only the sync peer has a clock at all:
//   - a block was delivered (legacy's own trigger, refreshed in BlockReceived);
//   - the header index tip moved while a headers-first round was running and
//     this peer had no block download outstanding to be judged on instead.
//     The tip is selected by cumulative work, so any move is a gain in work,
//     even one that lands on a shorter branch.
//
// That second source is this port's, and it is a deliberate departure from a
// choice legacy made on purpose. legacy netsync manager.go handleCheckSyncPeer
// (see the headers-first branch at lines 1043-1049) suppresses only the network
// speed check during headers-first mode and states that it still checks
// last-block-time "so stalled peers get rotated even during headers-first".
// Since lastBlockTime is refreshed on delivered blocks alone, that rotates a
// peer feeding us a long run of headers with no block yet to fetch. This port
// treats header progress as a stall substitute in exactly that case.
//
// Two limits keep the departure narrow, and the first is why the
// nBlocksInFlight guard below exists. Header progress is only allowed to stand
// in for block progress when there is no block progress to judge: a peer
// sitting on in-flight blocks is measured on those blocks, so it cannot hold
// the sync slot by trickling headers while withholding what we asked it for.
// Second, the refresh is scoped to headers-first rounds, which Phase 2 Task 5
// restricts to the sync peer.
//
// The tip is an imperfect witness even so: HeaderIndex is also fed by the
// blockchain-service subscription, so a rise can come from our own node rather
// than from this peer. That can only ever delay a rotation, never cause a wrong
// one, and only while the node is genuinely advancing.
//
// What a rotation leaves behind is the third thing this method has to answer
// for. A rotated peer stays connected, and SyncPeerTimedOut cleared its
// fSyncStarted, so the rotation clause cannot fire for it again while it holds
// no sync slot. It may take the slot back, but never on the pass that took it
// away: PeerManager.syncPass runs the header-sync eligibility sweep BEFORE this
// check for the same peer, which is the SendMessages order, so the sweep that
// could re-elect it has already been and gone. A LATER tick's sweep does hand
// the slot back — to this peer when it is the only candidate, and the rotation
// clause then governs it once more. The paragraphs below are about the other
// case, the conservative one: while it holds no slot, it is a PLAIN DOWNLOAD
// PEER. It keeps no clock of its own, and the scheduler may hand it blocks on
// any later tick. Two things govern it then, and the ORDER of the two clauses
// below is what makes the first of them work:
//
//   - The DetectStalling clause runs BEFORE the fSyncStarted early-return, so
//     it still judges a peer that holds no sync slot. Any block such a peer
//     heads the download window with starts its nStallingSince clock through
//     SendGetDataBlocks, and this method then disconnects it, which releases
//     the blocks through FinalizeNode. Reverse the two clauses and a
//     rotated-but-connected peer becomes ungovernable: blocks re-handed to it
//     would never come back. TestCheckStall_DisconnectsAStallerThatHoldsNoSyncSlot
//     pins the order.
//   - The per-block download timeout, the second DetectStalling clause below,
//     also runs before that early return. It reaches such a peer on the age of
//     the block at the head of its own queue, so it needs neither a second
//     peer nor a drained window.
//
// Both are needed, because the staller rule alone is expensive. A silent peer
// comes to rest holding MaxBlocksInTransitPerPeer blocks and no more, because
// SendGetDataBlocks is the only thing that marks blocks in flight and it asks
// for at most the remainder of that cap. nStallingSince cannot start until
// another peer has downloaded the whole rest of the download window and STILL
// cannot move, since that empty batch is the only thing that names a staller.
// So that recovery is bounded by the download time for up to
// BlockDownloadWindow blocks — our own tip stuck behind the hole for all of it
// — and only then by BlockStallingTimeout. With no second eligible peer it
// never completes at all.
// TestSyncPass_ReHandedBlocksToASilentRotatedPeerAreReleasedAgain
// (manager_test.go) pins that expensive path, and
// TestSyncPass_TimesOutASilentRotatedPeerAndRehomesItsBlocks beside it pins the
// cheap one: one timeout, no second peer, no drained window.
//
// ingest is what the caller observed about a block this peer is currently
// ingesting; the zero value means none. It is the input to the large-block
// suppression documented above the rotation branch.
func (bd *BlockDownloader) CheckStall(peer *SyncPeer, ingest IngestSnapshot, nowMicros int64) StallAction {
	if peer == nil || peer.State == nil {
		return StallActionNone
	}

	state := peer.State

	// Sampled on the way out, like legacy's deferred updateNetwork, so every
	// read below compares against the PREVIOUS tick's sample.
	defer sampleIngest(state, ingest, nowMicros)

	if state.nStallingSince != 0 && state.nStallingSince < nowMicros-microsPerSecond*int64(BlockStallingTimeout/time.Second) {
		// net_processing.cpp: "Peer=%d is stalling block download (current
		// speed %d), disconnecting".
		return StallActionDisconnect
	}

	// DetectStalling's second half (net_processing.cpp:5629-5661): the block at
	// the head of this peer's queue has been owed for longer than
	// maxDownloadTime, so the peer goes and its blocks return to the pool. Like
	// the staller clause above it sits ABOVE the fSyncStarted return, because it
	// must judge every peer holding blocks — including one a rotation stripped
	// of the sync slot, which no other rule can reach.
	//
	// IT IS UNCONDITIONAL, AND THAT IS THE POINT. Every other rule that governs
	// a slow peer — the staller clause above, SVNode's parallel-fetch trigger,
	// this port's own rotation suppression below — weighs throughput, and
	// throughput evidence is gameable: a peer trickling bytes at the floor
	// satisfies all of them for ever. This clause is the one bound that cannot
	// be talked out of, and SVNode pays a real price for it — the partial block
	// is discarded, and it discards it even when another peer has already
	// delivered the same block, since MarkBlockAsReceived
	// (net_processing.cpp:4058) clears only the delivering peer's entry. The
	// window moving matters more than the bytes already spent.
	//
	// WHAT THAT COSTS HERE, and why the constants are configurable when
	// BlockDownloadWindow and BlockStallingTimeout are not. SVNode can afford to
	// burn a slow peer because it races the block to up to
	// DEFAULT_MAX_BLOCK_PARALLEL_FETCH peers, so the block still arrives while
	// the slow holder is dropped. This port does not race (see the inFlight
	// field note), so a disconnect restarts the download from zero on another
	// peer with no more time than this one had. In the steady state the window
	// is one bare block interval — the per-peer term is zero precisely then,
	// because one block in flight means no other downloading peer — which is
	// ten minutes on mainnet, or 6.7 MB/s for a 4 GB block. A block that cannot
	// be fetched inside the window from ANY peer would never complete.
	//
	// SVNode leaves exactly this dial in the operator's hands
	// (-blockdownloadtimeoutbasepercent and its two siblings), and so does this
	// port, through the settings the three fields below are seeded from. The
	// defaults are SVNode's own, so out of the box the behavior is its
	// behavior; an operator running large blocks over a modest link raises the
	// base rather than patching the binary.
	if len(state.vBlocksInFlight) > 0 && nowMicros > state.nDownloadingSince+bd.maxDownloadTimeMicros(peer) {
		// net_processing.cpp: "Timeout downloading block %s from peer=%d,
		// disconnecting".
		return StallActionDisconnect
	}

	// legacy netsync handleCheckSyncPeer only ever examines the sync peer. This
	// return MUST stay below the DetectStalling clauses above: a rotated peer
	// reaches here with fSyncStarted cleared, and those two rules are the only
	// things left that can release the blocks it was re-handed. See the note on
	// what a rotation leaves behind.
	if !state.fSyncStarted {
		return StallActionNone
	}

	if tipHash, _ := bd.idx.Tip(); tipHash != bd.lastTipHash {
		bd.lastTipHash = tipHash

		if bd.hs.IsHeadersFirstMode() && state.nBlocksInFlight == 0 {
			state.nLastProgressTime = nowMicros
		}
	}

	// legacy seeds lastBlockTime when it elects the sync peer; seeding on the
	// first check instead keeps the machine free of an extra election call and
	// costs at most one tick of the rotation window.
	if state.nLastProgressTime == 0 {
		state.nLastProgressTime = nowMicros

		return StallActionNone
	}

	if nowMicros-state.nLastProgressTime <= int64(bd.maxLastBlockTime/time.Microsecond) {
		return StallActionNone
	}

	// legacy netsync manager.go handleCheckSyncPeer (the suppression at
	// manager.go:1052-1068): "A multi-GB block can take longer than
	// maxLastBlockTime to arrive... Don't rotate a sync peer that is still
	// pulling data at a healthy rate — it is making progress on a large block,
	// not stalled." Legacy measures the association's read bytes per tick;
	// here the ingest's own ProgressReader is the same measurement, taken
	// closer to the truth — those bytes are the block payload leaving the
	// socket.
	//
	// Without this a single block whose ingest outlives MaxLastBlockTime
	// rotates the peer that is in the middle of delivering it, which is the
	// one peer we most want to keep.
	//
	// The suppression is capped at MaxBlockDownloadTime
	// (services/legacy/peer/peer.go), so a peer that dribbles bytes just above
	// the rate floor cannot hold the sync slot for ever.
	if ingestProgressing(state, ingest, nowMicros) {
		return StallActionNone
	}

	// legacy netsync handleCheckSyncPeer: "sync peer %s is stalled due to %s,
	// updating sync peer" → clearRequestedState then updateSyncPeer.
	bd.clearPeer(peer)
	bd.hs.SyncPeerTimedOut(peer)

	state.nLastProgressTime = nowMicros

	return StallActionRotateSyncPeer
}

// ingestProgressing ports legacy netsync syncPeerState.hasHealthyDownloadThroughput
// (manager.go:302-329) together with the MaxBlockDownloadTime cap its caller
// applies. It needs a prior sample to compute a delta, requires bytes to have
// actually moved (a zero delta is not "downloading", whatever the rate floor
// is configured to), and guards the unsigned subtraction — a count that went
// backwards means a new ingest started, which is not evidence about this one.
func ingestProgressing(state *peerSyncState, ingest IngestSnapshot, nowMicros int64) bool {
	if !ingest.Active {
		return false
	}

	if nowMicros-ingest.StartedMicros >= microsPerSecond*int64(MaxBlockDownloadTime/time.Second) {
		return false
	}

	if state.nIngestSampleMicros == 0 || ingest.BytesRead < state.nIngestBytesLastSample {
		return false
	}

	elapsed := nowMicros - state.nIngestSampleMicros
	if elapsed <= 0 {
		return false
	}

	delta := ingest.BytesRead - state.nIngestBytesLastSample
	if delta == 0 {
		return false
	}

	// delta bytes over elapsed microseconds, compared in bytes per second.
	return delta*uint64(microsPerSecond) >= MinBlockDownloadBytesPerSec*uint64(elapsed) //nolint:gosec // both are positive here
}

// maxDownloadTimeMicros is the DetectStalling budget for the block at the head
// of peer's queue (net_processing.cpp:5645-5654):
//
//	maxDownloadTime = nPowTargetSpacing * (timeoutBase + timeoutPeers) * 10000
//
// The base is chosen by the initial-block-download predicate; the per-peer term
// forgives a peer for our own saturated downlink.
func (bd *BlockDownloader) maxDownloadTimeMicros(peer *SyncPeer) int64 {
	timeoutBase := bd.timeoutBasePercent

	// C++ asks IsInitialBlockDownload(), which weighs our tip's work against
	// nMinimumChainWork and its age against DEFAULT_MAX_TIP_AGE. Neither this
	// module's go-chaincfg version nor this machine carries the work half (see
	// the note in FindNextBlocksToDownload), so the predicate reduces to the age
	// half: a header index tip older than 24 hours means we are still catching
	// up. It is the same clock and the same window Task 4's second header-sync
	// slot uses, so the two relaxations agree on when the node is near the tip.
	if !bd.hs.tipIsNearAdjustedTime() {
		timeoutBase = bd.timeoutBaseIBDPercent
	}

	timeoutPeers := bd.timeoutPerPeerPercent * int64(bd.otherPeersWithDownloads(peer))

	return targetSpacingSeconds(bd.hs.cfg.Params) * (timeoutBase + timeoutPeers) * percentOfBlockIntervalMicros
}

// otherPeersWithDownloads is
// BlockDownloadTracker::GetPeersWithValidatedDownloadsCount() minus this peer,
// the nOtherPeersWithValidatedDownloads of net_processing.cpp:5638-5642.
//
// C++ keeps a running counter because its in-flight multimap can hold blocks
// whose headers are not validated, and it deliberately counts only the
// validated ones "so peers can't advertise non-existing block hashes to
// unreasonably increase our timeout". Here the distinct holders in inFlight ARE
// that set: FindNextBlocksToDownload only ever walks headers the index already
// holds, so every in-flight block has a validated header, and the map is
// single-assignment. Counting the map therefore needs no second counter to
// drift out of step with it, at the cost of one pass over at most
// BlockDownloadWindow entries per check.
func (bd *BlockDownloader) otherPeersWithDownloads(peer *SyncPeer) int {
	holders := make(map[*SyncPeer]struct{}, len(bd.inFlight))

	for _, entry := range bd.inFlight {
		for _, holder := range entry {
			if holder.peer != peer {
				holders[holder.peer] = struct{}{}
			}
		}
	}

	return len(holders)
}

// sampleIngest records this tick's ingest observation for the next tick's
// rate calculation, the syncPeerState.assocReadBytesLastTick roll-forward.
func sampleIngest(state *peerSyncState, ingest IngestSnapshot, nowMicros int64) {
	if !ingest.Active {
		state.nIngestBytesLastSample = 0
		state.nIngestSampleMicros = 0

		return
	}

	state.nIngestBytesLastSample = ingest.BytesRead
	state.nIngestSampleMicros = nowMicros
}

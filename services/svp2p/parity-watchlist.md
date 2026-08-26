# svp2p parity watch-list

Scenarios the svp2p unit tests cannot adjudicate. Each needs a scripted or
recorded SVNode peer, so each waits on the parity harness (design spec §9, a
scripted-peer replay rig on `cmd/peercli`).

The harness does not exist yet. Phase 3 landed without it, so this file is the
backlog the harness plan must consume. Nothing here is a known defect. Each entry
is a place where this port makes a choice a unit test cannot judge, with the
SVNode or legacy source that defines the correct answer.

SVNode line references are against `bitcoin-sv` @ `879fc8b42`.

## How to use an entry

Each entry gives the scenario to script, what to observe, and the pass criterion.
A divergence is filed against the task named in the entry, not against the
harness.

---

## 1. Headers batch with no progress

**Task 5, residual N1.**

An SVNode peer that re-sends its previous headers batch, rather than an empty
batch, trips `ErrHeadersNoProgress` in this port. Whether a real SVNode peer ever
does that is unknown — it is a question about SVNode's send path, not about our
receive path, so no unit test can settle it.

- **Script:** a recorded peer that answers a `getheaders` with the batch it
  already sent, at least twice in a row.
- **Observe:** whether `ErrHeadersNoProgress` fires, and whether the peer is
  scored or dropped.
- **Pass:** no honest SVNode peer reaches the error. If one does, the policy is
  wrong and Task 5 owns the fix.


**Verdict 2026-08-26 (parity harness, `services/svp2p/parity`, `TestParity_HeadersReplayIsDropped`):**
both implementations drop the replaying peer — legacy for "block header that does
not properly connect to the chain", svp2p through `ErrHeadersNoProgress` — after
asking twice. The honest case (an inv-drawn duplicate reply mid-round) is pinned
by `TestParity_HonestDuplicateReplyMidRoundStaysConnected` after fix 79f0870ba.
Whether a real SVNode ever replays a batch remains a question for a recorded peer.

## 2. Sync-peer election order

**headersync.go `PeerEstablished` note.**

This port elects the first eligible peer. Legacy ranks candidates by height. The
two agree whenever the first eligible peer is also the best one, and a unit test
cannot show how often that holds against real peers.

- **Script:** three peers announcing distinctly different heights, connecting in
  an order that makes the first eligible peer the worst choice.
- **Observe:** which peer is elected, and how long the node stays with a poor
  pick before rotation moves it.
- **Pass:** election quality is no worse than legacy's ranking over the corpus.
  Upgrade to a ranking only if the harness shows poor picks; do not pre-empt it.


**Verdict 2026-08-26 (`TestParity_SyncPeerElectionOrder`, peers claiming 10/200/50
over a 60-block chain):** legacy ranks by claimed height only among the
candidates present when `startSync` runs — often just the first peer to finish
its handshake — so across three runs it elected peer1, peer2 and peer1, and each
time downloaded all 60 blocks from that one peer and nothing from the others.
svp2p asks whichever candidate the sweep meets first and then spreads the window
over every useful peer (16/26/18). Both reach the tip in ~12 s. Election quality
is moot for svp2p's download because download does not follow election; no
ranking needed. (This assertion was the one intermittent parity failure seen
under -race; it assumed legacy always picks the tallest.)

## 3. Unrequested-headers policy score

**Task 11 (9be584555).**

Task 11 landed the unsolicited bulk-header scoring on unit tests alone, because
the harness it names as its verifier does not exist. The score VALUE is the risk:
too high fragments connectivity against honest peers that batch differently.

- **Script:** honest SVNode peers that send header batches we did not request,
  in the shapes SVNode's own send path produces.
- **Observe:** accumulated misbehavior score per honest peer over a full IBD.
- **Pass:** no honest peer reaches `banScoreThreshold` (100). Any honest peer
  that does means the value is wrong.


**Verdict 2026-08-26 (`TestParity_UnsolicitedHeadersScore`):** the non-elected
peer pushes one unsolicited 2000-header batch and five announcement-sized
batches while the round runs. svp2p keeps the peer with a score ≤ 20 (on this
rig the push landed after the 2040-header round had already closed, so the
observed score was 0; the 20 for a bulk batch mid-round is pinned by the unit
tests); legacy disconnects the peer for "unrequested headers". No honest shape
approaches the threshold.

## 4. Ban and misbehavior score parity across every handler

**Spec §11 risk. Touches Tasks 11, 18, 20 and the handshake.**

Every scoring site in this port carries the SVNode `Misbehaving` value it was
ported from. The values are individually cited but have never been compared to a
real peer's accumulated total across a session.

Sites and their ported values:

| Site | Value | SVNode source |
|---|---|---|
| `scoreInvalidBlock` | 100 | `block_download_tracker.cpp:113-127`, `validation.cpp` DoS(100) sites |
| `scoreTooManyHeaders` | 20 | `Misbehaving(pfrom, 20, "too-many-headers")` |
| `scoreNonContinuousHeaders` | 20 | `Misbehaving(pfrom, 20, "disconnected headers")` |
| `scoreTooManyUnconnectedHeaders` | 20 | `Misbehaving(pfrom, 20, "too-many-unconnected-headers")` |
| `scoreOversizedAddr` | 20 | `Misbehaving(pfrom, 20, "oversized-addr")` |
| `scoreMultipleVersion` | 1 | `Misbehaving(pfrom, 1, "multiple-version")` |
| `scoreMissingVersion` | 1 | `Misbehaving(pfrom, 1, "missing-version")` |
| threshold | 100 | `DEFAULT_BANSCORE_THRESHOLD`, `validation.h:191` |

- **Script:** a full session against honest peers, then against peers that
  misbehave in each scored way exactly once.
- **Observe:** the running total per peer, compared against SVNode's own
  `nMisbehavior` for the identical input.
- **Pass:** totals match SVNode's, and no honest peer accumulates anything.

Note two known structural differences, neither a defect:

- **No score decay.** SVNode's `nMisbehavior` is per-connection and dies with the
  connection; it never decays while the connection lives. This port matches that.
  The unrelated `p2p` service documents a -1/minute decay for its own scorer
  (`settings/p2p_settings.go:33`), which does not apply here.
- **`punish` is not ported.** The `Misbehaving` call in
  `block_download_tracker.cpp:124` is gated on `it->second.punish`, and both
  `punish=false` sites are BIP 152 compact-block paths
  (`net_processing.cpp:3683`, `:3984`). Compact blocks are out of Phase 3, so
  `punish=true` is the only reachable case today. **Whoever implements compact
  blocks must add the flag in the same change, or this port will start banning
  peers for innocently relayed compact blocks.**


**Verdict 2026-08-26 (`TestParity_MisbehaviourScores`, one peer per row, both
services):**

| Row | svp2p | legacy | SVNode |
|---|---|---|---|
| multiple-version | 1, keeps | 0, keeps (ignores) | 1, keeps |
| missing-version | 1, keeps | disconnects | 1, keeps |
| unconnected-headers ×10 | 20, keeps | disconnects ("unrequested headers") | 20, keeps |
| non-continuous headers | 20, keeps | disconnects ("unrequested headers") | 20, keeps |
| invalid block BODY (2nd coinbase) | **0, keeps — GAP** | disconnects | 100, bans |
| headers > 2000 | connection fails (go-wire decode) | keeps, swallows | 20, keeps |
| addr > 1000 | connection fails (go-wire decode) | keeps, swallows | 20, keeps |

svp2p matches SVNode on the four scored rows; legacy is harsher. The two decoder
rows cannot reach either scorer: go-wire refuses the payload (svp2p fails the
connection at `transport/conn.go`, legacy's read loop drops it silently). The
invalid-body row is a gap: the error reaches the bridge as TX_ERROR, outside
PeerFault's block-describing allow-list (Task 20), so the peer is neither scored
nor dropped — ledger carried residual 15. Also found while building the rig:
legacy rejects and BANS any peer whose user agent lacks "Bitcoin SV"/"BSV"
(`peer_server.go:617`); svp2p has no such fence — scenario 12 below.


**Update 2026-08-26 (later the same day):** the invalid-body gap is CLOSED —
`createTxMap` now judges a missing or second coinbase as a block failure before
any transaction reaches the validator (91673dd66), and the row asserts svp2p's
DoS(100) and disconnect.

## 5. Multi-peer download distribution

**Tasks 3-7, 6b.**

The lead item of Phase 3. Unit tests pin the rules — rotation release, all-peer
scheduling, the per-block timeout, parallel fetch — but not the distribution they
produce against real peers with real bandwidth.

- **Script:** identical scripted peers, then a deliberately mixed set (one fast,
  one slow, one silent), driving a multi-block download.
- **Observe:** blocks per peer, duplicate fetches, wall-clock to tip, and
  disconnects. Compare against SVNode on the same corpus.
- **Pass:** distribution and time-to-tip no worse than SVNode's; duplicate
  fetches bounded by `legacy_blockDownloadMaxParallelFetch`.

Watch specifically for the download-timeout livelock the ledger carries: at the
default steady-state window a 4 GB block needs 6.7 MB/s from ONE peer, and the
per-peer grant is zero exactly then. Racing covers most of it, but not with a
single useful peer, and not when every holder stalls at the cap.


**Verdict 2026-08-26 (`TestParity_MultiPeerDistribution`, 200 × 200 KB blocks,
fast/slow/silent peers, per-block budget 1 % = 6 s):** svp2p reaches the tip,
drops the silent peer on the carried per-block clock (79f0870ba) and is redialed
by `legacy_connect_peers`; legacy downloads from its single sync peer only.
Distribution is real (both serving peers carry blocks) but so is the cost: svp2p
served **1,316 blocks for a 200-block chain** (~5.6×) because a block arriving
before its parent is refused pre-admission, discarded and fetched again — the
ledger's carried residual 1, measured at "twice" on 36 blocks in Task 21 and at
five times here under a slow in-process ingest. Wall clock: legacy 40 s–2m40s
(depends on which peer its height-ranked election picks first; the silent one
costs a 180 s rotation), svp2p 51–67 s. The row pins the multiplier as a KNOWN
GAP; the rule-derived bound a fix must meet is in the test.

**Update 2026-08-26 (later the same day): CLOSED.** Orphan-block retention
(648f05a8a) spools a block that arrives before its parent into the TempStore and
ingests it when the parent lands; duplicate fetches on this scenario fell from
1,116 to **0**, svp2p 43 s beside legacy's 40 s. The row now asserts the
rule-derived bound.

---

## Phase 3 divergences worth observing at the same time

Not from the plan's watch-list, but the harness is the only thing that can see
them. Each is a deliberate, documented choice.

### 6. getheaders flood limit absent

SVNode disconnects a non-whitelisted peer whose queued getheaders replies pass a
limit (`net_processing.cpp:2974-2988`, `CNode::MonitoredPendingResponses`). It
measures send-queue depth per request type, which this port's transport does not
expose, and there is no whitelist notion either. One peer can ask for 2000
headers per message as fast as we serve them.

- **Script:** a peer issuing continuous `getheaders` at maximum rate.
- **Observe:** our send-queue depth, memory, and whether serving starves other
  peers.
- **Pass:** no unbounded growth. If it grows, the guard must be built — Task 25
  or later. Note Task 10 added a raw send lane to `transport.Conn`; check whether
  queue depth is now cheap to read.


**Verdict 2026-08-26 (`TestParity_GetHeadersFlood`, 300 getheaders in a burst):**
both answer all 300, keep the connection, and show no heap growth (GC-adjusted
delta negative on both). No flood guard needed at this rate; the send-queue depth
SVNode measures is still not exposed.

### 7. No unsolicited self-advertisement

SVNode advertises its local address to outbound peers unprompted
(`net_processing.cpp:1847-1864`, gated on `fListen && !IsInitialBlockDownload()`).
This port advertises only inside a `getaddr` reply, which is legacy's shape. A
node that never self-advertises is less discoverable.

- **Observe:** how many peers learn our address over a session, versus SVNode.


**Verdict 2026-08-26 (`TestParity_NoUnsolicitedSelfAdvertisement`):** over 8 s
after handshake with no getaddr, legacy sent 0 addr, svp2p sent 0 addr; SVNode
would send its own address once. Documented divergence, both sides alike.

### 8. Cold-start bootstrap

Neither DNS seeds (`net.cpp:1622`) nor the fixed-seed fallback
(`net.cpp:1842-1855`) is carried, and feelers (`net.cpp:1897-1918`) are absent so
the tried table is never warmed by probes. A node with an empty address table
cannot bootstrap outbound at all; it needs `legacy_connect_peers` or an inbound
peer.

- **Script:** a node started with no `peers.json` and no configured peers.
- **Observe:** whether it ever reaches `legacy_targetOutboundPeers`.
- **Pass:** this is expected to FAIL today. It is an owner decision, not a bug —
  record the behavior so the decision is made on evidence.


**Verdict 2026-08-26 (`TestParity_ColdStartBootstrap`):** with no configured
peers both sides hold 0 connections after 10 s (regtest legacy has no seeds
either). Expected FAIL, recorded; the owner decision stands.

### 9. Blocks at and over the 4 GiB envelope

A block a basic message header cannot declare is answered `notfound`
(`getdata.go` OPEN QUESTION 5 site) until Phase 4 brings the extended header.
SVNode frames such a payload with an extmsg header (`protocol.cpp:220-237`) and
so never has this branch.

- **Script:** a peer requesting a block above the envelope.
- **Observe:** `notfound` plus the log line, and that the connection survives.
- **Pass:** interop gap confirmed and bounded, not a crash or a stall.


**Verdict 2026-08-26: NOT ADJUDICABLE IN-PROCESS.** The `notfound` branch fires
on a block whose declared size exceeds the basic envelope, which needs a real
block above 4 GiB in the node's store. Needs a fat-block rig; carried.

### 10. Inv-driven getheaders amplification

**Task 25 (59dc6310c), the opposite direction to scenario 6.**

A peer that announces N distinct unknown block hashes draws N getheaders in reply,
one per hash — SVNode's own behavior (`ProcessInvMessage` answers inside the
per-entry loop, `net_processing.cpp:2461-2462`, pushing at `:2489-2493` with that
entry's own hash as hashStop at `:2492`). The rule is not collapsible: hashStop
TRUNCATES the peer's reply, so one getheaders can bound only one announced branch.
The cost is 36 bytes in, a full locator out, per fabricated hash, up to
`wire.MaxInvPerMsg`.

SVNode carries this identically at the identical site, so bounding it here would be
a divergence rather than a port — which is exactly why the harness, not a unit
test, is the right place to judge it.

- **Script:** a peer sending a maximal inv of distinct hashes that no chain
  contains.
- **Observe:** our outbound bytes versus the peer's inbound bytes, and whether
  serving other peers degrades.
- **Pass:** amplification no worse than SVNode's on the identical input. Read this
  together with scenario 6 — that one guards what a peer can make us SERVE, this
  one guards what a peer can make us ASK.


**Verdict 2026-08-26 (`TestParity_InvGetHeadersAmplification`, 500 fabricated
hashes):** svp2p drew exactly 500 getheaders (SVNode-identical); legacy drew 0
getheaders and instead sent a block GETDATA per hash (520 block requests) — the
bsvd shape. Neither side dropped the peer. Amplification is bounded by
`MaxInvPerMsg` on both; recorded, no change.

### 11. Addr forwarding widths

Task 18 ported `RelayAddress` (`net_processing.cpp:998-1041`) including the
daily-hash target pick (`:1010`) and both relay widths (`:1000-1001`). Legacy
dropped forwarding entirely, so there is no Teranode-side behavior to compare
against — only SVNode.

- **Observe:** how many peers each received addr is forwarded to, against SVNode
  on the same input.


**Verdict 2026-08-26 (`TestParity_AddrForwardingWidths`):** an outbound peer
announces one fresh routable address; svp2p forwards it to **2** of the inbound
peers (the reachable width, net_processing.cpp:1000), legacy to 0. Note the
gate: svp2p accepts an unrequested addr from an INBOUND peer only when it is
that peer's own address (addrrelay.go processAddrEntries), so the sender must be
outbound to exercise forwarding.

### 12. User-agent fence

Legacy rejects and bans (24 h) any peer whose user agent contains neither
"Bitcoin SV" nor "BSV" (`services/legacy/peer_server.go:617`, "Only BSV
Blockchain clients are supported"). svp2p accepts any agent; SVNode has no such
fence either. Found 2026-08-26 when the harness's scripted peer, announcing
`/svp2p-scripted-peer:1.0/`, was banned by the legacy leg and accepted by svp2p.

- **Observe:** which agents connect to each over a testnet session.
- **Pass:** an owner decision — carry legacy's fence into svp2p, or drop it at
  cutover as a Teranode-only policy with no SVNode counterpart.

**Verdict 2026-08-26 (`TestParity_UserAgentFence`):** legacy rejects and bans a
peer announcing `/scriptpeer:0.1/`; svp2p accepts it and syncs from it. Pinned;
owner decision at cutover.

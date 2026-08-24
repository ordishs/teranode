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

### 7. No unsolicited self-advertisement

SVNode advertises its local address to outbound peers unprompted
(`net_processing.cpp:1847-1864`, gated on `fListen && !IsInitialBlockDownload()`).
This port advertises only inside a `getaddr` reply, which is legacy's shape. A
node that never self-advertises is less discoverable.

- **Observe:** how many peers learn our address over a session, versus SVNode.

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

### 9. Blocks at and over the 4 GiB envelope

A block a basic message header cannot declare is answered `notfound`
(`getdata.go` OPEN QUESTION 5 site) until Phase 4 brings the extended header.
SVNode frames such a payload with an extmsg header (`protocol.cpp:220-237`) and
so never has this branch.

- **Script:** a peer requesting a block above the envelope.
- **Observe:** `notfound` plus the log line, and that the connection survives.
- **Pass:** interop gap confirmed and bounded, not a crash or a stall.

### 10. Addr forwarding widths

Task 18 ported `RelayAddress` (`net_processing.cpp:998-1041`) including the
daily-hash target pick (`:1010`) and both relay widths (`:1000-1001`). Legacy
dropped forwarding entirely, so there is no Teranode-side behavior to compare
against — only SVNode.

- **Observe:** how many peers each received addr is forwarded to, against SVNode
  on the same input.

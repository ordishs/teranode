//! Forward conflict cascade (`ProcessConflicting`) and its read helpers, ported
//! from `stores/utxo/process_conflicting.go`. This module currently provides the
//! `spends_for_tx` bridge that turns a parsed extended transaction into the
//! `[]Spend` records `Unspend`/`Spend` expect.

use std::collections::{HashMap, HashSet};
use std::time::Duration;

use ba_subtree_bench::hash::Hash;
use ba_subtree_bench::subtree::COINBASE_PLACEHOLDER;
use ba_subtree_bench::tx::Tx;

use crate::store::{IgnoreFlags, Spend, SpendingData, StoreError, UtxoStore};

/// Bounded back-off delays for phase 5's `set_locked` retry. Faithful port of
/// Go `step5RetryDelays` used by `setLockedWithRetry`
/// (stores/utxo/process_conflicting.go:761-779): three attempts at
/// `[0, 50ms, 200ms]`.
const STEP5_RETRY_DELAYS: [Duration; 3] = [
    Duration::from_millis(0),
    Duration::from_millis(50),
    Duration::from_millis(200),
];

/// Build the `Vec<Spend>` for `tx.inputs` in the same shape `unspend`/`spend`
/// expect. Faithful port of Go `spendsForTx` (stores/utxo/process_conflicting.go:612-630):
/// one `Spend` per input i with the spending UTXO identified by the input's
/// previous outpoint, the per-input UTXO hash, and the spender named as this tx
/// at vin i.
pub fn spends_for_tx(tx: &Tx) -> Result<Vec<Spend>, StoreError> {
    let mut spends = Vec::with_capacity(tx.inputs.len());

    for (i, input) in tx.inputs.iter().enumerate() {
        let utxo_hash = tx
            .utxo_hash_from_input(i)
            .map_err(|e| StoreError::Decode(format!("utxo hash for input {i}: {e:?}")))?;

        spends.push(Spend {
            tx_id: input.prev_txid,
            vout: input.vout,
            utxo_hash,
            spending_data: Some(SpendingData {
                tx_id: tx.txid(),
                vin: i as u32,
            }),
            conflicting_tx_id: None,
            block_ids: vec![],
        });
    }

    Ok(spends)
}

/// Compare two counter candidates by `(created_at, hash)`. Faithful port of Go
/// `isOlderCounter` (stores/utxo/process_conflicting.go:576-598). Returns true
/// when `(a_created, a_hash)` sorts strictly before `(b_created, b_hash)`:
/// `created_at` is the primary key, hash bytes are the lexicographic tiebreak.
/// A `created_at == 0` (missing on legacy records) is treated as NEWEST — never
/// preferred over a candidate with a real timestamp.
pub fn is_older_counter(a_created: i64, a_hash: Hash, b_created: i64, b_hash: Hash) -> bool {
    use std::cmp::Ordering;

    // Faithful to Go's switch (576-598). The `both == 0` case and the equal-
    // `created_at` case fall through to the hash compare; the other cases short-
    // circuit. A zero `created_at` is treated as newest.
    match (a_created, b_created) {
        (0, 0) => {}            // fall through to hash compare
        (0, _) => return false, // a unknown-vintage → never older
        (_, 0) => return true,  // b unknown-vintage → a is older
        _ => match a_created.cmp(&b_created) {
            Ordering::Less => return true,
            Ordering::Greater => return false,
            Ordering::Equal => {} // fall through to hash compare
        },
    }

    // equal created_at (or both zero) — lex compare the hash bytes for
    // determinism (first differing byte decides), matching Go's explicit loop.
    for i in 0..a_hash.len() {
        if a_hash[i] != b_hash[i] {
            return a_hash[i] < b_hash[i];
        }
    }

    false
}

/// True iff `tx` has any input spending `(parent, vout)`. Faithful port of Go
/// `candidateSpendsOutput` (stores/utxo/process_conflicting.go:600-608).
pub fn candidate_spends_output(tx: &Tx, parent: &Hash, vout: u32) -> bool {
    tx.inputs
        .iter()
        .any(|input| input.vout == vout && input.prev_txid == *parent)
}

/// Return true iff every input of the demoted tx `D` has
/// `parent.spending_datas[vout]` populated with a non-`None` spender that is not
/// `D` itself. Faithful port of Go `isReverseFullyApplied`
/// (stores/utxo/process_conflicting.go:444-473). Used as the post-`D.conflicting
/// == true` guard to distinguish a fully-applied reverse from a partial one
/// (Mark/Unspend done but Spend(C) failed last time).
///
/// Returns `false` (no error) on any input whose parent record is absent
/// (empty vec), whose `vout >= len`, whose `[vout]` is `None`, or whose
/// `[vout].tx_id == demoted_hash`. Returns `true` only when ALL inputs
/// unambiguously have a non-D spender.
pub async fn is_reverse_fully_applied(
    store: &dyn UtxoStore,
    demoted_tx: &Tx,
    demoted_hash: Hash,
) -> Result<bool, StoreError> {
    for input in &demoted_tx.inputs {
        let parent_hash = input.prev_txid;
        let vout = input.vout as usize;

        let spending_datas = store.get_spending_datas(&parent_hash).await?;

        // Parent absent → Mem returns an empty vec; treated as not-fully-applied.
        if spending_datas.is_empty() {
            return Ok(false);
        }

        if vout >= spending_datas.len() {
            return Ok(false);
        }

        match &spending_datas[vout] {
            None => return Ok(false),
            Some(sd) if sd.tx_id == demoted_hash => return Ok(false),
            Some(_) => {}
        }
    }

    Ok(true)
}

/// Walk the inputs of a demoted tx and return the set of counter txs to restore
/// as canonical spenders. Faithful port of Go `selectCountersForDemotedTx`
/// (stores/utxo/process_conflicting.go:501-569).
///
/// For each `(parent, vout)` the demoted tx spends, candidates are entries in
/// `parent.conflicting_children` that: (1) are not themselves being demoted in
/// this call, (2) have not already been chosen (dedup across inputs via `seen`),
/// (3) are currently `conflicting == true`, and (4) actually spend the same
/// `(parent, vout)`. Among the qualifiers the oldest by `created_at`
/// (lexicographic hash tiebreak, via `is_older_counter`) is chosen — at most one
/// per input. A candidate with no tx body is skipped (Go 540-542).
pub async fn select_counters_for_demoted_tx(
    store: &dyn UtxoStore,
    demoted_tx: &Tx,
    demoted_set: &HashSet<Hash>,
) -> Result<Vec<Hash>, StoreError> {
    let mut seen: HashSet<Hash> = HashSet::new();
    let mut result: Vec<Hash> = Vec::new();

    for input in &demoted_tx.inputs {
        let parent_hash = input.prev_txid;
        let vout = input.vout;

        let candidates = store.get_conflicting_children_bin(&parent_hash).await?;

        let mut best: Option<Hash> = None;
        let mut best_created_at: i64 = 0;

        for candidate in candidates {
            if demoted_set.contains(&candidate) {
                continue;
            }

            if seen.contains(&candidate) {
                continue;
            }

            let candidate_meta = match store.get_tx_meta(&candidate).await? {
                Some(m) if !m.tx_bytes.is_empty() => m,
                _ => continue,
            };

            if !candidate_meta.conflicting {
                continue;
            }

            let candidate_body = Tx::from_bytes(&candidate_meta.tx_bytes).map_err(|e| {
                StoreError::Decode(format!(
                    "[selectCountersForDemotedTx] candidate body: {e:?}"
                ))
            })?;

            if !candidate_spends_output(&candidate_body, &parent_hash, vout) {
                continue;
            }

            if best.is_none()
                || is_older_counter(
                    candidate_meta.created_at,
                    candidate,
                    best_created_at,
                    best.unwrap(),
                )
            {
                best = Some(candidate);
                best_created_at = candidate_meta.created_at;
            }
        }

        if let Some(b) = best {
            seen.insert(b);
            result.push(b);
        }
    }

    Ok(result)
}

/// Reverse the forward conflict cascade applied by `process_conflicting`.
/// Faithful port of Go `ReverseProcessConflicting`
/// (stores/utxo/process_conflicting.go:297-423).
///
/// `demoted_tx_hashes` are the winners from the block being moved back. When a
/// block whose subtree ran `ProcessConflicting` is reorged out, the conflict
/// swap it applied must be undone: the demoted winners become conflicting again,
/// their inputs are unspent, and the original mempool spenders (counters) are
/// restored as canonical.
///
/// For each demoted tx `D` (in input order):
///  1. skip the coinbase placeholder (`[0xFF; 32]`);
///  2. `get_tx_meta(D)` — skip if absent or has no tx body;
///  3. if `D.conflicting` AND `is_reverse_fully_applied(D)` → skip (idempotent);
///  4. `select_counters_for_demoted_tx(D, demoted_set)` → counters to promote;
///  5. `mark_conflicting_recursively([D])` → add the marked order to the
///     cascaded + touched sets;
///  6. `unspend(spends_for_tx(D), false)` so the parents no longer point at `D`;
///  7. per counter `C`: `get_tx_meta(C)` (skip if no body), `spend(spends_for_tx(C),
///     block_height, IgnoreFlags{both true})`, `unmark_conflicting_recursively([C])`
///     → add the cleared hashes to the touched set.
///
/// Returns `(cascaded_to_conflicting, all_touched)`, both insertion-ordered and
/// deduped. Per Go 408-410, an empty `touched_set` returns `(vec![], vec![])`.
pub async fn reverse_process_conflicting(
    store: &dyn UtxoStore,
    block_height: u32,
    demoted_tx_hashes: &[Hash],
) -> Result<(Vec<Hash>, Vec<Hash>), StoreError> {
    if demoted_tx_hashes.is_empty() {
        return Ok((Vec::new(), Vec::new()));
    }

    let demoted_set: HashSet<Hash> = demoted_tx_hashes.iter().copied().collect();

    // Insertion-ordered dedup sets for deterministic output (Go uses maps, whose
    // iteration order is not consensus-affecting; we keep first-seen order).
    let mut cascaded_seen: HashSet<Hash> = HashSet::new();
    let mut cascaded: Vec<Hash> = Vec::new();
    let mut touched_seen: HashSet<Hash> = HashSet::new();
    let mut touched: Vec<Hash> = Vec::new();

    for demoted_hash in demoted_tx_hashes {
        let demoted_hash = *demoted_hash;

        if demoted_hash == COINBASE_PLACEHOLDER {
            continue;
        }

        let demoted_meta = match store.get_tx_meta(&demoted_hash).await? {
            Some(m) if !m.tx_bytes.is_empty() => m,
            _ => continue,
        };

        let demoted_tx = Tx::from_bytes(&demoted_meta.tx_bytes).map_err(|e| {
            StoreError::Decode(format!("[ReverseProcessConflicting] demoted body: {e:?}"))
        })?;

        // Already-reversed guard: D.conflicting=true alone is not sufficient —
        // confirm completion via observable parent state. If fully reversed,
        // skip; otherwise fall through and re-run the idempotent steps.
        if demoted_meta.conflicting
            && is_reverse_fully_applied(store, &demoted_tx, demoted_hash).await?
        {
            continue;
        }

        // Step 1: identify counters per input.
        let counters_to_promote =
            select_counters_for_demoted_tx(store, &demoted_tx, &demoted_set).await?;

        // Step 2: re-mark D + descendants Conflicting=true.
        let (_affected, marked_order) = store.mark_conflicting_recursively(&[demoted_hash]).await?;

        for h in marked_order {
            if cascaded_seen.insert(h) {
                cascaded.push(h);
            }
            if touched_seen.insert(h) {
                touched.push(h);
            }
        }

        // Step 3: unspend D's inputs so parent.spending_datas[vout] no longer
        // points at D.
        let demoted_spends = spends_for_tx(&demoted_tx)?;
        store.unspend(&demoted_spends, false).await?;

        // Step 4 & 5: per counter, re-spend its inputs and un-cascade.
        for counter_hash in counters_to_promote {
            let counter_meta = match store.get_tx_meta(&counter_hash).await? {
                Some(m) if !m.tx_bytes.is_empty() => m,
                _ => continue,
            };

            let counter_tx = Tx::from_bytes(&counter_meta.tx_bytes).map_err(|e| {
                StoreError::Decode(format!("[ReverseProcessConflicting] counter body: {e:?}"))
            })?;

            let counter_spends = spends_for_tx(&counter_tx)?;
            store
                .spend(
                    &counter_spends,
                    block_height,
                    IgnoreFlags {
                        ignore_conflicting: true,
                        ignore_locked: true,
                    },
                )
                .await?;

            let unmarked = store
                .unmark_conflicting_recursively(&[counter_hash])
                .await?;
            for h in unmarked {
                if touched_seen.insert(h) {
                    touched.push(h);
                }
            }
        }
    }

    if touched.is_empty() {
        return Ok((Vec::new(), Vec::new()));
    }

    Ok((cascaded, touched))
}

/// Retry `set_locked` with bounded back-off. Faithful port of Go
/// `setLockedWithRetry` (stores/utxo/process_conflicting.go:761-779): try once
/// immediately, then after 50ms, then after 200ms; return the last error if all
/// attempts fail. Used by phase 5 (the deliberate "do NOT roll back" exception):
/// every other phase is already committed and correct, so a persistent unlock
/// failure is surfaced for operator action rather than triggering a rollback.
async fn set_locked_with_retry(
    store: &dyn UtxoStore,
    hashes: &[Hash],
    value: bool,
) -> Result<(), StoreError> {
    let mut last_err: Option<StoreError> = None;

    for delay in STEP5_RETRY_DELAYS {
        if !delay.is_zero() {
            tokio::time::sleep(delay).await;
        }

        match store.set_locked(hashes, value).await {
            Ok(()) => return Ok(()),
            Err(e) => last_err = Some(e),
        }
    }

    Err(last_err
        .unwrap_or_else(|| StoreError::Backend("set_locked_with_retry: no attempts".into())))
}

/// Forward 5-phase double-spend cascade. Faithful port of Go `ProcessConflicting`
/// (stores/utxo/process_conflicting.go:72-257). Given the `conflicting_tx_hashes`
/// winners (already flagged Conflicting by the caller) and `block_height`:
///
/// 0. **Gather** — for each winner: error if it is the frozen coinbase
///    placeholder; `get_tx_meta` (must be Conflicting OR in `processed`, else
///    error); `get_counter_conflicting` → its losers. Build the unique
///    `losing_tx_hashes` set and keep the winner tx bodies.
/// 1. **Mark** — `mark_conflicting_recursively(losing_tx_hashes)` →
///    `(affected_parent_spends, all_marked)`.
/// 2. **Unspend** — `unspend(affected_parent_spends, true)` then a SEPARATE
///    `set_locked(unique_parent_txids, true)` (db1 I2: the `unspend` UDF does not
///    lock). Record `marked_as_not_spendable` = those unique parent txids.
/// 3. **Spend winners** — for each winner body → `spends_for_tx` →
///    `spend(.., IgnoreFlags{both true})`, accumulating `step3_successful_spends`.
/// 4. **Clear winner flag** — `set_conflicting(conflicting_tx_hashes, false)`.
/// 5. **Unlock parents** — `set_locked_with_retry(marked_as_not_spendable, false)`.
///
/// Returns `(losing_tx_hashes, all_marked_conflicting)`.
///
/// The gather is SEQUENTIAL (Go uses an errgroup; ordering is not
/// consensus-affecting — see the design non-goals). On any error after phase 1
/// commits (and unless phase 5 failed), a deferred compensating rollback runs in
/// reverse order — see `rollback_process_conflicting`.
///
/// ## Option-A divergence: winners are excluded from the loser cascade
///
/// Go's `GetCounterConflictingTxHashes` self-includes the queried tx
/// (`stores/utxo/process_conflicting.go:999` — `counterConflictingMap[txHash] =
/// struct{}{}`). When `ProcessConflicting` unions the per-winner counter sets into
/// `losingTxHashes`, the winners therefore land in `losingTxHashes` →
/// `MarkConflictingRecursively` → `allMarkedHashes` (Go 169-189). In the FORWARD
/// path this is harmless: phase 4 (`SetConflicting(winners, false)`, Go 242) clears
/// the winner flag regardless. But the deferred rollback iterates `allMarkedHashes`
/// to (a) clear the conflicting flag (Go 740-744) and (b) re-spend each body (Go
/// 713-736) — so a rollback would WRONGLY clear the caller-owned winner's
/// conflicting flag and re-spend the winner. This latent Go bug is masked because
/// Go's rollback tests mock `GetCounterConflicting` to return loser-only
/// (`stores/utxo/process_conflicting_rollback_test.go:39-40`), and `step4Committed`
/// is in fact always false when the rollback runs (the only post-phase-4 statement
/// is phase 5, whose failure sets `step5Failed` and SKIPS the rollback, Go 102-104),
/// so the winner is never re-marked true — it only gets cleared.
///
/// We exclude the winners from the loser cascade at BOTH ends: before phase 1
/// (filter them out of `losing_tx_hashes`) and after `mark_conflicting_recursively`
/// (filter them out of `all_marked`, since the BFS can re-reach a winner as a
/// loser's spending descendant). This is equivalent to Go's forward path — winners
/// are still spent by the phase-3 winners loop and cleared by phase 4 — and it makes
/// the deferred rollback deterministic and state-restoring (it never touches a
/// winner's flag or re-spends a winner). `all_marked_conflicting` is consequently
/// the correct "ended Conflicting=true" set: losers + their descendants only.
pub async fn process_conflicting(
    store: &dyn UtxoStore,
    block_height: u32,
    conflicting_tx_hashes: &[Hash],
    processed: &HashMap<Hash, bool>,
) -> Result<(Vec<Hash>, Vec<Hash>), StoreError> {
    // State for the deferred compensating rollback (Go's named flags 83-92).
    let mut step1_committed = false;
    let mut step2_committed = false;
    let mut step4_committed = false;
    let mut step5_failed = false;
    let mut all_marked: Vec<Hash> = Vec::new();
    let mut marked_as_not_spendable: Vec<Hash> = Vec::new();
    let mut step3_successful_spends: Vec<Spend> = Vec::new();

    // Run the forward cascade in a helper so any early `Err` lands here and we can
    // fire the deferred rollback once (mirrors Go's `defer` reading the flags).
    let result = process_conflicting_inner(
        store,
        block_height,
        conflicting_tx_hashes,
        processed,
        &mut step1_committed,
        &mut step2_committed,
        &mut step4_committed,
        &mut step5_failed,
        &mut all_marked,
        &mut marked_as_not_spendable,
        &mut step3_successful_spends,
    )
    .await;

    match result {
        Ok(ok) => Ok(ok),
        Err(original_err) => {
            // Deferred rollback trigger (Go 94-115): only if phase 1 committed and
            // phase 5 did NOT fail. step5_failed deliberately skips the rollback —
            // steps 1-4 are correct; re-introducing conflicts would be worse.
            if step1_committed && !step5_failed {
                let rollback_err = rollback_process_conflicting(
                    store,
                    block_height,
                    conflicting_tx_hashes,
                    &all_marked,
                    &marked_as_not_spendable,
                    &step3_successful_spends,
                    step2_committed,
                    step4_committed,
                )
                .await;

                if let Err(re) = rollback_err {
                    return Err(StoreError::Backend(format!(
                        "[ProcessConflicting] MANUAL INTERVENTION REQUIRED: original={original_err} rollback={re}"
                    )));
                }
            }

            Err(original_err)
        }
    }
}

/// The forward phases 0-5. Splitting them out lets `process_conflicting` catch
/// any phase error and run the deferred rollback exactly once with the commit
/// flags it observed (`&mut` out-params mirror Go's named returns read by the
/// deferred block).
#[allow(clippy::too_many_arguments)]
async fn process_conflicting_inner(
    store: &dyn UtxoStore,
    block_height: u32,
    conflicting_tx_hashes: &[Hash],
    processed: &HashMap<Hash, bool>,
    step1_committed: &mut bool,
    step2_committed: &mut bool,
    step4_committed: &mut bool,
    step5_failed: &mut bool,
    all_marked: &mut Vec<Hash>,
    marked_as_not_spendable: &mut Vec<Hash>,
    step3_successful_spends: &mut Vec<Spend>,
) -> Result<(Vec<Hash>, Vec<Hash>), StoreError> {
    // Option A: the winners must NEVER appear in the loser cascade / marked /
    // unspend / rollback / return set (see the fn doc). Build the winner set once
    // and use it to filter `losing_tx_hashes` (below) and `all_marked` (after BFS).
    let winners: HashSet<Hash> = conflicting_tx_hashes.iter().copied().collect();

    // 0. Gather: fetch winners, verify conflicting, collect losers. SEQUENTIAL.
    let mut winning_tx_bodies: Vec<Tx> = Vec::with_capacity(conflicting_tx_hashes.len());
    let mut losing_seen: HashSet<Hash> = HashSet::new();
    let mut losing_tx_hashes: Vec<Hash> = Vec::new();

    for tx_hash in conflicting_tx_hashes {
        if *tx_hash == COINBASE_PLACEHOLDER {
            // the counter-conflicting tx is frozen, we should not process further.
            return Err(StoreError::Backend(format!(
                "[ProcessConflicting][{tx_hash:02x?}] tx is frozen"
            )));
        }

        let tx_meta = store.get_tx_meta(tx_hash).await?.ok_or_else(|| {
            StoreError::Backend(format!(
                "[ProcessConflicting][{tx_hash:02x?}] error getting tx"
            ))
        })?;

        // Must be conflicting unless it was already processed in this run.
        if !tx_meta.conflicting && processed.get(tx_hash) != Some(&true) {
            return Err(StoreError::Backend(format!(
                "[ProcessConflicting][{tx_hash:02x?}] tx is not conflicting"
            )));
        }

        let counter = store.get_counter_conflicting(*tx_hash).await?;
        for h in counter {
            // Option A: drop any winner from the loser set. Go's GetCounterConflicting
            // self-includes the winner (process_conflicting.go:999); excluding it here
            // keeps winners out of the cascade. A winner that is a genuine spending
            // descendant of a loser it displaces is contradictory input — for db2 we
            // FILTER it (don't error); db4: this should error once the reorg/queue
            // wiring exists.
            if !winners.contains(&h) && losing_seen.insert(h) {
                losing_tx_hashes.push(h);
            }
        }

        let tx = Tx::from_bytes(&tx_meta.tx_bytes)
            .map_err(|e| StoreError::Decode(format!("[ProcessConflicting] winner body: {e:?}")))?;
        winning_tx_bodies.push(tx);
    }

    // 1. Mark losers + spending children conflicting (BFS).
    let (affected_parent_spends, all_marked_hashes) = store
        .mark_conflicting_recursively(&losing_tx_hashes)
        .await?;

    // Option A (second end): the BFS can re-reach a winner as a loser's spending
    // descendant; winners must never be in the marked/unspend/rollback/return set.
    *all_marked = all_marked_hashes
        .into_iter()
        .filter(|h| !winners.contains(h))
        .collect();
    *step1_committed = true;

    // 2. Unspend the affected parent UTXOs, then lock those parents (db1 I2: the
    // unspend UDF does not lock; a separate set_locked does).
    store.unspend(&affected_parent_spends, true).await?;
    *step2_committed = true;

    let mut spendable_seen: HashSet<Hash> = HashSet::new();
    for spend in &affected_parent_spends {
        if spendable_seen.insert(spend.tx_id) {
            marked_as_not_spendable.push(spend.tx_id);
        }
    }

    store.set_locked(marked_as_not_spendable, true).await?;

    // 3. Spend the winners as normal (ignoring conflicting + locked). Our `spend`
    // is all-or-Err (no per-input Err like Go), so step3_successful_spends
    // accumulates the spends of WINNING TXS THAT FULLY SUCCEEDED before the
    // failing one (whole-tx granularity). The rollback still unspends exactly
    // what was spent. (Design "Adaptation note".)
    for tx in &winning_tx_bodies {
        let spends = spends_for_tx(tx)?;
        store
            .spend(
                &spends,
                block_height,
                IgnoreFlags {
                    ignore_conflicting: true,
                    ignore_locked: true,
                },
            )
            .await?;

        step3_successful_spends.extend(spends);
    }

    // 4. Clear the conflicting flag on the winners.
    store.set_conflicting(conflicting_tx_hashes, false).await?;
    *step4_committed = true;

    // 5. Unlock the parents again, with bounded retry. A persistent failure sets
    // step5_failed and surfaces the error WITHOUT rolling back (steps 1-4 are
    // correct; re-introducing conflicts would be strictly worse).
    if let Err(e) = set_locked_with_retry(store, marked_as_not_spendable, false).await {
        *step5_failed = true;
        return Err(e);
    }

    Ok((losing_tx_hashes, all_marked.clone()))
}

/// Deferred compensating rollback. Faithful port of Go
/// `rollbackProcessConflicting` (stores/utxo/process_conflicting.go:681-754):
/// best-effort, reverse-of-forward order, collecting (not aborting on) sub-errors
/// so each step still runs. A non-empty error return means the store may be torn
/// — the caller tags it MANUAL INTERVENTION REQUIRED.
///
/// 1. if step4 committed: `set_conflicting(winners, true)` — re-mark winners.
/// 2. `unspend(step3_successful_spends, false)` — undo partial winner spends
///    (flag_as_locked=false: parents are still locked from step 2; the lock is
///    cleared together at step 5 below).
/// 3. if step2 committed: for each `all_marked` hash (skip frozen placeholder):
///    `get_tx_meta` → `spends_for_tx` → `spend(.., IgnoreFlags{both true})` —
///    re-spend the cascade so original spending_data is restored; best-effort
///    per hash (a missing/unfetchable body is logged via the joined error).
/// 4. `set_conflicting(all_marked, false)` — clear the conflicting flag.
/// 5. if step2 committed: `set_locked(marked_as_not_spendable, false)` — undo
///    the step-2 lock.
#[allow(clippy::too_many_arguments)]
async fn rollback_process_conflicting(
    store: &dyn UtxoStore,
    block_height: u32,
    conflicting_tx_hashes: &[Hash],
    all_marked: &[Hash],
    marked_as_not_spendable: &[Hash],
    step3_successful_spends: &[Spend],
    step2_committed: bool,
    step4_committed: bool,
) -> Result<(), StoreError> {
    let mut errs: Vec<String> = Vec::new();

    // 1. Undo step 4 (re-mark winners conflicting).
    if step4_committed {
        if let Err(e) = store.set_conflicting(conflicting_tx_hashes, true).await {
            errs.push(format!(
                "rollback step 4 (re-mark winners conflicting) failed: {e}"
            ));
        }
    }

    // 2. Undo step 3 partial spends (flag_as_locked=false).
    if !step3_successful_spends.is_empty() {
        if let Err(e) = store.unspend(step3_successful_spends, false).await {
            errs.push(format!(
                "rollback step 3 (unspend partial winning spends) failed: {e}"
            ));
        }
    }

    // 3. Undo step 2: re-spend every cascade tx so original spending_data is
    // restored. Iterate all_marked (the BFS superset), skip the frozen
    // placeholder, best-effort per hash.
    if step2_committed {
        for h in all_marked {
            if *h == COINBASE_PLACEHOLDER {
                continue;
            }

            let tx_meta = match store.get_tx_meta(h).await {
                Ok(Some(m)) => m,
                Ok(None) => {
                    errs.push(format!("rollback step 2 (tx {h:02x?} has no body)"));
                    continue;
                }
                Err(e) => {
                    errs.push(format!("rollback step 2 (fetch tx {h:02x?}) failed: {e}"));
                    continue;
                }
            };

            let tx = match Tx::from_bytes(&tx_meta.tx_bytes) {
                Ok(tx) => tx,
                Err(e) => {
                    errs.push(format!(
                        "rollback step 2 (decode tx {h:02x?}) failed: {e:?}"
                    ));
                    continue;
                }
            };

            let spends = match spends_for_tx(&tx) {
                Ok(s) => s,
                Err(e) => {
                    errs.push(format!(
                        "rollback step 2 (spends_for_tx {h:02x?}) failed: {e}"
                    ));
                    continue;
                }
            };

            if let Err(e) = store
                .spend(
                    &spends,
                    block_height,
                    IgnoreFlags {
                        ignore_conflicting: true,
                        ignore_locked: true,
                    },
                )
                .await
            {
                errs.push(format!(
                    "rollback step 2 (re-spend tx {h:02x?}) failed: {e}"
                ));
            }
        }
    }

    // 4. Undo step 1: clear the conflicting flag on every cascaded hash.
    if !all_marked.is_empty() {
        if let Err(e) = store.set_conflicting(all_marked, false).await {
            errs.push(format!(
                "rollback step 1 (clear conflicting flag) failed: {e}"
            ));
        }
    }

    // 5. Undo the step-2 lock.
    if step2_committed && !marked_as_not_spendable.is_empty() {
        if let Err(e) = store.set_locked(marked_as_not_spendable, false).await {
            errs.push(format!(
                "rollback step 2 lock (set_locked false) failed: {e}"
            ));
        }
    }

    if errs.is_empty() {
        Ok(())
    } else {
        Err(StoreError::Backend(errs.join("; ")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Minimal EXTENDED tx: 1 input (prev=[7;32], vout 3), prev script
    /// [0x76,0xa9,0x88], prev satoshis 1000, 1 dummy output.
    fn ext_tx_one_input() -> Vec<u8> {
        let mut b = Vec::new();
        b.extend_from_slice(&2u32.to_le_bytes()); // version
        b.extend_from_slice(&[0, 0, 0, 0, 0, 0xEF]); // ext marker
        b.push(1); // input count
        b.extend_from_slice(&[7u8; 32]); // prev txid
        b.extend_from_slice(&3u32.to_le_bytes()); // vout
        b.push(0); // empty scriptSig
        b.extend_from_slice(&0xFFFF_FFFFu32.to_le_bytes()); // sequence
        b.extend_from_slice(&1000u64.to_le_bytes()); // prev satoshis
        b.push(3);
        b.extend_from_slice(&[0x76, 0xa9, 0x88]); // prev script
        b.push(1); // output count
        b.extend_from_slice(&500u64.to_le_bytes()); // satoshis
        b.push(1);
        b.push(0x6a); // locking script
        b.extend_from_slice(&0u32.to_le_bytes()); // locktime
        b
    }

    #[test]
    fn spends_for_tx_one_input() {
        let bytes = ext_tx_one_input();
        let tx = Tx::from_bytes(&bytes).unwrap();
        let spends = spends_for_tx(&tx).unwrap();

        assert_eq!(spends.len(), 1);
        assert_eq!(spends[0].tx_id, [7u8; 32]); // prev txid
        assert_eq!(spends[0].vout, 3);
        assert_eq!(spends[0].utxo_hash, tx.utxo_hash_from_input(0).unwrap());

        let sd = spends[0].spending_data.as_ref().unwrap();
        assert_eq!(sd.tx_id, tx.txid()); // spender = this tx
        assert_eq!(sd.vin, 0);
    }

    // ---- ProcessConflicting (phases 0-5) ----

    use std::collections::HashMap as StdHashMap;

    use crate::store::utxo_mem::MemUtxoStore;

    /// Minimal EXTENDED tx with a single input `(prev, vout)` and a 1-byte output
    /// script `out_script` (used to give two txs spending the same UTXO distinct
    /// canonical txids — a real double-spend has different bodies).
    fn ext_tx_out(prev: Hash, vout: u32, out_script: u8) -> Vec<u8> {
        let mut b = Vec::new();
        b.extend_from_slice(&2u32.to_le_bytes()); // version
        b.extend_from_slice(&[0, 0, 0, 0, 0, 0xEF]); // ext marker
        b.push(1); // input count
        b.extend_from_slice(&prev);
        b.extend_from_slice(&vout.to_le_bytes());
        b.push(0); // empty scriptSig
        b.extend_from_slice(&0xFFFF_FFFFu32.to_le_bytes()); // sequence
        b.extend_from_slice(&1000u64.to_le_bytes()); // prev satoshis
        b.push(3);
        b.extend_from_slice(&[0x76, 0xa9, 0x88]); // prev script
        b.push(1); // output count
        b.extend_from_slice(&500u64.to_le_bytes()); // satoshis
        b.push(1);
        b.push(out_script); // locking script (distinguishes txa from txb)
        b.extend_from_slice(&0u32.to_le_bytes()); // locktime
        b
    }

    fn ext_tx(prev: Hash, vout: u32) -> Vec<u8> {
        ext_tx_out(prev, vout, 0x6a)
    }

    /// Minimal EXTENDED tx with two inputs `(prev1, vout1)` and `(prev2, vout2)`.
    fn ext_tx_two_inputs(prev1: Hash, vout1: u32, prev2: Hash, vout2: u32) -> Vec<u8> {
        let mut b = Vec::new();
        b.extend_from_slice(&2u32.to_le_bytes()); // version
        b.extend_from_slice(&[0, 0, 0, 0, 0, 0xEF]); // ext marker
        b.push(2); // input count
        for (prev, vout) in [(prev1, vout1), (prev2, vout2)] {
            b.extend_from_slice(&prev);
            b.extend_from_slice(&vout.to_le_bytes());
            b.push(0); // empty scriptSig
            b.extend_from_slice(&0xFFFF_FFFFu32.to_le_bytes()); // sequence
            b.extend_from_slice(&1000u64.to_le_bytes()); // prev satoshis
            b.push(3);
            b.extend_from_slice(&[0x76, 0xa9, 0x88]); // prev script
        }
        b.push(1); // output count
        b.extend_from_slice(&500u64.to_le_bytes()); // satoshis
        b.push(1);
        b.push(0x6a); // locking script
        b.extend_from_slice(&0u32.to_le_bytes()); // locktime
        b
    }

    /// Build a REAL double-spend scenario where txa and txb have distinct bodies
    /// (hence distinct canonical txids). Pre-call reality: parent P (1 output) is
    /// currently spent by the VALID `txa` (the loser-to-be), which is therefore
    /// `Conflicting=false`; `txb` (the winner in the new block) is pre-marked
    /// `Conflicting=true` by the caller and also spends (P,0). Both txids are the
    /// canonical `Tx::txid()` of their bodies, so the store records exactly what the
    /// cascade computes. The `conflict_parents[txa]` entry makes Phase 1's
    /// `set_conflicting` return P[0] as the affected parent spend (Phase 2 unspends
    /// it; Phase 3 re-spends it with txb). Returns (P, txa, txb, txb_body).
    fn seed_double_spend(store: &MemUtxoStore) -> (Hash, Hash, Hash, Vec<u8>) {
        let parent: Hash = [0x50; 32];

        let txa_body = ext_tx_out(parent, 0, 0x6a);
        let txb_body = ext_tx_out(parent, 0, 0x51); // different output → different txid
        let txa = Tx::from_bytes(&txa_body).unwrap().txid();
        let txb = Tx::from_bytes(&txb_body).unwrap().txid();
        assert_ne!(txa, txb, "double-spend bodies must have distinct txids");

        // Parent P: output 0 currently spent by the valid txa (its canonical txid).
        store.seed_spending_datas(parent, vec![Some(SpendingData { tx_id: txa, vin: 0 })]);

        // txa: the current valid spender (loser-to-be), NOT conflicting pre-call.
        // txb: the winner in the new block, pre-marked conflicting by the caller.
        store.seed_tx(txa, txa_body, false, vec![], 0);
        store.seed_tx(txb, txb_body.clone(), true, vec![], 0);

        // Phase 1's set_conflicting must surface P[0] as an affected parent spend
        // so the cascade unspends and re-spends it. Tie it to txa (the loser whose
        // parent UTXO P[0] is being reclaimed).
        store.set_conflict_parents(
            txa,
            vec![Spend {
                tx_id: parent,
                vout: 0,
                utxo_hash: [0xAB; 32],
                spending_data: Some(SpendingData { tx_id: txa, vin: 0 }),
                conflicting_tx_id: None,
                block_ids: vec![],
            }],
        );

        (parent, txa, txb, txb_body)
    }

    #[tokio::test]
    async fn process_conflicting_happy_path() {
        let store = MemUtxoStore::default();
        let (parent, txa, txb, _txb_body) = seed_double_spend(&store);

        let (losers, all_marked) = process_conflicting(&store, 100, &[txb], &StdHashMap::new())
            .await
            .expect("happy path must succeed");

        // Return values: losers + all_marked contain the displaced original txa,
        // and EXCLUDE the winner txb (Option A).
        assert!(losers.contains(&txa), "losing set must contain txa");
        assert!(
            !losers.contains(&txb),
            "Option A: winner txb must NOT be in the losing set"
        );
        assert!(all_marked.contains(&txa), "all_marked must contain txa");
        assert!(
            !all_marked.contains(&txb),
            "Option A: winner txb must NOT be in all_marked"
        );

        // Observable end-state (not just call order):
        // - winner txb is no longer conflicting (cleared by phase 4).
        assert!(
            !store.get_tx_meta(&txb).await.unwrap().unwrap().conflicting,
            "winner txb must be cleared (Conflicting=false)"
        );
        // - loser txa is now conflicting (marked in phase 1; phase 4 clears only
        //   winners, never losers).
        assert!(
            store.get_tx_meta(&txa).await.unwrap().unwrap().conflicting,
            "loser txa must end Conflicting=true"
        );
        // - parent P[0] now points at the winner txb.
        let p = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(
            p[0],
            Some(SpendingData { tx_id: txb, vin: 0 }),
            "parent P[0] must now be spent by the winner txb"
        );

        // Phase order via recorders: set_conflicting(true) (phase 1) → unspend
        // (phase 2) → spend (phase 3) → set_conflicting(false) (phase 4) →
        // set_locked(false) (phase 5).
        let conflicting = store.conflicting_calls();
        assert!(
            conflicting.iter().any(|(_, v)| *v),
            "phase 1 marks conflicting=true"
        );
        assert!(
            !conflicting.last().unwrap().1,
            "phase 4 clears conflicting (last set_conflicting is false)"
        );
        assert_eq!(
            store.spend_calls().len(),
            1,
            "phase 3 spends the one winner"
        );
        assert_eq!(store.unspend_calls().len(), 1, "phase 2 unspends once");

        // Phase 5 unlock ran: a set_locked(.., false) on the parent.
        let locked = store.locked_calls();
        assert!(
            locked.iter().any(|(h, v)| !*v && h.contains(&parent)),
            "phase 5 must unlock the parent (set_locked false)"
        );
    }

    #[tokio::test]
    async fn process_conflicting_cascades_to_child() {
        let store = MemUtxoStore::default();
        let (_parent, txa, txb, _txb_body) = seed_double_spend(&store);

        // txa has a spending child txc (its body spends (txa,0)); both conflicting.
        let txc_body = ext_tx(txa, 0);
        let txc = Tx::from_bytes(&txc_body).unwrap().txid();
        store.seed_tx(txc, txc_body, true, vec![], 0);
        store.set_conflict_children(txa, vec![txc]);

        let (_losers, all_marked) = process_conflicting(&store, 100, &[txb], &StdHashMap::new())
            .await
            .expect("cascade path must succeed");

        // BFS superset must include both the loser txa and its child txc, and
        // EXCLUDE the winner txb (Option A).
        assert!(all_marked.contains(&txa), "all_marked ⊇ {{txa}}");
        assert!(
            all_marked.contains(&txc),
            "all_marked ⊇ {{txc}} (cascaded child)"
        );
        assert!(
            !all_marked.contains(&txb),
            "Option A: winner txb must NOT be in all_marked"
        );

        // Both losers end Conflicting=true; the winner txb is cleared.
        assert!(store.get_tx_meta(&txa).await.unwrap().unwrap().conflicting);
        assert!(store.get_tx_meta(&txc).await.unwrap().unwrap().conflicting);
        assert!(!store.get_tx_meta(&txb).await.unwrap().unwrap().conflicting);
    }

    #[tokio::test]
    async fn process_conflicting_frozen_winner_errors() {
        let store = MemUtxoStore::default();
        let frozen: Hash = [0xFF; 32];

        let res = process_conflicting(&store, 100, &[frozen], &StdHashMap::new()).await;
        assert!(res.is_err(), "frozen winner must error");

        // No mutation: no spend/unspend/set_conflicting/set_locked calls fired.
        assert!(store.conflicting_calls().is_empty());
        assert!(store.spend_calls().is_empty());
        assert!(store.unspend_calls().is_empty());
        assert!(store.locked_calls().is_empty());
    }

    #[tokio::test]
    async fn process_conflicting_non_conflicting_winner_errors() {
        let store = MemUtxoStore::default();
        let parent: Hash = [0x50; 32];
        let txb: Hash = [0x0B; 32];

        // Winner seeded Conflicting=false and NOT in the processed map.
        store.seed_spending_datas(parent, vec![None]);
        store.seed_tx(txb, ext_tx(parent, 0), false, vec![], 0);

        let res = process_conflicting(&store, 100, &[txb], &StdHashMap::new()).await;
        assert!(
            res.is_err(),
            "a non-conflicting winner not in `processed` must error (Go 146-148)"
        );

        // No cascade mutation fired.
        assert!(store.conflicting_calls().is_empty());
        assert!(store.spend_calls().is_empty());
        assert!(store.unspend_calls().is_empty());
    }

    // ---- Task 7: deferred rollback ----

    #[tokio::test]
    async fn process_conflicting_rolls_back_on_step3_failure() {
        let store = MemUtxoStore::default();
        let (parent, txa, txb, _txb_body) = seed_double_spend(&store);

        // Inject a phase-3 spend failure so the cascade fails AFTER step 1/2 commit.
        store.set_fail_next_spend(true);

        let res = process_conflicting(&store, 100, &[txb], &StdHashMap::new()).await;
        assert!(res.is_err(), "phase-3 failure must surface an error");

        // The store must be restored to its pre-call OBSERVABLE state:
        // - winner txb stays Conflicting=true: Option A keeps winners OUT of the
        //   marked/rollback set, so the rollback never touches txb's flag. Pre-call
        //   txb was Conflicting=true (caller pre-marked it) → still true.
        assert!(
            store.get_tx_meta(&txb).await.unwrap().unwrap().conflicting,
            "rollback must leave winner txb Conflicting=true (untouched; pre-call state)"
        );
        // - loser txa is restored to Conflicting=false: phase 1 marked it true, the
        //   rollback's "undo step 1" (set_conflicting(all_marked, false)) cleared it
        //   back. Pre-call txa was the valid spender → Conflicting=false.
        assert!(
            !store.get_tx_meta(&txa).await.unwrap().unwrap().conflicting,
            "rollback must restore loser txa to Conflicting=false (pre-call state)"
        );
        // - parent P[0] spender restored to txa (rollback re-spends the cascade).
        let p = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(
            p[0],
            Some(SpendingData { tx_id: txa, vin: 0 }),
            "rollback must restore parent P[0] to the original spender txa"
        );
        // - parents unlocked again (rollback's final set_locked false).
        let locked = store.locked_calls();
        assert!(
            locked.iter().any(|(h, v)| !*v && h.contains(&parent)),
            "rollback must unlock the parent (set_locked false)"
        );
    }

    // ---- db3 Task 2: pure helpers ----

    #[test]
    fn is_older_counter_created_at_then_hash() {
        // smaller created_at wins
        assert!(is_older_counter(100, [1u8; 32], 200, [0u8; 32]));
        assert!(!is_older_counter(200, [0u8; 32], 100, [1u8; 32]));
        // created_at==0 is treated as NEWEST (never preferred over a real timestamp)
        assert!(!is_older_counter(0, [0u8; 32], 100, [9u8; 32]));
        assert!(is_older_counter(100, [9u8; 32], 0, [0u8; 32]));
        // both zero -> lexicographic hash tiebreak
        assert!(is_older_counter(0, [1u8; 32], 0, [2u8; 32]));
        // equal created_at -> lexicographic hash tiebreak
        assert!(is_older_counter(50, [1u8; 32], 50, [2u8; 32]));
        assert!(!is_older_counter(50, [2u8; 32], 50, [1u8; 32]));
    }

    #[test]
    fn candidate_spends_output_matches_input() {
        // candidate tx spending (P=[7;32], vout 3)
        let bytes = ext_tx_out([7u8; 32], 3, 0x6a);
        let tx = Tx::from_bytes(&bytes).unwrap();
        assert!(candidate_spends_output(&tx, &[7u8; 32], 3));
        assert!(!candidate_spends_output(&tx, &[7u8; 32], 4)); // wrong vout
        assert!(!candidate_spends_output(&tx, &[8u8; 32], 3)); // wrong parent
    }

    // ---- db3 Task 3: is_reverse_fully_applied + select_counters_for_demoted_tx ----

    #[tokio::test]
    async fn is_reverse_fully_applied_true_when_all_inputs_non_d_spender() {
        let store = MemUtxoStore::default();
        let p: Hash = [0x50; 32];
        let q: Hash = [0x51; 32];
        let d: Hash = [0x0D; 32];
        let c: Hash = [0xC1; 32];
        let c2: Hash = [0xC2; 32];

        // D spends (P,0) and (Q,0).
        let d_body = ext_tx_two_inputs(p, 0, q, 0);
        let d_tx = Tx::from_bytes(&d_body).unwrap();

        // Both parents now point at non-D spenders.
        store.seed_spending_datas(p, vec![Some(SpendingData { tx_id: c, vin: 0 })]);
        store.seed_spending_datas(q, vec![Some(SpendingData { tx_id: c2, vin: 0 })]);

        assert!(is_reverse_fully_applied(&store, &d_tx, d).await.unwrap());
    }

    #[tokio::test]
    async fn is_reverse_fully_applied_false_when_input_points_at_d() {
        let store = MemUtxoStore::default();
        let p: Hash = [0x50; 32];
        let d: Hash = [0x0D; 32];

        let d_body = ext_tx(p, 0);
        let d_tx = Tx::from_bytes(&d_body).unwrap();

        // P[0] still points at D → reverse not fully applied.
        store.seed_spending_datas(p, vec![Some(SpendingData { tx_id: d, vin: 0 })]);

        assert!(!is_reverse_fully_applied(&store, &d_tx, d).await.unwrap());
    }

    #[tokio::test]
    async fn is_reverse_fully_applied_false_when_input_none() {
        let store = MemUtxoStore::default();
        let p: Hash = [0x50; 32];
        let d: Hash = [0x0D; 32];

        let d_body = ext_tx(p, 0);
        let d_tx = Tx::from_bytes(&d_body).unwrap();

        // P[0] is None (post-Unspend, pre-Spend state) → not fully applied.
        store.seed_spending_datas(p, vec![None]);

        assert!(!is_reverse_fully_applied(&store, &d_tx, d).await.unwrap());
    }

    #[tokio::test]
    async fn select_counters_picks_oldest_qualifying() {
        let store = MemUtxoStore::default();
        let p: Hash = [0x50; 32];
        let d: Hash = [0x0D; 32];
        let c1: Hash = [0xC1; 32];
        let c2: Hash = [0xC2; 32];

        // D spends (P,0).
        let d_body = ext_tx(p, 0);
        let d_tx = Tx::from_bytes(&d_body).unwrap();

        // Parent P has two conflicting children C1, C2, both spending (P,0).
        store.seed_conflicting_children(p, vec![c1, c2]);
        // C1.created_at=200, C2.created_at=100 → C2 (older) selected.
        store.seed_tx(c1, ext_tx(p, 0), true, vec![], 200);
        store.seed_tx(c2, ext_tx(p, 0), true, vec![], 100);

        let demoted_set: HashSet<Hash> = [d].into_iter().collect();
        let counters = select_counters_for_demoted_tx(&store, &d_tx, &demoted_set)
            .await
            .unwrap();

        assert_eq!(counters, vec![c2], "oldest created_at qualifier selected");
    }

    #[tokio::test]
    async fn select_counters_excludes_demoted_and_nonconflicting_and_nonspender() {
        let store = MemUtxoStore::default();
        let p: Hash = [0x50; 32];
        let d: Hash = [0x0D; 32];
        let in_demoted: Hash = [0xDD; 32]; // in demoted_set → excluded
        let non_conflicting: Hash = [0xE1; 32];
        let non_spender: Hash = [0xE2; 32]; // spends a different output → excluded

        let d_body = ext_tx(p, 0);
        let d_tx = Tx::from_bytes(&d_body).unwrap();

        store.seed_conflicting_children(p, vec![in_demoted, non_conflicting, non_spender]);
        // in_demoted: conflicting + spends (P,0) but in demoted_set.
        store.seed_tx(in_demoted, ext_tx(p, 0), true, vec![], 10);
        // non_conflicting: spends (P,0) but conflicting=false.
        store.seed_tx(non_conflicting, ext_tx(p, 0), false, vec![], 10);
        // non_spender: conflicting but spends (P,1), not (P,0).
        store.seed_tx(non_spender, ext_tx(p, 1), true, vec![], 10);

        let demoted_set: HashSet<Hash> = [d, in_demoted].into_iter().collect();
        let counters = select_counters_for_demoted_tx(&store, &d_tx, &demoted_set)
            .await
            .unwrap();

        assert!(counters.is_empty(), "no candidate qualifies for that input");
    }

    #[tokio::test]
    async fn process_conflicting_step5_failure_no_rollback() {
        let store = MemUtxoStore::default();
        let (parent, txa, txb, _txb_body) = seed_double_spend(&store);

        // Inject a phase-5 set_locked(false) failure. The bounded retry exhausts
        // and step5_failed is set → NO rollback (steps 1-4 must persist).
        store.set_fail_set_locked_false(true);

        let res = process_conflicting(&store, 100, &[txb], &StdHashMap::new()).await;
        assert!(res.is_err(), "phase-5 failure must surface an error");

        // Steps 1-4 PERSIST (no rollback):
        // - winner txb cleared (phase 4 ran).
        assert!(
            !store.get_tx_meta(&txb).await.unwrap().unwrap().conflicting,
            "step5 failure must NOT roll back: winner txb stays Conflicting=false"
        );
        // - loser txa stays conflicting.
        assert!(
            store.get_tx_meta(&txa).await.unwrap().unwrap().conflicting,
            "step5 failure must NOT roll back: loser txa stays Conflicting=true"
        );
        // - parent P[0] still spent by the winner txb (phase 3 persisted).
        let p = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(
            p[0],
            Some(SpendingData { tx_id: txb, vin: 0 }),
            "step5 failure must NOT roll back: parent stays spent by txb"
        );
    }

    // ---- db3 Task 4: reverse_process_conflicting orchestration ----

    #[tokio::test]
    async fn reverse_happy_path_restores_counter() {
        let store = MemUtxoStore::default();
        let parent: Hash = [0x50; 32];

        // D (the db2 winner) and C (the original loser) both spend (P,0). Distinct
        // output scripts → distinct canonical txids (a real double-spend).
        let d_body = ext_tx_out(parent, 0, 0x51);
        let c_body = ext_tx_out(parent, 0, 0x52);
        let d = Tx::from_bytes(&d_body).unwrap().txid();
        let c = Tx::from_bytes(&c_body).unwrap().txid();
        assert_ne!(d, c);

        // Pre-call db2 end-state: P[0] currently spent by D, D not conflicting.
        store.seed_spending_datas(parent, vec![Some(SpendingData { tx_id: d, vin: 0 })]);
        store.seed_tx(d, d_body, false, vec![], 0);
        // Counter C: in P.conflicting_children, conflicting=true, spends (P,0).
        store.seed_conflicting_children(parent, vec![c]);
        store.seed_tx(c, c_body, true, vec![], 100);

        let (cascaded, all_touched) = reverse_process_conflicting(&store, 100, &[d])
            .await
            .expect("reverse happy path must succeed");

        // D re-marked conflicting.
        assert!(
            store.get_tx_meta(&d).await.unwrap().unwrap().conflicting,
            "D must end Conflicting=true"
        );
        assert!(cascaded.contains(&d), "cascaded must contain D");

        // Counter C restored as the parent's spender.
        let p = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(
            p[0],
            Some(SpendingData { tx_id: c, vin: 0 }),
            "parent P[0] must be restored to the counter C"
        );

        // Counter C un-conflicted.
        assert!(
            !store.get_tx_meta(&c).await.unwrap().unwrap().conflicting,
            "counter C must end Conflicting=false"
        );

        // all_touched ⊇ {D, C}.
        assert!(all_touched.contains(&d), "all_touched ⊇ {{D}}");
        assert!(all_touched.contains(&c), "all_touched ⊇ {{C}}");
    }

    #[tokio::test]
    async fn reverse_skips_fully_applied() {
        let store = MemUtxoStore::default();
        let parent: Hash = [0x50; 32];
        let c: Hash = [0xC1; 32];

        let d_body = ext_tx(parent, 0);
        let d = Tx::from_bytes(&d_body).unwrap().txid();

        // D.conflicting=true AND P[0] points at a non-D spender C → fully applied.
        store.seed_spending_datas(parent, vec![Some(SpendingData { tx_id: c, vin: 0 })]);
        store.seed_tx(d, d_body, true, vec![], 0);

        let (cascaded, all_touched) = reverse_process_conflicting(&store, 100, &[d])
            .await
            .expect("fully-applied D must be a no-op");

        assert!(cascaded.is_empty(), "no cascade for a fully-applied D");
        assert!(all_touched.is_empty(), "no touch for a fully-applied D");

        // No mutation recorded for D: no spend/unspend fired.
        assert!(store.spend_calls().is_empty(), "no spend for skipped D");
        assert!(store.unspend_calls().is_empty(), "no unspend for skipped D");
        assert!(
            store.conflicting_calls().is_empty(),
            "no set_conflicting for skipped D"
        );
    }

    #[tokio::test]
    async fn reverse_no_counter_marks_d_only() {
        let store = MemUtxoStore::default();
        let parent: Hash = [0x50; 32];

        let d_body = ext_tx(parent, 0);
        let d = Tx::from_bytes(&d_body).unwrap().txid();

        // P[0] currently spent by D, D not conflicting; P has NO conflicting child.
        store.seed_spending_datas(parent, vec![Some(SpendingData { tx_id: d, vin: 0 })]);
        store.seed_tx(d, d_body, false, vec![], 0);

        let (cascaded, all_touched) = reverse_process_conflicting(&store, 100, &[d])
            .await
            .expect("no-counter reverse must succeed");

        // D still marked conflicting + unspent; no counter restored.
        assert!(
            store.get_tx_meta(&d).await.unwrap().unwrap().conflicting,
            "D must be marked Conflicting=true"
        );
        assert!(cascaded.contains(&d), "cascaded must contain D");
        assert!(all_touched.contains(&d), "all_touched must contain D");

        // D's input was unspent → P[0] cleared to None (no counter re-spent it).
        let p = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(p[0], None, "no counter restored → P[0] left cleared");

        // No counter spend fired (only the unspend of D's inputs).
        assert!(store.spend_calls().is_empty(), "no counter → no spend call");
        assert_eq!(store.unspend_calls().len(), 1, "D's inputs unspent once");
    }

    #[tokio::test]
    async fn reverse_coinbase_placeholder_skipped() {
        let store = MemUtxoStore::default();
        let frozen: Hash = [0xFF; 32];

        let (cascaded, all_touched) = reverse_process_conflicting(&store, 100, &[frozen])
            .await
            .expect("coinbase placeholder must be skipped without error");

        assert!(cascaded.is_empty());
        assert!(all_touched.is_empty());
        // No mutation fired.
        assert!(store.spend_calls().is_empty());
        assert!(store.unspend_calls().is_empty());
        assert!(store.conflicting_calls().is_empty());
    }

    #[tokio::test]
    async fn reverse_empty_demoted_returns_empty() {
        let store = MemUtxoStore::default();
        let (cascaded, all_touched) = reverse_process_conflicting(&store, 100, &[])
            .await
            .expect("empty demoted set must succeed");
        assert!(cascaded.is_empty());
        assert!(all_touched.is_empty());
    }
}

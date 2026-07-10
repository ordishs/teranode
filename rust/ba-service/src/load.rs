//! Boot-time unmined-tx load planning. The PURE part of `main.rs`'s startup
//! sequence, extracted so the §10 candidate-parity ordering is unit-testable and
//! the offline parity test exercises the REAL code path (not a reimplementation).
//!
//! Mirrors Go's `BlockAssembler.loadUnminedTransactions` partition + sort:
//!  - a tx already mined on the best chain (block_ids ∩ best_ids) -> mark, NOT added
//!  - a locked tx -> KEPT (added like any other tx) AND its hash collected into
//!    `unlock`, so the caller can `set_locked(.., false)` after the load — exactly
//!    what Go does (BlockAssembler.go: locked txs are appended to the subtree
//!    processor, tracked in `lockedTxs`, then unlocked via `SetLocked(.., false)`).
//!  - otherwise -> keep, sorted by `created_at` ascending (oldest first)

use std::collections::HashSet;

use ba_subtree_bench::hash::Hash;

use crate::store::UnminedTx;

/// The outcome of partitioning + sorting the unmined set at boot.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LoadPlan {
    /// Txs to add to the assembly, in `created_at` ascending (oldest-first) order.
    /// Includes locked txs (Go parity — they are added, not skipped).
    pub keep_sorted: Vec<UnminedTx>,
    /// Txs already mined on the best chain — mark on longest chain, do NOT add.
    pub mark_on_longest: Vec<Hash>,
    /// Hashes of locked txs that were added to the candidate and must be unlocked
    /// after the load via `UtxoStore::set_locked(.., false)` (Go parity).
    pub unlock: Vec<Hash>,
}

/// Partition the unmined set against the best-chain header-ID set and sort the
/// kept txs by `created_at` ascending. Pure: no I/O, deterministic for a given
/// input. An empty `best_ids` disables the best-chain filter (every non-mined
/// tx is kept) — used by the offline parity test.
///
/// Locked txs that are NOT already mined on the best chain are KEPT (added to
/// the candidate, Go parity) and their hashes collected into `unlock`. A tx
/// that is already mined on the best chain goes to `mark_on_longest` and is NOT
/// added, regardless of its locked flag (Go's already-mined check runs first).
pub fn plan_unmined_load(txs: Vec<UnminedTx>, best_ids: &HashSet<u32>) -> LoadPlan {
    let mut mark_on_longest: Vec<Hash> = Vec::new();
    let mut keep: Vec<UnminedTx> = Vec::new();
    let mut unlock: Vec<Hash> = Vec::new();

    for t in txs {
        let already_mined = t.block_ids.iter().any(|id| best_ids.contains(id));
        if already_mined {
            mark_on_longest.push(t.txid);
            continue;
        }
        if t.locked {
            unlock.push(t.txid);
        }
        keep.push(t);
    }

    // Go: sort.Slice(... CreatedAt < CreatedAt). Stable so ties keep sindex order.
    keep.sort_by_key(|t| t.created_at);

    LoadPlan {
        keep_sorted: keep,
        mark_on_longest,
        unlock,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tx(txid_byte: u8, created_at: i64, block_ids: Vec<u32>, locked: bool) -> UnminedTx {
        UnminedTx {
            txid: [txid_byte; 32],
            fee: 1,
            size: 1,
            block_ids,
            locked,
            created_at,
        }
    }

    #[test]
    fn keeps_are_sorted_by_created_at_ascending() {
        // Deliberately reverse-ordered created_at so the sort must reorder.
        let txs = vec![
            tx(1, 30, vec![], false),
            tx(2, 10, vec![], false),
            tx(3, 20, vec![], false),
        ];
        let plan = plan_unmined_load(txs, &HashSet::new());
        let order: Vec<i64> = plan.keep_sorted.iter().map(|t| t.created_at).collect();
        assert_eq!(order, vec![10, 20, 30]);
        let ids: Vec<u8> = plan.keep_sorted.iter().map(|t| t.txid[0]).collect();
        assert_eq!(ids, vec![2, 3, 1]);
    }

    #[test]
    fn partitions_mined_locked_and_kept() {
        let best: HashSet<u32> = [100, 200].into_iter().collect();
        let txs = vec![
            tx(1, 5, vec![100], false), // mined on best chain -> mark
            tx(2, 4, vec![], true),     // locked -> kept AND unlocked (Go parity)
            tx(3, 3, vec![], false),    // kept
            tx(4, 2, vec![999], false), // block_id not on best chain -> kept
        ];
        let plan = plan_unmined_load(txs, &best);

        assert_eq!(plan.mark_on_longest, vec![[1u8; 32]]);

        // The locked tx (tx2) is now collected for unlocking, not skipped.
        assert_eq!(plan.unlock, vec![[2u8; 32]]);

        let kept_ids: Vec<u8> = plan.keep_sorted.iter().map(|t| t.txid[0]).collect();
        // Locked tx2 is included; sorted by created_at asc: tx4(2), tx3(3), tx2(4).
        assert_eq!(kept_ids, vec![4, 3, 2]);
    }

    #[test]
    fn locked_tx_mined_on_best_chain_is_marked_not_unlocked() {
        // Already-mined check runs first: a locked tx that is mined on the best
        // chain goes to mark_on_longest and is NOT added/unlocked (Go order).
        let best: HashSet<u32> = [100].into_iter().collect();
        let txs = vec![tx(1, 5, vec![100], true)];
        let plan = plan_unmined_load(txs, &best);

        assert_eq!(plan.mark_on_longest, vec![[1u8; 32]]);
        assert!(plan.unlock.is_empty());
        assert!(plan.keep_sorted.is_empty());
    }

    #[test]
    fn empty_best_ids_disables_filter() {
        let txs = vec![tx(1, 1, vec![1, 2, 3], false)];
        let plan = plan_unmined_load(txs, &HashSet::new());
        assert!(plan.mark_on_longest.is_empty());
        assert_eq!(plan.keep_sorted.len(), 1);
    }
}

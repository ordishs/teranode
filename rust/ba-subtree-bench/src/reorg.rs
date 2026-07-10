//! A faithful *model* of the SubtreeProcessor reorg reconciliation's in-memory
//! work — the allocation/GC-heavy operations that motivate the Rust evaluation.
//! NOT a line-by-line port of `reorgBlocks` (which is wired into channels / blob
//! store / utxoStore and isn't standalone-callable); the real port lands in
//! Gate 2 with the stores wired. This models the data shape:
//!
//!   - moveBack:    mass-insert returning txs into the unmined set,
//!   - reverse cascade: evict every Nth moved-back tx (conflicting),
//!   - moveForward: mass-remove newly-mined txs,
//!   - losing conflicts: remove every Nth remaining tx,
//!   - mark lists:  allocate mark-on-chain / mark-false vectors,
//!   - rebuild:     re-chunk the surviving unmined set into subtrees + roots.
//!
//! The end-state (sorted chained roots) depends only on final set membership, so
//! it is deterministic across Go/Rust HashSet iteration order — enabling
//! cross-implementation golden validation.

use std::collections::HashSet;

use crate::hash::{sha256d, Hash};
use crate::subtree::Subtree;

#[derive(Clone, Copy)]
pub struct ReorgParams {
    /// Starting unmined assembly size.
    pub u: u32,
    /// Transactions per block.
    pub block: u32,
    /// Blocks moved back (un-mined).
    pub d_back: u32,
    /// Blocks moved forward (newly mined on the winning chain).
    pub d_fwd: u32,
    /// Conflict stride: every Nth tx is treated as conflicting. `0` disables
    /// conflicts entirely (the pure structural / D-a path — no reverse cascade,
    /// no losing-conflict eviction).
    pub conflict_stride: u32,
    /// Subtree capacity for the rebuild.
    pub cap: usize,
}

#[inline]
fn leaf(i: u32) -> Hash {
    sha256d(&i.to_le_bytes())
}

/// Run the reconciliation model; returns the sorted chained-subtree roots of the
/// rebuilt assembly (the end-state used for golden comparison).
pub fn reorg_reconcile(p: &ReorgParams) -> Vec<Hash> {
    // Starting assembly: leaf(0..u).
    let mut unmined: HashSet<Hash> = (0..p.u).map(leaf).collect();

    // moveBack: returning txs from the move-back blocks re-enter the assembly.
    let mb = p.d_back * p.block;
    let mut moved_back: Vec<u32> = Vec::with_capacity(mb as usize);
    for i in p.u..(p.u + mb) {
        unmined.insert(leaf(i));
        moved_back.push(i);
    }

    // Reverse-cascade conflicting: evict every conflict_stride-th moved-back tx.
    // conflict_stride == 0 disables conflicts (D-a structural-only path).
    let mut reverse_cascade: HashSet<Hash> = HashSet::new();
    if p.conflict_stride != 0 {
        for &i in &moved_back {
            if i % p.conflict_stride == 0 {
                let h = leaf(i);
                unmined.remove(&h);
                reverse_cascade.insert(h);
            }
        }
    }

    // moveForward: the first d_fwd*block starting txs are now mined → removed.
    let fb = (p.d_fwd * p.block).min(p.u);
    for i in 0..fb {
        unmined.remove(&leaf(i));
    }

    // Losing conflicts: remove every conflict_stride-th remaining starting tx.
    // conflict_stride == 0 disables conflicts (no losing-conflict eviction).
    let mut losing: Vec<Hash> = Vec::new();
    if p.conflict_stride != 0 {
        let mut i = fb;
        while i < p.u {
            let h = leaf(i);
            if unmined.remove(&h) {
                losing.push(h);
            }
            i += p.conflict_stride;
        }
    }

    // Mark lists (allocations mirrored from reorgBlocks).
    let _mark_on_chain: Vec<Hash> = moved_back
        .iter()
        .map(|&i| leaf(i))
        .filter(|h| !reverse_cascade.contains(h))
        .collect();
    let mut mark_false: Vec<Hash> = unmined.iter().copied().collect();
    mark_false.extend_from_slice(&losing);

    // Rebuild: deterministic order (sorted) so Go/Rust agree on roots.
    let mut remaining: Vec<Hash> = unmined.into_iter().collect();
    remaining.sort_unstable();

    let mut roots = Vec::new();
    for chunk in remaining.chunks(p.cap) {
        let mut st = Subtree::new();
        for (j, h) in chunk.iter().enumerate() {
            st.add_node(*h, j as u64, j as u64);
        }
        roots.push(st.root_hash().expect("non-empty chunk"));
    }
    roots
}

#[cfg(test)]
mod tests {
    use super::{reorg_reconcile, ReorgParams};

    #[test]
    fn reconcile_is_deterministic() {
        let p = ReorgParams {
            u: 10_000,
            block: 2_000,
            d_back: 2,
            d_fwd: 3,
            conflict_stride: 100,
            cap: 1024,
        };
        let a = reorg_reconcile(&p);
        let b = reorg_reconcile(&p);
        assert_eq!(a, b, "reconciliation must be deterministic");
        assert!(!a.is_empty());
    }
}

//! Pure reorg block-list construction: given the current tip and the new tip,
//! walk both chains (via a header lookup closure) back to their common ancestor
//! and produce the `move_back` / `move_forward` block lists. No I/O — the header
//! lookup is injected, so the walk logic is unit-tested deterministically over a
//! synthetic fork graph.
//!
//! Mirrors Go `BlockAssembler.getReorgBlockHeaders` (`BlockAssembler.go:1421`):
//! find the common ancestor, then the blocks from the current tip down to (but
//! excluding) the ancestor are moved back (re-add their orphaned txs), and the
//! blocks from ancestor+1 up to the new tip are moved forward (drop their txs).

use ba_subtree_bench::hash::Hash;

use crate::store::BlockHeaderInfo;

/// The two block lists a reorg produces.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ReorgLists {
    /// Blocks orphaned by the reorg: current tip down to (excluding) the common
    /// ancestor, DESCENDING (tip first). Their txs return to the assembly.
    pub move_back: Vec<Hash>,
    /// Blocks adopted by the reorg: common-ancestor+1 up to the new tip,
    /// ASCENDING (oldest first). Their txs are dropped from the assembly.
    pub move_forward: Vec<Hash>,
}

/// Walk both chains to their common ancestor and build the reorg block lists.
///
/// `get_header(hash) -> Option<BlockHeaderInfo>` returns a block's `prev_hash`
/// and `height`; `None` means the block is unknown (a broken/missing chain). The
/// walk is bounded by `max_walk` total back-steps — if no common ancestor is
/// found within that bound, an `Err` is returned (Go's "reorg is too big" /
/// "common ancestor not found" guard).
///
/// Algorithm:
///   1. If the tips are equal there is no reorg → empty lists.
///   2. Pull the DEEPER tip up (back via `prev_hash`) until both pointers sit at
///      the same height, recording each stepped-back hash on the appropriate
///      list.
///   3. Step BOTH pointers back in lockstep until the hashes match (the common
///      ancestor), recording each pre-step hash on both lists.
///   4. `move_forward` is built tip→ancestor (descending) during the walk, so
///      reverse it to ascending before returning.
pub fn common_ancestor_and_lists(
    current_tip: Hash,
    current_height: u32,
    new_tip: Hash,
    new_height: u32,
    get_header: impl Fn(&Hash) -> Option<BlockHeaderInfo>,
    max_walk: u32,
) -> Result<ReorgLists, String> {
    // No reorg: same tip.
    if current_tip == new_tip {
        return Ok(ReorgLists::default());
    }

    let mut move_back: Vec<Hash> = Vec::new();
    let mut move_forward_desc: Vec<Hash> = Vec::new();

    let mut cur = current_tip;
    let mut cur_h = current_height;
    let mut new = new_tip;
    let mut new_h = new_height;

    let mut steps: u32 = 0;
    let step = |steps: &mut u32, max_walk: u32| -> Result<(), String> {
        *steps += 1;
        if *steps > max_walk {
            return Err(format!(
                "common ancestor not found within {max_walk} blocks (reorg too big)"
            ));
        }
        Ok(())
    };

    // 1. Pull the deeper chain up to the shallower chain's height.
    while cur_h > new_h {
        move_back.push(cur);
        let info =
            get_header(&cur).ok_or_else(|| format!("missing header {}", hex::encode(cur)))?;
        cur = info.prev_hash;
        cur_h -= 1;
        step(&mut steps, max_walk)?;
    }

    while new_h > cur_h {
        move_forward_desc.push(new);
        let info =
            get_header(&new).ok_or_else(|| format!("missing header {}", hex::encode(new)))?;
        new = info.prev_hash;
        new_h -= 1;
        step(&mut steps, max_walk)?;
    }

    // 2. Step both back in lockstep until the hashes converge (the ancestor).
    while cur != new {
        move_back.push(cur);
        move_forward_desc.push(new);

        let cur_info =
            get_header(&cur).ok_or_else(|| format!("missing header {}", hex::encode(cur)))?;
        let new_info =
            get_header(&new).ok_or_else(|| format!("missing header {}", hex::encode(new)))?;
        cur = cur_info.prev_hash;
        new = new_info.prev_hash;

        step(&mut steps, max_walk)?;
    }

    // `cur == new` here is the common ancestor (excluded from both lists).
    // move_forward was collected tip→ancestor (descending); reverse to ascending.
    move_forward_desc.reverse();

    Ok(ReorgLists {
        move_back,
        move_forward: move_forward_desc,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    /// Hash for synthetic block `id`.
    fn h(id: u8) -> Hash {
        [id; 32]
    }

    /// A tiny fork graph: maps block-hash -> (prev_hash, height).
    struct Graph {
        m: HashMap<Hash, BlockHeaderInfo>,
    }

    impl Graph {
        fn new() -> Self {
            Self { m: HashMap::new() }
        }

        /// Add block `id` at `height` with parent `prev`.
        fn add(&mut self, id: u8, prev: u8, height: u32) -> &mut Self {
            self.m.insert(
                h(id),
                BlockHeaderInfo {
                    prev_hash: h(prev),
                    height,
                },
            );
            self
        }

        fn lookup(&self) -> impl Fn(&Hash) -> Option<BlockHeaderInfo> + '_ {
            move |hash: &Hash| self.m.get(hash).cloned()
        }
    }

    #[test]
    fn same_tip_is_empty() {
        let g = Graph::new();
        let r = common_ancestor_and_lists(h(5), 10, h(5), 10, g.lookup(), 100).unwrap();
        assert!(r.move_back.is_empty());
        assert!(r.move_forward.is_empty());
    }

    #[test]
    fn one_back_one_forward_equal_length_fork() {
        // ancestor A(h1) -> current B(h2); ancestor A(h1) -> new C(h2).
        let mut g = Graph::new();
        g.add(2, 1, 2).add(3, 1, 2);

        let r = common_ancestor_and_lists(h(2), 2, h(3), 2, g.lookup(), 100).unwrap();
        assert_eq!(r.move_back, vec![h(2)], "orphan current tip B");
        assert_eq!(r.move_forward, vec![h(3)], "adopt new tip C");
    }

    #[test]
    fn deeper_new_chain() {
        // ancestor A(h1).
        // current: A -> B(h2).
        // new:     A -> X(h2) -> Y(h3) -> Z(h4).
        let mut g = Graph::new();
        g.add(2, 1, 2); // B
        g.add(10, 1, 2).add(11, 10, 3).add(12, 11, 4); // X,Y,Z

        let r = common_ancestor_and_lists(h(2), 2, h(12), 4, g.lookup(), 100).unwrap();
        assert_eq!(r.move_back, vec![h(2)], "orphan B");
        assert_eq!(
            r.move_forward,
            vec![h(10), h(11), h(12)],
            "adopt X,Y,Z ascending"
        );
    }

    #[test]
    fn deeper_old_chain() {
        // ancestor A(h1).
        // current: A -> P(h2) -> Q(h3) -> R(h4).
        // new:     A -> S(h2).
        let mut g = Graph::new();
        g.add(20, 1, 2).add(21, 20, 3).add(22, 21, 4); // P,Q,R
        g.add(30, 1, 2); // S

        let r = common_ancestor_and_lists(h(22), 4, h(30), 2, g.lookup(), 100).unwrap();
        assert_eq!(
            r.move_back,
            vec![h(22), h(21), h(20)],
            "orphan R,Q,P descending"
        );
        assert_eq!(r.move_forward, vec![h(30)], "adopt S");
    }

    #[test]
    fn unequal_fork_after_height_align() {
        // ancestor A(h1).
        // current: A -> B(h2) -> C(h3).
        // new:     A -> X(h2) -> Y(h3).
        let mut g = Graph::new();
        g.add(2, 1, 2).add(3, 2, 3); // B,C
        g.add(10, 1, 2).add(11, 10, 3); // X,Y

        let r = common_ancestor_and_lists(h(3), 3, h(11), 3, g.lookup(), 100).unwrap();
        assert_eq!(r.move_back, vec![h(3), h(2)], "orphan C,B descending");
        assert_eq!(r.move_forward, vec![h(10), h(11)], "adopt X,Y ascending");
    }

    #[test]
    fn no_common_ancestor_in_range_errors() {
        // Two disjoint chains that never meet within max_walk.
        let mut g = Graph::new();
        // current chain: 2 -> 1 -> 0 (0 has parent 0, infinite loop guard).
        g.add(2, 1, 2).add(1, 0, 1).add(0, 0, 0);
        // new chain: 5 -> 4 -> 3 -> 3.
        g.add(5, 4, 2).add(4, 3, 1).add(3, 3, 0);

        let err = common_ancestor_and_lists(h(2), 2, h(5), 2, g.lookup(), 5).unwrap_err();
        assert!(err.contains("not found"), "got: {err}");
    }

    #[test]
    fn missing_header_errors() {
        // current tip B(h2) points at an unknown parent (99); new tip C(h2)
        // points at the (also unknown) ancestor 1. After the first lockstep
        // step cur=99 which has no header → "missing header".
        let mut g = Graph::new();
        g.add(2, 99, 2).add(3, 1, 2);

        let err = common_ancestor_and_lists(h(2), 2, h(3), 2, g.lookup(), 100).unwrap_err();
        assert!(err.contains("missing header"), "got: {err}");
    }
}

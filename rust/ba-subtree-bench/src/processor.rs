//! Native port of the SubtreeProcessor ingest path: dedup → accumulate into the
//! current subtree → emit a completed subtree (computing its merkle root) when it
//! reaches `cap_size`. Mirrors the steady-state behavior of
//! `subtreeprocessor/SubtreeProcessor.go` (queue dequeue → phase-2 dedup filter →
//! bulk insert → `processCompleteSubtree`). Single-threaded: the Go loop is one
//! processing goroutine, so this is the fair per-core comparison.

use std::collections::HashSet;

use crate::hash::Hash;
use crate::subtree::Subtree;

pub struct SubtreeProcessor {
    cap_size: usize,
    current: Subtree,
    chained: Vec<Subtree>,
    /// Dedup set, mirroring the processor's currentTxMap (skip already-seen txids).
    seen: HashSet<Hash>,
    /// Number of chained subtrees already handed out by `take_newly_chained`.
    /// The writer hook drains `chained[drained..]` and advances this index, so
    /// each completed subtree is enqueued for persistence exactly once.
    drained: usize,
}

impl SubtreeProcessor {
    pub fn new(cap_size: usize) -> Self {
        // Seed the coinbase placeholder at node 0 of the FIRST subtree, mirroring
        // go-subtree `SubtreeProcessor` (which calls `AddCoinbaseNode` on the first
        // subtree at init / after reset). It occupies node 0 of the first subtree
        // ONLY — subsequent subtrees created on completion do NOT carry it. The
        // placeholder is NOT added to the dedup set (Go seeds it directly,
        // bypassing currentTxMap), so a real tx with this hash is still deduped.
        let mut current = Subtree::new();
        current.add_coinbase_node();
        Self {
            cap_size,
            current,
            chained: Vec::new(),
            seen: HashSet::new(),
            drained: 0,
        }
    }

    /// Drain completed (chained) subtrees that have not yet been handed out, in
    /// completion order. Returns clones of `chained[drained..]` and advances the
    /// `drained` index so each subtree is returned exactly once. Empty when no new
    /// subtree has completed since the last call. The writer hook calls this (under
    /// the assembly lock) after each ingest, then persists the returned subtrees.
    pub fn take_newly_chained(&mut self) -> Vec<Subtree> {
        let out = self.chained[self.drained..].to_vec();
        self.drained = self.chained.len();
        out
    }

    /// Add one transaction. Duplicates (already-seen txids) are dropped, matching
    /// the phase-2 filter. Completes the current subtree when it fills.
    pub fn add(&mut self, hash: Hash, fee: u64, size_in_bytes: u64) {
        if !self.seen.insert(hash) {
            return;
        }
        self.current.add_node(hash, fee, size_in_bytes);
        if self.current.len() >= self.cap_size {
            self.complete_current();
        }
    }

    /// Move the full current subtree to `chained`, compute its merkle root (the
    /// expensive step the Go path pays when emitting/storing a subtree), and start
    /// a fresh current subtree.
    fn complete_current(&mut self) {
        let mut done = std::mem::take(&mut self.current);
        done.root_hash(); // compute + cache (matches RootHash() on emit)
        self.chained.push(done);
    }

    pub fn num_chained(&self) -> usize {
        self.chained.len()
    }

    pub fn current_len(&self) -> usize {
        self.current.len()
    }

    /// Total node count across chained + current subtrees, INCLUDING the single
    /// coinbase placeholder at subtree-0 node-0.
    pub fn total_nodes(&self) -> usize {
        self.chained.iter().map(|s| s.len()).sum::<usize>() + self.current.len()
    }

    /// Real transaction count = total nodes minus the one coinbase placeholder,
    /// matching Go `GetMiningCandidate` (`txCount-- ` after summing node lengths).
    pub fn num_txs(&self) -> usize {
        self.total_nodes().saturating_sub(1)
    }

    /// Merkle root of the current (partial) subtree, if non-empty.
    pub fn current_root(&mut self) -> Option<Hash> {
        self.current.root_hash()
    }

    /// All REAL transactions currently in the assembly (chained + current), in
    /// order, EXCLUDING the coinbase placeholder. Used by reorg/mined-block
    /// rebuilds, which re-seed a fresh placeholder via `SubtreeProcessor::new`.
    pub fn all_nodes(&self) -> Vec<crate::subtree::Node> {
        let mut v = Vec::new();
        for st in &self.chained {
            v.extend_from_slice(&st.nodes);
        }
        v.extend_from_slice(&self.current.nodes);
        v.retain(|n| n.hash != crate::subtree::COINBASE_PLACEHOLDER);
        v
    }

    /// Remove a tx from the dedup set and the current subtree (if present).
    /// Removal from an already-completed subtree is a reorg concern (Stage 4).
    pub fn remove(&mut self, hash: &Hash) -> bool {
        let was_seen = self.seen.remove(hash);
        self.current.remove_first(hash);
        was_seen
    }

    /// Roots of every chained subtree plus the current subtree when it is
    /// non-empty — Go `getSubtreeHashesChan` (`SubtreeProcessor.go:713-726`).
    pub fn subtree_hashes(&mut self) -> Vec<Hash> {
        let mut v = self.chained_roots();
        if !self.current.is_empty() {
            if let Some(r) = self.current.root_hash() {
                v.push(r);
            }
        }
        v
    }

    /// Every node hash (chained then current), INCLUDING the coinbase placeholder
    /// — Go `getTransactionHashesChan` (`SubtreeProcessor.go:771-783`).
    pub fn tx_hashes(&self) -> Vec<Hash> {
        let mut v = Vec::with_capacity(self.total_nodes());
        for st in &self.chained {
            v.extend(st.nodes.iter().map(|n| n.hash));
        }
        v.extend(self.current.nodes.iter().map(|n| n.hash));
        v
    }

    /// Consistency check — Go `checkSubtreeProcessor` (`SubtreeProcessor.go:2926`).
    /// Returns the first inconsistency: a node missing from the dedup set, a
    /// misplaced coinbase placeholder, or a dedup-set size that differs from the
    /// node count minus the placeholder.
    pub fn check(&self) -> Result<(), String> {
        for (si, st) in self.chained.iter().enumerate() {
            for (ni, n) in st.nodes.iter().enumerate() {
                if n.hash == crate::subtree::COINBASE_PLACEHOLDER {
                    if si != 0 || ni != 0 {
                        return Err(format!("coinbase placeholder not in first subtree {si}"));
                    }
                    continue;
                }
                if !self.seen.contains(&n.hash) {
                    return Err(format!(
                        "tx {} from subtree {si} not in tx map",
                        crate::hash::display_hex(&n.hash)
                    ));
                }
            }
        }

        for (ni, n) in self.current.nodes.iter().enumerate() {
            if self.seen.contains(&n.hash) {
                continue;
            }
            if n.hash == crate::subtree::COINBASE_PLACEHOLDER {
                if !self.chained.is_empty() || ni != 0 {
                    return Err(format!(
                        "coinbase placeholder not in first node of subtree {ni}"
                    ));
                }
                continue;
            }
            return Err(format!(
                "tx {} from current subtree not in tx map",
                crate::hash::display_hex(&n.hash)
            ));
        }

        let expected = self.total_nodes().saturating_sub(1);
        if self.seen.len() != expected {
            return Err(format!(
                "tx map size {} does not match tx count {expected}",
                self.seen.len()
            ));
        }
        Ok(())
    }

    /// Roots of all completed (chained) subtrees, in order.
    pub fn chained_roots(&mut self) -> Vec<Hash> {
        self.chained
            .iter_mut()
            .map(|s| s.root_hash().expect("completed subtree is non-empty"))
            .collect()
    }

    /// Clones of all completed (chained) subtrees, in order — node-sets and all.
    /// Used to build the coinbase merkle proof and to cache the mining job for a
    /// later SubmitMiningSolution. Does not affect the live assembly composition.
    pub fn chained_subtrees_clone(&self) -> Vec<Subtree> {
        self.chained.clone()
    }

    /// Clone of the current (incomplete) subtree when it holds at least one REAL
    /// tx beyond the coinbase placeholder (`len() > 1`), else `None`. Mirrors Go
    /// `createIncompleteSubtreeCopy` (`SubtreeProcessor.go:1080`): the placeholder
    /// already sits at node 0 (seeded on init), the real txs follow, and the
    /// accumulated per-subtree `fees` (B2.0) ride along on the clone. Used by the
    /// candidate path when no completed subtree exists, to publish the incomplete
    /// subtree (and reconcile the coinbase against its carried fees).
    pub fn current_subtree_clone(&self) -> Option<Subtree> {
        if self.current.len() > 1 {
            Some(self.current.clone())
        } else {
            None
        }
    }
}

#[cfg(test)]
mod tests {
    use super::SubtreeProcessor;
    use crate::hash::sha256d;
    use crate::subtree::COINBASE_PLACEHOLDER;

    #[test]
    fn seeds_coinbase_placeholder_at_subtree0_node0() {
        let p = SubtreeProcessor::new(1024);
        // Fresh processor: current subtree holds exactly the placeholder.
        assert_eq!(p.current_len(), 1);
        assert_eq!(p.all_nodes().len(), 0, "placeholder is not a real tx");
        assert_eq!(p.total_nodes(), 1);
        assert_eq!(p.num_txs(), 0);
        // After completing the first subtree, node 0 of subtree 0 == placeholder.
        let mut p = SubtreeProcessor::new(4);
        for i in 0..4u32 {
            p.add(sha256d(&i.to_le_bytes()), i as u64, 1);
        }
        assert_eq!(
            p.num_chained(),
            1,
            "subtree 0 completes at cap incl. placeholder"
        );
        let st0 = &p.chained_subtrees_clone()[0];
        assert_eq!(st0.nodes[0].hash, COINBASE_PLACEHOLDER);
        assert_eq!(st0.nodes[0].fee, 0);
        assert_eq!(st0.nodes[0].size_in_bytes, 0);
    }

    #[test]
    fn completes_subtrees_at_cap_and_keeps_remainder() {
        let cap = 1024;
        let n = 5000u32;
        let mut p = SubtreeProcessor::new(cap);
        for i in 0..n {
            let h = sha256d(&i.to_le_bytes());
            p.add(h, i as u64, i as u64);
        }
        // The placeholder consumes one slot in subtree 0, so the total node count
        // is n + 1: 5001 nodes / 1024 = 4 complete subtrees, remainder 905.
        assert_eq!(p.num_chained(), (n as usize + 1) / cap); // 4 complete subtrees
        assert_eq!(p.current_len(), (n as usize + 1) % cap); // 905 remainder
        assert_eq!(p.num_txs(), n as usize); // real tx count excludes placeholder
    }

    #[test]
    fn take_newly_chained_drains_once_per_completion() {
        let cap = 4;
        let mut p = SubtreeProcessor::new(cap);
        // Nothing completed yet.
        assert!(p.take_newly_chained().is_empty());

        // cap=4 with the coinbase placeholder at node 0 → 3 real txs complete
        // subtree 0.
        for i in 0..3u32 {
            p.add(sha256d(&i.to_le_bytes()), (i + 1) as u64, 10);
        }
        let mut drained = p.take_newly_chained();
        assert_eq!(drained.len(), 1, "exactly one subtree completed");
        let expected_root = p.chained_subtrees_clone()[0]
            .clone()
            .root_hash()
            .expect("non-empty");
        assert_eq!(
            drained[0].root_hash(),
            Some(expected_root),
            "drained subtree carries the completed root"
        );
        // Aggregate fees accumulate (placeholder 0 + 1 + 2 + 3 = 6).
        assert_eq!(
            drained[0].fees, 6,
            "drained subtree carries accumulated fees"
        );

        // Second drain is empty until another subtree fills.
        assert!(
            p.take_newly_chained().is_empty(),
            "no new subtree completed"
        );

        // Fill a second subtree (cap=4 real txs, no placeholder in subtree 1).
        for i in 3..7u32 {
            p.add(sha256d(&i.to_le_bytes()), 1, 10);
        }
        let drained2 = p.take_newly_chained();
        assert_eq!(drained2.len(), 1, "second subtree completed");
    }

    #[test]
    fn current_subtree_clone_none_until_real_tx_then_carries_placeholder_and_fees() {
        use crate::subtree::COINBASE_PLACEHOLDER;

        // Fresh processor: current holds only the placeholder → None.
        let mut p = SubtreeProcessor::new(1024);
        assert_eq!(p.current_len(), 1);
        assert!(
            p.current_subtree_clone().is_none(),
            "placeholder-only current is not a publishable incomplete subtree"
        );

        // Add 2 real txs → Some, node 0 is the placeholder, fees == sum of the 2.
        p.add(sha256d(&1u32.to_le_bytes()), 3, 1);
        p.add(sha256d(&2u32.to_le_bytes()), 7, 1);
        assert_eq!(p.current_len(), 3, "placeholder + 2 real txs");

        let inc = p
            .current_subtree_clone()
            .expect("incomplete subtree present");
        assert_eq!(
            inc.nodes[0].hash, COINBASE_PLACEHOLDER,
            "node 0 placeholder"
        );
        assert_eq!(inc.fees, 10, "carried fees == sum of the 2 real-tx fees");
    }

    #[test]
    fn dedup_drops_repeats() {
        let mut p = SubtreeProcessor::new(1024);
        let h = sha256d(b"dup");
        p.add(h, 1, 1);
        p.add(h, 1, 1);
        // placeholder + one real tx (the duplicate is dropped).
        assert_eq!(p.current_len(), 2);
        assert_eq!(p.num_txs(), 1);
    }

    #[test]
    fn tx_hashes_lists_placeholder_then_txs_in_order() {
        let mut p = SubtreeProcessor::new(4);
        let hs: Vec<_> = (0..5u32).map(|i| sha256d(&i.to_le_bytes())).collect();
        for h in &hs {
            p.add(*h, 1, 1);
        }
        let got = p.tx_hashes();
        assert_eq!(got.len(), 6);
        assert_eq!(got[0], COINBASE_PLACEHOLDER);
        assert_eq!(&got[1..], &hs[..]);
    }

    #[test]
    fn subtree_hashes_chained_roots_plus_nonempty_current() {
        let mut p = SubtreeProcessor::new(4);
        assert_eq!(
            p.subtree_hashes().len(),
            1,
            "placeholder-only current counts"
        );
        for i in 0..4u32 {
            p.add(sha256d(&i.to_le_bytes()), 1, 1);
        }
        let hashes = p.subtree_hashes();
        assert_eq!(p.num_chained(), 1);
        assert_eq!(p.current_len(), 1);
        assert_eq!(hashes.len(), 2);
        assert_eq!(hashes[0], p.chained_roots()[0]);
        assert_eq!(hashes[1], p.current_root().unwrap());
    }

    #[test]
    fn check_passes_on_consistent_state() {
        let mut p = SubtreeProcessor::new(4);
        assert!(p.check().is_ok(), "fresh processor");
        for i in 0..10u32 {
            p.add(sha256d(&i.to_le_bytes()), 1, 1);
        }
        assert!(p.check().is_ok());
    }

    #[test]
    fn check_detects_node_missing_from_tx_map() {
        let mut p = SubtreeProcessor::new(4);
        let h = sha256d(b"x");
        p.add(h, 1, 1);
        p.seen.remove(&h);
        let e = p.check().expect_err("current-subtree tx missing from map");
        assert!(e.contains("current subtree"), "{e}");

        let mut p = SubtreeProcessor::new(4);
        let hs: Vec<_> = (0..3u32).map(|i| sha256d(&i.to_le_bytes())).collect();
        for h in &hs {
            p.add(*h, 1, 1);
        }
        assert_eq!(p.num_chained(), 1);
        p.seen.remove(&hs[0]);
        let e = p.check().expect_err("chained tx missing from map");
        assert!(e.contains("subtree 0"), "{e}");
    }

    #[test]
    fn check_detects_stale_tx_map_entry() {
        let mut p = SubtreeProcessor::new(4);
        p.add(sha256d(b"a"), 1, 1);
        p.seen.insert(sha256d(b"ghost"));
        let e = p.check().expect_err("extra tx-map entry");
        assert!(e.contains("does not match"), "{e}");
    }

    #[test]
    fn check_detects_misplaced_placeholder() {
        let mut p = SubtreeProcessor::new(8);
        p.add(sha256d(b"a"), 1, 1);
        p.current.add_node(COINBASE_PLACEHOLDER, 0, 0);
        let e = p.check().expect_err("placeholder at node 2");
        assert!(e.contains("coinbase placeholder"), "{e}");
    }
}

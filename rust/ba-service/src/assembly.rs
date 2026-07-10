//! In-process block-assembly state: the native subtree engine, running
//! fee/size totals, and the in-process chain tip. Real stores arrive later;
//! reorg is Stage 4.

use std::collections::HashSet;

use ba_subtree_bench::hash::Hash;
use ba_subtree_bench::processor::SubtreeProcessor;

use crate::chain::ChainState;

pub struct AssemblyState {
    pub processor: SubtreeProcessor,
    cap: usize,
    pub total_fees: u64,
    pub total_size: u64,
    pub chain: ChainState,
    /// Hashes the reorg cascade flagged conflicting (the Go `conflictingMap`
    /// analog). `add` rejects any hash in this set so a late-arriving
    /// conflicting tx can never re-enter the assembly; `register_conflicting`
    /// populates it and rebuilds the processor without those hashes.
    conflicting: HashSet<Hash>,
}

impl AssemblyState {
    pub fn new(cap: usize) -> Self {
        Self {
            processor: SubtreeProcessor::new(cap),
            cap,
            total_fees: 0,
            total_size: 0,
            chain: ChainState::genesis(),
            conflicting: HashSet::new(),
        }
    }

    pub fn add(&mut self, hash: Hash, fee: u64, size: u64) {
        if self.conflicting.contains(&hash) {
            return;
        }
        self.processor.add(hash, fee, size);
        self.total_fees = self.total_fees.saturating_add(fee);
        self.total_size = self.total_size.saturating_add(size);
    }

    pub fn remove(&mut self, hash: &Hash) -> bool {
        self.processor.remove(hash)
    }

    /// Real transaction count (excludes the coinbase placeholder), matching Go
    /// `GetMiningCandidate`'s `NumTxs` (sum of subtree node lengths minus one).
    pub fn num_txs(&self) -> u64 {
        self.processor.num_txs() as u64
    }

    /// Completed (chained) subtree roots — the candidate/block subtrees. The
    /// current partial subtree's txs wait for it to fill (incomplete-subtree
    /// inclusion is a later refinement), matching Go's precomputed-data shape.
    pub fn chained_roots(&mut self) -> Vec<Hash> {
        self.processor.chained_roots()
    }

    /// Number of completed (chained) subtrees — drives the candidate's XOR subtree
    /// selection (completed set vs. incomplete-subtree fallback).
    pub fn num_chained(&self) -> usize {
        self.processor.num_chained()
    }

    /// Cloneable copies of the completed (chained) subtrees — node-sets and all.
    /// Used to compute the candidate's coinbase merkle proof and cache the job;
    /// does not mutate the live assembly composition.
    pub fn chained_subtrees_clone(&self) -> Vec<ba_subtree_bench::subtree::Subtree> {
        self.processor.chained_subtrees_clone()
    }

    /// Clone of the current (incomplete) subtree when it holds at least one real
    /// tx beyond the coinbase placeholder, else `None`. The candidate path
    /// publishes this when no completed subtree exists, matching Go's
    /// `GetIncompleteSubtreeMiningData` fallback.
    pub fn current_subtree_clone(&self) -> Option<ba_subtree_bench::subtree::Subtree> {
        self.processor.current_subtree_clone()
    }

    /// Drain subtrees completed since the last call (clones, in completion order).
    /// The ingest handlers call this under the assembly lock after each `add`, then
    /// enqueue the returned subtrees on the writer channel for persistence.
    pub fn take_newly_chained(&mut self) -> Vec<ba_subtree_bench::subtree::Subtree> {
        self.processor.take_newly_chained()
    }

    /// Clear the assembly (keep the chain tip). Used by Reset* and after a block.
    /// Clears the conflicting set so a fresh assembly does not carry stale
    /// per-reorg/per-cycle conflicts (matches Go's per-reorg conflictingMap).
    pub fn reset_assembly(&mut self) {
        self.processor = SubtreeProcessor::new(self.cap);
        self.total_fees = 0;
        self.total_size = 0;
        self.conflicting.clear();
    }

    /// Full reset: assembly + chain back to genesis.
    pub fn reset_fully(&mut self) {
        self.reset_assembly();
        self.chain = ChainState::genesis();
    }

    /// Rebuild the processor from the current survivors, dropping any node whose
    /// hash is in `exclude`, and recompute the running fee/size totals. Shared by
    /// `apply_mined_block` (drop mined txs) and `register_conflicting` (drop
    /// conflicting txs).
    fn rebuild_excluding(&mut self, exclude: &HashSet<Hash>) {
        let survivors = self.processor.all_nodes();
        let mut p = SubtreeProcessor::new(self.cap);
        let (mut fees, mut size) = (0u64, 0u64);
        for n in survivors {
            if exclude.contains(&n.hash) {
                continue;
            }
            p.add(n.hash, n.fee, n.size_in_bytes);
            fees = fees.saturating_add(n.fee);
            size = size.saturating_add(n.size_in_bytes);
        }
        self.processor = p;
        self.total_fees = fees;
        self.total_size = size;
    }

    /// moveForward primitive: a block was mined — drop its txs from the assembly
    /// and rebuild the subtrees from the survivors.
    pub fn apply_mined_block(&mut self, mined: &HashSet<Hash>) {
        self.rebuild_excluding(mined);
    }

    /// Register reorg-cascaded conflicting hashes: insert them into the
    /// conflicting set (so future `add`s reject them) and rebuild the processor
    /// excluding any node now flagged conflicting, recomputing fee/size totals.
    pub fn register_conflicting(&mut self, hashes: &[Hash]) {
        for &h in hashes {
            self.conflicting.insert(h);
        }
        let exclude = self.conflicting.clone();
        self.rebuild_excluding(&exclude);
    }

    /// Seed the chain tip from the real blockchain service at boot (replaces the
    /// genesis default for the standalone build).
    pub fn seed_tip(
        &mut self,
        hash: Hash,
        height: u32,
        n_bits: u32,
        version: u32,
        median_time: u32,
    ) {
        self.chain.best_hash = hash;
        self.chain.height = height;
        self.chain.n_bits = n_bits;
        self.chain.version = version;
        self.chain.median_time = median_time;
    }

    /// moveBack primitive: txs from an orphaned (reorged-out) block return to the
    /// assembly as unmined. Wired into the reorg handler (chain_grpc::reconcile_reorg).
    pub fn return_txs(&mut self, txs: &[(Hash, u64, u64)]) {
        for &(h, fee, size) in txs {
            self.add(h, fee, size);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::AssemblyState;
    use ba_subtree_bench::hash::sha256d;
    use std::collections::HashSet;

    #[test]
    fn seed_tip_sets_chain_state() {
        let mut st = AssemblyState::new(4);
        st.seed_tip([3u8; 32], 99, 0x1d00ffff, 0x20000000, 1_700_000_500);
        assert_eq!(st.chain.height, 99);
        assert_eq!(st.chain.best_hash, [3u8; 32]);
        assert_eq!(st.chain.n_bits, 0x1d00ffff);
    }

    fn leaf(i: u32) -> [u8; 32] {
        sha256d(&i.to_le_bytes())
    }

    /// Consensus parity: the first completed subtree's node 0 MUST be the exact
    /// go-subtree `CoinbasePlaceholder` (32 bytes of 0xFF), with fee 0 / size 0,
    /// so the published block passes `model/Block.go` Valid check #7. Seeded on
    /// init AND re-seeded after a mined-block rebuild.
    #[test]
    fn first_subtree_node0_is_exact_go_coinbase_placeholder() {
        const GO_PLACEHOLDER: [u8; 32] = [0xFFu8; 32];
        // Drive enough txs (cap 4) to complete the first subtree.
        let mut st = AssemblyState::new(4);
        for i in 0..4u32 {
            st.add(leaf(i), i as u64, 1);
        }
        let subtrees = st.chained_subtrees_clone();
        assert!(!subtrees.is_empty(), "first subtree completed");
        assert_eq!(
            subtrees[0].nodes[0].hash, GO_PLACEHOLDER,
            "subtree-0 node-0"
        );
        assert_eq!(subtrees[0].nodes[0].fee, 0, "placeholder fee 0");
        assert_eq!(subtrees[0].nodes[0].size_in_bytes, 0, "placeholder size 0");

        // Re-seeded after a mined-block rebuild (apply_mined_block -> new()).
        st.apply_mined_block(&HashSet::new());
        for i in 4..8u32 {
            st.add(leaf(i), i as u64, 1);
        }
        let rebuilt = st.chained_subtrees_clone();
        assert!(!rebuilt.is_empty(), "first subtree completed after rebuild");
        assert_eq!(
            rebuilt[0].nodes[0].hash, GO_PLACEHOLDER,
            "placeholder re-seeded after mined-block rebuild"
        );
    }

    #[test]
    fn apply_mined_block_drops_mined_and_rebuilds() {
        let mut st = AssemblyState::new(4);
        for i in 0..10u32 {
            st.add(leaf(i), i as u64, 1);
        }
        assert_eq!(st.num_txs(), 10);

        let mined: HashSet<[u8; 32]> = [leaf(0), leaf(5), leaf(9)].into_iter().collect();
        st.apply_mined_block(&mined);

        assert_eq!(st.num_txs(), 7, "3 mined txs dropped");
        // total_fees was 0+1+..+9=45; minus leaf0(0)+leaf5(5)+leaf9(9)=14 -> 31
        assert_eq!(st.total_fees, 31);
    }

    #[test]
    fn return_txs_adds_back() {
        let mut st = AssemblyState::new(4);
        st.add(leaf(1), 1, 1);
        assert_eq!(st.num_txs(), 1);
        st.return_txs(&[(leaf(2), 2, 1), (leaf(3), 3, 1)]);
        assert_eq!(st.num_txs(), 3);
    }

    #[test]
    fn add_rejects_registered_conflicting() {
        let mut st = AssemblyState::new(4);
        st.register_conflicting(&[leaf(7)]);
        st.add(leaf(7), 5, 1); // must be rejected
        st.add(leaf(8), 5, 1); // normal add
        assert_eq!(st.num_txs(), 1, "conflicting tx not added");
    }

    #[test]
    fn register_conflicting_evicts_already_added() {
        let mut st = AssemblyState::new(4);
        st.add(leaf(1), 1, 1);
        st.add(leaf(2), 2, 1);
        st.add(leaf(3), 3, 1);
        assert_eq!(st.num_txs(), 3);
        st.register_conflicting(&[leaf(2)]);
        assert_eq!(st.num_txs(), 2, "evicted tx dropped from assembly");
        assert_eq!(st.total_fees, 4, "fees recomputed without evicted tx (1+3)");
        // re-adding the evicted hash is now a no-op
        st.add(leaf(2), 2, 1);
        assert_eq!(st.num_txs(), 2);
    }

    #[test]
    fn reset_fully_clears_conflicting() {
        let mut st = AssemblyState::new(4);
        st.register_conflicting(&[leaf(5)]);
        st.reset_fully();
        st.add(leaf(5), 1, 1); // allowed again after reset
        assert_eq!(st.num_txs(), 1);
    }
}

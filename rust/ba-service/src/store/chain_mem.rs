//! In-memory BlockchainClient for unit tests and standalone runs.
use std::collections::HashMap;
use std::sync::Mutex;

use ba_subtree_bench::hash::Hash;
use tonic::async_trait;

use super::{BlockHeaderInfo, BlockchainClient, ChainTip, StoreError};

/// Recorded arguments of the most recent `add_block` call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AddBlockCall {
    pub header: Vec<u8>,
    pub subtree_hashes: Vec<Hash>,
    pub coinbase_tx: Vec<u8>,
    pub tx_count: u64,
    pub size_in_bytes: u64,
    pub coinbase_bump: Vec<u8>,
}

pub struct MemBlockchainClient {
    pub tip: ChainTip,
    add_block_calls: Mutex<Vec<AddBlockCall>>,
    subtrees_set_calls: Mutex<Vec<Hash>>,
    /// Whether `is_fsm_current_state("RUNNING")` reports true. Defaults to true
    /// (mirrors a running node); tests flip it to exercise the not-RUNNING gate.
    fsm_running: bool,
    subtree_notifications: Mutex<Vec<Hash>>,
    /// Fork-graph headers for the reorg walk (`block_header` lookups).
    headers: HashMap<Hash, BlockHeaderInfo>,
    /// Per-block (height, subtree roots) for `block_subtrees`. Falls back to the
    /// tip height + empty roots when a block is not present (legacy behaviour).
    block_subtrees: HashMap<Hash, (u32, Vec<Hash>)>,
    /// Per-block `(height, block_id, coinbase_tx_bytes)` for `block_coinbase`.
    /// Falls back to `(tip height, 0, empty)` when absent so the reorg
    /// moveForward coinbase-create is skipped gracefully in tests that omit it.
    block_coinbases: HashMap<Hash, (u32, u32, Vec<u8>)>,
    /// Next-block required difficulty returned by `get_next_work_required`.
    /// `None` falls back to the tip's `n_bits` so existing tests are unaffected.
    next_work: Option<u32>,
    /// Recorded `(prev_hash, next_block_time)` arguments of `get_next_work_required`
    /// calls, so tests can assert what time the candidate hands to the DAA.
    next_work_requests: Mutex<Vec<(Hash, i64)>>,
}

impl MemBlockchainClient {
    pub fn new(tip: ChainTip) -> Self {
        Self {
            tip,
            add_block_calls: Mutex::new(Vec::new()),
            subtrees_set_calls: Mutex::new(Vec::new()),
            fsm_running: true,
            subtree_notifications: Mutex::new(Vec::new()),
            headers: HashMap::new(),
            block_subtrees: HashMap::new(),
            block_coinbases: HashMap::new(),
            next_work: None,
            next_work_requests: Mutex::new(Vec::new()),
        }
    }

    /// Set whether the FSM reports RUNNING (default true).
    pub fn with_fsm_running(mut self, running: bool) -> Self {
        self.fsm_running = running;
        self
    }

    /// Set the value `get_next_work_required` returns (default: tip n_bits).
    pub fn with_next_work(mut self, v: u32) -> Self {
        self.next_work = Some(v);
        self
    }

    /// Register a block header (prev_hash + height) for the reorg fork graph.
    pub fn set_header(&mut self, hash: Hash, prev_hash: Hash, height: u32) {
        self.headers
            .insert(hash, BlockHeaderInfo { prev_hash, height });
    }

    /// Register a block's (height, subtree roots) for `block_subtrees`.
    pub fn set_block_subtrees(&mut self, hash: Hash, height: u32, roots: Vec<Hash>) {
        self.block_subtrees.insert(hash, (height, roots));
    }

    /// Register a block's `(height, block_id, coinbase_tx_bytes)` for
    /// `block_coinbase` (drives the reorg moveForward coinbase-create).
    pub fn set_block_coinbase(&mut self, hash: Hash, height: u32, block_id: u32, coinbase: Vec<u8>) {
        self.block_coinbases.insert(hash, (height, block_id, coinbase));
    }

    /// Subtree hashes for which `send_notification_subtree` was called.
    pub fn subtree_notifications(&self) -> Vec<Hash> {
        self.subtree_notifications.lock().unwrap().clone()
    }

    /// Number of times `add_block` has been called.
    pub fn add_block_count(&self) -> usize {
        self.add_block_calls.lock().unwrap().len()
    }

    /// Arguments of the most recent `add_block` call, if any.
    pub fn last_add_block(&self) -> Option<AddBlockCall> {
        self.add_block_calls.lock().unwrap().last().cloned()
    }

    /// Number of times `set_block_subtrees_set` has been called.
    pub fn set_subtrees_set_count(&self) -> usize {
        self.subtrees_set_calls.lock().unwrap().len()
    }

    /// Hash of the most recent `set_block_subtrees_set` call, if any.
    pub fn last_subtrees_set(&self) -> Option<Hash> {
        self.subtrees_set_calls.lock().unwrap().last().copied()
    }

    /// Arguments of the most recent `get_next_work_required` call, if any.
    pub fn last_next_work_request(&self) -> Option<(Hash, i64)> {
        self.next_work_requests.lock().unwrap().last().copied()
    }
}

#[async_trait]
impl BlockchainClient for MemBlockchainClient {
    async fn best_tip(&self) -> Result<ChainTip, StoreError> {
        Ok(self.tip.clone())
    }

    async fn get_next_work_required(
        &self,
        prev_hash: &Hash,
        next_block_time: i64,
    ) -> Result<u32, StoreError> {
        self.next_work_requests
            .lock()
            .unwrap()
            .push((*prev_hash, next_block_time));

        Ok(self.next_work.unwrap_or(self.tip.n_bits))
    }

    async fn block_header_ids(&self, _hash: &Hash, _n: u64) -> Result<Vec<u32>, StoreError> {
        Ok(vec![])
    }

    async fn block_subtrees(&self, hash: &Hash) -> Result<(u32, Vec<Hash>), StoreError> {
        if let Some((height, roots)) = self.block_subtrees.get(hash) {
            return Ok((*height, roots.clone()));
        }
        Ok((self.tip.height, vec![]))
    }

    async fn block_coinbase(&self, hash: &Hash) -> Result<(u32, u32, Vec<u8>), StoreError> {
        if let Some((height, id, cb)) = self.block_coinbases.get(hash) {
            return Ok((*height, *id, cb.clone()));
        }
        Ok((self.tip.height, 0, vec![]))
    }

    async fn block_header(&self, hash: &Hash) -> Result<BlockHeaderInfo, StoreError> {
        self.headers
            .get(hash)
            .cloned()
            .ok_or_else(|| StoreError::NotFound(format!("header {}", hex::encode(hash))))
    }

    async fn add_block(
        &self,
        header: &[u8],
        subtree_hashes: &[Hash],
        coinbase_tx: &[u8],
        tx_count: u64,
        size_in_bytes: u64,
        coinbase_bump: &[u8],
    ) -> Result<(), StoreError> {
        self.add_block_calls.lock().unwrap().push(AddBlockCall {
            header: header.to_vec(),
            subtree_hashes: subtree_hashes.to_vec(),
            coinbase_tx: coinbase_tx.to_vec(),
            tx_count,
            size_in_bytes,
            coinbase_bump: coinbase_bump.to_vec(),
        });

        Ok(())
    }

    async fn set_block_subtrees_set(&self, block_hash: &Hash) -> Result<(), StoreError> {
        self.subtrees_set_calls.lock().unwrap().push(*block_hash);

        Ok(())
    }

    async fn is_fsm_current_state(&self, state: &str) -> Result<bool, StoreError> {
        Ok(state == "RUNNING" && self.fsm_running)
    }

    async fn send_notification_subtree(&self, subtree_hash: &Hash) -> Result<(), StoreError> {
        self.subtree_notifications
            .lock()
            .unwrap()
            .push(*subtree_hash);

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tip(height: u32) -> ChainTip {
        ChainTip {
            hash: [0u8; 32],
            height,
            n_bits: 0,
            version: 1,
            median_time: 0,
        }
    }

    #[tokio::test]
    async fn returns_configured_tip() {
        let c = MemBlockchainClient::new(ChainTip {
            hash: [7u8; 32],
            height: 42,
            n_bits: 0x207fffff,
            version: 0x20000000,
            median_time: 1_700_000_000,
        });
        let tip = c.best_tip().await.unwrap();
        assert_eq!(tip.height, 42);
    }

    #[tokio::test]
    async fn block_header_ids_returns_empty() {
        let c = MemBlockchainClient::new(tip(10));
        let ids = c.block_header_ids(&[0u8; 32], 5).await.unwrap();
        assert!(ids.is_empty());
    }

    #[tokio::test]
    async fn block_subtrees_returns_tip_height_and_empty_hashes() {
        let c = MemBlockchainClient::new(tip(99));
        let (height, hashes) = c.block_subtrees(&[0u8; 32]).await.unwrap();
        assert_eq!(height, 99);
        assert!(hashes.is_empty());
    }

    #[tokio::test]
    async fn block_header_lookup_returns_registered_and_errors_on_missing() {
        let mut c = MemBlockchainClient::new(tip(5));
        c.set_header([2u8; 32], [1u8; 32], 7);

        let info = c.block_header(&[2u8; 32]).await.unwrap();
        assert_eq!(info.prev_hash, [1u8; 32]);
        assert_eq!(info.height, 7);

        let err = c.block_header(&[9u8; 32]).await.unwrap_err();
        assert!(matches!(err, StoreError::NotFound(_)));
    }

    #[tokio::test]
    async fn block_subtrees_returns_registered_roots() {
        let mut c = MemBlockchainClient::new(tip(5));
        c.set_block_subtrees([3u8; 32], 4, vec![[7u8; 32], [8u8; 32]]);
        let (height, roots) = c.block_subtrees(&[3u8; 32]).await.unwrap();
        assert_eq!(height, 4);
        assert_eq!(roots, vec![[7u8; 32], [8u8; 32]]);
    }

    #[tokio::test]
    async fn next_work_required_returns_configured_value() {
        let c = MemBlockchainClient::new(ChainTip {
            hash: [1u8; 32],
            height: 5,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        })
        .with_next_work(0x1d00_ffff);
        let nbits = c.get_next_work_required(&[0u8; 32], 123).await.unwrap();
        assert_eq!(nbits, 0x1d00_ffff);
    }

    #[tokio::test]
    async fn next_work_required_defaults_to_tip_nbits() {
        let c = MemBlockchainClient::new(ChainTip {
            hash: [1u8; 32],
            height: 5,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        });
        let nbits = c.get_next_work_required(&[0u8; 32], 123).await.unwrap();
        assert_eq!(nbits, 0x207f_ffff, "defaults to the tip's n_bits");
    }

    #[tokio::test]
    async fn records_add_block_and_subtrees_set_calls() {
        let c = MemBlockchainClient::new(tip(5));
        assert_eq!(c.add_block_count(), 0);
        assert!(c.last_add_block().is_none());

        let subtrees = vec![[1u8; 32], [2u8; 32]];
        c.add_block(&[0xaa; 80], &subtrees, &[0xbb; 10], 7, 4096, &[0xcc; 3])
            .await
            .unwrap();

        assert_eq!(c.add_block_count(), 1);
        let call = c.last_add_block().unwrap();
        assert_eq!(call.header, vec![0xaa; 80]);
        assert_eq!(call.subtree_hashes, subtrees);
        assert_eq!(call.coinbase_tx, vec![0xbb; 10]);
        assert_eq!(call.tx_count, 7);
        assert_eq!(call.size_in_bytes, 4096);
        assert_eq!(call.coinbase_bump, vec![0xcc; 3]);

        assert_eq!(c.set_subtrees_set_count(), 0);
        c.set_block_subtrees_set(&[9u8; 32]).await.unwrap();
        assert_eq!(c.set_subtrees_set_count(), 1);
        assert_eq!(c.last_subtrees_set(), Some([9u8; 32]));
    }
}

//! Trait-based store adapters (Stage 3). Each trait has a real backend impl
//! and an in-memory impl for unit tests.

use ba_subtree_bench::hash::Hash;
use ba_subtree_bench::subtree::Subtree;
use tonic::async_trait;

pub mod blob_fs;
pub mod chain_grpc;
pub mod chain_mem;
pub mod utxo_aero;
pub mod utxo_mem;

#[derive(Debug)]
pub enum StoreError {
    NotFound(String),
    Backend(String),
    Decode(String),
}

impl std::fmt::Display for StoreError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            StoreError::NotFound(s) => write!(f, "not found: {s}"),
            StoreError::Backend(s) => write!(f, "backend error: {s}"),
            StoreError::Decode(s) => write!(f, "decode error: {s}"),
        }
    }
}

impl std::error::Error for StoreError {}

impl From<StoreError> for tonic::Status {
    fn from(e: StoreError) -> Self {
        match e {
            StoreError::NotFound(s) => tonic::Status::not_found(s),
            StoreError::Backend(s) => tonic::Status::unavailable(s),
            StoreError::Decode(s) => tonic::Status::internal(s),
        }
    }
}

/// Arguments to the `setMined` UDF (mirrors Go MinedBlockInfo).
#[derive(Debug, Clone)]
pub struct MinedBlockInfo {
    pub block_id: u32,
    pub block_height: u32,
    pub subtree_idx: i32,
    pub on_longest_chain: bool,
    pub unset_mined: bool,
}

/// One unmined transaction as loaded at startup.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UnminedTx {
    pub txid: Hash,
    pub fee: u64,
    pub size: u64,
    pub block_ids: Vec<u32>,
    pub locked: bool,
    /// `createdAt` Aerospike bin (UnixMilli int). Used to sort the boot load
    /// oldest-first, matching Go's `sort.Slice(... CreatedAt < CreatedAt)`.
    pub created_at: i64,
}

/// Minimal header view the reorg walk needs: the previous-block hash (to step
/// one block back toward the common ancestor) and this block's height (to align
/// the two chains by height before stepping). Mirrors the `prev_hash` field of
/// the 80-byte header plus the blockchain service's stored block height.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BlockHeaderInfo {
    pub prev_hash: Hash,
    pub height: u32,
}

/// Information about the transaction that spends a UTXO. Mirrors Go
/// `stores/utxo/spend.SpendingData` (`{TxID, Vin}`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SpendingData {
    pub tx_id: Hash,
    pub vin: u32,
}

impl SpendingData {
    /// Serialize as `txid[32] ‖ vin(u32 LE)` = 36 bytes. Byte-identical to Go
    /// `SpendingData.Bytes()` (stores/utxo/spend/spending_data.go:86), which
    /// appends `TxID.CloneBytes()` (the raw 32-byte internal hash, NOT the
    /// display-reversed form) followed by the little-endian vin. The Aerospike
    /// `utxos` 68-byte spent entry stores exactly `utxoHash[32] ‖ this[36]`.
    pub fn bytes(&self) -> [u8; 36] {
        let mut out = [0u8; 36];
        out[..32].copy_from_slice(&self.tx_id);
        out[32..].copy_from_slice(&self.vin.to_le_bytes());
        out
    }
}

/// Stored per-tx metadata db2 reads. `tx_bytes` is the EXTENDED serialization
/// held in the UTXO store's `tx` field (callers parse via `ba_subtree_bench::tx`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TxMeta {
    pub tx_bytes: Vec<u8>,
    pub conflicting: bool,
    pub block_ids: Vec<u32>,
    /// `createdAt` Aerospike bin (UnixMilli int; Go `fields.CreatedAt`). Used by
    /// db3 counter selection to pick the oldest qualifying conflicting child.
    /// Defaults to `0` when the bin is absent.
    pub created_at: i64,
}

/// One UTXO spend record. Mirrors Go `stores/utxo.Spend`. `tx_id`/`vout`
/// identify the UTXO being spent; `spending_data` (when set) names the spender.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Spend {
    pub tx_id: Hash,
    pub vout: u32,
    pub utxo_hash: Hash,
    pub spending_data: Option<SpendingData>,
    pub conflicting_tx_id: Option<Hash>,
    pub block_ids: Vec<u32>,
}

/// Bypass flags for `spend`. Mirrors Go `stores/utxo.IgnoreFlags`.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct IgnoreFlags {
    pub ignore_conflicting: bool,
    pub ignore_locked: bool,
}

/// Minimal block header view the candidate needs.
#[derive(Debug, Clone)]
pub struct ChainTip {
    pub hash: Hash,
    pub height: u32,
    pub n_bits: u32,
    pub version: u32,
    pub median_time: u32,
}

/// UTXO store surface Stage 3 uses (NO Spend — that is the validator's path).
#[async_trait]
pub trait UtxoStore: Send + Sync {
    /// Load all currently-unmined transactions (Aerospike: `unminedSince` sindex).
    async fn unmined(&self) -> Result<Vec<UnminedTx>, StoreError>;
    /// Mark `hashes` mined; returns each hash -> its updated blockIDs list.
    /// Postcondition: every input hash appears in the returned map.
    async fn set_mined_multi(
        &self,
        hashes: &[Hash],
        info: &MinedBlockInfo,
    ) -> Result<std::collections::HashMap<Hash, Vec<u32>>, StoreError>;
    /// Flag/unflag transactions as being on the longest chain.
    async fn mark_on_longest_chain(&self, hashes: &[Hash], on: bool) -> Result<(), StoreError>;
    /// Set/clear the `locked` flag on transactions (Go: `UtxoStore.SetLocked`).
    /// Used at boot to UNLOCK the locked unmined txs that were added to the
    /// candidate, matching Go's `SetLocked(.., false)` after the load.
    async fn set_locked(&self, hashes: &[Hash], locked: bool) -> Result<(), StoreError>;

    /// Create the UTXO record for `tx`. Mirrors Go `UtxoStore.Create(tx,
    /// blockHeight, WithMinedBlockInfo, WithLocked)` as invoked by
    /// `SubtreeProcessor.processCoinbaseUtxos`: when a block extends the tip (or
    /// is adopted in a reorg moveForward), block assembly writes that block's
    /// COINBASE UTXO record — even for an empty (coinbase-only) block. Without
    /// this the authoritative UTXO store never gains the coinbase outputs.
    ///
    /// `mined`: when present, written as the single-element `blockIDs` /
    /// `blockHeights` / `subtreeIdxs` (so the record is NOT flagged unmined);
    /// when `None`, `unminedSince` is set instead. `locked` writes the `locked`
    /// bin (coinbase create passes `false`). A pre-existing record (Aerospike
    /// key-exists under CREATE_ONLY) is benign — Go treats `ErrTxExists` as a
    /// skip — and returns `Ok(())`.
    ///
    /// SCOPE: single-record only (the coinbase has one output). A tx with more
    /// outputs than `utxoBatchSize` (pagination) is rejected — regular multi-
    /// output tx UTXO creation is the validator's path, not block assembly's.
    async fn create(
        &self,
        tx: &ba_subtree_bench::tx::Tx,
        block_height: u32,
        mined: Option<MinedBlockInfo>,
        locked: bool,
    ) -> Result<(), StoreError>;

    /// Mark the UTXOs created by `tx` as spent by `tx` itself (the inputs of the
    /// supplied tx reference the outputs being spent). Mirrors Go
    /// `UtxoStore.Spend(tx, blockHeight, ignoreFlags) -> []*Spend`: returns one
    /// `Spend` per input, each carrying the `SpendingData` written. CONSENSUS-
    /// CRITICAL. The caller passes the pre-built `Spend` list (the validator
    /// constructs these from the tx inputs); this trait takes the already-derived
    /// spends to avoid a tx deserializer in this crate.
    async fn spend(
        &self,
        spends: &[Spend],
        block_height: u32,
        ignore: IgnoreFlags,
    ) -> Result<Vec<Spend>, StoreError>;

    /// Reverse `spend`: clear the spending data on each UTXO, optionally
    /// re-flagging it locked. Mirrors Go `UtxoStore.Unspend(spends, flagAsLocked)`.
    ///
    /// I2 — `flag_as_locked` is NOT applied by the `unspend` UDF itself.
    /// Go's `Unspend` wrapper (un_spend.go) issues a SEPARATE `SetLocked(.., true)`
    /// pass after the `unspend` UDF call when `flagAsLocked` is true. The `unspend`
    /// UDF args carry no lock flag. Callers that need to re-lock MUST call
    /// `set_locked(hashes, true)` explicitly after `unspend` — relying on
    /// `unspend` alone to re-lock will silently leave the UTXOs unlocked.
    /// VERIFY-ON-RIG: confirm no call site relies on `unspend` alone for locking.
    async fn unspend(&self, spends: &[Spend], flag_as_locked: bool) -> Result<(), StoreError>;

    /// Set/clear the `conflicting` flag on each of `tx_hashes` and return
    /// `(affected_parent_spends, spending_child_tx_hashes)`. Mirrors Go
    /// `UtxoStore.SetConflicting(txHashes, value) -> ([]*Spend, []Hash, error)`.
    ///
    /// - `affected_parent_spends`: one `Spend` per input of each tx (the parent
    ///   UTXOs whose spending data this conflicting tx owns).
    /// - `spending_child_tx_hashes`: the txids that spend any output of these
    ///   txs (the next BFS level for the cascade).
    async fn set_conflicting(
        &self,
        tx_hashes: &[Hash],
        value: bool,
    ) -> Result<(Vec<Spend>, Vec<Hash>), StoreError>;

    /// Fetch a tx's stored metadata (Go `Get(hash, fields.Tx, fields.Conflicting,
    /// fields.BlockIDs)`). `Ok(None)` when the record is absent.
    async fn get_tx_meta(&self, hash: &Hash) -> Result<Option<TxMeta>, StoreError>;
    /// The tx's per-output spender list (`utxos`/`SpendingDatas` bin); index = vout,
    /// `None` = unspent. Go reads this via `fields.Utxos`.
    async fn get_spending_datas(
        &self,
        hash: &Hash,
    ) -> Result<Vec<Option<SpendingData>>, StoreError>;
    /// The tx's `ConflictingChildren` bin (one level). Go `fields.ConflictingChildren`.
    async fn get_conflicting_children_bin(&self, hash: &Hash) -> Result<Vec<Hash>, StoreError>;

    /// Mark `hashes` conflicting, then iteratively mark every spending
    /// descendant via breadth-first traversal. Default impl ports Go
    /// `MarkConflictingRecursively` (stores/utxo/process_conflicting.go:796)
    /// exactly: seed `visited`+`marked_order` with the input hashes, then loop
    /// `set_conflicting(to_process, true)` — accumulate affected parent spends,
    /// filter unseen children into the next batch (visited dedup preserves BFS
    /// order). Returns `(all_affected_spends, marked_order)`.
    async fn mark_conflicting_recursively(
        &self,
        hashes: &[Hash],
    ) -> Result<(Vec<Spend>, Vec<Hash>), StoreError> {
        let mut all_affected_spends: Vec<Spend> = Vec::new();
        let mut to_process: Vec<Hash> = hashes.to_vec();

        let mut visited: std::collections::HashSet<Hash> = std::collections::HashSet::new();
        let mut marked_order: Vec<Hash> = Vec::with_capacity(hashes.len());

        for &h in hashes {
            if visited.insert(h) {
                marked_order.push(h);
            }
        }

        while !to_process.is_empty() {
            let (affected_parent_spends, spending_child_txs) =
                self.set_conflicting(&to_process, true).await?;

            all_affected_spends.extend(affected_parent_spends);

            // Filter out already-visited hashes to prevent infinite loops.
            let mut next_batch: Vec<Hash> = Vec::new();
            for child in spending_child_txs {
                if visited.insert(child) {
                    marked_order.push(child);
                    next_batch.push(child);
                }
            }

            to_process = next_batch;
        }

        Ok((all_affected_spends, marked_order))
    }

    /// Inverse of `mark_conflicting_recursively`: clear `conflicting` on `hashes`
    /// and every spending descendant. Default impl ports Go
    /// `UnmarkConflictingRecursively` (stores/utxo/process_conflicting.go:638):
    /// same BFS, calling `set_conflicting(.., false)`. Returns the BFS-ordered
    /// list of every hash whose flag this call cleared.
    async fn unmark_conflicting_recursively(
        &self,
        hashes: &[Hash],
    ) -> Result<Vec<Hash>, StoreError> {
        let mut to_process: Vec<Hash> = hashes.to_vec();

        let mut visited: std::collections::HashSet<Hash> = std::collections::HashSet::new();
        let mut cleared_order: Vec<Hash> = Vec::with_capacity(hashes.len());

        for &h in hashes {
            if visited.insert(h) {
                cleared_order.push(h);
            }
        }

        while !to_process.is_empty() {
            let (_affected, spending_child_txs) = self.set_conflicting(&to_process, false).await?;

            let mut next_batch: Vec<Hash> = Vec::new();
            for child in spending_child_txs {
                if visited.insert(child) {
                    cleared_order.push(child);
                    next_batch.push(child);
                }
            }

            to_process = next_batch;
        }

        Ok(cleared_order)
    }

    /// Collect every conflicting descendant of `root` (root excluded). Default
    /// impl ports Go `GetConflictingChildren`
    /// (stores/utxo/process_conflicting.go:912): BFS seeded with
    /// `visited = {root}`; at each level union the `ConflictingChildren` bin
    /// (`get_conflicting_children_bin`) with the current spenders read from the
    /// `SpendingDatas` bin (`get_spending_datas`, each `Some(sd).tx_id`), and
    /// enqueue every unseen child. The coinbase-placeholder root (`[0xFF; 32]`)
    /// short-circuits to an empty set (Go lines 917-920).
    async fn get_conflicting_children(&self, root: Hash) -> Result<Vec<Hash>, StoreError> {
        if root == ba_subtree_bench::subtree::COINBASE_PLACEHOLDER {
            // skip the coinbase placeholder hash
            return Ok(Vec::new());
        }

        let mut visited: std::collections::HashSet<Hash> = std::collections::HashSet::new();
        visited.insert(root);
        let mut current_level: Vec<Hash> = vec![root];

        while !current_level.is_empty() {
            let mut next_level: Vec<Hash> = Vec::new();

            for h in &current_level {
                let bin_children = self.get_conflicting_children_bin(h).await?;
                for child in bin_children {
                    if visited.insert(child) {
                        next_level.push(child);
                    }
                }

                let spending_datas = self.get_spending_datas(h).await?;
                for sd in spending_datas.into_iter().flatten() {
                    let child = sd.tx_id;
                    if visited.insert(child) {
                        next_level.push(child);
                    }
                }
            }

            current_level = next_level;
        }

        // exclude the root hash from the result
        visited.remove(&root);

        Ok(visited.into_iter().collect())
    }

    /// Find every tx that counter-conflicts `tx_hash` (the txs whose accepted
    /// state would be displaced if `tx_hash` were promoted). Default impl ports
    /// Go `GetCounterConflictingTxHashes`
    /// (stores/utxo/process_conflicting.go:988):
    ///
    /// - seed the result set with `tx_hash` itself (Go line 999);
    /// - parse the tx body (`get_tx_meta(tx_hash).tx_bytes`) via the extended-tx
    ///   deserializer to recover its inputs;
    /// - for each unique parent (`input.prev_txid`) read its `SpendingDatas` bin
    ///   once;
    /// - for each input, range-check `vout` against that parent's spending list
    ///   length (out of range → `Err`, Go lines 1034-1036), take the current
    ///   spender; if present, add it and union its `get_conflicting_children`,
    ///   erroring on a frozen child (`[0xFF; 32]`, Go lines 1049-1051).
    ///
    /// Returns the unique set (order-insensitive).
    async fn get_counter_conflicting(&self, tx_hash: Hash) -> Result<Vec<Hash>, StoreError> {
        let tx_meta = self
            .get_tx_meta(&tx_hash)
            .await?
            .ok_or_else(|| StoreError::NotFound(format!("tx_meta for {tx_hash:02x?}")))?;

        let tx = ba_subtree_bench::tx::Tx::from_bytes(&tx_meta.tx_bytes)
            .map_err(|e| StoreError::Decode(format!("{e:?}")))?;

        let mut counter: std::collections::HashSet<Hash> = std::collections::HashSet::new();
        counter.insert(tx_hash);

        // Read each unique parent's spending list once. Collect the unique
        // parents first (Go dedups parents into a map before reading), then read.
        let mut seen_parents: std::collections::HashSet<Hash> = std::collections::HashSet::new();
        let mut unique_parents: Vec<Hash> = Vec::new();
        for input in &tx.inputs {
            if seen_parents.insert(input.prev_txid) {
                unique_parents.push(input.prev_txid);
            }
        }

        let mut parent_spends: std::collections::HashMap<Hash, Vec<Option<SpendingData>>> =
            std::collections::HashMap::new();
        for parent in unique_parents {
            let sds = self.get_spending_datas(&parent).await?;
            parent_spends.insert(parent, sds);
        }

        for input in &tx.inputs {
            let sds = match parent_spends.get(&input.prev_txid) {
                Some(sds) => sds,
                None => continue,
            };

            let vout = input.vout as usize;
            if sds.len() <= vout {
                return Err(StoreError::Backend(format!(
                    "[GetCounterConflictingTxHashes][{:02x?}] cannot process counter conflicting, \
                     input {} of {:02x?} is out of range (len: {})",
                    tx_hash,
                    input.vout,
                    input.prev_txid,
                    sds.len()
                )));
            }

            if let Some(spending_data) = &sds[vout] {
                let spending_tx_id = spending_data.tx_id;
                counter.insert(spending_tx_id);

                let child_hashes = self.get_conflicting_children(spending_tx_id).await?;
                for child in child_hashes {
                    if child == ba_subtree_bench::subtree::COINBASE_PLACEHOLDER {
                        return Err(StoreError::Backend(format!(
                            "[GetCounterConflictingTxHashes][{spending_tx_id:02x?}] tx has frozen child"
                        )));
                    }
                    counter.insert(child);
                }
            }
        }

        Ok(counter.into_iter().collect())
    }
}

/// Blockchain service reads Stage 3 needs, plus subscription.
#[async_trait]
pub trait BlockchainClient: Send + Sync {
    async fn best_tip(&self) -> Result<ChainTip, StoreError>;
    /// Required next-block difficulty (compact nBits as a `u32`) for a block
    /// building on `prev_hash` at `next_block_time`. Mirrors Go
    /// `blockchainClient.GetNextWorkRequired`; the DAA itself lives server-side.
    async fn get_next_work_required(
        &self,
        prev_hash: &Hash,
        next_block_time: i64,
    ) -> Result<u32, StoreError>;
    /// Block header IDs from `hash` back `n` headers (for "is on best chain").
    async fn block_header_ids(&self, hash: &Hash, n: u64) -> Result<Vec<u32>, StoreError>;
    /// Subtree hashes + height for a block (used by reconciliation).
    async fn block_subtrees(&self, hash: &Hash) -> Result<(u32, Vec<Hash>), StoreError>;
    /// Block height, blockchain block ID, and raw coinbase tx bytes for `hash`
    /// (Go `GetBlock` -> `{Height, ID, CoinbaseTx}`). Used by the reorg
    /// moveForward pass to create each adopted block's coinbase UTXO. The
    /// coinbase bytes may be empty if the service did not return one.
    async fn block_coinbase(&self, hash: &Hash) -> Result<(u32, u32, Vec<u8>), StoreError>;
    /// Previous-block hash + height for `hash` (the reorg common-ancestor walk).
    async fn block_header(&self, hash: &Hash) -> Result<BlockHeaderInfo, StoreError>;
    /// Publish a fully-built block to the blockchain service.
    async fn add_block(
        &self,
        header: &[u8],
        subtree_hashes: &[Hash],
        coinbase_tx: &[u8],
        tx_count: u64,
        size_in_bytes: u64,
        coinbase_bump: &[u8],
    ) -> Result<(), StoreError>;
    /// Signal the block's subtrees are set (post-publish).
    async fn set_block_subtrees_set(&self, block_hash: &Hash) -> Result<(), StoreError>;
    /// Whether the blockchain FSM is currently in `state` (e.g. "RUNNING").
    /// Mirrors Go `IsFSMCurrentState`; used to gate the subtree notification.
    async fn is_fsm_current_state(&self, state: &str) -> Result<bool, StoreError>;
    /// Broadcast a `Subtree` notification for `subtree_hash` (Go
    /// `SendNotification` with `NotificationType_Subtree`). Best-effort.
    async fn send_notification_subtree(&self, subtree_hash: &Hash) -> Result<(), StoreError>;
}

/// Subtree blob reads + writes (filesystem in Stage 3).
///
/// Only the subtree `FileType` is exercised in Capability B, so a single `set`
/// for pre-serialized subtree blobs (header + body) is sufficient — a general
/// `FileType` enum is deferred. `set_dah` (delete-at-height / TTL) is recorded
/// best-effort; real pruning is deferred (Capability H).
#[async_trait]
pub trait BlobStore: Send + Sync {
    async fn tx_hashes(&self, subtree_hash: &Hash) -> Result<Vec<Hash>, StoreError>;
    /// Read the full deserialized subtree for `root` (its nodes carry fee/size,
    /// plus the `conflicting_nodes` list). moveBack uses this to re-add orphaned
    /// txs with their persisted fee/size — no extra UTXO read needed.
    async fn subtree(&self, root: &Hash) -> Result<Subtree, StoreError>;
    /// Write pre-serialized subtree blob bytes (8-byte header + `serialize()`
    /// body) to the reverse-hash `.subtree` path for `key`.
    async fn set(&self, key: &Hash, bytes: &[u8]) -> Result<(), StoreError>;
    /// Best-effort delete-at-height hint. Currently a no-op (logged); real
    /// pruning is deferred to the dedicated DAH/pruning work (Capability H).
    async fn set_dah(&self, key: &Hash, dah: u32) -> Result<(), StoreError>;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mined_block_info_constructs() {
        let info = MinedBlockInfo {
            block_id: 7,
            block_height: 100,
            subtree_idx: 0,
            on_longest_chain: true,
            unset_mined: false,
        };
        assert_eq!(info.block_id, 7);
    }

    #[test]
    fn store_error_displays() {
        let e = StoreError::Backend("boom".into());
        assert!(format!("{e}").contains("boom"));
    }

    #[test]
    fn traits_are_object_safe() {
        fn _assert(_: &dyn UtxoStore, _: &dyn BlobStore) {}
        // Compiles only if both traits are object-safe (usable as dyn).
    }

    #[test]
    fn spending_data_bytes_layout_is_36_txid_then_vin_le() {
        // txid is the raw 32-byte internal hash; vin is appended little-endian.
        let mut txid = [0u8; 32];
        for (i, b) in txid.iter_mut().enumerate() {
            *b = i as u8;
        }

        let sd = SpendingData {
            tx_id: txid,
            vin: 0x0A0B_0C0D,
        };
        let bytes = sd.bytes();

        assert_eq!(
            bytes.len(),
            36,
            "SpendingData serializes to exactly 36 bytes"
        );
        assert_eq!(&bytes[..32], &txid, "first 32 bytes are the raw txid");
        // 0x0A0B0C0D little-endian = [0x0D, 0x0C, 0x0B, 0x0A].
        assert_eq!(
            &bytes[32..],
            &[0x0D, 0x0C, 0x0B, 0x0A],
            "vin is a little-endian u32 in the trailing 4 bytes"
        );
    }

    #[test]
    fn spending_data_bytes_zero_vin() {
        let sd = SpendingData {
            tx_id: [0xFFu8; 32],
            vin: 0,
        };
        let bytes = sd.bytes();
        assert_eq!(&bytes[..32], &[0xFFu8; 32]);
        assert_eq!(&bytes[32..], &[0, 0, 0, 0]);
    }
}

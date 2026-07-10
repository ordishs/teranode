//! Real BlockchainClient over gRPC (tonic). The candidate handlers seed their
//! tip from `best_tip`; the notification subscription (Group H2) lives here too
//! (`run_subscription` / `handle_block`), reusing the connected client and
//! writing mined-state back to the UTXO store on block-extend.

use std::collections::HashSet;
use std::sync::{Arc, Mutex};

use ba_subtree_bench::hash::{sha256d, Hash};
use ba_subtree_bench::tx::Tx;
use tonic::async_trait;
use tonic::transport::Channel;

use super::blob_fs::FsBlobStore;
use super::{
    BlobStore, BlockHeaderInfo, BlockchainClient, ChainTip, MinedBlockInfo, StoreError, UtxoStore,
};
use crate::assembly::AssemblyState;
use crate::blockchain_api::blockchain_api_client::BlockchainApiClient;
use crate::blockchain_api::{
    AddBlockRequest, FsmStateType, GetBlockHeadersRequest, GetBlockRequest,
    GetNextWorkRequiredRequest, Notification, NotificationMetadata, SetBlockSubtreesSetRequest,
    SubscribeRequest,
};
use crate::model::NotificationType;

/// model.NotificationType.Block — the same value the Go BA filters on.
const NOTIF_BLOCK: i32 = 2;

#[derive(Clone)]
pub struct GrpcBlockchainClient {
    pub(crate) inner: BlockchainApiClient<Channel>,
}

impl GrpcBlockchainClient {
    pub async fn connect(addr: String) -> Result<Self, StoreError> {
        let inner = BlockchainApiClient::connect(addr.clone())
            .await
            .map_err(|e| StoreError::Backend(format!("blockchain connect {addr}: {e}")))?;

        Ok(Self { inner })
    }

    /// Accessor so the subscription can reuse the connected client.
    pub(crate) fn client(&self) -> BlockchainApiClient<Channel> {
        self.inner.clone()
    }
}

/// Parse an 80-byte block header into (version, n_bits, block_time).
fn parse_header(header: &[u8]) -> Result<(u32, u32, u32), StoreError> {
    if header.len() < 80 {
        return Err(StoreError::Decode(format!("header len {}", header.len())));
    }

    let version = u32::from_le_bytes([header[0], header[1], header[2], header[3]]);
    let block_time = u32::from_le_bytes([header[68], header[69], header[70], header[71]]);
    let n_bits = u32::from_le_bytes([header[72], header[73], header[74], header[75]]);

    Ok((version, n_bits, block_time))
}

#[async_trait]
impl BlockchainClient for GrpcBlockchainClient {
    async fn best_tip(&self) -> Result<ChainTip, StoreError> {
        let mut c = self.inner.clone();
        let resp = c
            .get_best_block_header(())
            .await
            .map_err(|e| StoreError::Backend(format!("get_best_block_header: {e}")))?
            .into_inner();

        let header = resp.block_header;
        let (version, n_bits, median_time) = parse_header(&header)?;
        let hash = sha256d(&header[..80]);

        Ok(ChainTip {
            hash,
            height: resp.height,
            n_bits,
            version,
            median_time,
        })
    }

    async fn get_next_work_required(
        &self,
        prev_hash: &Hash,
        next_block_time: i64,
    ) -> Result<u32, StoreError> {
        let mut c = self.inner.clone();
        let resp = c
            .get_next_work_required(GetNextWorkRequiredRequest {
                previous_block_hash: prev_hash.to_vec(),
                current_block_time: next_block_time,
            })
            .await
            .map_err(|e| StoreError::Backend(format!("get_next_work_required: {e}")))?
            .into_inner();

        let bits = resp.bits;
        if bits.len() != 4 {
            return Err(StoreError::Decode(format!(
                "get_next_work_required: bits len {} != 4",
                bits.len()
            )));
        }

        Ok(u32::from_le_bytes([bits[0], bits[1], bits[2], bits[3]]))
    }

    async fn block_header_ids(&self, hash: &Hash, n: u64) -> Result<Vec<u32>, StoreError> {
        let mut c = self.inner.clone();
        let resp = c
            .get_block_header_i_ds(GetBlockHeadersRequest {
                start_hash: hash.to_vec(),
                number_of_headers: n,
            })
            .await
            .map_err(|e| StoreError::Backend(format!("get_block_header_ids: {e}")))?
            .into_inner();

        Ok(resp.ids)
    }

    async fn block_subtrees(&self, hash: &Hash) -> Result<(u32, Vec<Hash>), StoreError> {
        let mut c = self.inner.clone();
        let blk = c
            .get_block(GetBlockRequest {
                hash: hash.to_vec(),
            })
            .await
            .map_err(|e| StoreError::Backend(format!("get_block: {e}")))?
            .into_inner();

        let subtrees = blk
            .subtree_hashes
            .iter()
            .filter_map(|s| <[u8; 32]>::try_from(s.as_slice()).ok())
            .collect();

        Ok((blk.height, subtrees))
    }

    async fn block_coinbase(&self, hash: &Hash) -> Result<(u32, u32, Vec<u8>), StoreError> {
        let mut c = self.inner.clone();
        let blk = c
            .get_block(GetBlockRequest {
                hash: hash.to_vec(),
            })
            .await
            .map_err(|e| StoreError::Backend(format!("get_block: {e}")))?
            .into_inner();

        Ok((blk.height, blk.id, blk.coinbase_tx))
    }

    async fn block_header(&self, hash: &Hash) -> Result<BlockHeaderInfo, StoreError> {
        // No dedicated lightweight header RPC is generated; reuse get_block,
        // which carries the 80-byte header (prev_hash @ [4..36]) and the stored
        // block height. The reorg walk only needs prev_hash + height.
        let mut c = self.inner.clone();
        let blk = c
            .get_block(GetBlockRequest {
                hash: hash.to_vec(),
            })
            .await
            .map_err(|e| StoreError::Backend(format!("get_block: {e}")))?
            .into_inner();

        if blk.header.len() < 80 {
            return Err(StoreError::Decode(format!(
                "block header len {}",
                blk.header.len()
            )));
        }

        let mut prev_hash = [0u8; 32];
        prev_hash.copy_from_slice(&blk.header[4..36]);

        Ok(BlockHeaderInfo {
            prev_hash,
            height: blk.height,
        })
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
        let mut c = self.inner.clone();
        c.add_block(AddBlockRequest {
            header: header.to_vec(),
            subtree_hashes: subtree_hashes.iter().map(|h| h.to_vec()).collect(),
            coinbase_tx: coinbase_tx.to_vec(),
            transaction_count: tx_count,
            size_in_bytes,
            external: false,
            peer_id: String::new(),
            option_mined_set: false,
            option_subtrees_set: false,
            option_invalid: false,
            option_id: 0,
            coinbase_bump: coinbase_bump.to_vec(),
        })
        .await
        .map_err(|e| StoreError::Backend(format!("add_block: {e}")))?;

        Ok(())
    }

    async fn set_block_subtrees_set(&self, block_hash: &Hash) -> Result<(), StoreError> {
        let mut c = self.inner.clone();
        c.set_block_subtrees_set(SetBlockSubtreesSetRequest {
            block_hash: block_hash.to_vec(),
        })
        .await
        .map_err(|e| StoreError::Backend(format!("set_block_subtrees_set: {e}")))?;

        Ok(())
    }

    async fn is_fsm_current_state(&self, state: &str) -> Result<bool, StoreError> {
        // No dedicated `IsFSMCurrentState` RPC exists in the generated client —
        // Go's helper is a server-side wrapper around the same state read. We do
        // the equivalent client-side: read `GetFSMCurrentState` and compare the
        // returned `FsmStateType` enum to the requested state name.
        let want = FsmStateType::from_str_name(state)
            .ok_or_else(|| StoreError::Backend(format!("unknown FSM state {state}")))?;

        let mut c = self.inner.clone();
        let resp = c
            .get_fsm_current_state(())
            .await
            .map_err(|e| StoreError::Backend(format!("get_fsm_current_state: {e}")))?
            .into_inner();

        Ok(resp.state == want as i32)
    }

    async fn send_notification_subtree(&self, subtree_hash: &Hash) -> Result<(), StoreError> {
        let mut c = self.inner.clone();
        c.send_notification(Notification {
            r#type: NotificationType::Subtree as i32,
            hash: subtree_hash.to_vec(),
            base_url: String::new(),
            metadata: Some(NotificationMetadata {
                metadata: std::collections::HashMap::new(),
            }),
        })
        .await
        .map_err(|e| StoreError::Backend(format!("send_notification: {e}")))?;

        Ok(())
    }
}

fn to_hash(bytes: &[u8]) -> Option<Hash> {
    <[u8; 32]>::try_from(bytes).ok()
}

/// Subscribe to the blockchain service's notification stream — the SAME
/// mechanism the Go BA uses — and reconcile each `Block` notification into the
/// assembly. On a chain extend we additionally persist mined-state to the UTXO
/// store via the `setMined` batch UDF.
///
/// `utxo` is a trait object so unit/integration callers can swap the backend;
/// `chain` is the concrete gRPC client (the subscription is gRPC-specific).
pub async fn run_subscription(
    chain: Arc<GrpcBlockchainClient>,
    utxo: Arc<dyn UtxoStore>,
    state: Arc<Mutex<AssemblyState>>,
    subtree_store_path: String,
) -> Result<(), Box<dyn std::error::Error>> {
    let blob = FsBlobStore::new(subtree_store_path);
    let mut client = chain.client();

    let mut stream = client
        .subscribe(SubscribeRequest {
            source: "ba-service-rust".to_string(),
        })
        .await?
        .into_inner();

    println!("ba-service: subscribed to blockchain notifications");

    while let Some(n) = stream.message().await? {
        if n.r#type == NOTIF_BLOCK {
            if let Err(e) = handle_block(&chain, &utxo, &blob, &state, &n.hash).await {
                eprintln!("ba-service: handle_block error: {e}");
            }
        }
    }

    Ok(())
}

async fn handle_block(
    chain: &Arc<GrpcBlockchainClient>,
    utxo: &Arc<dyn UtxoStore>,
    blob: &FsBlobStore,
    state: &Arc<Mutex<AssemblyState>>,
    block_hash: &[u8],
) -> Result<(), Box<dyn std::error::Error>> {
    // The notification is only a wake-up trigger — reconcile to the blockchain's
    // CURRENT BEST tip, NOT the notified block (Go parity:
    // BlockAssembler.processNewBlockAnnouncement fetches the best block header and
    // ignores the notification payload). Reacting to the notified block produced
    // spurious, growing "reorgs" (moveForward=0, moveBack climbing) whenever the
    // notification stream lagged or replayed blocks behind the real tip.
    let _ = block_hash; // notification hash intentionally not used as the new tip
    let best = chain.best_tip().await?;
    let new_hash = best.hash;

    // No-op if the assembly is already at the best tip (stale/duplicate notif).
    if new_hash == state.lock().unwrap().chain.best_hash {
        return Ok(());
    }

    let mut client = chain.client();
    let blk = client
        .get_block(GetBlockRequest {
            hash: new_hash.to_vec(),
        })
        .await?
        .into_inner();

    if blk.header.len() < 80 {
        return Err(format!("block header too short: {}", blk.header.len()).into());
    }
    let (_version, n_bits, block_time) = parse_header(&blk.header)?;
    let mut prev = [0u8; 32];
    prev.copy_from_slice(&blk.header[4..36]);

    // Collect the block's mined tx set from its subtrees (filesystem blob store).
    let mut mined: HashSet<Hash> = HashSet::new();
    for sh in &blk.subtree_hashes {
        if let Some(h) = to_hash(sh) {
            match blob.tx_hashes(&h).await {
                Ok(txs) => mined.extend(txs),
                Err(e) => eprintln!("ba-service: subtree {} read failed: {e}", hex::encode(h)),
            }
        }
    }

    // Detect extend vs reorg from the previous tip WITHOUT mutating state yet —
    // the reorg path needs the pre-reorg tip to walk to the common ancestor.
    let (is_extend, current_tip, current_height) = {
        let st = state.lock().unwrap();
        (
            prev == st.chain.best_hash,
            st.chain.best_hash,
            st.chain.height,
        )
    };

    if is_extend {
        // Extend: adopt the new tip and drop its mined txs from the assembly.
        {
            let mut st = state.lock().unwrap();
            st.chain
                .apply_block(new_hash, blk.height, n_bits, block_time);
            if !mined.is_empty() {
                st.apply_mined_block(&mined);
            }
        }

        // The setMined UDF (and the coinbase's blockIDs) key on the blockchain's
        // block ID, NOT the height. Prefer the ID the GetBlock response carries
        // (field 7); fall back to block_header_ids, then height as a last resort.
        let block_id = resolve_block_id(chain.as_ref(), &new_hash, blk.id, blk.height).await;

        // Create THIS block's coinbase UTXO (Go SubtreeProcessor.processCoinbaseUtxos,
        // run on every moveForward — including empty blocks). Without this an
        // empty-block-only chain produces ZERO UTXO records. Non-fatal on error,
        // like Go (which logs and continues): the tip has already advanced.
        create_block_coinbase_utxo(
            utxo.as_ref(),
            &blk.coinbase_tx,
            blk.height,
            block_id,
            &new_hash,
        )
        .await;

        if !mined.is_empty() {
            let hashes: Vec<Hash> = mined.iter().copied().collect();

            let info = MinedBlockInfo {
                block_id,
                block_height: blk.height,
                subtree_idx: 0,
                on_longest_chain: true,
                unset_mined: false,
            };

            if let Err(e) = utxo.set_mined_multi(&hashes, &info).await {
                eprintln!("ba-service: setMined failed: {e}");
            }
            println!(
                "ba-service: extend -> height {} (setMined {} txs, block_id {})",
                blk.height,
                hashes.len(),
                block_id
            );
        } else {
            println!(
                "ba-service: extend -> height {} (coinbase created, no mined txs in assembly)",
                blk.height
            );
        }

        return Ok(());
    }

    // DIAGNOSTIC: dump the exact divergence so we can see WHY this is a reorg.
    // local tip vs the notified block (its height + own prev). A growing
    // (current_height - blk.height) with the notified block on the local chain
    // means the local tip is racing ahead of the notification stream.
    eprintln!(
        "ba-service: NON-EXTEND notif — local_tip(h={current_height} hash={}) | notif_block(h={} hash={} prev={})",
        hex::encode(current_tip),
        blk.height,
        hex::encode(new_hash),
        hex::encode(prev),
    );

    // Reorg: walk to the common ancestor and reconcile, running the conflict
    // cascade (db4). The mined-set collected above is unused on this path;
    // `reconcile_reorg` re-reads each moveForward block's txs itself.
    let chain_dyn: Arc<dyn BlockchainClient> = chain.clone();
    reconcile_reorg(
        chain_dyn.as_ref(),
        blob,
        utxo.as_ref(),
        state,
        current_tip,
        current_height,
        new_hash,
        blk.height,
        n_bits,
        block_time,
    )
    .await
}

/// Resolve a block's blockchain ID for setMined / coinbase blockIDs. Prefer the
/// ID the GetBlock response already carried (`resp_id`, field 7) when non-zero;
/// otherwise query `block_header_ids`; fall back to `height` as a last resort.
pub(crate) async fn resolve_block_id(
    chain: &dyn BlockchainClient,
    hash: &Hash,
    resp_id: u32,
    height: u32,
) -> u32 {
    if resp_id != 0 {
        return resp_id;
    }
    match chain.block_header_ids(hash, 1).await {
        Ok(ids) if !ids.is_empty() => ids[0],
        Ok(_) => {
            eprintln!(
                "ba-service: block id unavailable for tip — falling back to height {height} as block_id"
            );
            height
        }
        Err(e) => {
            eprintln!(
                "ba-service: block_header_ids failed ({e}) — falling back to height {height} as block_id"
            );
            height
        }
    }
}

/// Create a block's coinbase UTXO record (Go SubtreeProcessor.processCoinbaseUtxos).
/// Parses the raw coinbase bytes, then writes a single-record UTXO with the
/// block's mined info. Non-fatal: every failure is logged and swallowed, exactly
/// as Go logs-and-continues — the tip has already advanced and a missing coinbase
/// UTXO must not wedge the subscription. A no-op when `coinbase_bytes` is empty.
pub(crate) async fn create_block_coinbase_utxo(
    utxo: &dyn UtxoStore,
    coinbase_bytes: &[u8],
    height: u32,
    block_id: u32,
    block_hash: &Hash,
) {
    if coinbase_bytes.is_empty() {
        eprintln!(
            "ba-service: no coinbase tx for block {} (h={height}) — skipping coinbase UTXO create",
            hex::encode(block_hash)
        );
        return;
    }

    let tx = match Tx::from_bytes(coinbase_bytes) {
        Ok(tx) => tx,
        Err(e) => {
            eprintln!(
                "ba-service: coinbase parse failed for block {} (h={height}): {e:?} — skipping coinbase UTXO create",
                hex::encode(block_hash)
            );
            return;
        }
    };

    let mined = MinedBlockInfo {
        block_id,
        block_height: height,
        subtree_idx: 0,
        on_longest_chain: true,
        unset_mined: false,
    };

    // Coinbase create passes locked=false (Go processCoinbaseUtxos uses only
    // WithMinedBlockInfo).
    if let Err(e) = utxo.create(&tx, height, Some(mined), false).await {
        eprintln!(
            "ba-service: coinbase UTXO create failed for block {} (h={height}): {e}",
            hex::encode(block_hash)
        );
    }
}

/// Maximum blocks the reorg walk will step before giving up (Go's
/// `MaxGetReorgHashes`; default mirrors the block-height retention bound).
const MAX_REORG_WALK: u32 = 10_000;

/// Reorg reconciliation with the conflict cascade (Capability D-b / db4).
///
/// Walks both chains to their common ancestor, then:
///   - **moveForward collect** (adopted blocks, ascending): gather the txs mined
///     on the adopted chain (the `moveForwardTxSet` filter + the drop set).
///   - **moveBack** (orphaned blocks, descending): for each orphaned block whose
///     subtree carries `conflicting_nodes`, run `reverse_process_conflicting`
///     (skipping re-mined txs and the coinbase placeholder), thread the demoted
///     hashes + restored counters into the shared processed map, and collect the
///     cascaded txs into the evict set. Re-add the block's non-placeholder,
///     non-evicted txs to the assembly with their persisted fee/size.
///   - **moveForward cascade** (adopted blocks, ascending): for each adopted
///     block with `conflicting_nodes`, run `process_conflicting` with the shared
///     processed map; collect everything it marks into the evict set.
///   - **assembly**: return orphaned txs, drop adopted txs, `register_conflicting`
///     the evict set (eviction + future-add rejection), adopt the new tip.
///   - **marks**: mark moveForward txs (winners not returned via moveBack and not
///     conflicting) on the longest chain; mark returned moveBack txs off it.
///
/// A cascade error PROPAGATES (`?`) and fails the reorg — a conflicting reorg the
/// authoritative UTXO store cannot reconcile must not silently proceed (this
/// replaces D-a's warn-and-proceed).
///
/// Generic over trait objects so unit tests drive it with mem chain/blob/utxo.
#[allow(clippy::too_many_arguments)]
async fn reconcile_reorg(
    chain: &dyn BlockchainClient,
    blob: &dyn BlobStore,
    utxo: &dyn UtxoStore,
    state: &Arc<Mutex<AssemblyState>>,
    current_tip: Hash,
    current_height: u32,
    new_tip: Hash,
    new_height: u32,
    n_bits: u32,
    block_time: u32,
) -> Result<(), Box<dyn std::error::Error>> {
    // Header lookups for the common-ancestor walk go through the blockchain
    // client. Pre-fetch into a cache so the pure walk stays synchronous.
    let lists = {
        let mut cache: std::collections::HashMap<Hash, BlockHeaderInfo> =
            std::collections::HashMap::new();

        // Eagerly fetch headers along both chains up to MAX_REORG_WALK so the
        // closure can resolve them without async. Walk current then new tip.
        for start in [current_tip, new_tip] {
            let mut h = start;
            for _ in 0..=MAX_REORG_WALK {
                if cache.contains_key(&h) {
                    break;
                }
                match chain.block_header(&h).await {
                    Ok(info) => {
                        let prev = info.prev_hash;
                        cache.insert(h, info);
                        h = prev;
                    }
                    Err(_) => break,
                }
            }
        }

        crate::reorg::common_ancestor_and_lists(
            current_tip,
            current_height,
            new_tip,
            new_height,
            |hash| cache.get(hash).cloned(),
            MAX_REORG_WALK,
        )?
    };

    // GUARD: no moveForward blocks => the "new" (best) tip is an ANCESTOR of the
    // current local tip, i.e. the local tip is already ahead. This is notification
    // lag — `best_tip()` trailing a block we just advanced past (e.g. a block this
    // service itself just submitted) — NOT a real reorg, which always moves at
    // least one block forward. Adopting it would REGRESS the tip and cause the
    // spurious off-tip candidates / `stale candidate` errors during self-mining.
    // Treat as a no-op and leave the local tip where it is.
    if lists.move_forward.is_empty() {
        return Ok(());
    }

    // Shared conflict-cascade state threaded across the moveBack reverse and the
    // moveForward forward passes (Go `processedConflictingHashesMap` + the reverse
    // cascade eviction set). `evict` holds every tx the cascades flagged
    // conflicting — they must NOT re-enter the assembly nor be marked as winners.
    let mut processed: std::collections::HashMap<Hash, bool> = std::collections::HashMap::new();
    let mut evict: HashSet<Hash> = HashSet::new();

    // moveForward tx set = the txs mined on the adopted chain (collected below as
    // `forward_set`). A tx re-mined on the new chain must NOT be reversed in the
    // moveBack pass (Go's `moveForwardTxSet` filter) — its canonical-spender
    // status carries across the reorg and moveForward handles it natively. We
    // collect moveForward first so the filter is available in the moveBack loop.
    let mut forward_set: HashSet<Hash> = HashSet::new();
    let mut forward_blocks: Vec<(u32, Vec<Hash>)> = Vec::new();
    for blk_hash in &lists.move_forward {
        let (h, roots) = chain.block_subtrees(blk_hash).await?;
        for root in &roots {
            match blob.tx_hashes(root).await {
                Ok(txs) => forward_set.extend(txs),
                Err(e) => eprintln!(
                    "ba-service: reorg moveForward subtree {} read failed: {e}",
                    hex::encode(root)
                ),
            }
        }

        // Create each adopted block's coinbase UTXO — Go runs
        // processCoinbaseUtxos for EVERY moveForward block, not just on a plain
        // extend. Best-effort fetch+create (the coinbase RPC may fail / be empty
        // in tests, in which case it's skipped without aborting the reorg).
        match chain.block_coinbase(blk_hash).await {
            Ok((cb_height, cb_id, cb_bytes)) => {
                create_block_coinbase_utxo(utxo, &cb_bytes, cb_height, cb_id, blk_hash).await;
            }
            Err(e) => eprintln!(
                "ba-service: reorg moveForward coinbase fetch {} failed: {e}",
                hex::encode(blk_hash)
            ),
        }

        forward_blocks.push((h, roots));
    }
    let move_forward_tx_set = &forward_set;

    // moveBack: for each orphaned block (descending) read its subtrees and
    // re-add the non-placeholder txs (with persisted fee/size) to the assembly,
    // SKIPPING any hash the reverse cascade flagged conflicting. When a block's
    // subtree carries `conflicting_nodes`, undo its original ProcessConflicting
    // swap via ReverseProcessConflicting (faithful to Go reorgBlocks 2934-3060).
    let mut returned_set: HashSet<Hash> = HashSet::new();
    let mut returned: Vec<(Hash, u64, u64)> = Vec::new();

    for blk_hash in &lists.move_back {
        let (block_height, roots) = chain.block_subtrees(blk_hash).await?;
        let mut subtrees: Vec<ba_subtree_bench::subtree::Subtree> = Vec::new();

        for root in &roots {
            match blob.subtree(root).await {
                Ok(st) => subtrees.push(st),
                Err(e) => {
                    eprintln!(
                        "ba-service: reorg moveBack subtree {} read failed: {e}",
                        hex::encode(root)
                    );
                }
            }
        }

        // Gather this block's conflicting nodes across its subtrees.
        let mut conflicting_nodes: Vec<Hash> = Vec::new();
        let mut conflicting_seen: HashSet<Hash> = HashSet::new();
        for st in &subtrees {
            for h in &st.conflicting_nodes {
                if conflicting_seen.insert(*h) {
                    conflicting_nodes.push(*h);
                }
            }
        }

        // Reverse the conflict resolution this orphaned block had applied. Skip
        // any tx re-mined on the new chain (moveForwardTxSet) and the frozen
        // coinbase placeholder (the latter is also skipped inside the cascade).
        if !conflicting_nodes.is_empty() {
            let reverse_inputs: Vec<Hash> = conflicting_nodes
                .iter()
                .copied()
                .filter(|h| {
                    !move_forward_tx_set.contains(h)
                        && *h != ba_subtree_bench::subtree::COINBASE_PLACEHOLDER
                })
                .collect();

            let (cascaded, touched) = crate::process_conflicting::reverse_process_conflicting(
                utxo,
                block_height,
                &reverse_inputs,
            )
            .await?;

            // processed-map handoff: both the demoted hashes AND the restored
            // counters are recorded so the moveForward pass skips re-running
            // ProcessConflicting on them (Go reorgBlocks 2986-2993).
            for h in conflicting_nodes.iter().copied().chain(touched.into_iter()) {
                processed.insert(h, true);
            }
            evict.extend(cascaded);
        }

        // Re-add the block's non-placeholder, non-evicted txs to the assembly.
        for st in &subtrees {
            for n in &st.nodes {
                if n.hash == ba_subtree_bench::subtree::COINBASE_PLACEHOLDER {
                    continue;
                }
                if evict.contains(&n.hash) {
                    continue;
                }
                if returned_set.insert(n.hash) {
                    returned.push((n.hash, n.fee, n.size_in_bytes));
                }
            }
        }
    }

    // moveForward conflict cascade: for each adopted block whose subtree carries
    // conflicting_nodes, run the forward ProcessConflicting with the shared
    // processed map; evict everything it marks (Go processConflictingTransactions).
    for (block_height, roots) in &forward_blocks {
        let mut conflicting_nodes: Vec<Hash> = Vec::new();
        let mut conflicting_seen: HashSet<Hash> = HashSet::new();
        for root in roots {
            if let Ok(st) = blob.subtree(root).await {
                for h in &st.conflicting_nodes {
                    if conflicting_seen.insert(*h) {
                        conflicting_nodes.push(*h);
                    }
                }
            }
        }

        if !conflicting_nodes.is_empty() {
            let (_losing, all_marked) = crate::process_conflicting::process_conflicting(
                utxo,
                *block_height,
                &conflicting_nodes,
                &processed,
            )
            .await?;
            evict.extend(all_marked);
        }
    }

    // Apply to the assembly under the lock: return orphaned txs (already filtered
    // by `evict`), drop adopted txs, register the conflicting cascade set (so any
    // already-present or future conflicting tx is evicted/rejected), adopt the tip.
    {
        let mut st = state.lock().unwrap();
        if !returned.is_empty() {
            st.return_txs(&returned);
        }
        if !forward_set.is_empty() {
            st.apply_mined_block(&forward_set);
        }
        if !evict.is_empty() {
            st.register_conflicting(&evict.iter().copied().collect::<Vec<_>>());
        }
        st.chain
            .apply_block(new_tip, new_height, n_bits, block_time);
    }

    // Longest-chain marks (authoritative, idempotent): winners = moveForward txs
    // not also returned via moveBack and not conflicting; losers = returned
    // moveBack txs (now unmined). Best-effort — log on error, never fail the reorg.
    //
    // KNOWN GAP (db4 review, follow-up): Go reorgBlocks marks an `allMarkFalse`
    // set = losing-conflicting txs UNION every tx currently resident in block
    // assembly (chained+current), and relies on BlockAssembler.Reset (run before
    // reorgBlocks) to handle moveBack txs. This port marks moveBack `returned`
    // losers + excludes `evict` from winners, but does NOT yet mark
    // resident-assembly txs off-longest-chain. This is a mempool on-longest-chain
    // FLAG-coverage gap (not a UTXO/conflict corruption); close it when the
    // Reset-equivalent layer is wired. Ref: SubtreeProcessor.go:3169-3206.
    let winners: Vec<Hash> = forward_set
        .iter()
        .filter(|h| !returned_set.contains(*h) && !evict.contains(*h))
        .copied()
        .collect();
    if !winners.is_empty() {
        if let Err(e) = utxo.mark_on_longest_chain(&winners, true).await {
            eprintln!("ba-service: reorg mark winners on-longest-chain failed: {e}");
        }
    }

    let losers: Vec<Hash> = returned.iter().map(|(h, _, _)| *h).collect();
    if !losers.is_empty() {
        if let Err(e) = utxo.mark_on_longest_chain(&losers, false).await {
            eprintln!("ba-service: reorg mark returned off-longest-chain failed: {e}");
        }
    }

    println!(
        "ba-service: REORG -> height {} (moveBack {} blocks/{} txs, moveForward {} blocks/{} txs)",
        new_height,
        lists.move_back.len(),
        returned.len(),
        lists.move_forward.len(),
        forward_set.len(),
    );

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_known_genesis_header() {
        // Bitcoin mainnet genesis block header (80 bytes).
        let header = hex::decode(
            "0100000000000000000000000000000000000000000000000000000000000000\
             000000003ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa\
             4b1e5e4a29ab5f49ffff001d1dac2b7c",
        )
        .unwrap();

        let (version, n_bits, block_time) = parse_header(&header).unwrap();
        assert_eq!(version, 1);
        assert_eq!(n_bits, 0x1d00_ffff);
        assert_eq!(block_time, 1_231_006_505);

        // Genesis block hash (double-SHA256 of the header, little-endian byte order).
        let hash = sha256d(&header[..80]);
        let expected =
            hex::decode("6fe28c0ab6f1b372c1a6a246ae63f74f931e8365e15a089c68d6190000000000")
                .unwrap();
        assert_eq!(&hash[..], &expected[..]);
    }

    #[test]
    fn rejects_short_header() {
        let err = parse_header(&[0u8; 40]).unwrap_err();
        assert!(matches!(err, StoreError::Decode(_)));
    }

    // get_next_work_required — rig-deferred. The DAA target the blockchain
    // service returns can only be exercised against a live endpoint; the offline
    // suite covers the request build + nBits decode via the Mem client
    // (chain_mem.rs). Marked `#[ignore]` so the suite stays green without a rig,
    // and carries a VERIFY-ON-RIG contract. Connection params come from the same
    // env the rig sets for the gRPC backend.
    #[tokio::test]
    #[ignore = "VERIFY-ON-RIG: requires live blockchain service for the real DAA target"]
    async fn rig_get_next_work_required_returns_real_daa_target() {
        // Connection address from the rig's env (mirrors main.rs, which wires a
        // real GrpcBlockchainClient from cfg.blockchain_grpc_address). On the rig
        // this MUST point at the running blockchain service.
        let addr = std::env::var("BLOCKCHAIN_GRPC_ADDRESS")
            .unwrap_or_else(|_| "http://127.0.0.1:8087".into());

        let client = GrpcBlockchainClient::connect(addr)
            .await
            .expect("connect to the rig blockchain service");

        // Use the live tip as the previous block; ask for the work required for a
        // block one second after the tip's median time (the candidate path uses
        // median_time + 1 as next_block_time).
        let tip = client
            .best_tip()
            .await
            .expect("best_tip from the rig blockchain service");
        let next_block_time = i64::from(tip.median_time) + 1;

        let n_bits = client
            .get_next_work_required(&tip.hash, next_block_time)
            .await
            .expect("get_next_work_required from the rig blockchain service");

        // The result is the compact nBits the DAA computed for the next block.
        // It must be a plausible non-zero compact target, and on a real chain it
        // equals the DAA target the blockchain service derived for the tip.
        assert_ne!(n_bits, 0, "DAA nBits must be non-zero");

        // Compact-form sanity: the exponent (high byte) must fit a 256-bit target
        // (Bitcoin's max compact exponent is 0x1d), and the mantissa is non-zero.
        let exponent = (n_bits >> 24) as u8;
        let mantissa = n_bits & 0x00ff_ffff;
        assert!(
            exponent <= 0x1d,
            "nBits exponent {exponent:#x} out of range"
        );
        assert_ne!(mantissa, 0, "nBits mantissa must be non-zero");
    }

    use std::collections::HashMap as StdHashMap;

    use ba_subtree_bench::subtree::Subtree;

    use crate::store::chain_mem::MemBlockchainClient;
    use crate::store::utxo_mem::MemUtxoStore;

    /// In-memory BlobStore for the reorg test: stores full subtrees by root,
    /// returns deserialized copies (with fee/size + conflicting list).
    #[derive(Default)]
    struct MemBlobStore {
        subtrees: Mutex<StdHashMap<Hash, Subtree>>,
    }

    impl MemBlobStore {
        fn insert(&self, root: Hash, st: Subtree) {
            self.subtrees.lock().unwrap().insert(root, st);
        }
    }

    #[async_trait]
    impl BlobStore for MemBlobStore {
        async fn tx_hashes(&self, root: &Hash) -> Result<Vec<Hash>, StoreError> {
            self.subtrees
                .lock()
                .unwrap()
                .get(root)
                .map(|st| st.tx_hashes())
                .ok_or_else(|| StoreError::NotFound(hex::encode(root)))
        }

        async fn subtree(&self, root: &Hash) -> Result<Subtree, StoreError> {
            self.subtrees
                .lock()
                .unwrap()
                .get(root)
                .cloned()
                .ok_or_else(|| StoreError::NotFound(hex::encode(root)))
        }

        async fn set(&self, _key: &Hash, _bytes: &[u8]) -> Result<(), StoreError> {
            Ok(())
        }

        async fn set_dah(&self, _key: &Hash, _dah: u32) -> Result<(), StoreError> {
            Ok(())
        }
    }

    fn leaf(i: u32) -> Hash {
        sha256d(&i.to_le_bytes())
    }

    /// Build a single-node subtree for `tx` (with its fee/size) and return its
    /// root + the subtree. The reorg path reads non-placeholder nodes; a bare
    /// one-tx subtree is enough to exercise moveBack/moveForward.
    fn subtree_for(tx: Hash, fee: u64, size: u64) -> (Hash, Subtree) {
        let mut st = Subtree::new();
        st.add_node(tx, fee, size);
        let root = st.root_hash().unwrap();
        (root, st)
    }

    /// Da3: a 2-back / 3-forward no-conflict reorg reconciles the assembly.
    ///
    /// Fork graph (ancestor A at height 10):
    ///   current: A -> B(11) -> C(12)          (orphaned by the reorg)
    ///   new:     A -> X(11) -> Y(12) -> Z(13) (adopted by the reorg)
    /// Orphaned txs (in B,C) return to the assembly; adopted txs (in X,Y,Z)
    /// are dropped. The assembly is seeded with the adopted txs as unmined
    /// (they should be removed) plus a survivor that touches neither fork.
    #[tokio::test]
    async fn reorg_2back_3forward_reconciles_assembly() {
        // Block hashes.
        let a = [10u8; 32];
        let b = [11u8; 32];
        let c = [12u8; 32];
        let x = [21u8; 32];
        let y = [22u8; 32];
        let z = [23u8; 32];

        // Orphaned (moveBack) txs — one per old block, with distinct fee/size.
        let tx_b = leaf(100);
        let tx_c = leaf(101);
        // Adopted (moveForward) txs — one per new block.
        let tx_x = leaf(200);
        let tx_y = leaf(201);
        let tx_z = leaf(202);
        // A survivor not on either fork.
        let tx_keep = leaf(300);

        // Mem blob: subtrees keyed by root.
        let blob = MemBlobStore::default();
        let (root_b, st_b) = subtree_for(tx_b, 5, 50);
        let (root_c, st_c) = subtree_for(tx_c, 6, 60);
        let (root_x, st_x) = subtree_for(tx_x, 7, 70);
        let (root_y, st_y) = subtree_for(tx_y, 8, 80);
        let (root_z, st_z) = subtree_for(tx_z, 9, 90);
        blob.insert(root_b, st_b);
        blob.insert(root_c, st_c);
        blob.insert(root_x, st_x);
        blob.insert(root_y, st_y);
        blob.insert(root_z, st_z);

        // Mem chain: headers + per-block subtree roots.
        let mut mem = MemBlockchainClient::new(ChainTip {
            hash: c,
            height: 12,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        });
        // Ancestor A: parent is some genesis-ish hash (height 10).
        mem.set_header(a, [9u8; 32], 10);
        mem.set_header(b, a, 11);
        mem.set_header(c, b, 12);
        mem.set_header(x, a, 11);
        mem.set_header(y, x, 12);
        mem.set_header(z, y, 13);
        mem.set_block_subtrees(b, 11, vec![root_b]);
        mem.set_block_subtrees(c, 12, vec![root_c]);
        mem.set_block_subtrees(x, 11, vec![root_x]);
        mem.set_block_subtrees(y, 12, vec![root_y]);
        mem.set_block_subtrees(z, 13, vec![root_z]);
        // Seed coinbases on the adopted (moveForward) blocks so the reorg path
        // creates their coinbase UTXOs (Go runs processCoinbaseUtxos per adopted
        // block). Orphaned moveBack blocks (b, c) get NO coinbase create.
        let cb = |height: u32| {
            crate::coinbase::create_coinbase(
                height,
                50_0000_0000,
                b"r",
                &[crate::config::DEFAULT_COINBASE_ADDRESS.to_string()],
                [0u8; 12],
            )
            .unwrap()
        };
        let (cb_x, cb_y, cb_z) = (cb(11), cb(12), cb(13));
        let cb_x_txid = Tx::from_bytes(&cb_x).unwrap().txid();
        let cb_y_txid = Tx::from_bytes(&cb_y).unwrap().txid();
        let cb_z_txid = Tx::from_bytes(&cb_z).unwrap().txid();
        mem.set_block_coinbase(x, 11, 211, cb_x);
        mem.set_block_coinbase(y, 12, 212, cb_y);
        mem.set_block_coinbase(z, 13, 213, cb_z);
        let chain: Arc<dyn BlockchainClient> = Arc::new(mem);

        let utxo = Arc::new(MemUtxoStore::default());

        // Seed the assembly: tip at C(12); unmined = adopted txs + survivor.
        let state = Arc::new(Mutex::new(AssemblyState::new(64)));
        {
            let mut st = state.lock().unwrap();
            st.chain.apply_block(c, 12, 0x207f_ffff, 1_700_000_000);
            st.add(tx_x, 7, 70);
            st.add(tx_y, 8, 80);
            st.add(tx_z, 9, 90);
            st.add(tx_keep, 1, 10);
        }

        let utxo_dyn: Arc<dyn UtxoStore> = utxo.clone();
        reconcile_reorg(
            chain.as_ref(),
            &blob,
            utxo_dyn.as_ref(),
            &state,
            c,             // current_tip
            12,            // current_height
            z,             // new_tip
            13,            // new_height
            0x207f_ffff,   // n_bits
            1_700_000_100, // block_time
        )
        .await
        .expect("reorg reconcile");

        // Assembly survivors: orphaned txs returned, adopted txs dropped, the
        // independent survivor kept.
        let nodes: Vec<Hash> = {
            let st = state.lock().unwrap();
            st.processor
                .all_nodes()
                .into_iter()
                .map(|n| n.hash)
                .collect()
        };
        let present: HashSet<Hash> = nodes.iter().copied().collect();
        assert!(present.contains(&tx_b), "orphaned tx_b returned");
        assert!(present.contains(&tx_c), "orphaned tx_c returned");
        assert!(present.contains(&tx_keep), "independent survivor kept");
        assert!(!present.contains(&tx_x), "adopted tx_x dropped");
        assert!(!present.contains(&tx_y), "adopted tx_y dropped");
        assert!(!present.contains(&tx_z), "adopted tx_z dropped");

        // Tip updated to the new chain tip.
        {
            let st = state.lock().unwrap();
            assert_eq!(st.chain.best_hash, z, "tip adopted");
            assert_eq!(st.chain.height, 13, "height adopted");
        }

        // Marks: winners (moveForward txs, on=true) and returned losers (off).
        let marks = utxo.mark_calls();
        let winners: HashSet<Hash> = marks
            .iter()
            .filter(|(_, on)| *on)
            .flat_map(|(hs, _)| hs.iter().copied())
            .collect();
        let losers: HashSet<Hash> = marks
            .iter()
            .filter(|(_, on)| !*on)
            .flat_map(|(hs, _)| hs.iter().copied())
            .collect();
        assert!(winners.contains(&tx_x) && winners.contains(&tx_y) && winners.contains(&tx_z));
        assert!(losers.contains(&tx_b) && losers.contains(&tx_c));
        // No fork-tx ends up on both mark lists.
        assert!(winners.is_disjoint(&losers), "winners and losers disjoint");

        // moveForward coinbase UTXOs created for each adopted block (x, y, z),
        // with the seeded block ids; orphaned b/c get none.
        let created: HashSet<Hash> = utxo
            .create_calls()
            .iter()
            .map(|(txid, _, _, _)| *txid)
            .collect();
        assert!(created.contains(&cb_x_txid), "coinbase X created");
        assert!(created.contains(&cb_y_txid), "coinbase Y created");
        assert!(created.contains(&cb_z_txid), "coinbase Z created");
        assert_eq!(created.len(), 3, "exactly the 3 adopted-block coinbases");
        let id_for = |txid: &Hash| {
            utxo.create_calls()
                .into_iter()
                .find(|(t, _, _, _)| t == txid)
                .and_then(|(_, _, m, _)| m)
                .map(|m| (m.block_id, m.block_height))
        };
        assert_eq!(id_for(&cb_x_txid), Some((211, 11)));
        assert_eq!(id_for(&cb_z_txid), Some((213, 13)));
    }

    use crate::store::SpendingData;
    use ba_subtree_bench::tx::Tx;

    /// The extend-path helper parses a real coinbase and records a `create` with
    /// the block's mined info (block_id, height, subtree 0) and locked=false.
    #[tokio::test]
    async fn create_block_coinbase_utxo_records_create() {
        let utxo = MemUtxoStore::default();
        let cb = crate::coinbase::create_coinbase(
            12,
            50_0000_0000,
            b"t",
            &[crate::config::DEFAULT_COINBASE_ADDRESS.to_string()],
            [0u8; 12],
        )
        .unwrap();
        let txid = Tx::from_bytes(&cb).unwrap().txid();

        create_block_coinbase_utxo(&utxo, &cb, 12, 7, &[0xab; 32]).await;

        let calls = utxo.create_calls();
        assert_eq!(calls.len(), 1, "one coinbase create");
        let (got_txid, h, mined, locked) = &calls[0];
        assert_eq!(*got_txid, txid);
        assert_eq!(*h, 12);
        assert!(!*locked, "coinbase create passes locked=false");
        let m = mined.as_ref().expect("mined info present");
        assert_eq!(m.block_id, 7);
        assert_eq!(m.block_height, 12);
        assert_eq!(m.subtree_idx, 0);
        assert!(m.on_longest_chain);
        assert!(!m.unset_mined);
    }

    /// An empty coinbase (none returned by the service) is a no-op, not a panic.
    #[tokio::test]
    async fn create_block_coinbase_utxo_skips_empty() {
        let utxo = MemUtxoStore::default();
        create_block_coinbase_utxo(&utxo, &[], 12, 7, &[0u8; 32]).await;
        assert!(utxo.create_calls().is_empty(), "no create for empty coinbase");
    }

    /// resolve_block_id prefers the GetBlock response id when non-zero, and falls
    /// back to height when it is zero and the chain returns no header ids (Mem).
    #[tokio::test]
    async fn resolve_block_id_prefers_response_then_height() {
        let mem = MemBlockchainClient::new(ChainTip {
            hash: [0u8; 32],
            height: 12,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        });
        assert_eq!(resolve_block_id(&mem, &[1u8; 32], 99, 12).await, 99);
        assert_eq!(resolve_block_id(&mem, &[1u8; 32], 0, 42).await, 42);
    }

    /// Minimal EXTENDED tx: one input spending `(prev, vout)`, a 1-byte output
    /// `out_script` (distinguishes two bodies spending the same UTXO so they get
    /// distinct canonical txids — a real double-spend).
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
        b.push(out_script); // locking script
        b.extend_from_slice(&0u32.to_le_bytes()); // locktime
        b
    }

    /// db4 Task 2: a reorg whose moveBack block carries `conflicting_nodes = [D]`
    /// runs the REVERSE cascade and evicts D from the assembly.
    ///
    /// Fork graph (ancestor A at height 10):
    ///   current: A -> B(11)            (orphaned; B's subtree has conflicting_nodes=[D])
    ///   new:     A -> X(11) -> Y(12)   (adopted)
    ///
    /// UTXO seeding mirrors the db3 reverse-happy-path: parent P[0] currently
    /// spent by D (the demoted winner), counter C in P.conflicting_children,
    /// C conflicting=true and spends (P,0). After `reconcile_reorg`:
    ///   - reverse ran: D conflicting==true, P[0] restored to the counter C;
    ///   - D is EVICTED from the assembly (NOT re-added even though moveBack
    ///     would otherwise return B's txs);
    ///   - tip adopted to Y(12).
    #[tokio::test]
    async fn reorg_with_conflicts_runs_cascade() {
        // Block hashes.
        let a = [10u8; 32];
        let b = [11u8; 32];
        let x = [21u8; 32];
        let y = [22u8; 32];

        // Parent P; D and C both spend (P,0) with distinct bodies → distinct txids.
        let parent: Hash = [0x50; 32];
        let d_body = ext_tx_out(parent, 0, 0x51);
        let c_body = ext_tx_out(parent, 0, 0x52);
        let d = Tx::from_bytes(&d_body).unwrap().txid();
        let c = Tx::from_bytes(&c_body).unwrap().txid();
        assert_ne!(d, c);

        // Adopted (moveForward) tx — no conflicts on the new chain.
        let tx_x = leaf(200);
        let tx_y = leaf(201);

        // Mem blob: B's subtree holds D as a node AND lists D in conflicting_nodes.
        let blob = MemBlobStore::default();
        let (root_b, mut st_b) = subtree_for(d, 5, 50);
        st_b.conflicting_nodes = vec![d];
        blob.insert(root_b, st_b);
        let (root_x, st_x) = subtree_for(tx_x, 7, 70);
        let (root_y, st_y) = subtree_for(tx_y, 8, 80);
        blob.insert(root_x, st_x);
        blob.insert(root_y, st_y);

        // Mem chain: headers + per-block subtree roots (B at height 11).
        let mut mem = MemBlockchainClient::new(ChainTip {
            hash: b,
            height: 11,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        });
        mem.set_header(a, [9u8; 32], 10);
        mem.set_header(b, a, 11);
        mem.set_header(x, a, 11);
        mem.set_header(y, x, 12);
        mem.set_block_subtrees(b, 11, vec![root_b]);
        mem.set_block_subtrees(x, 11, vec![root_x]);
        mem.set_block_subtrees(y, 12, vec![root_y]);
        let chain: Arc<dyn BlockchainClient> = Arc::new(mem);

        // UTXO: db3 reverse-happy-path seeding.
        let utxo = Arc::new(MemUtxoStore::default());
        // P[0] currently spent by D, D not conflicting (db2 end-state).
        utxo.seed_spending_datas(parent, vec![Some(SpendingData { tx_id: d, vin: 0 })]);
        utxo.seed_tx(d, d_body, false, vec![], 0);
        // Counter C: in P.conflicting_children, conflicting=true, spends (P,0).
        utxo.seed_conflicting_children(parent, vec![c]);
        utxo.seed_tx(c, c_body, true, vec![], 100);

        // Seed the assembly: tip at B(11); D unmined (moveBack would re-add it),
        // plus the adopted txs and an independent survivor.
        let tx_keep = leaf(300);
        let state = Arc::new(Mutex::new(AssemblyState::new(64)));
        {
            let mut st = state.lock().unwrap();
            st.chain.apply_block(b, 11, 0x207f_ffff, 1_700_000_000);
            st.add(tx_x, 7, 70);
            st.add(tx_y, 8, 80);
            st.add(tx_keep, 1, 10);
        }

        let utxo_dyn: Arc<dyn UtxoStore> = utxo.clone();
        reconcile_reorg(
            chain.as_ref(),
            &blob,
            utxo_dyn.as_ref(),
            &state,
            b,             // current_tip
            11,            // current_height
            y,             // new_tip
            12,            // new_height
            0x207f_ffff,   // n_bits
            1_700_000_100, // block_time
        )
        .await
        .expect("conflicting reorg reconcile");

        // Reverse cascade ran: D re-marked Conflicting=true.
        assert!(
            utxo.get_tx_meta(&d).await.unwrap().unwrap().conflicting,
            "reverse cascade must re-mark D Conflicting=true"
        );
        // Counter C restored as the parent's spender (counter restored).
        let p = utxo.get_spending_datas(&parent).await.unwrap();
        assert_eq!(
            p[0],
            Some(SpendingData { tx_id: c, vin: 0 }),
            "reverse cascade must restore parent P[0] to the counter C"
        );

        // D EVICTED from the assembly (not re-added by moveBack), survivors kept,
        // adopted txs dropped.
        let present: HashSet<Hash> = {
            let st = state.lock().unwrap();
            st.processor
                .all_nodes()
                .into_iter()
                .map(|n| n.hash)
                .collect()
        };
        assert!(!present.contains(&d), "conflicting D evicted from assembly");
        assert!(present.contains(&tx_keep), "independent survivor kept");
        assert!(!present.contains(&tx_x), "adopted tx_x dropped");
        assert!(!present.contains(&tx_y), "adopted tx_y dropped");

        // A subsequent add of the evicted D is a no-op (registered conflicting).
        {
            let mut st = state.lock().unwrap();
            st.add(d, 5, 50);
            assert!(
                !st.processor.all_nodes().into_iter().any(|n| n.hash == d),
                "re-adding evicted D must be a no-op"
            );
        }

        // Tip adopted to the new chain tip.
        {
            let st = state.lock().unwrap();
            assert_eq!(st.chain.best_hash, y, "tip adopted");
            assert_eq!(st.chain.height, 12, "height adopted");
        }

        // Marks: D (evicted/conflicting) is NOT in the winners set.
        let marks = utxo.mark_calls();
        let winners: HashSet<Hash> = marks
            .iter()
            .filter(|(_, on)| *on)
            .flat_map(|(hs, _)| hs.iter().copied())
            .collect();
        assert!(!winners.contains(&d), "evicted D must not be marked winner");
    }
}

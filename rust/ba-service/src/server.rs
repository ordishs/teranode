//! BlockAssemblyAPI implementation.
//! Stage 1: skeleton (Health). Stage 2: AddTx / GetMiningCandidate /
//! GetBlockAssemblyState wired to the native subtree engine, in-memory state.
//! Remaining RPCs return `Unimplemented` until Stage 3+ (real stores / reorg).

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use ba_subtree_bench::hash::{sha256d, Hash};
use ba_subtree_bench::subtree::Subtree;
use tonic::{Request, Response, Status};

use crate::assembly::AssemblyState;
use crate::block::{build_header, header_hash, meets_target};
use crate::blockassembly_api::block_assembly_api_server::BlockAssemblyApi;
use crate::blockassembly_api::{
    AddTxBatchColumnarRequest, AddTxBatchRequest, AddTxBatchResponse, AddTxRequest, AddTxResponse,
    EmptyMessage, GenerateBlocksRequest, GetBlockAssemblyBlockCandidateResponse,
    GetBlockAssemblyTxsResponse, GetCandidateBlockRequest, GetCandidateBlockResponse,
    GetCurrentDifficultyResponse, GetMiningCandidateRequest, HealthResponse, OkResponse,
    RemoveTxRequest, StateMessage, SubmitMiningSolutionRequest,
};
use crate::jobstore::{Job, JobStore};
use crate::model::MiningCandidate;
use crate::store::chain_grpc::{create_block_coinbase_utxo, resolve_block_id};
use crate::store::chain_mem::MemBlockchainClient;
use crate::store::{BlockchainClient, ChainTip, UtxoStore};

/// Default block-subsidy halving interval (mainnet). Regtest uses 150; the active
/// value is threaded into `BaService` from settings (Capability F) and only this
/// default is used when no config-derived interval is supplied (test constructors).
const DEFAULT_SUBSIDY_HALVING_INTERVAL: u32 = 210_000;

/// Documented default coinbase payout address used when none is configured —
/// matches `config::DEFAULT_COINBASE_ADDRESS`. Capability A scope only.
const DEFAULT_COINBASE_ADDRESS: &str = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2";

/// Fixed extranonce for the service-built coinbase path. The published coinbase is
/// only required to be a valid, height-committing coinbase that satisfies the
/// header PoW; the extranonce search space is not exercised here (Capability A),
/// so a constant (zeros) is used and documented.
const FIXED_EXTRANONCE: [u8; 12] = [0u8; 12];

pub struct BaService {
    state: Arc<Mutex<AssemblyState>>,
    ready: Arc<AtomicBool>,
    jobs: JobStore,
    /// Blockchain service handle used to publish built blocks (AddBlock +
    /// SetBlockSubtreesSet). In unit tests this is a `MemBlockchainClient` that
    /// records calls; in `main.rs` it is the real gRPC client.
    chain: Arc<dyn BlockchainClient>,
    /// UTXO store handle for writing the COINBASE UTXO of a self-mined block at
    /// submit time (Go SubtreeProcessor.processCoinbaseUtxos). REQUIRED here in
    /// addition to the notification path: `submit_mining_solution` advances the
    /// local tip optimistically, so by the time the block's Block notification
    /// reaches `handle_block` the tip already equals `best_tip()` and that path
    /// no-ops — without creating the coinbase here, self-mined blocks (the
    /// GenerateBlocks loop) would never get their coinbase UTXO. `create` is
    /// CREATE_ONLY/idempotent so the notification path (peer blocks) stays
    /// correct. `None` in unit tests that don't assert coinbase creation; set via
    /// `set_utxo_store` in `main.rs`.
    utxo: Option<Arc<dyn UtxoStore>>,
    /// Coinbase payout address(es) for the service-built coinbase path.
    coinbase_addresses: Vec<String>,
    /// Completed-subtree sink. Each subtree drained from the assembly on the
    /// ingest path is sent here for the writer task to persist. `None` in the
    /// standalone/unit-test constructor (no persistence); `main.rs` sets it via
    /// `set_subtree_sink` after spawning `run_subtree_writer`.
    subtree_tx: Option<tokio::sync::mpsc::UnboundedSender<Subtree>>,
    /// Blob store used to persist the job's subtrees SYNCHRONOUSLY at submit time
    /// (the writer-lag race fix). The async writer (`subtree_tx`) persists subtrees
    /// in the background as they complete; that flush may not have landed by the
    /// time `submit_mining_solution` calls `add_block`, after which the blockchain
    /// service re-reads those subtree blobs in `Block.Valid`. Persisting here,
    /// synchronously and BEFORE `add_block`, closes the race. The double-write
    /// (async writer + sync submit) is deliberate and idempotent: both write the
    /// identical placeholder-version blob under the identical root key.
    /// `None` in unit tests that don't persist; set via `set_blob_store` in
    /// `main.rs` (and injected with a recording store in the submit-persist test).
    blob: Option<Arc<dyn crate::store::BlobStore>>,
    /// Block-subsidy halving interval for the active network (Capability F).
    /// `main.rs` sets this from `Stage3Config::subsidy_halving_interval()`; the
    /// test constructors default to `DEFAULT_SUBSIDY_HALVING_INTERVAL` (210000).
    subsidy_interval: u32,
    /// Test clock injection: when set, `unix_now()` returns this instead of the
    /// system clock, keeping candidate time (and the id derived from it)
    /// deterministic in tests. `None` (production) = real wall clock.
    now_override: Option<u32>,
}

impl BaService {
    /// Standalone / unit-test constructor: an in-memory blockchain client seeded
    /// from genesis and the default coinbase address.
    pub fn new(cap: usize) -> Self {
        let tip = ChainTip {
            hash: [0u8; 32],
            height: 0,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        };
        Self::with_chain(
            cap,
            Arc::new(MemBlockchainClient::new(tip)),
            vec![DEFAULT_COINBASE_ADDRESS.to_string()],
        )
    }

    /// Full constructor wiring an explicit blockchain client + coinbase addresses
    /// (used by `main.rs` with the real gRPC client, and by tests with a Mem one).
    pub fn with_chain(
        cap: usize,
        chain: Arc<dyn BlockchainClient>,
        coinbase_addresses: Vec<String>,
    ) -> Self {
        let addresses = if coinbase_addresses.is_empty() {
            vec![DEFAULT_COINBASE_ADDRESS.to_string()]
        } else {
            coinbase_addresses
        };
        Self {
            state: Arc::new(Mutex::new(AssemblyState::new(cap))),
            ready: Arc::new(AtomicBool::new(false)),
            jobs: JobStore::new(),
            chain,
            utxo: None,
            coinbase_addresses: addresses,
            subtree_tx: None,
            blob: None,
            subsidy_interval: DEFAULT_SUBSIDY_HALVING_INTERVAL,
            now_override: None,
        }
    }

    /// Pin the wall clock (tests only): `unix_now()` returns `t` instead of the
    /// system time, making the candidate's time/id deterministic.
    pub fn set_now(&mut self, t: u32) {
        self.now_override = Some(t);
    }

    /// Current Unix time as u32 (Go `time.Now().Unix()` + Int64ToUint32), or the
    /// injected test value when set.
    fn unix_now(&self) -> u32 {
        if let Some(t) = self.now_override {
            return t;
        }
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as u32)
            .unwrap_or(0)
    }

    /// Install the UTXO store used to write the coinbase UTXO of self-mined blocks
    /// at submit time. `main.rs` sets it to the same `AeroUtxoStore` the
    /// subscription/boot paths use. Left `None` in unit tests that don't assert it.
    pub fn set_utxo_store(&mut self, utxo: Arc<dyn UtxoStore>) {
        self.utxo = Some(utxo);
    }

    /// Set the block-subsidy halving interval (Capability F). `main.rs` calls this
    /// with `Stage3Config::subsidy_halving_interval()`; tests use it to exercise
    /// the regtest (150) interval.
    pub fn set_subsidy_interval(&mut self, interval: u32) {
        self.subsidy_interval = interval;
    }

    /// Install the completed-subtree sink. Called from `main.rs` after spawning
    /// `run_subtree_writer` so each subtree drained on the ingest path is sent to
    /// the writer for persistence. Left `None` in unit tests that don't persist.
    pub fn set_subtree_sink(&mut self, tx: tokio::sync::mpsc::UnboundedSender<Subtree>) {
        self.subtree_tx = Some(tx);
    }

    /// Install the blob store used for the synchronous submit-time subtree persist
    /// (writer-lag race fix). Called from `main.rs` with the same `FsBlobStore` the
    /// async writer uses, so the two write byte-identical blobs under the same keys.
    pub fn set_blob_store(&mut self, blob: Arc<dyn crate::store::BlobStore>) {
        self.blob = Some(blob);
    }

    /// Synchronously persist a job's subtrees BEFORE the block is published, so the
    /// blockchain service's `Block.Valid` can re-read every referenced subtree blob
    /// even if the async writer hasn't flushed it yet (the writer-lag race).
    ///
    /// CONSENSUS-CRITICAL: the persisted blob MUST be the PLACEHOLDER version —
    /// node 0 of the first subtree is `COINBASE_PLACEHOLDER` (`[0xFF; 32]`), NOT the
    /// coinbase-substituted version. Go persists subtrees at COMPLETION (placeholder
    /// intact) and `Block.Valid` performs its own coinbase substitution when
    /// checking. `build_submission` clones `job.subtrees` and runs the merkle
    /// computation which mutates node 0 to the coinbase on THAT clone; `job.subtrees`
    /// itself is never mutated. We therefore clone `job.subtrees` here into a SEPARATE
    /// fresh copy (placeholder intact) and serialize from that, keyed by the
    /// pre-replacement root (the exact hashes published as the block's subtree_hashes).
    ///
    /// Idempotent by design: the async writer may already have written the identical
    /// bytes under the identical key; a re-write of identical bytes is harmless.
    /// Best-effort per subtree: a write error is logged, not fatal (the block is
    /// still published; a missing blob surfaces downstream in validation).
    async fn persist_job_subtrees_sync(&self, job: &Job) {
        let Some(blob) = &self.blob else {
            return;
        };
        for st in &job.subtrees {
            // Fresh clone: placeholder at node 0 stays intact (no coinbase swap).
            let mut clone = st.clone();
            let root = match clone.root_hash() {
                Some(r) => r,
                None => continue, // empty subtree → nothing to persist
            };
            let body = clone.serialize();
            let mut bytes =
                Vec::with_capacity(crate::subtree_store::SUBTREE_MAGIC.len() + body.len());
            bytes.extend_from_slice(&crate::subtree_store::SUBTREE_MAGIC);
            bytes.extend_from_slice(&body);
            if let Err(e) = blob.set(&root, &bytes).await {
                eprintln!(
                    "ba-service: submit-time subtree persist failed for {} (best-effort): {e}",
                    hex::encode(root)
                );
            }
        }
    }

    /// Send any subtrees completed since the last drain to the writer task. The
    /// caller drains under the assembly lock (cheap clone), drops the lock, then
    /// calls this. A send failure (writer gone) is logged, not fatal.
    fn enqueue_subtrees(&self, subtrees: Vec<Subtree>) {
        if let Some(tx) = &self.subtree_tx {
            for st in subtrees {
                if let Err(e) = tx.send(st) {
                    eprintln!("ba-service: subtree writer channel closed: {e}");
                }
            }
        }
    }

    /// Shared handle to the assembly state — handed to the blockchain
    /// subscription task so notifications drive the same chain state the
    /// candidate handlers read.
    pub fn shared_state(&self) -> Arc<Mutex<AssemblyState>> {
        self.state.clone()
    }

    /// Returns a cloned handle to the readiness flag, for the boot task to flip.
    pub fn ready_handle(&self) -> Arc<AtomicBool> {
        self.ready.clone()
    }

    /// Mark the service ready — called once the unmined-tx load completes.
    /// (Used by the readiness unit test; the boot task flips the handle directly.)
    #[allow(dead_code)]
    pub fn set_ready(&self) {
        self.ready.store(true, Ordering::SeqCst);
    }

    #[allow(clippy::result_large_err)]
    fn check_ready(&self) -> Result<(), Status> {
        if self.ready.load(Ordering::SeqCst) {
            Ok(())
        } else {
            Err(Status::unavailable(
                "service not ready - unmined transactions are still being loaded",
            ))
        }
    }

    /// Synchronously build (but do not publish) a block from a SubmitMiningSolution
    /// request: job lookup, stale-parent check, coinbase resolution, block merkle
    /// root via the engine, header construction, and the PoW check. Returns the
    /// fully-built pieces ready for `add_block`. Holds the state lock only briefly
    /// (to read the tip + clone the job's subtrees) and releases it before return,
    /// so the caller can `await` the publish without holding the std Mutex.
    ///
    /// Mirrors Go `submitMiningSolution` (Server.go:1322-1527).
    #[allow(clippy::result_large_err)]
    fn build_submission(&self, req: &SubmitMiningSolutionRequest) -> Result<BuiltBlock, Status> {
        // 1. Look up the cached job for this candidate id.
        let job = self
            .jobs
            .get(&req.id)
            .ok_or_else(|| Status::not_found("mining job not found"))?;

        // 2. Stale check: the job must build on the CURRENT tip. If the chain has
        //    advanced past the job's parent, reject (Go: candidate is stale).
        {
            let st = self.state.lock().unwrap();
            if job.previous_hash != st.chain.best_hash {
                return Err(Status::failed_precondition(
                    "stale candidate: chain has already advanced past its parent",
                ));
            }
        }

        // 3. Resolve the coinbase tx: use the client-supplied one (light-validated)
        //    or build one paying the configured address(es).
        let coinbase_tx: Vec<u8> = if !req.coinbase_tx.is_empty() {
            validate_coinbase_has_input(&req.coinbase_tx)?;
            req.coinbase_tx.clone()
        } else {
            let mut tx = crate::coinbase::create_coinbase(
                job.height,
                job.coinbase_value,
                b"",
                &self.coinbase_addresses,
                FIXED_EXTRANONCE,
            )
            .map_err(|e| Status::internal(format!("build coinbase: {e}")))?;
            apply_solution_fields(&mut tx, req.version, req.time, req.nonce)?;
            tx
        };

        // 4. coinbase txid = double-SHA256 of the coinbase tx bytes.
        let coinbase_txid = sha256d(&coinbase_tx);

        // 5. Block merkle root via the ENGINE — guarded so the gRPC handler never
        //    panics on the CVE-2012-2459 duplicate-root assert or an empty subtree.
        let mut subtrees = job.subtrees.clone();
        let merkle_root =
            ba_subtree_bench::block_merkle::try_block_merkle_root(&mut subtrees, &coinbase_txid)
                .map_err(|e| {
                    if e.contains("duplicate") {
                        Status::internal("duplicate subtree root")
                    } else {
                        Status::internal(format!("block merkle root: {e}"))
                    }
                })?;

        // version / time apply the request overrides over the job's candidate
        // values (Go: version/nTime overridden when present).
        let version = req.version.unwrap_or(job.version);
        let time = req.time.unwrap_or(job.time);

        // 6. Build the 80-byte header and hash it.
        let header = build_header(
            version,
            &job.previous_hash,
            &merkle_root,
            time,
            job.n_bits,
            req.nonce,
        );
        let block_hash = header_hash(&header);

        // 7. PoW: the header hash must meet the candidate's nBits target.
        if !meets_target(&block_hash, job.n_bits) {
            return Err(Status::invalid_argument("proof of work not met"));
        }

        // 8. Coinbase BUMP (BRC-74), Go Server.go:1438-1443: computed from the
        //    SAME coinbase-replaced subtrees the merkle root was built from (the
        //    `try_block_merkle_root` call above already performed the coinbase
        //    substitution on these clones). Best-effort like Go — a failure logs
        //    and publishes without a BUMP; coinbase-only blocks carry none.
        let coinbase_bump: Vec<u8> = if subtrees.is_empty() {
            Vec::new()
        } else {
            match ba_subtree_bench::bump::coinbase_bump(&mut subtrees, &coinbase_txid, job.height) {
                Ok(b) => b,
                Err(e) => {
                    eprintln!("ba-service: coinbase BUMP failed (publishing without): {e}");
                    Vec::new()
                }
            }
        };

        // 9. Subtree hashes the block stores = the job's subtree roots BEFORE the
        //    coinbase replacement (Go sends jobSubtreeHashes). tx_count = sum of the
        //    subtree node counts; the coinbase replaces a placeholder, not adds.
        //    Coinbase-only block → tx_count = 1.
        let mut subtree_root_hashes: Vec<Hash> = Vec::with_capacity(job.subtrees.len());
        let mut tx_count: u64 = 0;
        let mut subtree_size: u64 = 0;
        for st in &job.subtrees {
            let mut clone = st.clone();
            let root = clone
                .root_hash()
                .ok_or_else(|| Status::internal("empty subtree in job"))?;
            subtree_root_hashes.push(root);
            tx_count += st.len() as u64;
            subtree_size += st.size_in_bytes;
        }
        if job.subtrees.is_empty() {
            tx_count = 1; // coinbase only
        }

        // size: subtree byte totals + 80-byte header + coinbase length. Approximate
        // (does not add the tx-count varint); documented as best-effort for A.
        let size_in_bytes = subtree_size + 80 + coinbase_tx.len() as u64;

        Ok(BuiltBlock {
            header: header.to_vec(),
            block_hash,
            coinbase_tx,
            subtree_root_hashes,
            tx_count,
            size_in_bytes,
            coinbase_bump,
            height: job.height,
            n_bits: job.n_bits,
            time,
        })
    }
}

/// A fully-built (but not yet published) block, produced by `build_submission`.
struct BuiltBlock {
    header: Vec<u8>,
    block_hash: Hash,
    coinbase_tx: Vec<u8>,
    /// Subtree roots BEFORE coinbase replacement (what the block stores).
    subtree_root_hashes: Vec<Hash>,
    tx_count: u64,
    size_in_bytes: u64,
    coinbase_bump: Vec<u8>,
    height: u32,
    n_bits: u32,
    time: u32,
}

/// Read a Bitcoin varint at `pos`, returning (value, bytes_consumed).
fn read_varint(buf: &[u8], pos: usize) -> Option<(u64, usize)> {
    let first = *buf.get(pos)?;
    match first {
        0xff => {
            let b: [u8; 8] = buf.get(pos + 1..pos + 9)?.try_into().ok()?;
            Some((u64::from_le_bytes(b), 9))
        }
        0xfe => {
            let b: [u8; 4] = buf.get(pos + 1..pos + 5)?.try_into().ok()?;
            Some((u32::from_le_bytes(b) as u64, 5))
        }
        0xfd => {
            let b: [u8; 2] = buf.get(pos + 1..pos + 3)?.try_into().ok()?;
            Some((u16::from_le_bytes(b) as u64, 3))
        }
        n => Some((n as u64, 1)),
    }
}

/// BSV consensus limit on the coinbase scriptSig length (bytes).
const MAX_COINBASE_SCRIPT_SIG_SIZE: usize = 100;

/// Coinbase validation (Go parity): exactly one input whose scriptSig length
/// is in [2, MAX_COINBASE_SCRIPT_SIG_SIZE]. Parses the wire format directly
/// without a full `bt` dependency — version(4) | in_count varint | prevout(36)
/// | scriptSig_len varint | scriptSig | sequence(4) | ...
#[allow(clippy::result_large_err)]
fn validate_coinbase_has_input(tx: &[u8]) -> Result<(), Status> {
    // Minimum: version(4) + in_count(1) + prevout(36) + script_len(1) + script(≥2)
    // + sequence(4) = 48 bytes.
    if tx.len() < 48 {
        return Err(Status::invalid_argument("coinbase tx too short"));
    }

    let (input_count, in_count_len) = read_varint(tx, 4)
        .ok_or_else(|| Status::invalid_argument("coinbase tx: malformed input count"))?;

    if input_count != 1 {
        return Err(Status::invalid_argument(format!(
            "coinbase transaction must have exactly one input, got {input_count}"
        )));
    }

    // Skip: version(4) + in_count varint + prevout txid(32) + prevout index(4).
    let script_len_pos = 4 + in_count_len + 32 + 4;
    let (script_len, _) = read_varint(tx, script_len_pos)
        .ok_or_else(|| Status::invalid_argument("coinbase tx: malformed scriptSig length"))?;

    if script_len < 2 || script_len as usize > MAX_COINBASE_SCRIPT_SIG_SIZE {
        return Err(Status::invalid_argument(format!(
            "coinbase scriptSig length {script_len} out of range [2, {MAX_COINBASE_SCRIPT_SIG_SIZE}]"
        )));
    }

    Ok(())
}

/// Apply the SubmitMiningSolution overrides to a SERVICE-BUILT coinbase, matching
/// Go: `version` → tx version (bytes 0..4), `time` → locktime (last 4 bytes),
/// `nonce` → the (single) input's sequence number. Only valid for the canonical
/// coinbase `create_coinbase` produces (1 input, sequence is the 4 bytes before
/// the output count). Returns an error if the layout is unexpected.
#[allow(clippy::result_large_err)]
fn apply_solution_fields(
    tx: &mut [u8],
    version: Option<u32>,
    time: Option<u32>,
    nonce: u32,
) -> Result<(), Status> {
    if tx.len() < 4 {
        return Err(Status::internal("built coinbase too short"));
    }
    if let Some(v) = version {
        tx[0..4].copy_from_slice(&v.to_le_bytes());
    }
    if let Some(t) = time {
        let n = tx.len();
        tx[n - 4..n].copy_from_slice(&t.to_le_bytes());
    }
    if nonce != 0 {
        // Locate the single input's sequence: version(4) | in_count varint(1=0x01)
        // | prevout txid(32) | prevout index(4) | scriptSig_len varint | scriptSig
        // | sequence(4). Walk to the sequence and overwrite it.
        let (in_count, in_count_len) = read_varint(tx, 4)
            .ok_or_else(|| Status::internal("built coinbase: bad input count"))?;
        if in_count != 1 {
            return Err(Status::internal(
                "service-built coinbase must have exactly one input",
            ));
        }
        let mut pos = 4 + in_count_len + 32 + 4; // after prevout txid + index
        let (script_len, script_len_bytes) = read_varint(tx, pos)
            .ok_or_else(|| Status::internal("built coinbase: bad scriptSig len"))?;
        pos += script_len_bytes + script_len as usize;
        if pos + 4 > tx.len() {
            return Err(Status::internal("built coinbase: sequence out of range"));
        }
        tx[pos..pos + 4].copy_from_slice(&nonce.to_le_bytes());
    }
    Ok(())
}

#[allow(clippy::result_large_err)]
fn hash32(bytes: &[u8]) -> Result<Hash, Status> {
    bytes.try_into().map_err(|_| {
        Status::invalid_argument(format!("txid must be 32 bytes, got {}", bytes.len()))
    })
}

macro_rules! todo_rpc {
    ($name:literal) => {
        Err(Status::unimplemented(concat!(
            $name,
            " not implemented (Gate 2 stage 3+)"
        )))
    };
}

#[tonic::async_trait]
impl BlockAssemblyApi for BaService {
    async fn health_grpc(
        &self,
        _request: Request<EmptyMessage>,
    ) -> Result<Response<HealthResponse>, Status> {
        Ok(Response::new(HealthResponse {
            ok: true,
            details: "ba-service (rust) — Gate 2 stage 2".to_string(),
            timestamp: None,
        }))
    }

    async fn add_tx(&self, r: Request<AddTxRequest>) -> Result<Response<AddTxResponse>, Status> {
        self.check_ready()?;
        let req = r.into_inner();
        let hash = hash32(&req.txid)?;
        let drained = {
            let mut st = self.state.lock().unwrap();
            st.add(hash, req.fee, req.size);
            st.take_newly_chained()
        };
        self.enqueue_subtrees(drained);
        Ok(Response::new(AddTxResponse { ok: true }))
    }

    async fn remove_tx(
        &self,
        r: Request<RemoveTxRequest>,
    ) -> Result<Response<EmptyMessage>, Status> {
        self.check_ready()?;
        let hash = hash32(&r.into_inner().txid)?;
        self.state.lock().unwrap().remove(&hash);
        Ok(Response::new(EmptyMessage {}))
    }

    async fn add_tx_batch(
        &self,
        r: Request<AddTxBatchRequest>,
    ) -> Result<Response<AddTxBatchResponse>, Status> {
        self.check_ready()?;
        let req = r.into_inner();
        let drained = {
            let mut st = self.state.lock().unwrap();
            for tx in &req.tx_requests {
                let hash = hash32(&tx.txid)?;
                st.add(hash, tx.fee, tx.size);
            }
            st.take_newly_chained()
        };
        self.enqueue_subtrees(drained);
        Ok(Response::new(AddTxBatchResponse { ok: true }))
    }

    async fn add_tx_batch_columnar(
        &self,
        r: Request<AddTxBatchColumnarRequest>,
    ) -> Result<Response<AddTxBatchResponse>, Status> {
        self.check_ready()?;
        let req = r.into_inner();
        if !req.txids_packed.len().is_multiple_of(32) {
            return Err(Status::invalid_argument(
                "txids_packed length not a multiple of 32",
            ));
        }
        let n = req.txids_packed.len() / 32;
        if req.fees.len() != n || req.sizes.len() != n {
            return Err(Status::invalid_argument(format!(
                "fees ({}) / sizes ({}) must each equal tx count ({n})",
                req.fees.len(),
                req.sizes.len()
            )));
        }
        // Parent inpoints (parent_tx_hashes_packed / vout_idxs_packed) are not
        // needed to build subtrees; they feed conflict tracking, wired in Stage 4.
        let drained = {
            let mut st = self.state.lock().unwrap();
            for i in 0..n {
                let mut h = [0u8; 32];
                h.copy_from_slice(&req.txids_packed[i * 32..(i + 1) * 32]);
                st.add(h, req.fees[i], req.sizes[i]);
            }
            st.take_newly_chained()
        };
        self.enqueue_subtrees(drained);
        Ok(Response::new(AddTxBatchResponse { ok: true }))
    }

    async fn get_mining_candidate(
        &self,
        _r: Request<GetMiningCandidateRequest>,
    ) -> Result<Response<MiningCandidate>, Status> {
        self.check_ready()?;

        // FSM gate (Go Server.go:1225-1232): a candidate must only be produced
        // when the blockchain FSM is RUNNING — never while syncing/catching up.
        // Queried BEFORE the std-Mutex `state` lock below: the guard must not be
        // held across this `.await`.
        if !self
            .chain
            .is_fsm_current_state("RUNNING")
            .await
            .map_err(Status::from)?
        {
            return Err(Status::failed_precondition(
                "cannot get mining candidate when FSM is not in RUNNING state",
            ));
        }

        // Wall-clock "now" (Go BlockAssembler.go:1190-1196): `time.Now()` is BOTH
        // the candidate's Time and the currentBlockTime handed to the DAA below.
        let time_now = self.unix_now();

        // Read the parent hash under a BRIEF lock and release it before awaiting
        // the difficulty RPC — the std-Mutex guard must not cross `.await` (same
        // rule as the FSM gate above).
        let best_hash = {
            let st = self.state.lock().unwrap();
            st.chain.best_hash
        };

        // Next-block difficulty from the blockchain service (Go `getNextNbits` →
        // `GetNextWorkRequired`). The candidate carries THIS nBits (not the tip's),
        // and the job stores it so `submit`'s `meets_target` validates against the
        // difficulty handed to the miner. A failure fails the candidate (Go errors
        // too — a candidate cannot be built without a target).
        let next_n_bits = self
            .chain
            .get_next_work_required(&best_hash, time_now as i64)
            .await
            .map_err(Status::from)?;

        let st = self.state.lock().unwrap();
        // Read chain context first (copies), then take the subtree roots.
        let previous_hash = st.chain.best_hash.to_vec();
        let height = st.chain.height + 1;
        let version = st.chain.version;
        let n_bits_raw = next_n_bits;
        let n_bits = next_n_bits.to_le_bytes().to_vec();
        let time = time_now;
        let size_without_coinbase = st.total_size;
        let previous_best_hash = st.chain.best_hash;

        // Candidate subtree set — XOR selection, mirroring Go
        // `BlockAssembler.GetMiningCandidate` (`:1115-1134`):
        //   - completed (chained) subtrees exist → publish those (the leftover
        //     partial txs wait for the current subtree to fill);
        //   - else the current subtree holds real txs → publish a copy of it
        //     (the incomplete subtree, placeholder at node 0, carried fees), via
        //     `GetIncompleteSubtreeMiningData` / `createIncompleteSubtreeCopy`;
        //   - else nothing → empty candidate (`generateEmptyBlockCandidate`).
        let mut candidate_subtrees: Vec<Subtree> = if st.num_chained() > 0 {
            st.chained_subtrees_clone()
        } else if let Some(inc) = st.current_subtree_clone() {
            vec![inc]
        } else {
            vec![]
        };

        // CONSENSUS: coinbase value reconciles with the PUBLISHED subtrees —
        // subsidy + Σ(per-subtree fees) over exactly the candidate set, NOT the
        // running `total_fees` (which would overclaim when the incomplete subtree
        // holds fees that are not published). Mirrors Go `:1158`/`:1217`.
        let published_fees: u64 = candidate_subtrees.iter().map(|s| s.fees).sum();
        let coinbase_value =
            crate::coinbase::block_subsidy(height, self.subsidy_interval) + published_fees;

        // num_txs = Σ(node counts over published subtrees) − 1 (the single coinbase
        // placeholder at subtree-0 node-0), saturating to 0 for the empty case.
        let total_nodes: usize = candidate_subtrees.iter().map(|s| s.len()).sum();
        let num_txs = total_nodes.saturating_sub(1) as u32;

        // Published subtree roots = each candidate subtree's root (the exact hashes
        // that go on the block as subtree_hashes).
        let roots: Vec<Hash> = candidate_subtrees
            .iter_mut()
            .map(|s| s.root_hash().expect("non-empty candidate subtree"))
            .collect();
        let subtree_hashes: Vec<Vec<u8>> = roots.iter().map(|h| h.to_vec()).collect();

        // Deterministic id from prev-hash + published subtree roots + time. (Stage-3
        // parity tests use ≥cap completed sets → identical roots/derivation. The id
        // may now fold the incomplete root in the fallback case — that is fine.)
        let mut id_input = previous_hash.clone();
        for h in &roots {
            id_input.extend_from_slice(h);
        }
        id_input.extend_from_slice(&time.to_le_bytes());
        let id = sha256d(&id_input).to_vec();

        // Coinbase merkle proof over the candidate set's RAW roots (empty vec when
        // there are no subtrees — `coinbase_merkle_proof` returns `[]` for empty).
        let merkle_proof: Vec<Vec<u8>> =
            ba_subtree_bench::block_merkle::coinbase_merkle_proof(&mut candidate_subtrees)
                .iter()
                .map(|h| h.to_vec())
                .collect();

        self.jobs.insert(
            id.clone(),
            Job {
                previous_hash: previous_best_hash,
                subtrees: candidate_subtrees,
                coinbase_value,
                height,
                n_bits: n_bits_raw,
                version,
                time,
            },
        );

        Ok(Response::new(MiningCandidate {
            id,
            previous_hash,
            coinbase_value,
            version,
            n_bits,
            time,
            height,
            merkle_proof,
            subtree_count: subtree_hashes.len() as u32,
            num_txs,
            size_without_coinbase,
            subtree_hashes,
        }))
    }

    async fn get_current_difficulty(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<GetCurrentDifficultyResponse>, Status> {
        self.check_ready()?;
        let st = self.state.lock().unwrap();
        // NOTE: true Go parity computes difficulty from the NEXT block's required
        // nBits (GetNextWorkRequired over the retarget window) — out of scope here.
        // This uses the current tip's nBits, which is correct on regtest and on a
        // chain not at a retarget boundary.
        Ok(Response::new(GetCurrentDifficultyResponse {
            difficulty: difficulty_from_nbits(st.chain.n_bits),
            block_hash: st.chain.best_hash.to_vec(),
        }))
    }

    async fn submit_mining_solution(
        &self,
        r: Request<SubmitMiningSolutionRequest>,
    ) -> Result<Response<OkResponse>, Status> {
        self.check_ready()?;
        let req = r.into_inner();
        let built = self.build_submission(&req)?;

        // WRITER-LAG RACE FIX: persist the job's subtrees (PLACEHOLDER version)
        // synchronously BEFORE add_block. The async writer persists subtrees in the
        // background as they complete, but that flush may not have landed yet; once
        // add_block returns, the blockchain service re-reads every referenced subtree
        // blob in Block.Valid, which would fail on a not-yet-flushed blob. This
        // closes the gap. The double-write (async writer + here) is deliberate and
        // idempotent — see `persist_job_subtrees_sync`. No-op when no blob store is
        // wired (unit tests that don't persist).
        if let Some(job) = self.jobs.get(&req.id) {
            self.persist_job_subtrees_sync(&job).await;
        }

        // Publish the block. Done OUTSIDE the state lock (await across a std Mutex
        // guard is not allowed and would block the candidate handlers).
        self.chain
            .add_block(
                &built.header,
                &built.subtree_root_hashes,
                &built.coinbase_tx,
                built.tx_count,
                built.size_in_bytes,
                &built.coinbase_bump,
            )
            .await?;

        // Best-effort: mark the subtrees set. Log on error, do not fail the submit
        // (mirrors Go, which only logs the SetBlockSubtreesSet error).
        if let Err(e) = self.chain.set_block_subtrees_set(&built.block_hash).await {
            eprintln!(
                "ba-service: set_block_subtrees_set failed for {}: {e}",
                hex::encode(built.block_hash)
            );
        }

        // Create THIS block's coinbase UTXO (Go SubtreeProcessor.processCoinbaseUtxos).
        // Done HERE — not only on the notification path — because the optimistic
        // tip advance below makes `handle_block` no-op when the Block notification
        // arrives (local tip already == best_tip). The block now exists in the
        // blockchain store, so `block_header_ids` resolves its real ID. `create`
        // is CREATE_ONLY so a later notification-path create is a benign no-op.
        if let Some(utxo) = &self.utxo {
            let block_id = resolve_block_id(self.chain.as_ref(), &built.block_hash, 0, built.height).await;
            create_block_coinbase_utxo(
                utxo.as_ref(),
                &built.coinbase_tx,
                built.height,
                block_id,
                &built.block_hash,
            )
            .await;
        }

        // Advance the local tip + reset assembly, then clear all cached jobs
        // (Go: jobStore.DeleteAll after a successful submit).
        {
            let mut st = self.state.lock().unwrap();
            st.chain
                .apply_block(built.block_hash, built.height, built.n_bits, built.time);
            st.reset_assembly();
        }
        self.jobs.clear();

        Ok(Response::new(OkResponse { ok: true }))
    }

    async fn reset_block_assembly(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<EmptyMessage>, Status> {
        self.check_ready()?;
        self.state.lock().unwrap().reset_assembly();
        Ok(Response::new(EmptyMessage {}))
    }
    async fn reset_block_assembly_fully(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<EmptyMessage>, Status> {
        self.check_ready()?;
        self.state.lock().unwrap().reset_fully();
        Ok(Response::new(EmptyMessage {}))
    }
    async fn reset_block_assembly_validate_inputs(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<EmptyMessage>, Status> {
        todo_rpc!("ResetBlockAssemblyValidateInputs")
    }
    async fn check_block_assembly_validate_inputs(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<EmptyMessage>, Status> {
        todo_rpc!("CheckBlockAssemblyValidateInputs")
    }

    async fn get_block_assembly_state(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<StateMessage>, Status> {
        self.check_ready()?;
        let st = self.state.lock().unwrap();
        Ok(Response::new(StateMessage {
            block_assembly_state: "running".to_string(),
            subtree_processor_state: "running".to_string(),
            subtree_count: st.processor.num_chained() as u32,
            tx_count: st.num_txs(),
            ..Default::default()
        }))
    }

    async fn generate_blocks(
        &self,
        r: Request<GenerateBlocksRequest>,
    ) -> Result<Response<EmptyMessage>, Status> {
        self.check_ready()?;
        let req = r.into_inner();
        let count = req.count.max(0);

        // Per-call entropy so regenerating blocks never reproduces a prior run's
        // (deterministic) block hash. The height in the coinbase makes *different*
        // heights differ (BIP34); the extranonce gives uniqueness at the *same*
        // height — exactly what it exists for. Without it, FIXED_EXTRANONCE makes
        // empty blocks byte-identical across runs -> ux_blocks_hash collisions.
        let call_seed = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos() as u64)
            .unwrap_or(0);

        for i in 0..count {
            let mut extranonce = [0u8; 12];
            extranonce[0..8].copy_from_slice(&call_seed.to_le_bytes());
            extranonce[8..12].copy_from_slice(&(i as u32).to_le_bytes());
            // 1. Fresh candidate (inserts the job and gives us its id + nBits).
            let candidate = self
                .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
                .await?
                .into_inner();

            // Guard: the candidate must build on the blockchain's actual best tip.
            // Confirmed advancing cleanly (43->44->45, on_tip=true) on the live rig;
            // warn only if a candidate ever builds off-tip (the local optimistic tip
            // diverging from the chain — would mean blocks aren't being adopted).
            if let Ok(real) = self.chain.best_tip().await {
                if real.hash.as_slice() != candidate.previous_hash.as_slice() {
                    eprintln!(
                        "generate[{i}]: WARNING off-tip — blockchain best_tip h={} hash={} but candidate h={} builds on prev={}",
                        real.height,
                        hex::encode(real.hash),
                        candidate.height,
                        hex::encode(&candidate.previous_hash),
                    );
                }
            }

            // 2. Build the coinbase paying req.address (or the configured default),
            //    so we can nonce-search a header that meets the target. The same
            //    coinbase is then handed to the submit path.
            let job = self
                .jobs
                .get(&candidate.id)
                .ok_or_else(|| Status::internal("generate: job vanished after candidate"))?;
            let addresses: Vec<String> = match &req.address {
                Some(a) if !a.is_empty() => vec![a.clone()],
                _ => self.coinbase_addresses.clone(),
            };
            let coinbase_tx = crate::coinbase::create_coinbase(
                job.height,
                job.coinbase_value,
                b"",
                &addresses,
                extranonce,
            )
            .map_err(|e| Status::internal(format!("generate: build coinbase: {e}")))?;
            let coinbase_txid = sha256d(&coinbase_tx);

            // 3. Block merkle root for this coinbase (engine, guarded).
            let mut subtrees = job.subtrees.clone();
            let merkle_root = ba_subtree_bench::block_merkle::try_block_merkle_root(
                &mut subtrees,
                &coinbase_txid,
            )
            .map_err(|e| Status::internal(format!("generate: block merkle: {e}")))?;

            // 4. Nonce search: regtest 0x207fffff is met almost immediately. Bounded
            //    by u32::MAX; error out rather than loop forever.
            let mut found: Option<u32> = None;
            for nonce in 0..=u32::MAX {
                let header = build_header(
                    job.version,
                    &job.previous_hash,
                    &merkle_root,
                    job.time,
                    job.n_bits,
                    nonce,
                );
                if meets_target(&header_hash(&header), job.n_bits) {
                    found = Some(nonce);
                    break;
                }
            }
            let nonce = found.ok_or_else(|| {
                Status::internal("generate: no nonce met the target within u32 range")
            })?;

            // 5. Submit via the real publish path (build + PoW check + add_block +
            //    set_block_subtrees_set + tip advance + jobs.clear).
            self.submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
                id: candidate.id.clone(),
                nonce,
                coinbase_tx: coinbase_tx.clone(),
                time: None,
                version: None,
            }))
            .await?;
        }

        Ok(Response::new(EmptyMessage {}))
    }
    async fn check_block_assembly(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<OkResponse>, Status> {
        todo_rpc!("CheckBlockAssembly")
    }
    async fn get_block_assembly_block_candidate(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<GetBlockAssemblyBlockCandidateResponse>, Status> {
        todo_rpc!("GetBlockAssemblyBlockCandidate")
    }
    async fn get_block_assembly_txs(
        &self,
        _r: Request<EmptyMessage>,
    ) -> Result<Response<GetBlockAssemblyTxsResponse>, Status> {
        todo_rpc!("GetBlockAssemblyTxs")
    }
    async fn get_candidate_block(
        &self,
        r: Request<GetCandidateBlockRequest>,
    ) -> Result<Response<GetCandidateBlockResponse>, Status> {
        // Mirrors Go GetCandidateBlock (Server.go:1662): rebuild the full proposal
        // block for a cached candidate — default coinbase, merkle root over the
        // coinbase-replaced subtrees, 80-byte header with nonce=0 (PoW skipped in
        // proposal mode). Returns the ORIGINAL subtree roots for tx streaming.
        self.check_ready()?;
        let req = r.into_inner();

        let job = self
            .jobs
            .get(&req.id)
            .ok_or_else(|| Status::not_found("candidate not found"))?;

        // Default coinbase paying the configured address(es) (Go:
        // CreateCoinbaseTxCandidate). Deterministic extranonce is fine — this is a
        // proposal, not a submitted block.
        let coinbase_tx = crate::coinbase::create_coinbase(
            job.height,
            job.coinbase_value,
            b"",
            &self.coinbase_addresses,
            FIXED_EXTRANONCE,
        )
        .map_err(|e| Status::internal(format!("get_candidate_block: build coinbase: {e}")))?;
        let coinbase_txid = sha256d(&coinbase_tx);

        // Merkle root over the coinbase-replaced subtrees (clone; the engine
        // substitutes the coinbase placeholder in subtree[0]).
        let mut subtrees = job.subtrees.clone();
        let merkle_root =
            ba_subtree_bench::block_merkle::try_block_merkle_root(&mut subtrees, &coinbase_txid)
                .map_err(|e| Status::internal(format!("get_candidate_block: block merkle: {e}")))?;

        // ORIGINAL subtree roots (before coinbase replacement) — the asset service
        // streams the block's txs from the subtree store by these hashes.
        let subtree_hashes: Vec<Vec<u8>> = job
            .subtrees
            .clone()
            .iter_mut()
            .map(|s| s.root_hash().map(|h| h.to_vec()).unwrap_or_default())
            .collect();

        // transaction_count = Σ node counts over the subtrees (incl. coinbase
        // placeholder); 1 for a coinbase-only block.
        let transaction_count: u64 = if job.subtrees.is_empty() {
            1
        } else {
            job.subtrees.iter().map(|s| s.len() as u64).sum()
        };

        let header = build_header(
            job.version,
            &job.previous_hash,
            &merkle_root,
            job.time,
            job.n_bits,
            0, // proposal mode: nonce = 0
        );

        Ok(Response::new(GetCandidateBlockResponse {
            header: header.to_vec(),
            coinbase_tx,
            subtree_hashes,
            transaction_count,
        }))
    }
}

/// Decode a compact "bits"/nBits target into mining difficulty.
///
/// nBits compact format: the high byte is the base-256 exponent and the low 3
/// bytes are the mantissa (target = mantissa * 256^(exponent - 3)). Difficulty
/// is `difficulty_1_target / target`, where the difficulty-1 target corresponds
/// to nBits `0x1d00ffff` (mantissa 0x00ffff, exponent 0x1d).
///
/// Computed in f64 directly from the exponent/mantissa decomposition to avoid a
/// bignum dependency: difficulty = (0xffff / mantissa) * 256^(0x1d - exponent),
/// i.e. 256^((0x1d - exponent)) scaled by the mantissa ratio. This matches the
/// classic `difficulty_1_target / target` to full f64 precision for all valid
/// nBits.
fn difficulty_from_nbits(n_bits: u32) -> f64 {
    let exponent = (n_bits >> 24) & 0xff;
    let mantissa = n_bits & 0x00ff_ffff;

    if mantissa == 0 {
        return 0.0;
    }

    // difficulty = (0xffff * 256^(0x1d-3)) / (mantissa * 256^(exponent-3))
    //            = (0xffff / mantissa) * 256^(0x1d - exponent)
    // Use 8 * (29 - exponent) as a base-2 exponent for the 256^k factor.
    let ratio = f64::from(0xffffu32) / f64::from(mantissa);
    let shift = 8.0 * (29.0 - f64::from(exponent));
    ratio * 2.0f64.powf(shift)
}

#[cfg(test)]
mod coinbase_validation_tests {
    use super::{validate_coinbase_has_input, MAX_COINBASE_SCRIPT_SIG_SIZE};
    use tonic::Code;

    /// Build a minimal coinbase tx wire encoding:
    ///   version(4) | 0x01 (1 input) | prevout_txid(32) | prevout_index(4)
    ///   | script_len(1) | script(script_len bytes) | sequence(4)
    ///   | 0x00 (0 outputs) | locktime(4)
    fn make_coinbase(script_len: usize) -> Vec<u8> {
        let mut tx = Vec::new();
        // version = 1 (LE)
        tx.extend_from_slice(&1u32.to_le_bytes());
        // input count = 1
        tx.push(0x01);
        // null prevout txid (32 bytes of 0x00)
        tx.extend_from_slice(&[0u8; 32]);
        // prevout index = 0xffffffff (coinbase)
        tx.extend_from_slice(&0xffffffff_u32.to_le_bytes());
        // scriptSig length (single-byte varint for 0..=252)
        tx.push(script_len as u8);
        // scriptSig body
        tx.extend(std::iter::repeat_n(0xab_u8, script_len));
        // sequence
        tx.extend_from_slice(&0xffffffff_u32.to_le_bytes());
        // output count = 0
        tx.push(0x00);
        // locktime = 0
        tx.extend_from_slice(&0u32.to_le_bytes());
        tx
    }

    /// Build a coinbase with `input_count` inputs (each with a 4-byte scriptSig).
    fn make_coinbase_multi_input(input_count: usize) -> Vec<u8> {
        let mut tx = Vec::new();
        tx.extend_from_slice(&1u32.to_le_bytes()); // version
        tx.push(input_count as u8); // input count varint
        for _ in 0..input_count {
            tx.extend_from_slice(&[0u8; 32]); // prevout txid
            tx.extend_from_slice(&0xffffffff_u32.to_le_bytes()); // prevout index
            tx.push(4u8); // scriptSig len = 4 (valid range)
            tx.extend_from_slice(&[0xab; 4]); // scriptSig
            tx.extend_from_slice(&0xffffffff_u32.to_le_bytes()); // sequence
        }
        tx.push(0x00); // output count
        tx.extend_from_slice(&0u32.to_le_bytes()); // locktime
        tx
    }

    #[test]
    fn valid_script_len_min_accepted() {
        // scriptSig len = 2 (minimum allowed)
        assert!(validate_coinbase_has_input(&make_coinbase(2)).is_ok());
    }

    #[test]
    fn valid_script_len_max_accepted() {
        // scriptSig len = MAX_COINBASE_SCRIPT_SIG_SIZE (100) — should pass
        assert!(validate_coinbase_has_input(&make_coinbase(MAX_COINBASE_SCRIPT_SIG_SIZE)).is_ok());
    }

    #[test]
    fn valid_script_len_mid_accepted() {
        // A typical coinbase scriptSig in the middle of the range
        assert!(validate_coinbase_has_input(&make_coinbase(50)).is_ok());
    }

    #[test]
    fn zero_inputs_rejected() {
        // A tx with 0 inputs is malformed (too short to hold a valid coinbase input);
        // the validator must reject it with InvalidArgument regardless of which
        // short-circuit fires first.
        let tx = make_coinbase_multi_input(0);
        let err = validate_coinbase_has_input(&tx).unwrap_err();
        assert_eq!(err.code(), Code::InvalidArgument);
    }

    #[test]
    fn two_inputs_rejected() {
        let tx = make_coinbase_multi_input(2);
        let err = validate_coinbase_has_input(&tx).unwrap_err();
        assert_eq!(err.code(), Code::InvalidArgument);
        assert!(
            err.message().contains("exactly one input"),
            "unexpected msg: {}",
            err.message()
        );
    }

    #[test]
    fn script_len_one_rejected() {
        // scriptSig len = 1 (below minimum of 2)
        let err = validate_coinbase_has_input(&make_coinbase(1)).unwrap_err();
        assert_eq!(err.code(), Code::InvalidArgument);
        assert!(
            err.message().contains("out of range"),
            "unexpected msg: {}",
            err.message()
        );
    }

    #[test]
    fn script_len_zero_rejected() {
        let err = validate_coinbase_has_input(&make_coinbase(0)).unwrap_err();
        assert_eq!(err.code(), Code::InvalidArgument);
        assert!(
            err.message().contains("out of range"),
            "unexpected msg: {}",
            err.message()
        );
    }

    #[test]
    fn script_len_over_max_rejected() {
        // scriptSig len = MAX_COINBASE_SCRIPT_SIG_SIZE + 1 (101)
        let err = validate_coinbase_has_input(&make_coinbase(MAX_COINBASE_SCRIPT_SIG_SIZE + 1))
            .unwrap_err();
        assert_eq!(err.code(), Code::InvalidArgument);
        assert!(
            err.message().contains("out of range"),
            "unexpected msg: {}",
            err.message()
        );
    }

    #[test]
    fn too_short_rejected() {
        // A tx shorter than the 48-byte minimum
        let err = validate_coinbase_has_input(&[0u8; 10]).unwrap_err();
        assert_eq!(err.code(), Code::InvalidArgument);
    }
}

#[cfg(test)]
mod difficulty_tests {
    use super::difficulty_from_nbits;

    #[test]
    fn difficulty_one_target_is_one() {
        // 0x1d00ffff is the difficulty-1 target by definition.
        assert_eq!(difficulty_from_nbits(0x1d00_ffff), 1.0);
    }

    #[test]
    fn regtest_target_is_tiny() {
        // 0x207fffff (regtest powLimit). Expected value computed in Python:
        //   (0xffff * 256^26) / (0x7fffff * 256^29) = 4.6565423739069247e-10
        let got = difficulty_from_nbits(0x207f_ffff);
        let expected = 4.656_542_373_906_924_7e-10;
        assert!(
            (got - expected).abs() <= expected * 1e-12,
            "got {got:e}, expected {expected:e}"
        );
    }

    #[test]
    fn zero_mantissa_is_zero() {
        assert_eq!(difficulty_from_nbits(0x1d00_0000), 0.0);
    }

    #[test]
    fn higher_target_means_lower_difficulty() {
        // A larger mantissa at the same exponent => easier target => lower diff.
        let easy = difficulty_from_nbits(0x1d00_ffff);
        let easier = difficulty_from_nbits(0x1d01_ffff);
        assert!(easier < easy);
    }
}

#[cfg(test)]
mod ready_tests {
    use super::*;
    use tonic::Request;

    #[tokio::test]
    async fn rpcs_unavailable_until_ready() {
        let svc = BaService::new(4);

        let err = svc
            .get_block_assembly_state(Request::new(EmptyMessage {}))
            .await
            .unwrap_err();
        assert_eq!(err.code(), tonic::Code::Unavailable);

        svc.set_ready();
        let ok = svc
            .get_block_assembly_state(Request::new(EmptyMessage {}))
            .await;
        assert!(ok.is_ok());
    }
}

#[cfg(test)]
mod candidate_tests {
    use super::*;
    use ba_subtree_bench::block_merkle::block_merkle_root;
    use ba_subtree_bench::hash::sha256d;
    use tonic::Request;

    fn leaf(i: u32) -> Hash {
        sha256d(&i.to_le_bytes())
    }

    /// GetMiningCandidate must carry the real coinbase value (subsidy + fees) and
    /// a non-empty coinbase merkle proof for a multi-subtree set, and must cache a
    /// retrievable job under the candidate id.
    #[tokio::test]
    async fn candidate_has_real_coinbase_value_proof_and_job() {
        let svc = BaService::new(4);
        svc.set_ready();

        // 8 txs at cap 4 (placeholder at subtree-0 node 0) → 2 completed subtrees
        // publishing leaf0..6 (8 nodes), leaf7 stays leftover in the current
        // subtree. Coinbase reconciles with the PUBLISHED fees only: 0+1+..+6 = 21.
        let published_fees: u64 = (0u32..7).map(u64::from).sum();
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let height = svc.state.lock().unwrap().chain.height + 1;

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        let expected_value =
            crate::coinbase::block_subsidy(height, DEFAULT_SUBSIDY_HALVING_INTERVAL)
                + published_fees;
        assert_eq!(
            resp.coinbase_value, expected_value,
            "subsidy + published fees"
        );
        assert!(!resp.merkle_proof.is_empty(), "merkle proof non-empty");
        assert_eq!(resp.subtree_count, 2, "two completed subtrees");

        // The job is retrievable by id and holds the subtrees.
        let job = svc.jobs.get(&resp.id).expect("job cached by id");
        assert_eq!(job.coinbase_value, expected_value);
        assert_eq!(job.height, height);
        assert_eq!(job.subtrees.len(), 2);
    }

    #[tokio::test]
    async fn get_candidate_block_returns_full_proposal_block() {
        let svc = BaService::new(4);
        svc.set_ready();
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }

        let candidate = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        let block = svc
            .get_candidate_block(Request::new(GetCandidateBlockRequest {
                id: candidate.id.clone(),
            }))
            .await
            .expect("candidate block")
            .into_inner();

        // 80-byte header; proposal mode -> nonce (bytes 76..80) is zero.
        assert_eq!(block.header.len(), 80);
        assert_eq!(&block.header[76..80], &[0u8; 4], "nonce must be 0");
        // prev-hash field matches the candidate's previous hash.
        assert_eq!(&block.header[4..36], candidate.previous_hash.as_slice());
        // a coinbase is built.
        assert!(!block.coinbase_tx.is_empty());
        // original subtree roots == the candidate's (2 completed subtrees).
        assert_eq!(block.subtree_hashes.len(), 2);
        assert_eq!(block.subtree_hashes, candidate.subtree_hashes);
        // tx count = Σ node counts incl. the coinbase placeholder (2 × 4).
        assert_eq!(block.transaction_count, 8);

        // unknown candidate id -> NotFound.
        assert!(svc
            .get_candidate_block(Request::new(GetCandidateBlockRequest { id: vec![0u8; 32] }))
            .await
            .is_err());
    }

    /// Single-subtree case: the candidate's coinbase proof view equals the
    /// block-root view, so folding the coinbase txid through the proof must
    /// reconstruct `block_merkle_root(subtrees_with_coinbase, coinbase_txid)`.
    #[tokio::test]
    async fn single_subtree_proof_folds_to_block_root() {
        let svc = BaService::new(4);
        svc.set_ready();

        // Exactly one full subtree (cap 4, add 4).
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..4u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();
        assert_eq!(resp.subtree_count, 1);
        assert!(!resp.merkle_proof.is_empty());

        let coinbase_txid = sha256d(b"coinbase");

        // Fold the coinbase txid through the returned proof.
        let mut acc = coinbase_txid;
        for sib in &resp.merkle_proof {
            let mut buf = [0u8; 64];
            buf[..32].copy_from_slice(&acc);
            buf[32..].copy_from_slice(sib);
            acc = sha256d(&buf);
        }

        // Independently compute the block merkle root from the cached job's
        // subtrees (single-subtree → proof view == block-root view).
        let job = svc.jobs.get(&resp.id).expect("job");
        let mut subtrees = job.subtrees.clone();
        let want = block_merkle_root(&mut subtrees, &coinbase_txid);
        assert_eq!(acc, want, "folded proof must equal block merkle root");
    }

    /// CONSENSUS reconciliation — completed-only (the C1 scenario). cap 4 → adding
    /// 5 txs completes ONE subtree (placeholder + 3 real) and leaves 2 leftover txs
    /// in the current subtree. The candidate publishes ONLY the completed subtree;
    /// its coinbase counts ONLY that subtree's fees (NOT the leftover), and the
    /// leftover subtree is NOT among `subtree_hashes`.
    #[tokio::test]
    async fn coinbase_reconciles_completed_only_excludes_leftover_partial() {
        let svc = BaService::new(4);
        svc.set_ready();

        // fees: leaf0..leaf4 -> 10,20,30,40,50. cap 4 with placeholder at node 0
        // completes subtree 0 after 3 real txs (10+20+30=60). Remaining 40+50 stay
        // in the incomplete current subtree (NOT published).
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..5u32 {
                st.add(leaf(i), (i as u64 + 1) * 10, 1);
            }
            assert_eq!(st.num_chained(), 1, "exactly one completed subtree");
        }
        let height = svc.state.lock().unwrap().chain.height + 1;

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        let completed_fees: u64 = 10 + 20 + 30; // only subtree 0's real txs
        let expected = crate::coinbase::block_subsidy(height, DEFAULT_SUBSIDY_HALVING_INTERVAL)
            + completed_fees;
        assert_eq!(
            resp.coinbase_value, expected,
            "coinbase = subsidy + ONLY published (completed) fees, not leftover"
        );
        assert_eq!(
            resp.subtree_count, 1,
            "only the completed subtree published"
        );
        assert_eq!(resp.num_txs, 3, "3 real txs in the completed subtree");

        // The leftover partial subtree's root must NOT be among subtree_hashes.
        let inc_root = {
            let st = svc.state.lock().unwrap();
            let mut inc = st
                .current_subtree_clone()
                .expect("leftover partial present");
            inc.root_hash().expect("non-empty")
        };
        assert!(
            !resp.subtree_hashes.iter().any(|h| h.as_slice() == inc_root),
            "leftover partial subtree must not be published"
        );
    }

    /// CONSENSUS reconciliation — incomplete-only. cap 4, add 2 real txs: no
    /// completed subtree, so the candidate publishes the incomplete subtree. The
    /// coinbase = subsidy + those 2 fees; subtree_hashes has length 1; that subtree's
    /// node 0 is the coinbase placeholder.
    #[tokio::test]
    async fn coinbase_reconciles_incomplete_only() {
        use ba_subtree_bench::subtree::COINBASE_PLACEHOLDER;

        let svc = BaService::new(4);
        svc.set_ready();
        {
            let mut st = svc.state.lock().unwrap();
            st.add(leaf(1), 11, 1);
            st.add(leaf(2), 22, 1);
            assert_eq!(st.num_chained(), 0, "no completed subtree");
        }
        let height = svc.state.lock().unwrap().chain.height + 1;

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        let expected =
            crate::coinbase::block_subsidy(height, DEFAULT_SUBSIDY_HALVING_INTERVAL) + 11 + 22;
        assert_eq!(resp.coinbase_value, expected, "subsidy + incomplete fees");
        assert_eq!(resp.subtree_count, 1, "incomplete subtree published");
        assert_eq!(resp.num_txs, 2, "2 real txs (placeholder excluded)");

        // The published subtree (cached in the job) carries the placeholder at node 0.
        let job = svc.jobs.get(&resp.id).expect("job");
        assert_eq!(job.subtrees.len(), 1);
        assert_eq!(
            job.subtrees[0].nodes[0].hash, COINBASE_PLACEHOLDER,
            "incomplete subtree node 0 is the coinbase placeholder"
        );
    }

    /// Capability F: the candidate's nBits comes from `get_next_work_required`
    /// (the next-block target), NOT a copy of the tip's nBits, and the cached job
    /// stores THAT nBits (so submit validates against the difficulty handed out).
    #[tokio::test]
    async fn candidate_uses_next_work_required_nbits() {
        use crate::store::chain_mem::MemBlockchainClient;
        use crate::store::ChainTip;

        // Tip nBits is the regtest easy target; next-work is a DISTINCT value.
        let tip_nbits = 0x207f_ffff;
        let next_nbits = 0x1d00_ffff;
        let mem = std::sync::Arc::new(
            MemBlockchainClient::new(ChainTip {
                hash: [0u8; 32],
                height: 0,
                n_bits: tip_nbits,
                version: 0x2000_0000,
                median_time: 1_700_000_000,
            })
            .with_next_work(next_nbits),
        );
        let svc = BaService::with_chain(4, mem, vec![DEFAULT_COINBASE_ADDRESS.to_string()]);
        svc.set_ready();

        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        // Candidate n_bits field decodes (LE) to the next-work value, not the tip's.
        assert_eq!(resp.n_bits.len(), 4, "n_bits is 4 bytes");
        let decoded = u32::from_le_bytes([
            resp.n_bits[0],
            resp.n_bits[1],
            resp.n_bits[2],
            resp.n_bits[3],
        ]);
        assert_eq!(decoded, next_nbits, "candidate carries next-work nBits");
        assert_ne!(decoded, tip_nbits, "candidate nBits differs from the tip");

        // The cached job stores the same next-work nBits.
        let job = svc.jobs.get(&resp.id).expect("job cached");
        assert_eq!(job.n_bits, next_nbits, "job stores next-work nBits");
    }

    /// Go parity (BlockAssembler.go:1190-1196): the candidate's time is the WALL
    /// CLOCK — `time.Now()` is both the candidate `Time` and the `currentBlockTime`
    /// handed to GetNextWorkRequired — not `median_time + 1`. The injected clock
    /// (`set_now`) keeps the test deterministic.
    #[tokio::test]
    async fn candidate_time_is_wall_clock_and_feeds_daa() {
        use crate::store::chain_mem::MemBlockchainClient;
        use crate::store::ChainTip;

        let mem = std::sync::Arc::new(MemBlockchainClient::new(ChainTip {
            hash: [0u8; 32],
            height: 0,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        }));
        let mut svc =
            BaService::with_chain(4, mem.clone(), vec![DEFAULT_COINBASE_ADDRESS.to_string()]);
        svc.set_now(1_750_000_123);
        svc.set_ready();
        svc.state.lock().unwrap().chain.median_time = 1_700_000_000;

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        assert_eq!(
            resp.time, 1_750_000_123,
            "candidate time is the (injected) wall clock, not median_time+1"
        );

        let (_, daa_time) = mem.last_next_work_request().expect("DAA was called");
        assert_eq!(
            daa_time, 1_750_000_123,
            "GetNextWorkRequired receives the same wall-clock time"
        );
    }

    /// Without an injected clock the candidate time is the REAL system clock —
    /// strictly after 2026-01-01, far beyond the seeded median_time (1.7e9).
    #[tokio::test]
    async fn candidate_time_defaults_to_system_clock() {
        let svc = BaService::new(4);
        svc.set_ready();

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        assert!(
            resp.time >= 1_767_225_600,
            "candidate time {} should be the system clock (>= 2026-01-01), not MTP+1",
            resp.time
        );
    }

    /// Capability F: a `BaService` built with the regtest subsidy interval (150)
    /// yields a coinbase value computed against THAT interval. At height 150 the
    /// regtest subsidy has halved once; with the mainnet interval (210000) it has
    /// not — so the two diverge, proving the interval is config-driven.
    #[tokio::test]
    async fn coinbase_uses_configured_subsidy_interval() {
        use crate::store::chain_mem::MemBlockchainClient;
        use crate::store::ChainTip;

        // Tip height 149 → candidate height 150 (the first regtest halving).
        let mem = std::sync::Arc::new(MemBlockchainClient::new(ChainTip {
            hash: [0u8; 32],
            height: 149,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        }));
        let mut svc = BaService::with_chain(4, mem, vec![DEFAULT_COINBASE_ADDRESS.to_string()]);
        svc.set_subsidy_interval(150);
        svc.state.lock().unwrap().chain.height = 149;
        svc.set_ready();

        let height = 150u32;

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        assert_eq!(resp.height, height, "candidate height is 150");
        let regtest = crate::coinbase::block_subsidy(height, 150);
        let mainnet = crate::coinbase::block_subsidy(height, 210_000);
        assert_ne!(
            regtest, mainnet,
            "interval must change the subsidy at h=150"
        );
        assert_eq!(
            resp.coinbase_value, regtest,
            "coinbase uses the regtest (150) interval, not the mainnet default"
        );
    }

    /// CONSENSUS reconciliation — empty. No txs: empty candidate. subtree_hashes
    /// and merkle_proof are empty, coinbase = subsidy, num_txs = 0.
    #[tokio::test]
    async fn coinbase_reconciles_empty_candidate() {
        let svc = BaService::new(4);
        svc.set_ready();
        let height = svc.state.lock().unwrap().chain.height + 1;

        let resp = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner();

        let expected = crate::coinbase::block_subsidy(height, DEFAULT_SUBSIDY_HALVING_INTERVAL);
        assert_eq!(resp.coinbase_value, expected, "empty → coinbase = subsidy");
        assert!(resp.subtree_hashes.is_empty(), "no published subtrees");
        assert_eq!(resp.subtree_count, 0);
        assert!(
            resp.merkle_proof.is_empty(),
            "empty candidate → empty proof"
        );
        assert_eq!(resp.num_txs, 0, "no real txs");
    }
}

#[cfg(test)]
mod submit_tests {
    use std::collections::HashMap;

    use super::*;
    use crate::store::chain_mem::MemBlockchainClient;
    use crate::store::{BlobStore, ChainTip, StoreError};
    use ba_subtree_bench::hash::sha256d;
    use ba_subtree_bench::subtree::COINBASE_PLACEHOLDER;
    use tonic::async_trait;
    use tonic::Request;

    fn leaf(i: u32) -> Hash {
        sha256d(&i.to_le_bytes())
    }

    /// Recording in-memory BlobStore: captures every `set` (key + bytes) so the
    /// submit-persist test can assert the synchronously-persisted blob without
    /// touching the filesystem.
    #[derive(Default)]
    struct RecordingBlobStore {
        writes: Mutex<HashMap<Hash, Vec<u8>>>,
    }

    #[async_trait]
    impl BlobStore for RecordingBlobStore {
        async fn tx_hashes(&self, _subtree_hash: &Hash) -> Result<Vec<Hash>, StoreError> {
            Err(StoreError::NotFound("recording store has no reader".into()))
        }

        async fn subtree(
            &self,
            _root: &Hash,
        ) -> Result<ba_subtree_bench::subtree::Subtree, StoreError> {
            Err(StoreError::NotFound("recording store has no reader".into()))
        }

        async fn set(&self, key: &Hash, bytes: &[u8]) -> Result<(), StoreError> {
            self.writes.lock().unwrap().insert(*key, bytes.to_vec());
            Ok(())
        }

        async fn set_dah(&self, _key: &Hash, _dah: u32) -> Result<(), StoreError> {
            Ok(())
        }
    }

    /// Build a ready service whose in-process tip uses `n_bits`, wired to a
    /// `MemBlockchainClient` we keep a handle to for call assertions. The Mem tip
    /// matches genesis so the in-process chain (used by the handlers) is the source
    /// of truth for the stale check.
    fn service_with_nbits(n_bits: u32) -> (BaService, std::sync::Arc<MemBlockchainClient>) {
        let mem = std::sync::Arc::new(MemBlockchainClient::new(ChainTip {
            hash: [0u8; 32],
            height: 0,
            n_bits,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        }));
        let svc = BaService::with_chain(4, mem.clone(), vec![DEFAULT_COINBASE_ADDRESS.to_string()]);
        // Align the in-process chain n_bits with the requested difficulty.
        svc.state.lock().unwrap().chain.n_bits = n_bits;
        svc.set_ready();
        (svc, mem)
    }

    async fn candidate_id(svc: &BaService) -> Vec<u8> {
        svc.get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("candidate")
            .into_inner()
            .id
    }

    /// Mirror the service-built-coinbase header construction to find a nonce that
    /// meets the job's target, so the submit path's PoW check passes.
    fn find_meeting_nonce(svc: &BaService, id: &[u8]) -> u32 {
        let job = svc.jobs.get(id).expect("job");
        for nonce in 0..=u32::MAX {
            // Mirror the submit path EXACTLY: the nonce is stamped into the
            // coinbase (when non-zero), which changes the coinbase txid and thus
            // the block merkle root. Rebuild both per candidate nonce.
            let mut coinbase = crate::coinbase::create_coinbase(
                job.height,
                job.coinbase_value,
                b"",
                &svc.coinbase_addresses,
                FIXED_EXTRANONCE,
            )
            .expect("coinbase");
            apply_solution_fields(&mut coinbase, None, None, nonce).expect("apply solution");
            let coinbase_txid = sha256d(&coinbase);
            let mut subtrees = job.subtrees.clone();
            let merkle_root = ba_subtree_bench::block_merkle::try_block_merkle_root(
                &mut subtrees,
                &coinbase_txid,
            )
            .expect("merkle");
            let header = build_header(
                job.version,
                &job.previous_hash,
                &merkle_root,
                job.time,
                job.n_bits,
                nonce,
            );
            if meets_target(&header_hash(&header), job.n_bits) {
                return nonce;
            }
        }
        panic!("no meeting nonce");
    }

    /// A valid solution (regtest target, fresh job, nonce 0 meets it) publishes the
    /// block: add_block + set_block_subtrees_set are both invoked once.
    #[tokio::test]
    async fn valid_solution_publishes_block() {
        let (svc, mem) = service_with_nbits(0x207f_ffff);
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let id = candidate_id(&svc).await;

        // Find a nonce whose service-built coinbase header meets the regtest target
        // (the same coinbase + nonce the submit path will rebuild).
        let nonce = find_meeting_nonce(&svc, &id);

        let resp = svc
            .submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
                id: id.clone(),
                nonce,
                coinbase_tx: vec![],
                time: None,
                version: None,
            }))
            .await
            .expect("submit ok");
        assert!(resp.into_inner().ok);

        assert_eq!(mem.add_block_count(), 1, "add_block called once");
        assert_eq!(mem.set_subtrees_set_count(), 1, "subtrees_set called once");

        // The block stored the pre-coinbase subtree roots (2 completed subtrees at cap 4).
        let call = mem.last_add_block().unwrap();
        assert_eq!(call.subtree_hashes.len(), 2);
        assert_eq!(call.tx_count, 8, "8 txs across the two subtrees");
        // Jobs cleared after a successful submit.
        assert!(svc.jobs.get(&id).is_none());
    }

    /// A successful submit writes the block's COINBASE UTXO via the attached store
    /// (Go processCoinbaseUtxos), even though the optimistic tip advance makes the
    /// later notification path a no-op. Without this the GenerateBlocks loop would
    /// leave Aerospike with at most one coinbase (the rare notification that beat
    /// its optimistic apply).
    #[tokio::test]
    async fn submit_creates_coinbase_utxo() {
        let (mut svc, _mem) = service_with_nbits(0x207f_ffff);
        let utxo = std::sync::Arc::new(crate::store::utxo_mem::MemUtxoStore::default());
        svc.set_utxo_store(utxo.clone());
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let id = candidate_id(&svc).await;
        let nonce = find_meeting_nonce(&svc, &id);

        // Rebuild the submission the same way submit does, to know the coinbase txid.
        let built = svc
            .build_submission(&SubmitMiningSolutionRequest {
                id: id.clone(),
                nonce,
                coinbase_tx: vec![],
                time: None,
                version: None,
            })
            .expect("build submission");
        let expected_cb_txid = ba_subtree_bench::tx::Tx::from_bytes(&built.coinbase_tx)
            .expect("parse built coinbase")
            .txid();

        svc.submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
            id: id.clone(),
            nonce,
            coinbase_tx: vec![],
            time: None,
            version: None,
        }))
        .await
        .expect("submit ok");

        let calls = utxo.create_calls();
        assert_eq!(calls.len(), 1, "exactly one coinbase create on submit");
        let (txid, height, mined, locked) = &calls[0];
        assert_eq!(*txid, expected_cb_txid, "created the block's coinbase txid");
        assert_eq!(*height, built.height);
        assert!(!*locked, "coinbase create passes locked=false");
        assert!(mined.is_some(), "coinbase carries mined-block info");
    }

    /// Submit still succeeds when no UTXO store is attached (unit-test path / a
    /// deployment without one): the coinbase create is simply skipped.
    #[tokio::test]
    async fn submit_without_utxo_store_still_publishes() {
        let (svc, mem) = service_with_nbits(0x207f_ffff);
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let id = candidate_id(&svc).await;
        let nonce = find_meeting_nonce(&svc, &id);
        svc.submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
            id,
            nonce,
            coinbase_tx: vec![],
            time: None,
            version: None,
        }))
        .await
        .expect("submit ok without utxo store");
        assert_eq!(mem.add_block_count(), 1);
    }

    /// Go parity (Server.go:1438-1443): the published block carries a coinbase
    /// BUMP (BRC-74) computed from the job's subtrees. Proof: parse the recorded
    /// BUMP and fold the coinbase txid through its siblings — the result must be
    /// the merkle root the submitted header commits to (bytes [36..68]).
    #[tokio::test]
    async fn submit_carries_coinbase_bump_folding_to_merkle_root() {
        use ba_subtree_bench::hash::hash_pair;
        use ba_subtree_bench::tx::varint;

        let (svc, mem) = service_with_nbits(0x207f_ffff);
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let id = candidate_id(&svc).await;
        let nonce = find_meeting_nonce(&svc, &id);

        svc.submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
            id,
            nonce,
            coinbase_tx: vec![],
            time: None,
            version: None,
        }))
        .await
        .expect("submit ok");

        let call = mem.last_add_block().unwrap();
        assert!(!call.coinbase_bump.is_empty(), "block carries a BUMP");

        let bump = &call.coinbase_bump;
        let mut off = 0usize;
        let height = varint::read(bump, &mut off).expect("height varint");
        assert_eq!(height, 1, "BUMP block height = mined height");
        let tree_height = bump[off] as usize;
        off += 1;
        assert!(tree_height > 0, "non-trivial path for an 8-tx block");

        let coinbase_txid = sha256d(&call.coinbase_tx);
        let mut acc = coinbase_txid;
        for level in 0..tree_height {
            let node_count = varint::read(bump, &mut off).expect("node count");
            let mut sibling: Option<Hash> = None;
            for _ in 0..node_count {
                let offset = varint::read(bump, &mut off).expect("offset");
                let flag = bump[off];
                off += 1;
                let mut hash = [0u8; 32];
                hash.copy_from_slice(&bump[off..off + 32]);
                off += 32;
                match flag {
                    0x02 => {
                        assert_eq!(offset, 0, "txid node offset");
                        assert_eq!(hash, coinbase_txid, "txid node is the coinbase");
                    }
                    0x00 => {
                        assert_eq!(offset, 1, "coinbase sibling offset");
                        sibling = Some(hash);
                    }
                    f => panic!("unexpected BUMP flag {f:#x} at level {level}"),
                }
            }
            acc = hash_pair(&acc, &sibling.expect("sibling at every level"));
        }
        assert_eq!(off, bump.len(), "no trailing bytes");
        assert_eq!(
            &call.header[36..68],
            &acc[..],
            "BUMP folds to the header's merkle root"
        );
    }

    /// Capability F: the cached job's nBits is the next-work-required target (not
    /// the tip's), and a submit whose header meets THAT target validates and
    /// publishes. Tip nBits is the unmeetable hard target; next-work is the easy
    /// regtest target, so the submit can only pass if `meets_target` checks the
    /// job's next-work nBits.
    #[tokio::test]
    async fn submit_validates_against_job_nbits() {
        let tip_nbits = 0x0300_0001; // unmeetable at nonce 0
        let next_nbits = 0x207f_ffff; // regtest easy
        let mem = std::sync::Arc::new(
            MemBlockchainClient::new(ChainTip {
                hash: [0u8; 32],
                height: 0,
                n_bits: tip_nbits,
                version: 0x2000_0000,
                median_time: 1_700_000_000,
            })
            .with_next_work(next_nbits),
        );
        let svc = BaService::with_chain(4, mem.clone(), vec![DEFAULT_COINBASE_ADDRESS.to_string()]);
        svc.state.lock().unwrap().chain.n_bits = tip_nbits;
        svc.set_ready();

        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let id = candidate_id(&svc).await;

        // The job stores the next-work nBits, not the tip's.
        {
            let job = svc.jobs.get(&id).expect("job");
            assert_eq!(job.n_bits, next_nbits, "job stores next-work nBits");
            assert_ne!(job.n_bits, tip_nbits, "job nBits differs from the tip");
        }

        let nonce = find_meeting_nonce(&svc, &id);
        svc.submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
            id: id.clone(),
            nonce,
            coinbase_tx: vec![],
            time: None,
            version: None,
        }))
        .await
        .expect("submit ok against job (next-work) nBits");

        assert_eq!(mem.add_block_count(), 1, "block published");
    }

    /// A stale job (chain advanced past its parent) is rejected; nothing published.
    #[tokio::test]
    async fn stale_job_is_rejected() {
        let (svc, mem) = service_with_nbits(0x207f_ffff);
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..4u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let id = candidate_id(&svc).await;

        // Advance the tip out from under the job.
        svc.state.lock().unwrap().chain.best_hash = [9u8; 32];

        let err = svc
            .submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
                id,
                nonce: 0,
                coinbase_tx: vec![],
                time: None,
                version: None,
            }))
            .await
            .unwrap_err();
        assert_eq!(err.code(), tonic::Code::FailedPrecondition);
        assert_eq!(
            mem.add_block_count(),
            0,
            "nothing published for a stale job"
        );
    }

    /// A hard target the nonce-0 header cannot meet → invalid_argument, no publish.
    #[tokio::test]
    async fn bad_pow_is_rejected() {
        // 0x03000001 → target = 0x000001 * 256^0 = 1: essentially unmeetable.
        let (svc, mem) = service_with_nbits(0x0300_0001);
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..4u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let id = candidate_id(&svc).await;

        let err = svc
            .submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
                id,
                nonce: 0,
                coinbase_tx: vec![],
                time: None,
                version: None,
            }))
            .await
            .unwrap_err();
        assert_eq!(err.code(), tonic::Code::InvalidArgument);
        assert_eq!(mem.add_block_count(), 0, "no publish on bad PoW");
    }

    /// An unknown candidate id → not_found, no publish.
    #[tokio::test]
    async fn missing_job_is_not_found() {
        let (svc, mem) = service_with_nbits(0x207f_ffff);
        let err = svc
            .submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
                id: vec![1, 2, 3],
                nonce: 0,
                coinbase_tx: vec![],
                time: None,
                version: None,
            }))
            .await
            .unwrap_err();
        assert_eq!(err.code(), tonic::Code::NotFound);
        assert_eq!(mem.add_block_count(), 0);
    }

    /// WRITER-LAG RACE FIX (B4.2): a successful submit persists the job's subtrees
    /// SYNCHRONOUSLY before publish, and the persisted blob is the PLACEHOLDER
    /// version (node 0 == [0xFF; 32]) — NOT the coinbase-substituted version.
    /// Also asserts the persisted roots exactly match the block's published
    /// subtree_hashes (so Block.Valid re-reads the right keys).
    #[tokio::test]
    async fn submit_persists_placeholder_subtrees_before_publish() {
        let (mut svc, mem) = service_with_nbits(0x207f_ffff);
        let rec = Arc::new(RecordingBlobStore::default());
        let store: Arc<dyn BlobStore> = rec.clone();
        svc.set_blob_store(store);

        // 8 txs at cap 4 → 2 completed subtrees. The FIRST completed subtree carries
        // the coinbase placeholder at node 0 (added when the subtree opened).
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..8u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let id = candidate_id(&svc).await;
        let nonce = find_meeting_nonce(&svc, &id);

        // Capture the job's pre-replacement subtree roots BEFORE the submit clears
        // the job store — these are exactly what the block publishes.
        let expected_roots: Vec<Hash> = {
            let job = svc.jobs.get(&id).expect("job");
            job.subtrees
                .iter()
                .map(|s| s.clone().root_hash().expect("root"))
                .collect()
        };

        svc.submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
            id: id.clone(),
            nonce,
            coinbase_tx: vec![],
            time: None,
            version: None,
        }))
        .await
        .expect("submit ok");

        // The block was published and stored those exact subtree roots.
        let call = mem.last_add_block().expect("add_block call");
        assert_eq!(
            call.subtree_hashes, expected_roots,
            "published subtree_hashes must equal the job's pre-replacement roots"
        );

        // Every published subtree root was persisted synchronously.
        let writes = rec.writes.lock().unwrap();
        assert_eq!(
            writes.len(),
            expected_roots.len(),
            "each job subtree persisted synchronously at submit"
        );
        for root in &expected_roots {
            let bytes = writes
                .get(root)
                .unwrap_or_else(|| panic!("subtree {} persisted", hex::encode(root)));
            // Magic header, then the serialized body.
            assert_eq!(
                &bytes[..crate::subtree_store::SUBTREE_MAGIC.len()],
                &crate::subtree_store::SUBTREE_MAGIC,
                "blob carries the magic header"
            );
            let back = Subtree::deserialize(&bytes[crate::subtree_store::SUBTREE_MAGIC.len()..])
                .expect("deserialize persisted subtree");
            // CONSENSUS DETAIL: node 0 of the first subtree is the PLACEHOLDER, never
            // the coinbase. (The non-first subtrees never hold node-0 placeholder, so
            // we assert it only for the first.)
            if *root == expected_roots[0] {
                assert_eq!(
                    back.nodes[0].hash, COINBASE_PLACEHOLDER,
                    "persisted blob must be the placeholder version, not coinbase-substituted"
                );
            }
            // Sanity: the persisted root (header field) round-trips to the key.
            let mut clone = back;
            assert_eq!(
                clone.root_hash().expect("root"),
                *root,
                "persisted blob's recomputed root equals its key"
            );
        }
    }

    /// IS3 — an incomplete-only job (no completed subtree) submits and persists the
    /// INCOMPLETE subtree synchronously under its published root, placeholder at
    /// node 0. The async writer only handles completed subtrees, so this sync
    /// submit-persist is the only thing that makes the incomplete-subtree blob exist
    /// for `Block.Valid`.
    #[tokio::test]
    async fn submit_persists_incomplete_subtree_under_published_root() {
        let (mut svc, mem) = service_with_nbits(0x207f_ffff);
        let rec = Arc::new(RecordingBlobStore::default());
        let store: Arc<dyn BlobStore> = rec.clone();
        svc.set_blob_store(store);

        // 2 real txs at cap 4 → NO completed subtree; the candidate publishes the
        // incomplete subtree (placeholder + 2 real txs).
        {
            let mut st = svc.state.lock().unwrap();
            st.add(leaf(1), 11, 1);
            st.add(leaf(2), 22, 1);
            assert_eq!(st.num_chained(), 0, "no completed subtree");
        }
        let id = candidate_id(&svc).await;
        let nonce = find_meeting_nonce(&svc, &id);

        // The single published root = the incomplete subtree's root.
        let expected_roots: Vec<Hash> = {
            let job = svc.jobs.get(&id).expect("job");
            assert_eq!(job.subtrees.len(), 1, "exactly the incomplete subtree");
            job.subtrees
                .iter()
                .map(|s| s.clone().root_hash().expect("root"))
                .collect()
        };

        svc.submit_mining_solution(Request::new(SubmitMiningSolutionRequest {
            id: id.clone(),
            nonce,
            coinbase_tx: vec![],
            time: None,
            version: None,
        }))
        .await
        .expect("submit ok");

        let call = mem.last_add_block().expect("add_block call");
        assert_eq!(
            call.subtree_hashes, expected_roots,
            "published subtree_hashes == the incomplete subtree root"
        );

        let writes = rec.writes.lock().unwrap();
        assert_eq!(
            writes.len(),
            1,
            "the incomplete subtree persisted at submit"
        );
        let root = &expected_roots[0];
        let bytes = writes
            .get(root)
            .unwrap_or_else(|| panic!("incomplete subtree {} persisted", hex::encode(root)));
        assert_eq!(
            &bytes[..crate::subtree_store::SUBTREE_MAGIC.len()],
            &crate::subtree_store::SUBTREE_MAGIC,
            "blob carries the magic header"
        );
        let back = Subtree::deserialize(&bytes[crate::subtree_store::SUBTREE_MAGIC.len()..])
            .expect("deserialize persisted incomplete subtree");
        assert_eq!(
            back.nodes[0].hash, COINBASE_PLACEHOLDER,
            "persisted incomplete subtree node 0 is the placeholder"
        );
        let mut clone = back;
        assert_eq!(
            clone.root_hash().expect("root"),
            *root,
            "persisted blob's recomputed root equals its key"
        );
    }

    /// generate_blocks mines + publishes: count:1 on a regtest tip calls add_block
    /// once and advances the in-process tip height.
    #[tokio::test]
    async fn generate_blocks_publishes_and_advances() {
        let (svc, mem) = service_with_nbits(0x207f_ffff);
        {
            let mut st = svc.state.lock().unwrap();
            for i in 0..4u32 {
                st.add(leaf(i), i as u64, 1);
            }
        }
        let height_before = svc.state.lock().unwrap().chain.height;

        svc.generate_blocks(Request::new(GenerateBlocksRequest {
            count: 1,
            address: None,
            max_tries: None,
        }))
        .await
        .expect("generate ok");

        assert_eq!(mem.add_block_count(), 1, "one block published");
        assert_eq!(mem.set_subtrees_set_count(), 1);
        let height_after = svc.state.lock().unwrap().chain.height;
        assert_eq!(height_after, height_before + 1, "tip advanced by one");
    }

    /// Capability E: GetMiningCandidate must reject with FailedPrecondition when
    /// the blockchain FSM is not RUNNING (Go Server.go:1225-1232). Readiness is
    /// satisfied (set_ready) so the FSM gate — not check_ready — is exercised.
    #[tokio::test]
    async fn get_mining_candidate_rejected_when_fsm_not_running() {
        let mem = std::sync::Arc::new(
            MemBlockchainClient::new(ChainTip {
                hash: [0u8; 32],
                height: 0,
                n_bits: 0x207f_ffff,
                version: 0x2000_0000,
                median_time: 1_700_000_000,
            })
            .with_fsm_running(false),
        );
        let svc = BaService::with_chain(4, mem, vec![DEFAULT_COINBASE_ADDRESS.to_string()]);
        svc.set_ready();

        let err = svc
            .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect_err("not-RUNNING FSM must reject GetMiningCandidate");
        assert_eq!(err.code(), tonic::Code::FailedPrecondition);
        assert!(
            err.message().contains("not in RUNNING state"),
            "message: {}",
            err.message()
        );
    }

    /// Companion: with the FSM RUNNING (mem default) the gate is a no-op and a
    /// candidate is produced — guards against the gate over-rejecting.
    #[tokio::test]
    async fn get_mining_candidate_ok_when_fsm_running() {
        let (svc, _mem) = service_with_nbits(0x207f_ffff);
        svc.get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
            .await
            .expect("RUNNING FSM -> candidate produced");
    }
}

//! Gate 2 — native Rust Block Assembly service. Thin binary: all logic and the
//! generated proto types live in the `ba_service` library so this target and the
//! integration tests share one crate root (one proto type tree). Stage 3 boots
//! from `settings.conf`: connect the real Aerospike UTXO store + Go blockchain
//! service, seed the chain tip, load unmined txs (then flip ready), and run the
//! notification subscription (reconciliation + setMined on extend).

use std::net::ToSocketAddrs;
use std::sync::atomic::Ordering;
use std::sync::Arc;

use ba_service::blockassembly_api::block_assembly_api_server::BlockAssemblyApiServer;
use ba_service::config::Stage3Config;
use ba_service::server::BaService;
use ba_service::store::blob_fs::FsBlobStore;
use ba_service::store::chain_grpc::{run_subscription, GrpcBlockchainClient};
use ba_service::store::utxo_aero::AeroUtxoStore;
use ba_service::store::{BlobStore, BlockchainClient, UtxoStore};
use ba_service::subtree_writer::run_subtree_writer;
use tonic::transport::Server;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Print all resolved settings on startup, like the Go BlockAssembler does via
    // gocore.Stats (CMDLINE / SETTINGS_ENV / every setting, encrypted values masked).
    // Done before typed derivation so the dump appears even if a value is invalid.
    println!("{}", rustcore::config::config().stats());

    let cfg = Stage3Config::load()?;

    // Subtree capacity (initial_merkle_items_per_subtree). Dynamic sizing arrives
    // with the full processor; this is the starting size, Go-parity default 1M.
    let cap: usize = cfg.subtree_initial_size;

    // gRPC listen address from blockassembly_grpcListenAddress. A leading ':'
    // (e.g. ":8085") means all interfaces; resolve hostnames via ToSocketAddrs.
    let listen = if cfg.grpc_listen_address.starts_with(':') {
        format!("0.0.0.0{}", cfg.grpc_listen_address)
    } else {
        cfg.grpc_listen_address.clone()
    };
    // Prefer IPv4: `localhost` resolves to both 127.0.0.1 and ::1, and picking
    // the IPv6 one first would leave `127.0.0.1:PORT` (what grpcurl/most tooling
    // dials by default) refused. Fall back to whatever resolved if no IPv4.
    let candidates: Vec<_> = listen
        .to_socket_addrs()
        .map_err(|e| format!("grpc listen address '{listen}': {e}"))?
        .collect();
    let addr = candidates
        .iter()
        .find(|a| a.is_ipv4())
        .or_else(|| candidates.first())
        .copied()
        .ok_or_else(|| format!("grpc listen address '{listen}' resolved to nothing"))?;

    // Real stores (no mocks on the production path). A connection failure here is
    // fatal: the service must not come up half-initialised.
    let chain = Arc::new(GrpcBlockchainClient::connect(cfg.blockchain_grpc_address.clone()).await?);
    let utxo = Arc::new(
        AeroUtxoStore::connect(
            &cfg.aerospike_hosts,
            &cfg.aerospike_namespace,
            &cfg.aerospike_set,
            &cfg.udf_module,
            cfg.block_height_retention,
        )
        .await?,
    );

    let mut svc = BaService::with_chain(cap, chain.clone(), cfg.coinbase_addresses.clone());
    svc.set_subsidy_interval(cfg.subsidy_halving_interval());
    // The service writes self-mined blocks' coinbase UTXOs at submit time (the
    // notification path no-ops after the optimistic tip advance), so it needs the
    // same UTXO store the subscription/boot paths use.
    svc.set_utxo_store(utxo.clone());
    let state = svc.shared_state();
    let ready = svc.ready_handle();

    // Completed-subtree persistence: a writable filesystem blob store + the async
    // writer task. The ingest handlers drain newly-completed subtrees and send
    // them here; the writer serializes (magic header + body) and `set`s each under
    // its root hash, byte-identical to Go's subtree blobs, then sends the
    // FSM-gated subtree notification via `chain`.
    {
        let blob: Arc<dyn BlobStore> = Arc::new(FsBlobStore::new(cfg.subtree_store_path.clone()));
        let writer_chain: Arc<dyn BlockchainClient> = chain.clone();
        let (subtree_tx, subtree_rx) = tokio::sync::mpsc::unbounded_channel();
        svc.set_subtree_sink(subtree_tx);
        // Same blob store on the synchronous submit path (writer-lag race fix): the
        // submit handler persists the job's subtrees (placeholder version) just
        // before add_block, in case the async writer below hasn't flushed yet. Both
        // write byte-identical blobs under the same keys (idempotent).
        svc.set_blob_store(blob.clone());
        tokio::spawn(run_subtree_writer(subtree_rx, blob, writer_chain));
        println!(
            "ba-service: subtree writer started (store={})",
            cfg.subtree_store_path
        );
    }

    // 1. Seed the real chain tip. Fatal on failure — the candidate must build on
    //    the true tip, never on the genesis default.
    let tip = chain.best_tip().await?;
    {
        let mut st = state.lock().unwrap();
        st.seed_tip(
            tip.hash,
            tip.height,
            tip.n_bits,
            tip.version,
            tip.median_time,
        );
    }
    println!(
        "ba-service: seeded tip height={} hash={}",
        tip.height,
        hex::encode(tip.hash)
    );

    // 2. Load unmined txs, THEN flip ready. If the load fails we deliberately do
    //    NOT flip ready: the service stays Unavailable rather than serve a
    //    half-initialised assembly (a candidate missing the mempool would be
    //    silently wrong). Fix the backend and restart.
    //
    //    Faithful to Go's BlockAssembler.loadUnminedTransactions (§10 candidate
    //    parity): filter out txs already mined on the best chain, mark those for
    //    longest-chain reconciliation, then add the rest in `createdAt`
    //    oldest-first order so the subtree composition matches Go. Tx insertion
    //    order determines subtree_hashes, so the sort is consensus-adjacent.
    {
        let utxo = utxo.clone();
        let chain = chain.clone();
        let state = state.clone();
        let ready = ready.clone();
        let tip_hash = tip.hash;
        // scan_depth: how far back from the tip we fetch header IDs to decide
        // "already on the best chain". Go uses `bestBlockHeight + 1` (all headers
        // back to genesis). Stage 3 uses `block_height_retention` per the plan —
        // a bounded window. NOTE/CONCERN: this differs from Go; a tx mined in a
        // block older than the retention window will NOT be filtered out and will
        // be (incorrectly) re-added to assembly. Acceptable for the retention
        // horizon Teranode operates within, but flagged for the parity gate.
        let scan_depth = cfg.block_height_retention as u64;
        tokio::spawn(async move {
            match utxo.unmined().await {
                Ok(txs) => {
                    // Best-chain header IDs. On error: empty set = no filtering
                    // (degraded path — every unmined tx is added; nothing marked).
                    let best_ids: std::collections::HashSet<u32> = match chain
                        .block_header_ids(&tip_hash, scan_depth)
                        .await
                    {
                        Ok(ids) => ids.into_iter().collect(),
                        Err(e) => {
                            eprintln!(
                                "ba-service: block_header_ids failed: {e} — best-chain filter disabled (adding all unmined txs)"
                            );
                            std::collections::HashSet::new()
                        }
                    };

                    let total = txs.len();

                    // Partition + sort via the PURE plan_unmined_load (the exact
                    // path the offline candidate-parity test exercises):
                    //  - already mined on best chain (block_ids ∩ best_ids) -> mark, NOT added
                    //  - locked -> KEPT (added) AND collected into plan.unlock
                    //  - otherwise -> keep, sorted by createdAt oldest-first
                    //
                    // GO PARITY (K1): Go's loadUnminedTransactions ADDS locked txs
                    // to the subtree processor (BlockAssembler.go: the append at
                    // :2269 runs before the locked check at :2271), tracks them in
                    // `lockedTxs`, then unlocks them via SetLocked(.., false) at the
                    // end (:2473). We now mirror this: locked txs are added like any
                    // other tx and their hashes returned in plan.unlock for the
                    // set_locked call below.
                    let plan = ba_service::load::plan_unmined_load(txs, &best_ids);

                    // Reconcile data inconsistency (block_ids on main chain but still
                    // flagged unmined). Best-effort: log on error, do NOT abort.
                    if !plan.mark_on_longest.is_empty() {
                        if let Err(e) = utxo
                            .mark_on_longest_chain(&plan.mark_on_longest, true)
                            .await
                        {
                            eprintln!(
                                "ba-service: mark_on_longest_chain failed for {} txs: {e} — continuing load",
                                plan.mark_on_longest.len()
                            );
                        }
                    }

                    let loaded = plan.keep_sorted.len();
                    {
                        let mut st = state.lock().unwrap();
                        for t in &plan.keep_sorted {
                            st.add(t.txid, t.fee, t.size);
                        }
                    }

                    // Unlock the locked txs we just added (Go: SetLocked(.., false)
                    // after the load). Best-effort: log on error, do NOT abort —
                    // the txs are already in the candidate.
                    // DIVERGENCE FROM GO: Go treats a SetLocked failure here as a
                    // fatal ProcessingError that aborts the load (BlockAssembler.go
                    // :2474). We log and continue for boot resilience; the txs stay
                    // locked in the store but are present in the candidate. VERIFY-ON-RIG.
                    let unlocked = plan.unlock.len();
                    if !plan.unlock.is_empty() {
                        if let Err(e) = utxo.set_locked(&plan.unlock, false).await {
                            eprintln!(
                                "ba-service: set_locked(unlock) failed for {unlocked} txs: {e} — continuing (txs already added)"
                            );
                        }
                    }

                    // Single summary line (like Go's "loaded N ... unlocked ...").
                    println!(
                        "ba-service: unmined load complete — loaded={loaded} marked_on_longest_chain={} unlocked={unlocked} total={total}",
                        plan.mark_on_longest.len()
                    );
                    ready.store(true, Ordering::SeqCst);
                    println!("ba-service: ready — serving RPCs");
                }
                Err(e) => {
                    eprintln!(
                        "ba-service: unmined load failed: {e} — staying Unavailable (not flipping ready)"
                    );
                }
            }
        });
    }

    // 3. Start the notification subscription (reconciliation + setMined on extend).
    {
        let chain = chain.clone();
        let utxo = utxo.clone();
        let state = state.clone();
        let subtree_store_path = cfg.subtree_store_path.clone();
        tokio::spawn(async move {
            if let Err(e) = run_subscription(chain, utxo, state, subtree_store_path).await {
                eprintln!("ba-service: subscription ended: {e}");
            }
        });
    }

    // gRPC reflection so `grpcurl list` / `describe` work without proto files.
    const FILE_DESCRIPTOR_SET: &[u8] =
        include_bytes!(concat!(env!("OUT_DIR"), "/ba_descriptor.bin"));
    let reflection_v1 = tonic_reflection::server::Builder::configure()
        .register_encoded_file_descriptor_set(FILE_DESCRIPTOR_SET)
        .build_v1()?;
    let reflection_v1alpha = tonic_reflection::server::Builder::configure()
        .register_encoded_file_descriptor_set(FILE_DESCRIPTOR_SET)
        .build_v1alpha()?;

    println!("ba-service listening on {addr} (cap={cap})");
    Server::builder()
        .add_service(reflection_v1)
        .add_service(reflection_v1alpha)
        .add_service(BlockAssemblyApiServer::new(svc))
        .serve(addr)
        .await?;

    Ok(())
}

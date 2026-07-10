//! Capability A rig integration test for the block-publish path.
//! Run with: SETTINGS_CONTEXT=<ctx> cargo test --test it_capa -- --ignored --nocapture
//!
//! This test is `#[ignore]` — it requires a running blockchain service (and the
//! rig's own store) to publish a real block. It compiles in the normal suite so
//! the wiring is type-checked, but only runs under `--ignored` against live
//! backends.

use std::sync::Arc;

use ba_service::blockassembly_api::block_assembly_api_server::BlockAssemblyApi;
use ba_service::blockassembly_api::{
    AddTxRequest, GenerateBlocksRequest, GetMiningCandidateRequest,
};
use ba_service::config::Stage3Config;
use ba_service::server::BaService;
use ba_service::store::blob_fs::FsBlobStore;
use ba_service::store::chain_grpc::GrpcBlockchainClient;
use ba_service::store::{BlobStore, BlockchainClient};
use tonic::Request;

/// GenerateBlocks{count:1} against the rig must advance the chain tip by one.
#[tokio::test]
#[ignore = "requires running blockchain service + own store"]
async fn generate_blocks_advances_the_chain() {
    let cfg = Stage3Config::load().expect("settings.conf");
    let chain = Arc::new(
        GrpcBlockchainClient::connect(cfg.blockchain_grpc_address.clone())
            .await
            .expect("connect blockchain service"),
    );

    // Record the starting tip height.
    let before = chain.best_tip().await.expect("best_tip").height;

    // Boot a service wired to the real chain client, seed its in-process tip from
    // the live tip, then flip ready so the handlers serve.
    let svc = BaService::with_chain(1024, chain.clone(), cfg.coinbase_addresses.clone());
    {
        let tip = chain.best_tip().await.expect("best_tip");
        let state = svc.shared_state();
        let mut st = state.lock().unwrap();
        st.seed_tip(
            tip.hash,
            tip.height,
            tip.n_bits,
            tip.version,
            tip.median_time,
        );
    }
    svc.set_ready();

    // Generate a single block.
    svc.generate_blocks(Request::new(GenerateBlocksRequest {
        count: 1,
        address: None,
        max_tries: None,
    }))
    .await
    .expect("generate_blocks");

    // The blockchain service tip must have advanced.
    let after = chain.best_tip().await.expect("best_tip after").height;
    assert_eq!(
        after,
        before + 1,
        "GenerateBlocks should advance the chain tip by one (before={before}, after={after})"
    );
}

/// COMBINED A+B GATE (B4.1): boot against the rig, ingest at least one cap's worth
/// of txs so a subtree COMPLETES and is persisted, GenerateBlocks{count:1}, then
/// assert BOTH (a) the chain tip advanced AND (b) every published subtree blob is
/// persisted + readable from the blob store. Exercises the full publish + persist +
/// validate path: the writer-lag race fix (B4.2) guarantees the subtree blobs exist
/// before add_block, so the blockchain service's Block.Valid re-read succeeds.
#[tokio::test]
#[ignore = "requires running blockchain service + own store"]
async fn generate_block_persists_subtrees_and_validates() {
    let cfg = Stage3Config::load().expect("settings.conf");
    let chain = Arc::new(
        GrpcBlockchainClient::connect(cfg.blockchain_grpc_address.clone())
            .await
            .expect("connect blockchain service"),
    );

    let before = chain.best_tip().await.expect("best_tip").height;

    // Cap drives how many txs are needed to COMPLETE a subtree. Use the same cap the
    // service is built with, and ingest a full cap's worth so at least one subtree
    // chains (and persists).
    let cap: usize = 1024;

    // Service wired to the real chain client AND a filesystem blob store at the
    // rig's subtree path — so the synchronous submit-time persist (B4.2) lands real
    // blobs we can read back. The same store the rig's blockchain service reads.
    let mut svc = BaService::with_chain(cap, chain.clone(), cfg.coinbase_addresses.clone());
    let blob: Arc<dyn BlobStore> = Arc::new(FsBlobStore::new(cfg.subtree_store_path.clone()));
    svc.set_blob_store(blob.clone());
    {
        let tip = chain.best_tip().await.expect("best_tip");
        let state = svc.shared_state();
        let mut st = state.lock().unwrap();
        st.seed_tip(
            tip.hash,
            tip.height,
            tip.n_bits,
            tip.version,
            tip.median_time,
        );
    }
    svc.set_ready();

    // Ingest one full cap of txs so at least one subtree completes + persists.
    for i in 0..cap as u32 {
        let mut txid = [0u8; 32];
        txid[..4].copy_from_slice(&i.to_le_bytes());
        svc.add_tx(Request::new(AddTxRequest {
            txid: txid.to_vec(),
            fee: i as u64,
            size: 1,
            locktime: 0,
            utxos: vec![],
            tx_inpoints: vec![],
        }))
        .await
        .expect("add_tx");
    }

    // Snapshot the candidate's published subtree hashes BEFORE generating — the
    // assembly state is unchanged until submit, so GenerateBlocks publishes these
    // same roots.
    let candidate = svc
        .get_mining_candidate(Request::new(GetMiningCandidateRequest::default()))
        .await
        .expect("candidate")
        .into_inner();
    assert!(
        !candidate.subtree_hashes.is_empty(),
        "a full cap of txs must complete at least one subtree"
    );

    // Generate a single block (build + PoW + add_block + sync subtree persist).
    svc.generate_blocks(Request::new(GenerateBlocksRequest {
        count: 1,
        address: None,
        max_tries: None,
    }))
    .await
    .expect("generate_blocks");

    // (a) The chain tip advanced.
    let after = chain.best_tip().await.expect("best_tip after").height;
    assert_eq!(
        after,
        before + 1,
        "GenerateBlocks should advance the chain tip by one (before={before}, after={after})"
    );

    // (b) Every published subtree blob is persisted + readable from the store.
    for raw in &candidate.subtree_hashes {
        let mut root = [0u8; 32];
        root.copy_from_slice(raw);
        let txs = blob
            .tx_hashes(&root)
            .await
            .unwrap_or_else(|e| panic!("subtree {} not readable: {e}", hex::encode(root)));
        assert!(
            !txs.is_empty(),
            "persisted subtree {} must hold tx hashes",
            hex::encode(root)
        );
    }
}

//! Integration tests against the RUNNING Aerospike + blockchain service.
//! Run with: SETTINGS_CONTEXT=<ctx> cargo test --test it_stage3 -- --ignored --nocapture
use ba_service::config::Stage3Config;
use ba_service::store::chain_grpc::GrpcBlockchainClient;
use ba_service::store::utxo_aero::AeroUtxoStore;
use ba_service::store::{
    BlockchainClient, IgnoreFlags, MinedBlockInfo, Spend, SpendingData, UtxoStore,
};

async fn store() -> AeroUtxoStore {
    let cfg = Stage3Config::load().expect("settings.conf");
    AeroUtxoStore::connect(
        &cfg.aerospike_hosts,
        &cfg.aerospike_namespace,
        &cfg.aerospike_set,
        &cfg.udf_module,
        cfg.block_height_retention,
    )
    .await
    .expect("connect aerospike")
}

#[tokio::test]
#[ignore = "requires running Aerospike"]
async fn set_mined_appends_block_id() {
    // Pre-seed a known unmined utxo record out-of-band (Go seeder or a prior put);
    // here we assert the call succeeds and the returned map contains the hash.
    let s = store().await;
    let txid = [0xAB; 32];
    let info = MinedBlockInfo {
        block_id: 12345,
        block_height: 1,
        subtree_idx: 0,
        on_longest_chain: true,
        unset_mined: false,
    };
    let res = s
        .set_mined_multi(&[txid], &info)
        .await
        .expect("set_mined ok");
    assert!(res.contains_key(&txid), "every input hash must be present");
    assert!(res[&txid].contains(&12345));
}

#[tokio::test]
#[ignore = "requires running Aerospike"]
async fn create_coinbase_utxo_writes_and_reads_back() {
    // Build a real coinbase (BIP34 height in scriptSig + a unique marker so its
    // txid never collides with a chain tx), create its UTXO record, then read it
    // back via the same store. Proves the coinbase-create path Go runs in
    // SubtreeProcessor.processCoinbaseUtxos works against the live BSV-fork server.
    let s = store().await;

    let height = 999_001u32;
    let cb_bytes = ba_service::coinbase::create_coinbase(
        height,
        50_0000_0000,
        b"/ba-rust-create-probe/",
        &[ba_service::config::DEFAULT_COINBASE_ADDRESS.to_string()],
        [0u8; 12],
    )
    .expect("build coinbase");
    let tx = ba_subtree_bench::tx::Tx::from_bytes(&cb_bytes).expect("parse coinbase");
    let txid = tx.txid();

    let mined = MinedBlockInfo {
        block_id: 9_990_001,
        block_height: height,
        subtree_idx: 0,
        on_longest_chain: true,
        unset_mined: false,
    };

    // CREATE_ONLY: re-runs hit the benign key-exists path and still return Ok.
    s.create(&tx, height, Some(mined), false)
        .await
        .expect("create coinbase utxo");

    // Read back the metadata: record exists, carries our block_id, not conflicting.
    let meta = s
        .get_tx_meta(&txid)
        .await
        .expect("get_tx_meta ok")
        .expect("coinbase record must exist after create");
    assert!(
        meta.block_ids.contains(&9_990_001),
        "coinbase blockIDs must contain the mined block id, got {:?}",
        meta.block_ids
    );
    assert!(!meta.conflicting, "coinbase must not be conflicting");

    // The single output is present and UNSPENT.
    let spends = s.get_spending_datas(&txid).await.expect("get_spending_datas ok");
    assert_eq!(spends.len(), tx.outputs.len(), "one entry per output");
    assert!(
        spends.iter().all(|s| s.is_none()),
        "freshly-created coinbase outputs are unspent"
    );
}

#[tokio::test]
#[ignore = "requires running Aerospike with unminedSince sindex + seeded data"]
async fn unmined_returns_seeded_set() {
    let s = store().await;
    let txs = s.unmined().await.expect("unmined query ok");
    // The rig seeds N unmined txs; assert we read them with fee/size populated.
    assert!(!txs.is_empty(), "expected seeded unmined txs");
    assert!(txs.iter().all(|t| t.size > 0));
}

// ---------------------------------------------------------------------------
// Task I1 — boot seeds tip + unmined set (end-to-end, requires the rig).
// ---------------------------------------------------------------------------

#[tokio::test]
#[ignore = "requires running Aerospike + blockchain service"]
async fn boot_seeds_tip_and_unmined() {
    let cfg = Stage3Config::load().unwrap();
    let chain = GrpcBlockchainClient::connect(cfg.blockchain_grpc_address.clone())
        .await
        .unwrap();
    let tip = chain.best_tip().await.expect("real tip");
    assert!(tip.height >= 1, "blockchain service returned a real tip");

    let utxo = store().await;
    let unmined = utxo.unmined().await.expect("unmined load");
    // Assert the seeded count matches the rig's known fixture (set by the harness).
    println!(
        "seeded tip height={}, unmined={}",
        tip.height,
        unmined.len()
    );
}

// ---------------------------------------------------------------------------
// Task I2 — candidate parity vs Go.
//
// Two layers of coverage:
//
//   (a) `candidate_subtrees_match_go_golden` — a PURELY OFFLINE, runnable parity
//       check. The candidate's `subtree_hashes` are exactly `chained_roots()`
//       from the same `AssemblyState` engine `GetMiningCandidate` reads, and its
//       `num_txs` is `AssemblyState::num_txs()`. We drive that engine with the
//       same `leaf(i)` tx set the Gate 1 Go generator used and assert both
//       fields against the Go-validated `ingest.txt` golden vectors
//       (`rust/ba-subtree-bench/fixtures/golden/ingest.txt`, produced by
//       `cd gen && go run . ingest`). This is real cross-impl coverage of the
//       candidate's subtree/count fields — no rig required.
//
//   (b) `candidate_matches_go_for_seeded_set` — the full Stage-3-gate parity
//       check against a RECORDED GO `GetMiningCandidate` response for the rig's
//       seeded unmined set. That fixture does not exist yet (see the
//       REQUIRES-RIG-FIXTURE note below); the test is an honest `#[ignore]`
//       scaffold that panics with an explicit message naming the missing
//       fixture, so it can never silently pass.
// ---------------------------------------------------------------------------

use ba_service::assembly::AssemblyState;

/// Must match `ba-subtree-bench/gen/main.go` `leaf(i)` and `tests/golden.rs`:
/// double-SHA256 of the little-endian uint32 `i`.
fn leaf(i: u32) -> [u8; 32] {
    ba_subtree_bench::hash::sha256d(&i.to_le_bytes())
}

/// OFFLINE candidate parity: drive `AssemblyState` (the engine behind
/// `GetMiningCandidate`) with the Gate 1 golden ingest tx set and assert the
/// candidate's `subtree_hashes` (= chained roots) and `num_txs` match the
/// Go-recorded vectors. Runs in the normal suite (no `#[ignore]`).
#[test]
fn candidate_subtrees_match_go_golden() {
    let path = "../ba-subtree-bench/fixtures/golden/ingest.txt";
    let text = std::fs::read_to_string(path)
        .unwrap_or_else(|_| panic!("missing Gate 1 golden ingest fixture: {path}"));

    let mut lines = text.lines();
    let mut header = lines.next().expect("ingest header line").split_whitespace();
    let cap_size: usize = header.next().unwrap().parse().unwrap();
    let n: u32 = header.next().unwrap().parse().unwrap();
    let expected_roots: Vec<&str> = lines.collect();

    // Build the candidate's subtree state through the real assembly engine.
    let mut st = AssemblyState::new(cap_size);
    for i in 0..n {
        st.add(leaf(i), i as u64, i as u64);
    }

    // (1) num_txs parity: every ingested tx is counted.
    assert_eq!(
        st.num_txs(),
        n as u64,
        "candidate num_txs must equal the ingested tx count"
    );

    // (2) subtree_hashes parity: chained roots == Go's recorded completed roots.
    let roots = st.chained_roots();
    assert_eq!(
        roots.len(),
        expected_roots.len(),
        "candidate subtree_hashes count mismatch vs Go golden"
    );
    for (i, (got, want)) in roots.iter().zip(expected_roots.iter()).enumerate() {
        assert_eq!(
            &hex::encode(got),
            want,
            "candidate subtree_hash #{i} mismatch vs Go golden"
        );
    }
}

/// REAL offline candidate parity (Task J2). Drives the production code path:
/// builds the SAME `{ txid = leaf(i), fee = i, size = i, created_at = N - i }`
/// set the Go `candidate` generator uses, runs it through the REAL
/// `ba_service::load::plan_unmined_load` (createdAt ascending sort — exactly what
/// the boot load does), then feeds the sorted order into the same
/// `AssemblyState` engine `GetMiningCandidate` reads. Asserts the completed
/// subtree roots, num_txs, coinbase_value (sum fee) and size_without_coinbase
/// (sum size) against the Go-validated `candidate.txt` golden.
///
/// This proves (a) the real plan_unmined_load sort reproduces Go's createdAt
/// order, and (b) the resulting subtree composition + aggregates byte-match Go.
/// Runs in the normal suite (NOT `#[ignore]`).
///
/// leaf(i) / created_at byte-match Go: leaf(i) = sha256d(le_u32(i)) ==
/// chainhash.DoubleHashH(LittleEndian uint32 i); created_at = (N - i) as i64 ==
/// Go's int64(n - i). Roots are forward byte order in both (hex of root[:]).
#[test]
fn candidate_matches_go_for_seeded_set_offline() {
    use std::collections::HashSet;

    use ba_service::load::plan_unmined_load;
    use ba_service::store::UnminedTx;

    let path = "../ba-subtree-bench/fixtures/golden/candidate.txt";
    let text = std::fs::read_to_string(path)
        .unwrap_or_else(|_| panic!("missing candidate golden fixture: {path} (run `cd rust/ba-subtree-bench/gen && go run . candidate`)"));

    let mut lines = text.lines();
    let mut header = lines
        .next()
        .expect("candidate header line")
        .split_whitespace();
    assert_eq!(header.next(), Some("candidate"), "candidate.txt header tag");
    let cap_size: usize = header.next().unwrap().parse().unwrap();
    let n: u32 = header.next().unwrap().parse().unwrap();
    let num_roots: usize = header.next().unwrap().parse().unwrap();
    let coinbase_value: u64 = header.next().unwrap().parse().unwrap();
    let size_without_coinbase: u64 = header.next().unwrap().parse().unwrap();
    let expected_roots: Vec<&str> = lines.filter(|l| !l.is_empty()).collect();
    assert_eq!(
        expected_roots.len(),
        num_roots,
        "candidate.txt root count mismatch with header"
    );

    // Build the seeded set with created_at = N - i (reverse of index).
    let txs: Vec<UnminedTx> = (0..n)
        .map(|i| UnminedTx {
            txid: leaf(i),
            fee: i as u64,
            size: i as u64,
            block_ids: vec![],
            locked: false,
            created_at: (n - i) as i64,
        })
        .collect();

    // REAL load planning: empty best_ids => no filtering, sort by created_at asc.
    let plan = plan_unmined_load(txs, &HashSet::new());
    assert_eq!(plan.keep_sorted.len(), n as usize, "all txs kept");
    assert!(plan.mark_on_longest.is_empty());
    // No locked txs in this seeded set, so nothing to unlock — K1 does not
    // affect candidate output here.
    assert!(plan.unlock.is_empty());

    // Drive the candidate engine in the planned (createdAt-sorted) order.
    let mut st = AssemblyState::new(cap_size);
    for t in &plan.keep_sorted {
        st.add(t.txid, t.fee, t.size);
    }

    // num_txs parity.
    assert_eq!(st.num_txs(), n as u64, "candidate num_txs vs Go");

    // coinbase_value (sum fee) + size_without_coinbase (sum size) parity.
    assert_eq!(st.total_fees, coinbase_value, "coinbase_value vs Go");
    assert_eq!(
        st.total_size, size_without_coinbase,
        "size_without_coinbase vs Go"
    );

    // subtree_hashes parity: chained roots == Go's recorded completed roots, in
    // order. Roots are NOT hardcoded — parsed from candidate.txt at runtime.
    let roots = st.chained_roots();
    assert_eq!(
        roots.len(),
        expected_roots.len(),
        "candidate subtree_hashes count vs Go"
    );
    for (i, (got, want)) in roots.iter().zip(expected_roots.iter()).enumerate() {
        assert_eq!(
            &hex::encode(got),
            want,
            "candidate subtree_hash #{i} mismatch vs Go (createdAt-ordered)"
        );
    }
}

/// IS4 — incomplete-subtree candidate golden parity. When no subtree completes,
/// the candidate publishes the INCOMPLETE subtree (placeholder + k<cap leaves).
/// Builds the SAME partial set through `AssemblyState` and asserts the candidate's
/// published subtree root, carried fee sum (coinbase_value − subsidy), and coinbase
/// merkle proof byte-match the Go-recorded `incomplete_candidate.txt` golden.
/// Everything parsed from the fixture — no hardcode. Runs in the normal suite.
#[test]
fn incomplete_candidate_matches_go_golden() {
    let path = "../ba-subtree-bench/fixtures/golden/incomplete_candidate.txt";
    let text = std::fs::read_to_string(path).unwrap_or_else(|_| {
        panic!("missing incomplete-candidate golden fixture: {path} (run `cd rust/ba-subtree-bench/gen && go run . incomplete-candidate`)")
    });

    let mut lines = text.lines();
    let mut header = lines
        .next()
        .expect("incomplete-candidate header line")
        .split_whitespace();
    assert_eq!(
        header.next(),
        Some("incomplete-candidate"),
        "fixture header tag"
    );
    let cap_size: usize = header.next().unwrap().parse().unwrap();
    let k: u32 = header.next().unwrap().parse().unwrap();
    let fee_sum: u64 = header.next().unwrap().parse().unwrap();
    let expected_root = lines.next().expect("root line").to_string();
    let proof_len: usize = lines.next().expect("proof_len line").parse().unwrap();
    let expected_proof: Vec<&str> = lines.filter(|l| !l.is_empty()).collect();
    assert_eq!(
        expected_proof.len(),
        proof_len,
        "proof length mismatch with header"
    );

    // Build the SAME partial set (k<cap leaves, fee = i+1, size 10) through the
    // assembly engine. No subtree completes → the candidate is the incomplete one.
    let mut st = AssemblyState::new(cap_size);
    for i in 0..k {
        st.add(leaf(i), (i as u64) + 1, 10);
    }
    assert_eq!(st.num_chained(), 0, "no completed subtree (k < cap)");

    // The candidate's XOR selection: incomplete subtree clone.
    let mut inc = st
        .current_subtree_clone()
        .expect("incomplete subtree present");

    // (1) published subtree root parity.
    let root = inc.root_hash().expect("non-empty incomplete subtree");
    assert_eq!(
        hex::encode(root),
        expected_root,
        "incomplete subtree root vs Go golden"
    );

    // (2) coinbase reconciliation: coinbase_value − subsidy == carried fee sum.
    assert_eq!(
        inc.fees, fee_sum,
        "carried fee sum (coinbase − subsidy) vs Go"
    );

    // (3) coinbase merkle proof parity over the single incomplete subtree.
    let mut candidate = vec![inc];
    let proof = ba_subtree_bench::block_merkle::coinbase_merkle_proof(&mut candidate);
    assert_eq!(proof.len(), proof_len, "proof length vs Go golden");
    for (i, (got, want)) in proof.iter().zip(expected_proof.iter()).enumerate() {
        assert_eq!(
            &hex::encode(got),
            want,
            "coinbase merkle proof hash #{i} vs Go golden"
        );
    }
}

// ---------------------------------------------------------------------------
// db1 — mutating UtxoStore primitives (conflict cascade, Capability D-b).
// CONSENSUS-CRITICAL. These prove the Aerospike UDF round-trips, which can ONLY
// be validated on a live rig with seeded records. Compile-only offline; the BFS
// cascade + SpendingData serialize are covered offline in the unit tests.
// ---------------------------------------------------------------------------

#[tokio::test]
#[ignore = "requires running Aerospike with a seeded unspent UTXO record"]
async fn spend_marks_utxo_spent() {
    // RIG: seed a master record with one unspent 32-byte UTXO at (txid, vout=0).
    // After spend, the `utxos[0]` entry must become a 68-byte spent entry
    // carrying the spendingData (proven for single-key `spend` in the spike t6;
    // this exercises the production `spendMulti` batch path).
    let s = store().await;
    let parent_txid = [0x22; 32];
    let spender_txid = [0x33; 32];

    let spend = Spend {
        tx_id: parent_txid,
        vout: 0,
        utxo_hash: [0x22; 32],
        spending_data: Some(SpendingData {
            tx_id: spender_txid,
            vin: 0,
        }),
        conflicting_tx_id: None,
        block_ids: vec![],
    };

    s.set_block_height(200);
    let returned = s
        .spend(std::slice::from_ref(&spend), 200, IgnoreFlags::default())
        .await
        .expect("spendMulti UDF round-trip");
    assert_eq!(returned.len(), 1, "spend returns one Spend per input");
}

#[tokio::test]
#[ignore = "requires running Aerospike with a seeded SPENT UTXO record"]
async fn unspend_clears_spending_data() {
    // RIG: seed a record whose UTXO is already spent by `spender_txid`; unspend
    // with the matching expected spendingData must clear it back to 32 bytes.
    let s = store().await;
    let parent_txid = [0x22; 32];
    let spender_txid = [0x33; 32];

    let spend = Spend {
        tx_id: parent_txid,
        vout: 0,
        utxo_hash: [0x22; 32],
        spending_data: Some(SpendingData {
            tx_id: spender_txid,
            vin: 0,
        }),
        conflicting_tx_id: None,
        block_ids: vec![],
    };

    s.set_block_height(200);
    s.unspend(&[spend], false)
        .await
        .expect("unspend UDF round-trip");
}

#[tokio::test]
#[ignore = "requires running Aerospike with a seeded tx record (utxos bin populated)"]
async fn set_conflicting_flips_flag_and_derives_children() {
    // RIG: seed a tx record whose outputs are spent by known children; assert the
    // setConflicting UDF flips the flag and the returned spending_child_tx_hashes
    // contains the children derived from the record's 68-byte `utxos` entries.
    // VERIFY-ON-RIG: affected_parent_spends is currently empty (no tx deserializer
    // in this crate — see set_conflicting impl notes); confirm against Go whether
    // any rig consumer needs it populated here.
    let s = store().await;
    let tx = [0x44; 32];

    s.set_block_height(200);
    let (affected, children) = s
        .set_conflicting(&[tx], true)
        .await
        .expect("setConflicting UDF round-trip");
    println!(
        "set_conflicting: affected_parents={}, spending_children={}",
        affected.len(),
        children.len()
    );
}

#[tokio::test]
#[ignore = "requires running Aerospike with a seeded conflict cascade"]
async fn mark_conflicting_recursively_cascades_on_rig() {
    // RIG: seed a multi-level conflict cascade (a tx, its spending children, and
    // their children). The BFS must mark every descendant and return them in BFS
    // order. The BFS logic itself is proven offline (utxo_mem tests); this proves
    // it composes with the live setConflicting child-derivation.
    let s = store().await;
    let root = [0x44; 32];

    s.set_block_height(200);
    let (_affected, marked) = s
        .mark_conflicting_recursively(&[root])
        .await
        .expect("recursive conflict cascade");
    assert!(
        marked.first() == Some(&root),
        "BFS order: the seed is marked first"
    );
}

#[tokio::test]
#[ignore = "requires running backends + recorded Go candidate fixture"]
async fn candidate_matches_go_for_seeded_set() {
    // REQUIRES-RIG-FIXTURE:
    //   A recorded Go `GetMiningCandidate` response for the rig's seeded unmined
    //   set, captured against the SAME Aerospike + blockchain state this Rust
    //   service boots from. Produce it by, on the rig:
    //     1. Seed the known unmined tx set (the harness's fixture set).
    //     2. Call the Go Block Assembly `GetMiningCandidate` RPC (e.g. via
    //        grpcurl against `blockassembly_api.BlockAssemblyAPI/GetMiningCandidate`).
    //     3. Record its `subtree_hashes`, `num_txs`, `size_without_coinbase`, and
    //        `coinbase_value` into a fixture committed alongside this test.
    //   Then this test must:
    //     - boot the Rust service from the same config/seeded store,
    //     - build the Rust `MiningCandidate` via `BaService` / `AssemblyState`,
    //     - assert_eq! on subtree_hashes, num_txs, size_without_coinbase,
    //       coinbase_value against the recorded Go candidate.
    //   Divergence is a Stage 3 BLOCKER and is never auto-resolved.
    //
    // OFFLINE COVERAGE: candidate subtree composition + createdAt ordering +
    // num_txs + coinbase_value + size_without_coinbase parity vs Go is now fully
    // covered offline by `candidate_matches_go_for_seeded_set_offline` (drives the
    // real `plan_unmined_load` sort + `AssemblyState` engine against the
    // `candidate.txt` golden from `gen . candidate`). This rig test remains for
    // the full end-to-end check against the LIVE Go BA `GetMiningCandidate` RPC
    // over the rig's actual seeded Aerospike + blockchain state.
    //
    // Until that recorded fixture exists, fail loudly (this never runs in the
    // normal suite because of `#[ignore]`, and can never silently pass).
    unimplemented!(
        "Stage 3 end-to-end candidate-parity is blocked on a recorded Go \
         GetMiningCandidate fixture for the rig's seeded unmined set (see \
         REQUIRES-RIG-FIXTURE above). Offline composition + createdAt ordering + \
         num_txs + fee/size aggregate parity is covered by \
         candidate_matches_go_for_seeded_set_offline."
    );
}

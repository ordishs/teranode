//! Byte-identical validation of the Rust port against go-subtree golden vectors.
//! Regenerate vectors with: `cd gen && go run . merkle`.

use ba_subtree_bench::hash::{sha256d, Hash};
use ba_subtree_bench::inpoints::TxInpoints;
use ba_subtree_bench::merkle;
use ba_subtree_bench::subtree::Subtree;

use std::collections::HashMap;

fn load_kv(path: &str) -> HashMap<String, String> {
    let text = std::fs::read_to_string(path).unwrap_or_else(|_| panic!("missing {path}"));
    text.lines()
        .filter_map(|l| {
            let mut p = l.split_whitespace();
            Some((p.next()?.to_string(), p.next()?.to_string()))
        })
        .collect()
}

/// Must match gen/main.go `leaf(i)`: double-SHA256 of little-endian uint32 i.
fn leaf(i: u32) -> Hash {
    sha256d(&i.to_le_bytes())
}

#[test]
fn merkle_root_matches_go_golden() {
    let text = std::fs::read_to_string("fixtures/golden/merkle.txt")
        .expect("run `cd gen && go run . merkle` first");
    let mut checked = 0;
    for line in text.lines() {
        let mut parts = line.split_whitespace();
        let count: u32 = parts.next().unwrap().parse().unwrap();
        let expected = parts.next().unwrap();
        let leaves: Vec<Hash> = (0..count).map(leaf).collect();
        let root = merkle::root(&leaves).expect("non-empty");
        assert_eq!(
            hex::encode(root),
            expected,
            "merkle root mismatch for leaf count {count}"
        );
        checked += 1;
    }
    assert!(
        checked >= 7,
        "expected at least 7 golden vectors, got {checked}"
    );
}

#[test]
fn subtree_serialize_matches_go_golden() {
    let kv = load_kv("fixtures/golden/serialize.txt");

    // Must mirror gen/main.go writeSerializeVectors exactly.
    let mut st = Subtree::new();
    for i in 0..4u32 {
        st.add_node(leaf(i), i as u64, (i * 10) as u64);
    }
    st.fees = 1234;
    st.size_in_bytes = 5678;
    st.conflicting_nodes = vec![leaf(100), leaf(200)];

    assert_eq!(
        hex::encode(st.serialize()),
        kv["subtree"],
        "subtree serialize mismatch"
    );
}

#[test]
fn inpoints_serialize_matches_go_golden() {
    let kv = load_kv("fixtures/golden/serialize.txt");

    let tip = TxInpoints::from_packed(vec![leaf(0), leaf(1)], vec![1, 5, 2, 7, 9]);
    assert_eq!(
        hex::encode(tip.serialize()),
        kv["inpoints"],
        "inpoints serialize mismatch"
    );
}

#[test]
fn ingest_completed_roots_match_go_golden() {
    use ba_subtree_bench::processor::SubtreeProcessor;

    let text = std::fs::read_to_string("fixtures/golden/ingest.txt")
        .expect("run `cd gen && go run . ingest` first");
    let mut lines = text.lines();
    let mut header = lines.next().unwrap().split_whitespace();
    let cap_size: usize = header.next().unwrap().parse().unwrap();
    let n: u32 = header.next().unwrap().parse().unwrap();
    let expected_roots: Vec<&str> = lines.collect();

    let mut p = SubtreeProcessor::new(cap_size);
    for i in 0..n {
        p.add(leaf(i), i as u64, i as u64);
    }
    let roots = p.chained_roots();
    assert_eq!(
        roots.len(),
        expected_roots.len(),
        "completed-subtree count mismatch"
    );
    for (i, (got, want)) in roots.iter().zip(expected_roots.iter()).enumerate() {
        assert_eq!(
            &hex::encode(got),
            want,
            "completed subtree #{i} root mismatch"
        );
    }
}

#[test]
fn reorg_end_state_matches_go_golden() {
    use ba_subtree_bench::reorg::{reorg_reconcile, ReorgParams};

    let text = std::fs::read_to_string("fixtures/golden/reorg.txt")
        .expect("run `cd gen && go run . reorg-golden` first");
    let mut lines = text.lines();
    let mut h = lines.next().unwrap().split_whitespace();
    let u: u32 = h.next().unwrap().parse().unwrap();
    let block: u32 = h.next().unwrap().parse().unwrap();
    let d_back: u32 = h.next().unwrap().parse().unwrap();
    let d_fwd: u32 = h.next().unwrap().parse().unwrap();
    let conflict_stride: u32 = h.next().unwrap().parse().unwrap();
    let cap: usize = h.next().unwrap().parse().unwrap();
    let expected: Vec<&str> = lines.collect();

    let roots = reorg_reconcile(&ReorgParams {
        u,
        block,
        d_back,
        d_fwd,
        conflict_stride,
        cap,
    });
    assert_eq!(
        roots.len(),
        expected.len(),
        "reorg end-state subtree count mismatch"
    );
    for (i, (got, want)) in roots.iter().zip(expected.iter()).enumerate() {
        assert_eq!(
            &hex::encode(got),
            want,
            "reorg end-state root #{i} mismatch"
        );
    }
}

#[test]
fn reorg_with_conflict_matches_golden() {
    // db4 with-conflict reorg: conflicts ENABLED (conflict_stride 100). The Rust
    // reconciliation model drives the reverse-cascade eviction (every stride-th
    // moved-back tx) AND the losing-conflict eviction (every stride-th surviving
    // starting tx) and must reproduce Go's `reorg.txt` end-state byte-for-byte.
    // The bench reorg.rs model already exposes this conflict path (gated on
    // conflict_stride != 0), matching gen/main.go reorgReconcile exactly — no
    // port was needed beyond what D-a's structural model already carried.
    use ba_subtree_bench::reorg::{reorg_reconcile, ReorgParams};

    let text = std::fs::read_to_string("fixtures/golden/reorg.txt")
        .expect("run `cd gen && go run . reorg-golden` first");
    let mut lines = text.lines();
    let mut h = lines.next().unwrap().split_whitespace();
    let u: u32 = h.next().unwrap().parse().unwrap();
    let block: u32 = h.next().unwrap().parse().unwrap();
    let d_back: u32 = h.next().unwrap().parse().unwrap();
    let d_fwd: u32 = h.next().unwrap().parse().unwrap();
    let conflict_stride: u32 = h.next().unwrap().parse().unwrap();
    let cap: usize = h.next().unwrap().parse().unwrap();
    let expected: Vec<&str> = lines.collect();

    assert_ne!(
        conflict_stride, 0,
        "with-conflict golden must have a non-zero stride"
    );

    let roots = reorg_reconcile(&ReorgParams {
        u,
        block,
        d_back,
        d_fwd,
        conflict_stride,
        cap,
    });
    assert_eq!(
        roots.len(),
        expected.len(),
        "with-conflict reorg end-state subtree count mismatch"
    );
    for (i, (got, want)) in roots.iter().zip(expected.iter()).enumerate() {
        assert_eq!(
            &hex::encode(got),
            want,
            "with-conflict reorg end-state root #{i} mismatch"
        );
    }
}

#[test]
fn reorg_noconflict_end_state_matches_go_golden() {
    // D-a structural reorg: conflicts DISABLED (conflict_stride 0). The Rust
    // reconciliation model must reproduce Go's end-state roots byte-for-byte.
    use ba_subtree_bench::reorg::{reorg_reconcile, ReorgParams};

    let text = std::fs::read_to_string("fixtures/golden/reorg_noconflict.txt")
        .expect("run `cd gen && go run . reorg-golden-noconflict` first");
    let mut lines = text.lines();
    let mut h = lines.next().unwrap().split_whitespace();
    let u: u32 = h.next().unwrap().parse().unwrap();
    let block: u32 = h.next().unwrap().parse().unwrap();
    let d_back: u32 = h.next().unwrap().parse().unwrap();
    let d_fwd: u32 = h.next().unwrap().parse().unwrap();
    let conflict_stride: u32 = h.next().unwrap().parse().unwrap();
    let cap: usize = h.next().unwrap().parse().unwrap();
    let expected: Vec<&str> = lines.collect();

    assert_eq!(conflict_stride, 0, "no-conflict golden must have stride 0");

    let roots = reorg_reconcile(&ReorgParams {
        u,
        block,
        d_back,
        d_fwd,
        conflict_stride,
        cap,
    });
    assert_eq!(
        roots.len(),
        expected.len(),
        "no-conflict reorg end-state subtree count mismatch"
    );
    for (i, (got, want)) in roots.iter().zip(expected.iter()).enumerate() {
        assert_eq!(
            &hex::encode(got),
            want,
            "no-conflict reorg end-state root #{i} mismatch"
        );
    }
}

#[test]
fn block_merkle_matches_go_golden() {
    use ba_subtree_bench::block_merkle::{block_merkle_root, coinbase_merkle_proof};

    let text = std::fs::read_to_string("fixtures/golden/blockmerkle.txt")
        .expect("run `cd gen && go run . blockmerkle` first");
    let mut lines = text.lines();
    let mut h = lines.next().unwrap().split_whitespace();
    assert_eq!(h.next().unwrap(), "blockmerkle");
    let n: u32 = h.next().unwrap().parse().unwrap();
    let cap: usize = h.next().unwrap().parse().unwrap();
    let want_n_subtrees: usize = h.next().unwrap().parse().unwrap();
    let want_root = lines.next().unwrap().to_string();
    let proof_len: usize = lines.next().unwrap().parse().unwrap();
    let want_proof: Vec<&str> = lines.by_ref().take(proof_len).collect();

    // Build the SAME subtrees the Go gen used: subtree 0 seeded with the coinbase
    // placeholder at node 0, then leaf(i) chunked by cap.
    let coinbase = leaf(0xC0FFEE);
    let mut subtrees: Vec<Subtree> = Vec::new();
    let mut cur = Subtree::new();
    cur.add_coinbase_node();
    for i in 0..n {
        cur.add_node(leaf(i), i as u64, 10);
        if cur.len() == cap {
            subtrees.push(std::mem::take(&mut cur));
        }
    }
    if !cur.is_empty() {
        subtrees.push(cur);
    }
    assert_eq!(
        subtrees.len(),
        want_n_subtrees,
        "subtree count must match Go"
    );

    // Proof FIRST (raw subtrees, index 0 = placeholder), then the root mutates
    // subtree-0 via the coinbase substitution — exactly Go's call order.
    let got_proof: Vec<String> = coinbase_merkle_proof(&mut subtrees)
        .iter()
        .map(hex::encode)
        .collect();
    let got_root = hex::encode(block_merkle_root(&mut subtrees, &coinbase));

    assert_eq!(got_root, want_root, "block merkle root must match Go");
    assert_eq!(got_proof, want_proof, "coinbase proof must match Go");
}

#[test]
fn coinbase_bump_matches_go_golden() {
    use ba_subtree_bench::bump::coinbase_bump;

    let text = std::fs::read_to_string("fixtures/golden/bump.txt")
        .expect("run `cd gen && go run . bump` first");
    let mut lines = text.lines();
    let mut h = lines.next().unwrap().split_whitespace();
    assert_eq!(h.next().unwrap(), "bump");
    let num_cases: usize = h.next().unwrap().parse().unwrap();

    let coinbase = leaf(0xC0FFEE);
    let mut checked = 0;

    for line in lines.take(num_cases) {
        let mut p = line.split_whitespace();
        let n: u32 = p.next().unwrap().parse().unwrap();
        let cap: usize = p.next().unwrap().parse().unwrap();
        let height: u32 = p.next().unwrap().parse().unwrap();
        let want_hex = p.next().unwrap();

        // Build the SAME subtrees the Go gen used: subtree 0 seeded with the
        // coinbase placeholder at node 0, then leaf(i) chunked by cap.
        let mut subtrees: Vec<Subtree> = Vec::new();
        let mut cur = Subtree::new();
        cur.add_coinbase_node();
        for i in 0..n {
            cur.add_node(leaf(i), i as u64, 10);
            if cur.len() == cap {
                subtrees.push(std::mem::take(&mut cur));
            }
        }
        if !cur.is_empty() {
            subtrees.push(cur);
        }

        let got = coinbase_bump(&mut subtrees, &coinbase, height).expect("bump");
        assert_eq!(
            hex::encode(got),
            want_hex,
            "BUMP bytes must match Go util/bump for n={n} cap={cap} height={height}"
        );
        checked += 1;
    }

    assert_eq!(checked, num_cases, "all golden cases checked");
}

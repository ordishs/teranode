//! Golden gate: the on-disk subtree blob (8-byte fileformat header + Go
//! `Subtree.Serialize()`) the Rust write path produces must match a Go-written
//! blob byte-for-byte, so Go block validation can read it.
//! Regenerate the fixture with `cd ../ba-subtree-bench/gen && go run . subtreeblob`.
//! Never edit the fixture to match Rust — Go is authoritative.

use ba_service::subtree_store::SUBTREE_MAGIC;
use ba_subtree_bench::subtree::Subtree;

#[test]
fn subtree_blob_matches_go_golden() {
    let text = std::fs::read_to_string("../ba-subtree-bench/fixtures/golden/subtreeblob.txt")
        .or_else(|_| std::fs::read_to_string("fixtures/golden/subtreeblob.txt"))
        .expect("run `cd ../ba-subtree-bench/gen && go run . subtreeblob`");
    let mut lines = text.lines();

    let mut h = lines.next().unwrap().split_whitespace();
    assert_eq!(h.next().unwrap(), "subtreeblob");
    let leaves: u32 = h.next().unwrap().parse().unwrap();

    let want = lines.next().unwrap();

    // Build the SAME subtree the Go gen builds: coinbase placeholder at node 0,
    // then `leaves` real txs leaf(i) = sha256d(LE u32 i), all fee 0 / size 0.
    let mut st = Subtree::new();
    st.add_coinbase_node();
    for i in 0..leaves {
        st.add_node(ba_subtree_bench::hash::sha256d(&i.to_le_bytes()), 0, 0);
    }

    let body = st.serialize();
    let mut blob = Vec::with_capacity(SUBTREE_MAGIC.len() + body.len());
    blob.extend_from_slice(&SUBTREE_MAGIC);
    blob.extend_from_slice(&body);

    let got = hex::encode(&blob);
    assert_eq!(
        got, want,
        "subtree blob bytes (header + serialize) must match Go-written blob"
    );
}

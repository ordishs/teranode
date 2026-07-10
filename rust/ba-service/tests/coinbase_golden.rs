//! Golden gate: create_coinbase must match Go model.CreateCoinbase byte-for-byte.
//! Regenerate the fixture with `cd ../ba-subtree-bench/gen && go run . coinbase`.
//! Never edit the fixture to match Rust — Go is authoritative.

#[test]
fn coinbase_matches_go_golden() {
    let text = std::fs::read_to_string("../ba-subtree-bench/fixtures/golden/coinbase.txt")
        .or_else(|_| std::fs::read_to_string("fixtures/golden/coinbase.txt"))
        .expect("run `cd ../ba-subtree-bench/gen && go run . coinbase`");
    let mut lines = text.lines();

    let mut h = lines.next().unwrap().split_whitespace();
    assert_eq!(h.next().unwrap(), "coinbase");
    let height: u32 = h.next().unwrap().parse().unwrap();
    let value: u64 = h.next().unwrap().parse().unwrap();
    let extranonce_hex = h.next().unwrap();
    let mut extranonce = [0u8; 12];
    hex::decode_to_slice(extranonce_hex, &mut extranonce).unwrap();

    let want = lines.next().unwrap();
    let got = hex::encode(
        ba_service::coinbase::create_coinbase(
            height,
            value,
            b"rust",
            &["1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2".to_string()],
            extranonce,
        )
        .unwrap(),
    );
    assert_eq!(got, want, "coinbase bytes must match Go CreateCoinbase");
}

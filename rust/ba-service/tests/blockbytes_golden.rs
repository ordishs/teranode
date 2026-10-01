//! Golden gate: block_bytes must match Go model.Block.Bytes() byte-for-byte.
//! Regenerate with `cd ../ba-subtree-bench/gen && go run . blockbytes`.
//! Never edit the fixture to match Rust — Go is authoritative.

fn dash_none(s: &str) -> Vec<u8> {
    if s == "-" {
        Vec::new()
    } else {
        hex::decode(s).unwrap()
    }
}

#[test]
fn block_bytes_matches_go_golden() {
    let text = std::fs::read_to_string("../ba-subtree-bench/fixtures/golden/blockbytes.txt")
        .expect("run `cd ../ba-subtree-bench/gen && go run . blockbytes`");
    let mut lines = text.lines();

    let mut h = lines.next().unwrap().split_whitespace();
    assert_eq!(h.next().unwrap(), "blockbytes");
    let n: usize = h.next().unwrap().parse().unwrap();

    let mut seen = 0;
    for line in lines {
        let f: Vec<&str> = line.split_whitespace().collect();
        assert_eq!(f.len(), 8, "bad fixture line: {line}");

        let mut header = [0u8; 80];
        hex::decode_to_slice(f[0], &mut header).unwrap();
        let tx_count: u64 = f[1].parse().unwrap();
        let size: u64 = f[2].parse().unwrap();
        let height: u32 = f[3].parse().unwrap();
        let bump = dash_none(f[4]);
        let coinbase = hex::decode(f[5]).unwrap();
        let subtrees: Vec<[u8; 32]> = if f[6] == "-" {
            Vec::new()
        } else {
            f[6].split(',')
                .map(|s| {
                    let mut a = [0u8; 32];
                    hex::decode_to_slice(s, &mut a).unwrap();
                    a
                })
                .collect()
        };
        let want = f[7];

        let got = hex::encode(ba_service::block::block_bytes(
            &header, tx_count, size, &subtrees, &coinbase, height, &bump,
        ));
        assert_eq!(got, want, "block bytes must match Go Block.Bytes()");
        seen += 1;
    }
    assert_eq!(seen, n);
}

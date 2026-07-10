//! Consensus gate: the Rust tx deserializer + UTXO hash must match go-bt
//! byte-for-byte. Go independently computes the expected serialization, raw
//! txid, and per-input UTXO hash (via the util/utxo_hash.go formula); Rust only
//! parses the extended bytes and asserts equality against those fixture values.
//! Regenerate vectors with: `cd gen && go run . txhash`.

use ba_subtree_bench::tx::Tx;
use serde::Deserialize;

#[derive(Deserialize)]
struct TxHashInput {
    utxo_hash_raw_hex: String,
}

#[derive(Deserialize)]
struct TxHashVector {
    extended_hex: String,
    standard_hex: String,
    txid_raw_hex: String,
    inputs: Vec<TxHashInput>,
}

#[test]
fn tx_deserializer_and_utxo_hash_match_go_golden() {
    let path = concat!(env!("CARGO_MANIFEST_DIR"), "/fixtures/golden/txhash.json");
    let text = std::fs::read_to_string(path)
        .unwrap_or_else(|_| panic!("run `cd gen && go run . txhash` first ({path})"));
    let vec: TxHashVector = serde_json::from_str(&text).expect("parse txhash.json");

    let extended = hex::decode(&vec.extended_hex).expect("decode extended_hex");
    let tx = Tx::from_bytes(&extended).expect("parse extended tx");

    assert_eq!(
        hex::encode(tx.standard_bytes()),
        vec.standard_hex,
        "standard serialization mismatch"
    );
    assert_eq!(
        hex::encode(tx.txid()),
        vec.txid_raw_hex,
        "raw txid mismatch"
    );
    assert_eq!(tx.inputs.len(), vec.inputs.len(), "input count mismatch");

    for (i, expected) in vec.inputs.iter().enumerate() {
        let got = tx
            .utxo_hash_from_input(i)
            .unwrap_or_else(|e| panic!("utxo_hash_from_input({i}) errored: {e:?}"));
        assert_eq!(
            hex::encode(got),
            expected.utxo_hash_raw_hex,
            "utxo hash mismatch for input {i}"
        );
    }
}

//! Consensus gate: Rust `spends_for_tx` must match Go `spendsForTx`
//! (stores/utxo/process_conflicting.go:612-630) byte-for-byte. Go independently
//! computes, per input, the prev outpoint, the per-input UTXO hash (via the
//! util/utxo_hash.go formula), and the spender txid; Rust parses the SAME
//! extended tx bytes and asserts field-equality against those fixture values
//! (raw hex, no hardcode). Regenerate with: `cd gen && go run . spendsfortx`.

use ba_service::process_conflicting::spends_for_tx;
use ba_subtree_bench::tx::Tx;
use serde::Deserialize;

#[derive(Deserialize)]
struct SpendRecord {
    tx_id: String,
    vout: u32,
    utxo_hash: String,
    spending_tx_id: String,
    vin: u32,
}

#[derive(Deserialize)]
struct SpendsForTxVector {
    extended_hex: String,
    spends: Vec<SpendRecord>,
}

#[test]
fn spends_for_tx_matches_go_golden() {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../ba-subtree-bench/fixtures/golden/spendsfortx.json"
    );
    let text = std::fs::read_to_string(path)
        .unwrap_or_else(|_| panic!("run `cd gen && go run . spendsfortx` first ({path})"));
    let vec: SpendsForTxVector = serde_json::from_str(&text).expect("parse spendsfortx.json");

    let extended = hex::decode(&vec.extended_hex).expect("decode extended_hex");
    let tx = Tx::from_bytes(&extended).expect("parse extended tx");

    let spends = spends_for_tx(&tx).expect("spends_for_tx");

    assert_eq!(spends.len(), vec.spends.len(), "spend count mismatch vs Go");

    for (i, expected) in vec.spends.iter().enumerate() {
        let got = &spends[i];
        assert_eq!(expected.vin as usize, i, "fixture vin out of order at {i}");

        assert_eq!(
            hex::encode(got.tx_id),
            expected.tx_id,
            "tx_id mismatch at input {i}"
        );
        assert_eq!(got.vout, expected.vout, "vout mismatch at input {i}");
        assert_eq!(
            hex::encode(got.utxo_hash),
            expected.utxo_hash,
            "utxo_hash mismatch at input {i}"
        );

        let sd = got
            .spending_data
            .as_ref()
            .unwrap_or_else(|| panic!("spending_data missing at input {i}"));
        assert_eq!(
            hex::encode(sd.tx_id),
            expected.spending_tx_id,
            "spending_tx_id mismatch at input {i}"
        );
        assert_eq!(sd.vin, expected.vin, "vin mismatch at input {i}");
    }
}

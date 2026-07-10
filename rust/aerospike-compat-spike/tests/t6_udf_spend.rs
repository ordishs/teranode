//! T6 — the make-or-break test: invoke the production `spend` Lua UDF via the
//! Rust client against a Go-seeded record, and verify the UTXO is marked spent
//! (32-byte entry -> 68-byte entry with spending data). Uses single-key
//! execute_udf, which shares the arg/response encoding with the batch UDF path.

mod common;

use aerospike::{Bins, Key, ReadPolicy, Value, WritePolicy};
use aerospike_compat_spike::keys::calculate_key_source_internal;
use common::{register_udf, seed, start_aerospike, NAMESPACE, SET, UDF_MODULE};

// 36-byte spending data: spendTxID_LE(32) || vin_LE(4).
fn spending_data(txid: [u8; 32], vin: u32) -> Vec<u8> {
    let mut v = Vec::with_capacity(36);
    v.extend_from_slice(&txid);
    v.extend_from_slice(&vin.to_le_bytes());
    v
}

#[tokio::test]
async fn t6_spend_udf_marks_utxo_spent() {
    let (_container, client, addr) = start_aerospike().await;
    register_udf(&client).await;

    // Seed a master record with one unspent 32-byte utxo (hash 0x22..).
    let hash_hex = "2222222222222222222222222222222222222222222222222222222222222222";
    seed(&addr, &["-schema", "-hash", hash_hex]);

    let hash = [0x22u8; 32];
    let key = Key::new(NAMESPACE, SET, Value::Blob(calculate_key_source_internal(&hash, 0))).unwrap();

    let spend_txid = [0x33u8; 32];
    // spend(rec, offset, utxoHash, spendingData, ignoreConflicting, ignoreLocked,
    //       currentBlockHeight, blockHeightRetention)
    let args = vec![
        Value::Int(0),
        Value::Blob(vec![0x22u8; 32]),
        Value::Blob(spending_data(spend_txid, 0)),
        Value::Bool(false),
        Value::Bool(false),
        Value::Int(200),
        Value::Int(288),
    ];

    let resp = client
        .execute_udf(&WritePolicy::default(), &key, UDF_MODULE, "spend", Some(&args))
        .await
        .expect("spend UDF must execute");
    println!("spend UDF response: {resp:?}");

    // Re-read; utxos[0] must now be a 68-byte spent entry carrying spend_txid.
    let after = client
        .get(&ReadPolicy::default(), &key, Bins::All)
        .await
        .unwrap();
    match after.bins.get("utxos") {
        Some(Value::List(items)) => match &items[0] {
            Value::Blob(b) => {
                assert_eq!(b.len(), 68, "spent utxo entry must be 68 bytes, got {}", b.len());
                assert_eq!(&b[32..64], &spend_txid, "spending txid mismatch");
            }
            other => panic!("utxo entry not a blob: {other:?}"),
        },
        other => panic!("utxos bin not a list: {other:?}"),
    }
}

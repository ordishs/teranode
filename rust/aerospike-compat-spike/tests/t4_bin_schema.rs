//! T4 — bin-schema encode/decode equivalence: the Rust client must decode the
//! mixed bin types of a real UTXO record (int / blob / list-of-blobs /
//! list-of-ints) identically to how the Go client wrote them.

mod common;

use aerospike::{Bins, Key, ReadPolicy, Value};
use aerospike_compat_spike::keys::calculate_key_source_internal;
use common::{seed, start_aerospike, NAMESPACE, SET};

#[tokio::test]
async fn t4_rust_decodes_utxo_schema() {
    let (_container, client, addr) = start_aerospike().await;

    let hash_hex = "2222222222222222222222222222222222222222222222222222222222222222";
    seed(&addr, &["-schema", "-hash", hash_hex]);

    let hash = [0x22u8; 32];
    let key = Key::new(NAMESPACE, SET, Value::Blob(calculate_key_source_internal(&hash, 0))).unwrap();
    let rec = client
        .get(&ReadPolicy::default(), &key, Bins::All)
        .await
        .expect("schema record must be readable");

    // Scalars (Aerospike stores all integers as 64-bit).
    assert_eq!(rec.bins.get("fee"), Some(&Value::Int(1000)));
    assert_eq!(rec.bins.get("totalUtxos"), Some(&Value::Int(1)));

    // utxos: list with one unspent 32-byte blob entry.
    match rec.bins.get("utxos") {
        Some(Value::List(items)) => {
            assert_eq!(items.len(), 1, "expected one utxo entry");
            match &items[0] {
                Value::Blob(b) => assert_eq!(b.len(), 32, "unspent utxo entry must be 32 bytes"),
                other => panic!("utxo entry not a blob: {other:?}"),
            }
        }
        other => panic!("utxos bin not a list: {other:?}"),
    }

    // blockIDs: list of ints, order preserved.
    match rec.bins.get("blockIDs") {
        Some(Value::List(items)) => {
            assert_eq!(items, &vec![Value::Int(123), Value::Int(456)]);
        }
        other => panic!("blockIDs not a list: {other:?}"),
    }
}

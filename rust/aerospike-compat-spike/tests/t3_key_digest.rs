//! T3 — key-digest compatibility: a record written by the Go client must be
//! found by the Rust client using the same key-source bytes. This proves the two
//! clients compute identical record digests (the highest-value single test).

mod common;

use aerospike::{Bins, Client, Key, ReadPolicy, Value};
use aerospike_compat_spike::keys::calculate_key_source_internal;
use common::{seed, start_aerospike, NAMESPACE, SET};

async fn read_marker(client: &Client, num: u32) -> String {
    let hash = [0x11u8; 32];
    let ks = calculate_key_source_internal(&hash, num);
    let key = Key::new(NAMESPACE, SET, Value::Blob(ks)).unwrap();
    let rec = client
        .get(&ReadPolicy::default(), &key, Bins::All)
        .await
        .expect("record written by Go must be found by Rust (digest match)");
    match rec.bins.get("marker") {
        Some(Value::String(s)) => s.clone(),
        other => panic!("unexpected marker bin: {other:?}"),
    }
}

#[tokio::test]
async fn t3_rust_reads_go_written_records() {
    let (_container, client, addr) = start_aerospike().await;

    let hash_hex = "1111111111111111111111111111111111111111111111111111111111111111";
    seed(&addr, &["-hash", hash_hex, "-num", "0"]); // master record (32-byte key)
    seed(&addr, &["-hash", hash_hex, "-num", "1"]); // pagination record (36-byte key)

    assert_eq!(read_marker(&client, 0).await, "from-go"); // 32-byte blob key
    assert_eq!(read_marker(&client, 1).await, "from-go"); // 36-byte blob key
}

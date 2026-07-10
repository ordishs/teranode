//! T1 — connectivity: does the upstream Rust client speak to server 8.0?
//! Proven by an unambiguous put -> get round-trip.

mod common;

use aerospike::{Bin, Bins, Key, ReadPolicy, Value, WritePolicy};
use common::{start_aerospike, NAMESPACE, SET};

#[tokio::test]
async fn t1_connects_and_roundtrips() {
    let (_container, client, _addr) = start_aerospike().await;

    let key = Key::new(NAMESPACE, SET, Value::Blob(vec![0xEEu8; 32])).unwrap();
    client
        .put(
            &WritePolicy::default(),
            &key,
            &[Bin::new("probe".to_string(), Value::Int(7))],
        )
        .await
        .expect("put must succeed against aerospike-server:8.0");

    let rec = client
        .get(&ReadPolicy::default(), &key, Bins::All)
        .await
        .expect("get must succeed");
    assert_eq!(rec.bins.get("probe"), Some(&Value::Int(7)));
}

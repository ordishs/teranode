//! T8 — batch read (the BatchDecorate access pattern): fetch a selected set of
//! bins for a key via the batch API and confirm only those bins return.

mod common;

use aerospike::{BatchOperation, BatchPolicy, BatchReadPolicy, Bins, Key, Value};
use aerospike_compat_spike::keys::calculate_key_source_internal;
use common::{seed, start_aerospike, NAMESPACE, SET};

#[tokio::test]
async fn t8_batch_read_selected_bins() {
    let (_container, client, addr) = start_aerospike().await;

    let hash_hex = "2222222222222222222222222222222222222222222222222222222222222222";
    seed(&addr, &["-schema", "-hash", hash_hex]);

    let hash = [0x22u8; 32];
    let key = Key::new(NAMESPACE, SET, Value::Blob(calculate_key_source_internal(&hash, 0))).unwrap();

    // BatchDecorate fetches a selected bin set, e.g. [fee, sizeInBytes, blockIDs].
    let selected = Bins::Some(vec!["fee".into(), "sizeInBytes".into(), "blockIDs".into()]);
    let op = BatchOperation::read(&BatchReadPolicy::default(), key, selected);
    let recs = client
        .batch(&BatchPolicy::default(), &[op])
        .await
        .expect("batch read must succeed");

    let rec = recs[0].record.as_ref().expect("record present in batch result");
    assert_eq!(rec.bins.get("fee"), Some(&Value::Int(1000)));
    assert_eq!(rec.bins.get("sizeInBytes"), Some(&Value::Int(256)));
    assert!(rec.bins.get("utxos").is_none(), "unselected bin must be absent");
}

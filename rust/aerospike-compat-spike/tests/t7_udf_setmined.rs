//! T7 — invoke the production `setMined` Lua UDF via the Rust client and verify
//! it appends the block ID to the blockIDs list.

mod common;

use aerospike::{Bins, Key, ReadPolicy, Value, WritePolicy};
use aerospike_compat_spike::keys::calculate_key_source_internal;
use common::{register_udf, seed, start_aerospike, NAMESPACE, SET, UDF_MODULE};

#[tokio::test]
async fn t7_setmined_appends_block_id() {
    let (_container, client, addr) = start_aerospike().await;
    register_udf(&client).await;

    let hash_hex = "2222222222222222222222222222222222222222222222222222222222222222";
    seed(&addr, &["-schema", "-hash", hash_hex]); // blockIDs starts as [123, 456]

    let hash = [0x22u8; 32];
    let key = Key::new(NAMESPACE, SET, Value::Blob(calculate_key_source_internal(&hash, 0))).unwrap();

    // setMined(rec, blockID, blockHeight, subtreeIdx, currentBlockHeight,
    //          blockHeightRetention, onLongestChain, unsetMined)
    let args = vec![
        Value::Int(789),
        Value::Int(100),
        Value::Int(0),
        Value::Int(200),
        Value::Int(288),
        Value::Bool(true),
        Value::Bool(false),
    ];
    let resp = client
        .execute_udf(&WritePolicy::default(), &key, UDF_MODULE, "setMined", Some(&args))
        .await
        .expect("setMined UDF must execute");
    println!("setMined UDF response: {resp:?}");

    let after = client
        .get(&ReadPolicy::default(), &key, Bins::All)
        .await
        .unwrap();
    match after.bins.get("blockIDs") {
        Some(Value::List(items)) => {
            assert!(items.contains(&Value::Int(789)), "blockID 789 must be appended; got {items:?}")
        }
        other => panic!("blockIDs not a list: {other:?}"),
    }
}

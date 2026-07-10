//! T9 (YELLOW-tolerated) — filter expressions + CDT writes. These back the
//! settings-gated optimizations (EnableSpendFilterExpressions etc., default off),
//! so this is informational, not a Gate 0 blocker. Proves the upstream Rust crate
//! can build filter expressions and CDT list operations the store relies on.

mod common;

use aerospike::operations::lists;
use aerospike::{expressions as exp, Bins, Key, ListPolicy, ReadPolicy, Value, WritePolicy};
use aerospike_compat_spike::keys::calculate_key_source_internal;
use common::{seed, start_aerospike, NAMESPACE, SET};

fn key() -> Key {
    let hash = [0x22u8; 32];
    Key::new(NAMESPACE, SET, Value::Blob(calculate_key_source_internal(&hash, 0))).unwrap()
}

async fn block_ids(client: &aerospike::Client, key: &Key) -> Vec<Value> {
    let rec = client.get(&ReadPolicy::default(), key, Bins::All).await.unwrap();
    match rec.bins.get("blockIDs") {
        Some(Value::List(items)) => items.clone(),
        other => panic!("blockIDs not a list: {other:?}"),
    }
}

#[tokio::test]
async fn t9_filter_expression_gates_cdt_append() {
    let (_container, client, addr) = start_aerospike().await;
    seed(&addr, &["-schema", "-hash",
        "2222222222222222222222222222222222222222222222222222222222222222"]);
    let key = key();

    // CDT list append, gated by a PASSING filter (fee bin exists). Mirrors the
    // store's ListAppendWithPolicyOp + FilterExpression pattern.
    let mut wp = WritePolicy::default();
    wp.base_policy.filter_expression = Some(exp::bin_exists("fee".to_string()));
    let op = lists::append(&ListPolicy::default(), "blockIDs", Value::Int(999));
    client.operate(&wp, &key, &[op]).await.expect("expr-gated CDT append (passing filter)");
    assert!(block_ids(&client, &key).await.contains(&Value::Int(999)), "999 must be appended");

    // Now a BLOCKING filter (fee == 1) — fee is 1000, so the record is filtered
    // out and the append must NOT apply.
    let mut wp2 = WritePolicy::default();
    wp2.base_policy.filter_expression =
        Some(exp::eq(exp::int_bin("fee".to_string()), exp::int_val(1)));
    let op2 = lists::append(&ListPolicy::default(), "blockIDs", Value::Int(1000));
    let _ = client.operate(&wp2, &key, &[op2]).await; // expected to be filtered out (Err) or no-op
    assert!(
        !block_ids(&client, &key).await.contains(&Value::Int(1000)),
        "blocking filter must prevent the append"
    );
}

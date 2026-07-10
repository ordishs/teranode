//! T5 — register the production teranode_v59 Lua module (read in place) via the
//! Rust client and confirm registration converges on the server.

mod common;

use common::{register_udf, start_aerospike};

#[tokio::test]
async fn t5_registers_teranode_lua() {
    let (_container, client, _addr) = start_aerospike().await;
    // register_udf reads ../../stores/utxo/aerospike/teranode.lua in place and
    // panics if registration fails or does not converge.
    register_udf(&client).await;
}

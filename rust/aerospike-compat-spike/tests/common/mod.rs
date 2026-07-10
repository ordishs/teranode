//! Shared test harness: ephemeral Aerospike via testcontainers, client connect,
//! UDF registration (reads the production Lua in place), and the Go seeder bridge.
//!
//! Lives in the test tree (not src/lib.rs) because testcontainers/tokio are
//! dev-dependencies and the returned `ContainerAsync` type is test-only.
//!
//! ## Docker connectivity note
//! A single-node Aerospike dev container advertises its *internal* bridge IP
//! (e.g. 192.168.x.y:3000), which is not routable from the host on Docker
//! Desktop. The Rust client rebuilds its node list from that advertised address,
//! so we must (a) pin the host port to 3000:3000 and (b) supply an `ip_map`
//! translating the bridge IP -> 127.0.0.1. (The client's `ip_map` only rewrites
//! the host, not the port, which is why the fixed 3000:3000 mapping is required.)
//! Consequence: tests use a fixed port and MUST run serially
//! (`--test-threads=1`, one test binary at a time).
#![allow(dead_code)] // each test binary uses only part of the harness

use std::collections::HashMap;
use std::process::Command;

use aerospike::{AdminPolicy, Client, ClientPolicy, Task, UDFLang};
use testcontainers::{
    core::{IntoContainerPort, WaitFor},
    runners::AsyncRunner,
    ContainerAsync, GenericImage, ImageExt,
};

pub use aerospike_compat_spike::{LUA_PATH, NAMESPACE, SET, UDF_MODULE};

// The spike's Aerospike service port. NOT 3000 — a developer may already have a
// (production-like) Aerospike on 3000; we set the container's SERVICE_PORT to this
// and pin host==container so the advertised port is reachable after ip_map.
const SERVICE_PORT: u16 = 3500;

/// Starts an ephemeral aerospike-server:8.0 (single-node, CE-mode `test`
/// namespace) on a fixed non-conflicting port, and returns (container, connected
/// client, "host:port" for the seeder). Hold the container for the test's lifetime.
pub async fn start_aerospike() -> (ContainerAsync<GenericImage>, Client, String) {
    let container = GenericImage::new("aerospike/aerospike-server", "8.0")
        // with_wait_for is inherent on GenericImage; call it before the ImageExt
        // methods that yield a ContainerRequest.
        // Aerospike logs to stderr, not stdout.
        .with_wait_for(WaitFor::message_on_stderr(
            "service ready: soon there will be cake!",
        ))
        .with_exposed_port(SERVICE_PORT.tcp())
        // ImageExt methods below convert to ContainerRequest; keep them last.
        .with_env_var("SERVICE_PORT", SERVICE_PORT.to_string())
        .with_mapped_port(SERVICE_PORT, SERVICE_PORT.tcp())
        .start()
        .await
        .expect("aerospike container must start");

    let bridge_ip = container
        .get_bridge_ip_address()
        .await
        .expect("bridge ip")
        .to_string();
    let addr = format!("127.0.0.1:{SERVICE_PORT}");

    // Translate the advertised bridge IP back to localhost so the client can
    // reach the node via the fixed host port.
    let mut ip_map = HashMap::new();
    ip_map.insert(bridge_ip, "127.0.0.1".to_string());
    let policy = ClientPolicy {
        ip_map: Some(ip_map),
        ..ClientPolicy::default()
    };

    // The "service ready" log precedes the cluster fully accepting clients;
    // retry briefly until the node list stabilises.
    let client = connect_with_retry(&policy, &addr).await;
    (container, client, addr)
}

async fn connect_with_retry(policy: &ClientPolicy, addr: &str) -> Client {
    let mut last_err = None;
    for _ in 0..50 {
        match Client::new(policy, &addr.to_string()).await {
            Ok(c) => return c,
            Err(e) => {
                last_err = Some(e);
                tokio::time::sleep(std::time::Duration::from_millis(200)).await;
            }
        }
    }
    panic!("rust client failed to connect after retries: {last_err:?}");
}

/// Registers the production teranode_v59 module, reading the Lua source IN PLACE
/// from the Go tree (read-only — never copied or modified).
pub async fn register_udf(client: &Client) {
    let lua = std::fs::read(LUA_PATH).expect("read teranode.lua in place (read-only)");
    let task = client
        .register_udf(
            &AdminPolicy::default(),
            &lua,
            &format!("{UDF_MODULE}.lua"),
            UDFLang::Lua,
        )
        .await
        .expect("register_udf");
    task.wait_till_complete(None)
        .await
        .expect("udf registration converges");
}

/// Runs the Go seeder against the given "host:port". Extra args are appended
/// verbatim. The seeder writes records with the production BSV aerospike client
/// + key logic, so records are byte-identical to production.
pub fn seed(addr: &str, extra: &[&str]) {
    let (host, port) = addr.split_once(':').expect("host:port");
    let mut args: Vec<String> = vec![
        "run".into(),
        ".".into(),
        "-host".into(),
        host.into(),
        "-port".into(),
        port.into(),
    ];
    args.extend(extra.iter().map(|s| s.to_string()));
    let status = Command::new("go")
        .args(&args)
        .current_dir("seeder")
        .status()
        .expect("go run seeder");
    assert!(status.success(), "seeder failed for args {extra:?}");
}

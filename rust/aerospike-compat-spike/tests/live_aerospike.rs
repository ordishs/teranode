//! Live-rig probe: prove the Rust aerospike client can READ the production UTXO
//! store and WRITE (to a throwaway set — never the real `utxo` set) against the
//! actual BSV-fork server, not a testcontainer.
//!
//! Run on demand (the live rig must be up):
//!   cargo test --test live_aerospike -- --ignored --nocapture
//! Env overrides: AERO_ADDR (127.0.0.1:3000), AERO_NS (utxo-store),
//!                AERO_BRIDGE_IP (192.168.107.5 — advertised IP to remap to localhost).

use std::collections::HashMap;

use aerospike::{Bin, Bins, Client, ClientPolicy, Key, ReadPolicy, Value, WritePolicy};

#[tokio::test]
#[ignore]
async fn live_read_real_set_and_write_throwaway_set() {
    let addr = std::env::var("AERO_ADDR").unwrap_or_else(|_| "127.0.0.1:3000".to_string());
    let ns = std::env::var("AERO_NS").unwrap_or_else(|_| "utxo-store".to_string());

    // The node's advertised address is directly routable here, so no ip_map is
    // needed (this mirrors how AeroUtxoStore connects via cfg.aerospike_hosts).
    let policy = ClientPolicy::default();
    let _ = HashMap::<String, String>::new();

    // Retry briefly — the initial tend can report "cluster in flux" transiently.
    let mut client = None;
    let mut last = String::new();
    for attempt in 0..25 {
        match Client::new(&policy, &addr).await {
            Ok(c) => {
                client = Some(c);
                break;
            }
            Err(e) => {
                last = format!("{e}");
                if attempt == 0 {
                    println!("connect attempt failed (retrying): {last}");
                }
                tokio::time::sleep(std::time::Duration::from_millis(400)).await;
            }
        }
    }
    let client = client.unwrap_or_else(|| panic!("connect to live aerospike failed after retries: {last}"));
    println!("connected to live aerospike at {addr} (ns={ns})");

    // ---- READ the real `utxo` set (safe: a get of an absent key is a full read
    //      round-trip proving the read path works against the BSV-fork server). ----
    let probe_key = Key::new(ns.clone(), "utxo".to_string(), Value::Blob(vec![0xEEu8; 32])).unwrap();
    match client.get(&ReadPolicy::default(), &probe_key, Bins::All).await {
        Ok(_) => println!("READ utxo set: found a record (read OK)"),
        Err(e) => {
            let m = format!("{e}").to_lowercase();
            assert!(
                m.contains("notfound") || m.contains("not found"),
                "READ failed for a non-not-found reason (connectivity/protocol?): {e}"
            );
            println!("READ utxo set: KeyNotFound (read round-trip OK against live BSV-fork server)");
        }
    }

    // ---- WRITE + READ a THROWAWAY set (never the real utxo set). ----
    let tset = "ba_rust_probe";
    let key = Key::new(ns, tset.to_string(), Value::Blob(vec![0x01u8; 32])).unwrap();
    client
        .put(
            &WritePolicy::default(),
            &key,
            &[Bin::new("probe".to_string(), Value::Int(12345))],
        )
        .await
        .expect("write to throwaway set");
    let rec = client
        .get(&ReadPolicy::default(), &key, Bins::All)
        .await
        .expect("read back from throwaway set");
    assert_eq!(rec.bins.get("probe"), Some(&Value::Int(12345)));
    println!("WRITE+READ throwaway set '{tset}': OK (value round-tripped)");

    // Cleanup the probe record (best-effort).
    let _ = client.delete(&WritePolicy::default(), &key).await;
    println!("cleaned up probe record");
}

//! Stage3Config derivation tests. Raw-value resolution (file discovery,
//! settings_local.conf precedence, SETTINGS_CONTEXT fallback, ${} interpolation,
//! env-first) is rustcore's job and is covered by rustcore's own test suite —
//! these tests exercise ba-service's DERIVATION from resolved values via
//! `from_getter`, plus one live end-to-end read through rustcore.

use ba_service::config::Stage3Config;

/// Map-backed getter standing in for rustcore's resolved view.
fn getter(pairs: &'static [(&str, &str)]) -> impl Fn(&str) -> Option<String> {
    move |k| {
        pairs
            .iter()
            .find(|(key, _)| *key == k)
            .map(|(_, v)| v.to_string())
    }
}

#[test]
fn parses_utxostore_url_into_parts() {
    let cfg = Stage3Config::from_getter(
        getter(&[
            (
                "utxostore",
                "aerospike://localhost:3000/utxo-store?set=utxo&externalStore=file://x",
            ),
            ("blockchain_grpcAddress", "localhost:8087"),
            (
                "subtreestore",
                "file:///data/subtreestore?localTTLStore=file | /data/ttl-2",
            ),
            ("global_blockHeightRetention", "288"),
        ]),
        "",
    )
    .expect("config derives");
    assert_eq!(cfg.aerospike_hosts, "localhost:3000");
    assert_eq!(cfg.aerospike_namespace, "utxo-store");
    assert_eq!(cfg.aerospike_set, "utxo");
    assert_eq!(cfg.blockchain_grpc_address, "http://localhost:8087");
    assert_eq!(cfg.subtree_store_path, "/data/subtreestore");
    assert_eq!(cfg.block_height_retention, 288);
    assert_eq!(cfg.udf_module, "teranode_v59");
}

#[test]
fn utxostore_port_defaults_to_3000() {
    let cfg = Stage3Config::from_getter(
        getter(&[
            ("utxostore", "aerospike://aero-host/utxo-store?set=utxo"),
            ("blockchain_grpcAddress", "http://localhost:8087"),
            ("subtreestore", "file:///data/subtreestore"),
        ]),
        "",
    )
    .expect("config derives");
    assert_eq!(cfg.aerospike_hosts, "aero-host:3000");
    assert_eq!(cfg.blockchain_grpc_address, "http://localhost:8087");
}

#[test]
fn non_aerospike_utxostore_errors_with_diagnostic() {
    // The Rust BA is aerospike-only: a context resolving utxostore to sqlite
    // (e.g. SETTINGS_CONTEXT=dev.simon -> utxostore.dev = sqlite:///utxostore)
    // must fail with an error naming the resolved URL, the context, and the
    // requirement — not an opaque "no host".
    let err = Stage3Config::from_getter(
        getter(&[
            ("utxostore", "sqlite:///utxostore"),
            ("blockchain_grpcAddress", "localhost:8087"),
            ("subtreestore", "file:///data/subtreestore"),
        ]),
        "dev.simon",
    )
    .unwrap_err();
    assert!(
        err.contains("sqlite:///utxostore"),
        "names the resolved URL: {err}"
    );
    assert!(err.contains("dev.simon"), "names the context: {err}");
    assert!(
        err.contains("aerospike://"),
        "states the requirement: {err}"
    );
}

#[test]
fn subtreestore_relative_datadir_reconstructs_path() {
    // ${DATADIR} = ./data makes the URL file://./data/subtreestore: the url
    // crate parses "." as host — the derivation must rebuild "./data/...".
    let cfg = Stage3Config::from_getter(
        getter(&[
            (
                "utxostore",
                "aerospike://localhost:3000/utxo-store?set=utxo",
            ),
            ("blockchain_grpcAddress", "localhost:8087"),
            ("subtreestore", "file://./data/subtreestore"),
        ]),
        "",
    )
    .expect("config derives");
    assert_eq!(cfg.subtree_store_path, "./data/subtreestore");
}

#[test]
fn retention_prefers_utxostore_key_over_global_then_defaults() {
    let base: &[(&str, &str)] = &[
        (
            "utxostore",
            "aerospike://localhost:3000/utxo-store?set=utxo",
        ),
        ("blockchain_grpcAddress", "localhost:8087"),
        ("subtreestore", "file:///data/subtreestore"),
        ("utxostore_blockHeightRetention", "100"),
        ("global_blockHeightRetention", "200"),
    ];
    let cfg = Stage3Config::from_getter(getter(base), "").unwrap();
    assert_eq!(cfg.block_height_retention, 100, "utxostore key wins");

    let no_specific: &[(&str, &str)] = &[
        (
            "utxostore",
            "aerospike://localhost:3000/utxo-store?set=utxo",
        ),
        ("blockchain_grpcAddress", "localhost:8087"),
        ("subtreestore", "file:///data/subtreestore"),
        ("global_blockHeightRetention", "200"),
    ];
    let cfg = Stage3Config::from_getter(getter(no_specific), "").unwrap();
    assert_eq!(cfg.block_height_retention, 200, "global fallback");

    let neither: &[(&str, &str)] = &[
        (
            "utxostore",
            "aerospike://localhost:3000/utxo-store?set=utxo",
        ),
        ("blockchain_grpcAddress", "localhost:8087"),
        ("subtreestore", "file:///data/subtreestore"),
    ];
    let cfg = Stage3Config::from_getter(getter(neither), "").unwrap();
    assert_eq!(cfg.block_height_retention, 288, "default 288");
}

#[test]
fn network_drives_subsidy_interval() {
    let regtest: &[(&str, &str)] = &[
        (
            "utxostore",
            "aerospike://localhost:3000/utxo-store?set=utxo",
        ),
        ("blockchain_grpcAddress", "localhost:8087"),
        ("subtreestore", "file:///data/subtreestore"),
        ("network", "regtest"),
    ];
    let cfg = Stage3Config::from_getter(getter(regtest), "").unwrap();
    assert_eq!(cfg.network, "regtest");
    assert_eq!(cfg.subsidy_halving_interval(), 150);

    let absent: &[(&str, &str)] = &[
        (
            "utxostore",
            "aerospike://localhost:3000/utxo-store?set=utxo",
        ),
        ("blockchain_grpcAddress", "localhost:8087"),
        ("subtreestore", "file:///data/subtreestore"),
    ];
    let cfg = Stage3Config::from_getter(getter(absent), "").unwrap();
    assert_eq!(cfg.network, "mainnet", "defaults to mainnet");
    assert_eq!(cfg.subsidy_halving_interval(), 210_000);
}

#[test]
fn coinbase_addresses_parse_and_default() {
    let with_addrs: &[(&str, &str)] = &[
        (
            "utxostore",
            "aerospike://localhost:3000/utxo-store?set=utxo",
        ),
        ("blockchain_grpcAddress", "localhost:8087"),
        ("subtreestore", "file:///data/subtreestore"),
        ("BA_COINBASE_ADDRESS", " addrA , addrB ,, "),
    ];
    let cfg = Stage3Config::from_getter(getter(with_addrs), "").unwrap();
    assert_eq!(cfg.coinbase_addresses, vec!["addrA", "addrB"]);

    let without: &[(&str, &str)] = &[
        (
            "utxostore",
            "aerospike://localhost:3000/utxo-store?set=utxo",
        ),
        ("blockchain_grpcAddress", "localhost:8087"),
        ("subtreestore", "file:///data/subtreestore"),
    ];
    let cfg = Stage3Config::from_getter(getter(without), "").unwrap();
    assert_eq!(
        cfg.coinbase_addresses,
        vec![ba_service::config::DEFAULT_COINBASE_ADDRESS.to_string()]
    );
}

/// Live end-to-end: rustcore discovers the repo settings.conf (walking up from
/// the test executable / CWD), resolves the dev.legacy context — including the
/// network.dev fallback and ${DATADIR} interpolation — and the derivation
/// produces the aerospike config. This is the path `Stage3Config::load()` runs.
#[test]
fn rustcore_resolves_real_settings_conf_dev_legacy() {
    let c = rustcore::config::config_for_context("dev.legacy");
    let cfg = Stage3Config::from_getter(|k| c.get(k), c.context())
        .expect("dev.legacy derives via rustcore");
    assert_eq!(cfg.aerospike_hosts, "localhost:3000");
    assert_eq!(cfg.aerospike_namespace, "utxo-store");
    assert_eq!(cfg.aerospike_set, "utxo");
    assert!(
        cfg.blockchain_grpc_address.starts_with("http://"),
        "grpc address normalised: {}",
        cfg.blockchain_grpc_address
    );
    assert_eq!(
        cfg.network, "regtest",
        "network.dev fallback under dev.legacy"
    );
    assert_eq!(cfg.subsidy_halving_interval(), 150);
}

//! Stage 3 settings, sourced through **rustcore** (the Rust counterpart of
//! gocore — same files, same semantics): rustcore owns file discovery
//! (`settings.conf` / `settings_local.conf`, walking up from the executable
//! directory then the CWD), env-var-first precedence, `SETTINGS_CONTEXT`
//! fallback (default `"dev"`), `${KEY}` interpolation and encrypted values.
//!
//! This module only DERIVES the typed [`Stage3Config`] the service consumes
//! from those raw values (URL decomposition, scheme validation, defaults).
//! The derivation is injected with a plain `key -> Option<value>` getter so it
//! stays unit-testable without the rustcore global singleton.

use url::Url;

/// The subset of settings the service consumes, fully resolved.
#[derive(Debug, Clone)]
pub struct Stage3Config {
    pub aerospike_hosts: String,
    pub aerospike_namespace: String,
    pub aerospike_set: String,
    pub udf_module: String,
    pub blockchain_grpc_address: String,
    pub subtree_store_path: String,
    pub block_height_retention: u32,
    /// Miner wallet address(es) the built coinbase pays to when SubmitMiningSolution
    /// / GenerateBlocks construct the coinbase themselves (no client-supplied one).
    /// Sourced from `BA_COINBASE_ADDRESS` (comma-separated; env wins over conf via
    /// rustcore precedence) or the documented default below.
    pub coinbase_addresses: Vec<String>,
    /// Active network (`network` key), default `"mainnet"`. Drives the
    /// block-subsidy halving interval (regtest differs from all other networks).
    pub network: String,
    /// gRPC listen address (`blockassembly_grpcListenAddress`, e.g. `:8085`).
    pub grpc_listen_address: String,
    /// Initial subtree capacity / items-per-subtree
    /// (`initial_merkle_items_per_subtree`, default 1,048,576 — Go parity).
    pub subtree_initial_size: usize,
}

pub const DEFAULT_UDF_MODULE: &str = "teranode_v59";

/// Documented default coinbase payout address (a well-known P2PKH address) used
/// when no `BA_COINBASE_ADDRESS` is configured — Capability A scope only.
pub const DEFAULT_COINBASE_ADDRESS: &str = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2";

impl Stage3Config {
    /// Load via the rustcore global configuration (gocore-parity discovery,
    /// precedence, context and interpolation).
    pub fn load() -> Result<Self, String> {
        let c = rustcore::config::config();
        Self::from_getter(|k| c.get(k), c.context())
    }

    /// Derive the typed config from already-resolved raw values. `get` must
    /// apply whatever precedence/context/interpolation the source defines
    /// (rustcore in production; a plain map in tests). `context` is only used
    /// in diagnostics.
    pub fn from_getter(
        get: impl Fn(&str) -> Option<String>,
        context: &str,
    ) -> Result<Self, String> {
        let utxostore = get("utxostore").ok_or("missing utxostore")?;
        let u = Url::parse(&utxostore).map_err(|e| format!("utxostore url: {e}"))?;
        if u.scheme() != "aerospike" {
            return Err(format!(
                "utxostore resolved to '{utxostore}' under SETTINGS_CONTEXT '{context}' — \
                 this service requires an aerospike:// utxostore (the shared production \
                 store); use a context whose utxostore is aerospike, e.g. dev.legacy, \
                 teratestnet or docker.m"
            ));
        }
        let aerospike_hosts = format!(
            "{}:{}",
            u.host_str()
                .ok_or_else(|| format!("utxostore '{utxostore}': no host"))?,
            u.port().unwrap_or(3000)
        );
        let aerospike_namespace = u.path().trim_start_matches('/').to_string();
        let aerospike_set = u
            .query_pairs()
            .find(|(k, _)| k == "set")
            .map(|(_, v)| v.to_string())
            .ok_or("utxostore: no ?set=")?;

        let grpc = get("blockchain_grpcAddress").ok_or("missing blockchain_grpcAddress")?;
        let blockchain_grpc_address = if grpc.starts_with("http") {
            grpc
        } else {
            format!("http://{grpc}")
        };

        let subtree_raw = get("subtreestore").ok_or("missing subtreestore")?;
        let primary = subtree_raw
            .split('|')
            .next()
            .unwrap_or("")
            .trim()
            .to_string();

        // Strip the query string before parsing to avoid ambiguity, then
        // reconstruct scheme + authority + path for URL parsing.
        let primary_no_query = primary
            .split('?')
            .next()
            .unwrap_or(&primary)
            .trim()
            .to_string();

        let su = Url::parse(&primary_no_query).map_err(|e| format!("subtreestore url: {e}"))?;
        if su.scheme() != "file" {
            return Err(format!("subtreestore must be file://, got {}", su.scheme()));
        }

        // When DATADIR is relative (e.g. ./data), the URL becomes file://./data/subtreestore
        // which the url crate parses with "./data" as the host and "/subtreestore" as path.
        // Reconstruct the actual filesystem path from host + path in that case.
        let subtree_store_path = match su.host_str() {
            None | Some("") => su.path().to_string(),
            Some(host) => {
                // host is something like "." or "./data" — prepend to path
                format!("{}{}", host, su.path())
            }
        };

        let block_height_retention = get("utxostore_blockHeightRetention")
            .or_else(|| get("global_blockHeightRetention"))
            .and_then(|v| v.parse().ok())
            .unwrap_or(288);

        let coinbase_addresses = get("BA_COINBASE_ADDRESS")
            .map(|s| {
                s.split(',')
                    .map(|a| a.trim().to_string())
                    .filter(|a| !a.is_empty())
                    .collect::<Vec<_>>()
            })
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| vec![DEFAULT_COINBASE_ADDRESS.to_string()]);

        let network = get("network").unwrap_or_else(|| "mainnet".to_string());

        let grpc_listen_address =
            get("blockassembly_grpcListenAddress").unwrap_or_else(|| "127.0.0.1:18087".to_string());

        let subtree_initial_size = get("initial_merkle_items_per_subtree")
            .and_then(|v| v.parse().ok())
            .unwrap_or(1_048_576);

        Ok(Self {
            aerospike_hosts,
            aerospike_namespace,
            aerospike_set,
            udf_module: DEFAULT_UDF_MODULE.to_string(),
            blockchain_grpc_address,
            subtree_store_path,
            block_height_retention,
            coinbase_addresses,
            network,
            grpc_listen_address,
            subtree_initial_size,
        })
    }

    /// Block-subsidy halving interval for the active network. Regtest uses 150;
    /// all other BSV networks (mainnet/test/stn/teratestnet) use 210000.
    pub fn subsidy_halving_interval(&self) -> u32 {
        if self.network == "regtest" {
            150
        } else {
            210_000
        }
    }
}

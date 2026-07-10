//! Gate 0 — Aerospike Rust client compatibility spike (throwaway).
//!
//! Proves whether the upstream Rust `aerospike` crate can interoperate with
//! Teranode's Aerospike UTXO store: identical key digests, bin encodings, and
//! the server-side `teranode_v59` Lua UDFs. The Go implementation is read-only;
//! everything here lives under `rust/`.

pub mod keys;

// Namespace / set / module constants (see stores/utxo/factory/aerospike.go and
// stores/utxo/aerospike/teranode.go).
pub const NAMESPACE: &str = "test";
pub const SET: &str = "utxos";
pub const UDF_MODULE: &str = "teranode_v59";

// Read the production Lua module IN PLACE (read-only). Relative to the crate
// root, which is the CWD when `cargo test` runs in rust/aerospike-compat-spike/.
pub const LUA_PATH: &str = "../../stores/utxo/aerospike/teranode.lua";

// Bin names mirrored from stores/utxo/fields/fields.go
pub const BIN_UTXOS: &str = "utxos";
pub const BIN_TOTAL_UTXOS: &str = "totalUtxos";
pub const BIN_RECORD_UTXOS: &str = "recordUtxos";
pub const BIN_SPENT_UTXOS: &str = "spentUtxos";
pub const BIN_TOTAL_EXTRA_RECS: &str = "totalExtraRecs";
pub const BIN_SPENT_EXTRA_RECS: &str = "spentExtraRecs";
pub const BIN_BLOCK_IDS: &str = "blockIDs";
pub const BIN_FEE: &str = "fee";
pub const BIN_SIZE: &str = "sizeInBytes";
pub const BIN_CREATING: &str = "creating";
pub const BIN_CONFLICTING: &str = "conflicting";
pub const BIN_DELETE_AT_HEIGHT: &str = "deleteAtHeight";

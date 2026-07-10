# Gate 0 Findings — Aerospike Rust Client Compatibility

**Date:** 2026-06-04
**Crate tested:** `aerospike = "=2.1.0"` (stable, upstream — default features: async + rt-tokio)
**Test server:** `aerospike/aerospike-server:8.0` (stock, runs single-node CE-mode `test` namespace)
**Harness:** `testcontainers 0.27` + a nested Go seeder using the production BSV client
(`github.com/bsv-blockchain/aerospike-client-go/v8 v8.7.1-bsv3`) and `uaerospike` key logic.

## Verdict: **GREEN — proceed to Gate 1**

Every compatibility tier passed against stable `2.1.0`. The `3.0.0-alpha` (Task 10) was
**not needed** — there is no pre-GA dependency.

| Tier | Test | Result | Notes |
|------|------|--------|-------|
| Connectivity (T1) | Rust client ↔ server 8.0 | **PASS** | put/get round-trip |
| Key source (T2) | `calculate_key_source` vectors | **PASS** | 4 unit tests (32-byte master, 36-byte paginated) |
| Key digest (T3) | Go-write → Rust-read | **PASS** | **byte-identical digests across forked-Go and upstream-Rust clients** |
| Bin schema (T4) | int / blob / list-of-blobs / list-of-ints decode | **PASS** | `Value::Int` (i64), `Value::Blob`, `Value::List` |
| UDF register (T5) | `teranode_v59` Lua module | **PASS** | production `teranode.lua` registers & converges on stock 8.0 |
| **UDF spend (T6)** | `spend` via `execute_udf` | **PASS** | utxo 32→68 bytes; response `{status:"OK", blockIDs:[…]}` |
| UDF setMined (T7) | `setMined` via `execute_udf` | **PASS** | blockID appended → `[123,456,789]` |
| Batch read (T8) | selected-bin batch (`BatchOperation::read`) | **PASS** | only selected bins returned |
| Expressions/CDT (T9) | filter expr + `lists::append` | **PASS** | passing filter applies, blocking filter prevents write |
| Fork delta | `-bsv3` vs upstream Go client | **DONE** | wire-identical on the store's path (see design spec §4) |

## What this proves

The upstream Rust `aerospike` 2.1.0 client interoperates with Teranode's Aerospike UTXO
store at the wire level: identical key digests, identical bin encodings, and correct
invocation + response-parsing of the production server-side Lua UDFs (`spend`, `setMined`).
A native-Rust Block Assembly can therefore own the UTXO store via this client — the single
biggest external dependency is cleared.

## Real API vs. the plan's assumptions (corrected during execution)

- Batch API is `client.batch(&BatchPolicy, &[BatchOperation])` (not `BatchUDF::new`).
  Single-key `execute_udf` was used for T6/T7 — same arg/response encoding, simpler.
- `register_udf` takes `&AdminPolicy` (not `&WritePolicy`).
- `Value::Blob(Vec<u8>)` / `Value::Int(i64)` / `Value::List` are the variants used.
- The harness lives in `tests/common/mod.rs` (testcontainers/tokio are dev-deps; a library
  crate cannot use them).
- UDF response is `Value::HashMap` keyed by `String` (e.g. `{"status":"OK", …}`).

## Environment notes (non-obvious, captured for Gate 1+)

1. **Aerospike logs to stderr**, not stdout — testcontainers wait uses
   `WaitFor::message_on_stderr("service ready: soon there will be cake!")`.
2. **Docker connectivity:** a single-node container advertises its internal bridge IP
   (e.g. `192.168.x.y:3000`), unreachable from the host. The Rust client rebuilds its node
   list from that address, so the harness pins host==container port and supplies an `ip_map`
   translating bridge-IP → `127.0.0.1`. (The Go client tolerates the single-node seed without
   this; only the Rust client needs it.) `ip_map` rewrites host **but not port**, so a fixed
   port is required → **tests run serially (`--test-threads=1`, one binary at a time)**.
3. The spike container uses `SERVICE_PORT=3500` to avoid clashing with any local Aerospike.

## Watch-items (do not block Gate 0)

- **Production server is a custom BSV build:** a developer instance was observed running
  `ghcr.io/bsv-blockchain/aerospike-server:8.1.2.0-1` (not stock `:8.0`). The Lua path was
  validated against stock 8.0 and is standard Aerospike, so it is representative — but for full
  fidelity, **re-run T5–T7 against `ghcr.io/bsv-blockchain/aerospike-server:8.1.2.0-1`** before
  cutover (it likely carries the private `mod-teranode` native module).
- **Native opcode-200 path** (`operation_teranode.go` / `mod-teranode`) is staged in the BSV
  fork but **unused** by the store today. If the store later adopts it, a Rust drop-in needs a
  small client-side opcode-200 op plus a server with the `mod-teranode` module (server-side,
  language-agnostic). See design spec §4.

## How to run

```bash
cd rust/aerospike-compat-spike
# requires a running Docker daemon + the `go` toolchain on PATH
cargo test --test t2 keys                     # pure unit tests, no Docker
for t in t1_connect t3_key_digest t4_bin_schema t5_register_udf \
         t6_udf_spend t7_udf_setmined t8_batch_read t9_expressions; do
  cargo test --test $t -- --test-threads=1 --nocapture
done
```

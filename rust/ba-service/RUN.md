# Running ba-service

The native Rust Block Assembly service. Configuration is sourced entirely through
**rustcore** (the Rust counterpart of gocore) — the same `settings.conf` /
`settings_local.conf`, contexts, env-first precedence and `${VAR}` interpolation
the Go services use. There is no Rust-specific `BA_PORT`/`BA_CAP` anymore.

## Build

```bash
cd rust/ba-service
RUSTFLAGS="-C target-cpu=native" cargo build --release   # target-cpu=native enables hardware SHA
```

## Run

This is a **Stage-3, config-driven** service: it boots against the real backends,
so before starting, make sure your settings context provides them.

```bash
SETTINGS_CONTEXT=<ctx> ./target/release/ba-service
```

It requires, for the chosen `SETTINGS_CONTEXT`:
- `utxostore` resolving to an `aerospike://host:port/ns?set=...` URL (it refuses to
  start otherwise — it must share the production UTXO store);
- a reachable `blockchain_grpcAddress` (the Go blockchain service);
- a writable `subtreestore` `file://` path.

Pick a context whose `utxostore` is aerospike (e.g. `dev.legacy`, `teratestnet`,
`docker.m`), or point the keys at your node.

### What happens on boot
1. Prints **all resolved settings** (gocore.Stats parity: CMDLINE / SETTINGS_ENV / SETTINGS).
2. Connects the Aerospike UTXO store + blockchain gRPC client (fatal on failure).
3. Seeds the chain tip, loads unmined txs, then flips **ready**.
4. Subscribes to blockchain notifications (extend + setMined; reorg reconciliation).
5. Serves gRPC on `blockassembly_grpcListenAddress`.

## Configuration knobs (env wins; key name is exact)

| What | Key | Default |
|---|---|---|
| Listen address | `blockassembly_grpcListenAddress` (`:${BLOCK_ASSEMBLY_GRPC_PORT}`) | `:8085` (dev: `localhost:8085`) |
| Port only | `BLOCK_ASSEMBLY_GRPC_PORT` | `8085` |
| Subtree size | `initial_merkle_items_per_subtree` | `1048576` |
| Context | `SETTINGS_CONTEXT` | `dev` |
| UTXO store | `utxostore` | per context |
| Blockchain svc | `blockchain_grpcAddress` | per context |
| Subtree store | `subtreestore` | per context |

```bash
# examples (env overrides any conf value)
BLOCK_ASSEMBLY_GRPC_PORT=18087 ./target/release/ba-service
blockassembly_grpcListenAddress=127.0.0.1:9999 ./target/release/ba-service
initial_merkle_items_per_subtree=1024 ./target/release/ba-service
```

## Driving it

```bash
# reflection is enabled — grpcurl needs no proto files
grpcurl -plaintext localhost:8085 list blockassembly_api.BlockAssemblyAPI
grpcurl -plaintext -d '{}' localhost:8085 blockassembly_api.BlockAssemblyAPI/HealthGRPC
grpcurl -plaintext -d '{}' localhost:8085 blockassembly_api.BlockAssemblyAPI/GetMiningCandidate

# Go health probe (uses the canonical generated client)
cd healthcheck && go run . -addr localhost:8085
```

## Notes
- `Health` returns Unavailable until the unmined-tx load completes (ready flip).
- The earlier `stage2check` / `fullcheck` clients are Stage-2 artifacts that assumed
  a standalone in-memory service; they do **not** apply to this config-driven build,
  which requires the live backends. Integration coverage now lives in
  `tests/it_stage3.rs` and the golden tests.

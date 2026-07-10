# Gate 1 Verdict — Native Rust vs Tuned Go (subtree engine)

**Date:** 2026-06-04
**Hardware:** Apple M5 Max (Mac17,6), 18 cores
**Go:** 1.26.2 (`crypto/sha256` uses ARMv8 hardware SHA + parallel merkle in go-subtree)
**Rust:** 1.95.0, release + `target-cpu=native`, **`sha2` `asm` feature (hardware SHA — essential, see below)**
**Workload:** 2,000,000 deterministic txs, single processing thread (the Go subtree processor is one goroutine).

## Verdict: **WORTH IT** — ingest AND reorg both clear the bar decisively

Native Rust beats the *already-tuned* Go path on every dimension that motivated this evaluation
— ingest throughput and reorg latency/allocation — while producing byte-identical output. Full
detail below; the reorg result (Stage D) is the strongest signal.

## Numbers (ingest hero benchmark)

| Metric | Tuned Go (1 core) | Tuned Go (18 cores) | Native Rust (1 core) | Rust vs Go |
|--------|-------------------|---------------------|----------------------|-----------|
| Ingest throughput, cap=65536 | 4.43M tx/s | 7.29M tx/s | **8.67M tx/s** | **1.96× vs 1-core; beats 18-core Go** |
| Ingest throughput, cap=1024 | 4.53M tx/s | — | **8.49M tx/s** | **1.87×** |
| Dedup + accumulate (no merkle) | 9.24M tx/s | — | **18.6M tx/s** | **2.01×** |
| GC stop-the-world pause (over run) | 13–68 µs | 13–68 µs | **0 (no GC)** | — |
| Allocation churn over run | ~329 MB, 2 GC cycles | ~329 MB | **negligible** | — |
| Peak RSS | ~420 MB | ~420 MB | ~474 MB | comparable |

**Headline:** Rust on **one core** (8.67M tx/s) outperforms Go using **all 18 cores** (7.29M),
with **zero GC pauses** and **no allocation churn**. The dedup/accumulation path — exactly where
Go's columnar + `sync.Pool` tuning is aimed and where GC pressure lives — is **2× faster** in
Rust with no GC at all.

## Correctness

All golden-vector tests PASS against go-subtree: merkle roots (counts 1/2/3/4/7/1000/1024),
`Subtree.Serialize`, `TxInpoints.Serialize`, and the full ingest workload's completed-subtree
roots. The benchmark is not "fast because wrong."

## Critical lesson: the SHA backend dominates the merkle

The biggest gotcha this gate surfaced — and a must-carry for any real port:

- The default RustCrypto `sha2` crate ran **software SHA256** here (even with
  `target-cpu=native`), giving **2.46M tx/s** — *slower* than Go.
- Enabling `sha2`'s **`asm` feature** (pulls `sha2-asm`, hardware SHA) jumped merkle to
  **8.58M tx/s** — a **3.5× speedup**, and the difference between losing to Go and beating it.
- Go's `crypto/sha256` uses hardware SHA automatically *and* go-subtree parallelizes the merkle
  across goroutines for trees > 1024 (`routineSplitSize`). Rust matched/beat it **single-core**;
  adding `rayon` parallelism to the Rust merkle would widen the gap further.

**Takeaway:** the merkle is SHA-throughput-bound; pick a hardware-SHA backend (`sha2`+`asm`,
or `ring`) from day one. With that, Rust's advantage is real and large.

## How to reproduce

```bash
cd rust/ba-subtree-bench
# .cargo/config.toml already sets target-cpu=native; Cargo.toml enables sha2 "asm"
cargo build --release --bin replay
./target/release/replay 2000000 65536        # rust, single core
# Go baseline:
cd gen && go build -o /tmp/gobench .
GOMAXPROCS=1 /tmp/gobench bench 2000000 65536 # go, single core
/tmp/gobench bench 2000000 65536              # go, all cores
```

## Stage D — Reorg reconciliation (the motivating path)

**Scope note:** the real `reorgBlocks` (~600 lines) is wired into channels / blob store /
utxoStore and is not standalone-callable, so this benchmarks a **faithful model** of its
in-memory reconciliation — mass set inserts/removes (moveBack/moveForward), reverse-cascade
conflict eviction, mark-list allocation, and subtree rebuild — implemented *identically* in Go
and Rust. Correctness gate: the two implementations produce **byte-identical end-state**
(chained roots) for the same input (cross-implementation golden, `reorg.txt`). The line-by-line
port with the real stores wired is Gate 2 work.

Workload: u=500,000 unmined, 3 blocks back / 4 forward, conflict stride 1000, cap=65536 → 7
rebuilt subtrees.

| Metric | Tuned Go (1 core) | Tuned Go (18 cores) | Native Rust (1 core) | Rust vs Go |
|--------|-------------------|---------------------|----------------------|-----------|
| Reorg reconciliation latency | 0.275 s | 0.218 s | **0.129 s** | **2.13× vs 1-core; beats 18-core** |
| GC stop-the-world pause | 26 µs (6 cycles) | 80 µs (5 cycles) | **0 (no GC)** | — |
| Allocation churn | **113 MB** | 113 MB | **negligible** | — |
| Peak RSS | 108 MB | — | **76 MB** | 0.70× |

**As predicted, reorg is where Rust wins biggest.** The allocation-heavy map/slice churn that
drives GC in Go (113 MB, 6 GC cycles per reorg) is essentially free in Rust: 2.1× faster, zero
GC, and 30% less peak memory. This is the exact pathology that motivated the evaluation, and the
data is unambiguous.

## Overall Gate 1 conclusion: **WORTH IT**

Native Rust beats the already-tuned Go path on **both** motivating dimensions — ingest
throughput (1-core Rust > 18-core Go, 0 GC) and reorg latency/allocation (2.1× faster, 0 GC,
−30% RSS) — with byte-identical output validated against go-subtree throughout. The GC-pressure
premise behind this evaluation is confirmed with real numbers.

**Recommendation:** proceed to a full strangler-pattern design (Gate 2) — wire the real stores
(Gate 0 proved the Aerospike client works), port the full reorg semantics, stand up the gRPC
service, and shadow-run against Go. Carry forward the hardware-SHA backend requirement
(`sha2`+`asm` or `ring`) as a day-one decision.

## Caveats / honesty

- Single-thread comparison by design (the Go subtree processor is one goroutine). Go's
  *multi-core* merkle parallelism was measured separately and Rust beat it single-core anyway;
  Rust + `rayon` would widen the gap but was not needed to clear the bar.
- The reorg figure is a model of the reconciliation's allocation shape, not the full
  `reorgBlocks` semantics (see scope note). It captures the GC-relevant work; edge-case fidelity
  is Gate 2.
- Benchmarked on Apple M5 Max (18 cores, hardware SHA). Re-confirm margins on the target
  production CPU before final commitment.

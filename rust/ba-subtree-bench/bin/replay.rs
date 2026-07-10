//! Ingest hero benchmark (Rust side). Pre-builds a deterministic workload of N
//! txs (excluded from timing), then times the ingest pipeline (dedup + add +
//! merkle root on each completed subtree). Run under `/usr/bin/time -l` for RSS.
//!
//! Usage: replay [N] [cap_size]   (defaults: 2_000_000, 65536)

use std::time::Instant;

use ba_subtree_bench::hash::{sha256d, Hash};
use ba_subtree_bench::processor::SubtreeProcessor;

fn main() {
    let mut args = std::env::args().skip(1);
    let n: u32 = args.next().and_then(|s| s.parse().ok()).unwrap_or(2_000_000);
    let cap: usize = args.next().and_then(|s| s.parse().ok()).unwrap_or(65536);

    // Workload prep — NOT timed (matches the Go baseline's prep exclusion).
    let txs: Vec<(Hash, u64, u64)> = (0..n)
        .map(|i| (sha256d(&i.to_le_bytes()), i as u64, i as u64))
        .collect();

    let start = Instant::now();
    let mut p = SubtreeProcessor::new(cap);
    for (h, f, s) in &txs {
        p.add(*h, *f, *s);
    }
    let completed = p.num_chained();
    let elapsed = start.elapsed().as_secs_f64();

    let tps = n as f64 / elapsed;
    println!(
        "rust  n={n} cap={cap} completed_subtrees={completed} elapsed={elapsed:.3}s tx/s={tps:.0} gc_pause_ns=0",
    );
}

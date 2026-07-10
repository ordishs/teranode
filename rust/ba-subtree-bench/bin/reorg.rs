//! Reorg reconciliation benchmark (Rust side). Times the in-memory model in
//! `reorg::reorg_reconcile`. Run under `/usr/bin/time -l` for RSS.
//!
//! Usage: reorg [u] [block] [d_back] [d_fwd]
//! Defaults: u=500000 block=50000 d_back=3 d_fwd=4 (stride=1000, cap=65536)

use std::time::Instant;

use ba_subtree_bench::reorg::{reorg_reconcile, ReorgParams};

fn main() {
    let mut a = std::env::args().skip(1);
    let u: u32 = a.next().and_then(|s| s.parse().ok()).unwrap_or(500_000);
    let block: u32 = a.next().and_then(|s| s.parse().ok()).unwrap_or(50_000);
    let d_back: u32 = a.next().and_then(|s| s.parse().ok()).unwrap_or(3);
    let d_fwd: u32 = a.next().and_then(|s| s.parse().ok()).unwrap_or(4);

    let p = ReorgParams { u, block, d_back, d_fwd, conflict_stride: 1000, cap: 65536 };

    let start = Instant::now();
    let roots = reorg_reconcile(&p);
    let elapsed = start.elapsed().as_secs_f64();

    println!(
        "rust  reorg u={u} block={block} d_back={d_back} d_fwd={d_fwd} subtrees={} elapsed={elapsed:.4}s gc_pause_ns=0",
        roots.len()
    );
}

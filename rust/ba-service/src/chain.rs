//! In-process chain state (replaces the blockchain client for the standalone /
//! shadow build). Tracks the current tip the mining candidate builds on.

use ba_subtree_bench::hash::Hash;

#[derive(Clone)]
pub struct ChainState {
    pub best_hash: Hash,
    pub height: u32,
    /// Compact difficulty target (nBits). Regtest-style easy default.
    pub n_bits: u32,
    pub version: u32,
    pub median_time: u32,
}

impl ChainState {
    pub fn genesis() -> Self {
        Self {
            best_hash: [0u8; 32],
            height: 0,
            n_bits: 0x207f_ffff,
            version: 0x2000_0000,
            median_time: 1_700_000_000,
        }
    }

    /// nBits as 4 little-endian bytes (the candidate/header field shape).
    pub fn n_bits_bytes(&self) -> Vec<u8> {
        self.n_bits.to_le_bytes().to_vec()
    }

    /// Adopt a new tip (from a blockchain Block notification).
    pub fn apply_block(&mut self, hash: Hash, height: u32, n_bits: u32, block_time: u32) {
        self.best_hash = hash;
        self.height = height;
        self.n_bits = n_bits;
        self.median_time = block_time;
    }
}

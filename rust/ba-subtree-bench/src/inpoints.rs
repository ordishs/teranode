//! `TxInpoints` + serialization, byte-identical to `go-subtree/inpoints.go`.
//!
//! `vout_idxs` uses the same count-prefixed packed layout as Go:
//!   [count_0, v0_0, .., count_1, v1_0, ..]
//! Serialize layout (little-endian):
//!   parentCount(u32) ‖ parentHash[32] * P ‖ voutIdx(u32) * len(vout_idxs)

use crate::hash::Hash;

#[derive(Clone, Debug, Default)]
pub struct TxInpoints {
    pub parent_tx_hashes: Vec<Hash>,
    /// Count-prefixed packed vout indices (see module docs).
    pub vout_idxs: Vec<u32>,
}

impl TxInpoints {
    /// Mirrors `NewTxInpointsFromPacked`: store parents and the already-packed
    /// vout layout directly.
    pub fn from_packed(parents: Vec<Hash>, vout_idxs: Vec<u32>) -> Self {
        Self { parent_tx_hashes: parents, vout_idxs }
    }

    /// Mirrors `Serialize`.
    pub fn serialize(&self) -> Vec<u8> {
        let parent_count = self.parent_tx_hashes.len();
        // Layout invariant (matches Go): empty parents => empty vout_idxs.
        debug_assert!(
            !(parent_count == 0 && !self.vout_idxs.is_empty()
                || parent_count > 0 && self.vout_idxs.len() < parent_count),
            "TxInpoints layout invariant violated"
        );
        let mut out = Vec::with_capacity(4 + parent_count * 32 + self.vout_idxs.len() * 4);
        out.extend_from_slice(&(parent_count as u32).to_le_bytes());
        for h in &self.parent_tx_hashes {
            out.extend_from_slice(h);
        }
        for v in &self.vout_idxs {
            out.extend_from_slice(&v.to_le_bytes());
        }
        out
    }
}

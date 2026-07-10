//! Caches the mining candidate (+ its subtree node-sets) per candidate id so
//! SubmitMiningSolution can rebuild the block. Mirrors Go jobStore.

use std::collections::HashMap;
use std::sync::Mutex;

use ba_subtree_bench::hash::Hash;
use ba_subtree_bench::subtree::Subtree;

#[derive(Clone)]
pub struct Job {
    pub previous_hash: Hash,
    /// Full subtree node-sets (not just roots) — submit needs them to substitute
    /// the coinbase and recompute the block merkle root. `Subtree` derives Clone.
    pub subtrees: Vec<Subtree>,
    pub coinbase_value: u64,
    pub height: u32,
    pub n_bits: u32,
    pub version: u32,
    pub time: u32,
}

#[derive(Default)]
pub struct JobStore {
    jobs: Mutex<HashMap<Vec<u8>, Job>>,
}

impl JobStore {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn insert(&self, id: Vec<u8>, job: Job) {
        self.jobs.lock().unwrap().insert(id, job);
    }

    pub fn get(&self, id: &[u8]) -> Option<Job> {
        self.jobs.lock().unwrap().get(id).cloned()
    }

    pub fn clear(&self) {
        self.jobs.lock().unwrap().clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn insert_lookup_clear() {
        let js = JobStore::new();
        let id: Vec<u8> = vec![1, 2, 3];
        js.insert(
            id.clone(),
            Job {
                previous_hash: [0u8; 32],
                subtrees: vec![],
                coinbase_value: 50,
                height: 1,
                n_bits: 0x207fffff,
                version: 0x20000000,
                time: 100,
            },
        );
        assert!(js.get(&id).is_some());
        js.clear();
        assert!(js.get(&id).is_none());
    }
}

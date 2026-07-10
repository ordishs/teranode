//! Filesystem BlobStore (wraps the existing FsSubtreeStore reader).
use ba_subtree_bench::hash::Hash;
use tonic::async_trait;

use ba_subtree_bench::subtree::Subtree;

use super::{BlobStore, StoreError};
use crate::subtree_store::FsSubtreeStore;

pub struct FsBlobStore {
    inner: FsSubtreeStore,
}

impl FsBlobStore {
    pub fn new(path: impl Into<std::path::PathBuf>) -> Self {
        Self {
            inner: FsSubtreeStore::new(path),
        }
    }
}

impl FsBlobStore {
    /// Path of the blob for `key` (reverse-hash hex + `.subtree`). Test helper.
    #[cfg(test)]
    fn path(&self, key: &Hash) -> std::path::PathBuf {
        self.inner.path(key)
    }
}

#[async_trait]
impl BlobStore for FsBlobStore {
    async fn tx_hashes(&self, subtree_hash: &Hash) -> Result<Vec<Hash>, StoreError> {
        self.inner
            .tx_hashes(subtree_hash)
            .map_err(StoreError::Backend)
    }

    /// Read the full deserialized subtree (nodes with fee/size + conflicting
    /// list) from the reverse-hash `.subtree` blob — the inverse of `set`.
    async fn subtree(&self, root: &Hash) -> Result<Subtree, StoreError> {
        self.inner.subtree(root).map_err(StoreError::Backend)
    }

    /// Write pre-serialized subtree blob bytes (header + body) verbatim to the
    /// reverse-hash `.subtree` path. The caller is responsible for prepending
    /// the 8-byte `SUBTREE_MAGIC` header (mirrors Go `subtreeStore.Set`).
    async fn set(&self, key: &Hash, bytes: &[u8]) -> Result<(), StoreError> {
        self.inner
            .write_bytes(key, bytes)
            .map_err(StoreError::Backend)
    }

    /// Best-effort delete-at-height. No-op for now (real pruning deferred to
    /// Capability H); logged so the call is observable.
    async fn set_dah(&self, key: &Hash, dah: u32) -> Result<(), StoreError> {
        let mut rev = *key;
        rev.reverse();
        eprintln!(
            "set_dah (best-effort no-op, pruning deferred): subtree={} dah={dah}",
            hex::encode(rev)
        );
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn missing_subtree_is_not_found() {
        let s = FsBlobStore::new("/nonexistent-path-xyz");
        let err = s.tx_hashes(&[9u8; 32]).await.unwrap_err();
        assert!(matches!(
            err,
            StoreError::NotFound(_) | StoreError::Backend(_)
        ));
    }

    #[tokio::test]
    async fn set_writes_bytes_to_reverse_hash_path() {
        let dir = std::env::temp_dir().join(format!("ba_blob_set_{}", std::process::id()));
        let s = FsBlobStore::new(&dir);
        let key = [7u8; 32];
        let bytes = vec![1u8, 2, 3, 4, 5];
        s.set(&key, &bytes).await.unwrap();
        let raw = std::fs::read(s.path(&key)).unwrap();
        assert_eq!(raw, bytes);
    }

    #[tokio::test]
    async fn subtree_round_trips_nodes_with_fee_and_size() {
        use crate::subtree_store::SUBTREE_MAGIC;
        use ba_subtree_bench::hash::sha256d;
        use ba_subtree_bench::subtree::Subtree;

        let dir = std::env::temp_dir().join(format!("ba_blob_subtree_{}", std::process::id()));
        let s = FsBlobStore::new(&dir);

        // Build a subtree with distinct fee/size per node, serialize with the
        // 8-byte magic header (the on-disk format), write it, read it back.
        let mut st = Subtree::new();
        let txs: Vec<[u8; 32]> = (0u32..4).map(|i| sha256d(&i.to_le_bytes())).collect();
        for (i, h) in txs.iter().enumerate() {
            st.add_node(*h, (i as u64) * 7, (i as u64) * 11 + 1);
        }
        let root = st.root_hash().unwrap();
        let mut blob = SUBTREE_MAGIC.to_vec();
        blob.extend_from_slice(&st.serialize());
        s.set(&root, &blob).await.unwrap();

        let got = s.subtree(&root).await.unwrap();
        assert_eq!(got.nodes.len(), 4);
        for (i, h) in txs.iter().enumerate() {
            assert_eq!(got.nodes[i].hash, *h);
            assert_eq!(got.nodes[i].fee, (i as u64) * 7, "fee preserved");
            assert_eq!(
                got.nodes[i].size_in_bytes,
                (i as u64) * 11 + 1,
                "size preserved"
            );
        }
    }

    #[tokio::test]
    async fn set_dah_is_best_effort_ok() {
        let s = FsBlobStore::new("/nonexistent-path-xyz");
        s.set_dah(&[1u8; 32], 42).await.unwrap();
    }
}

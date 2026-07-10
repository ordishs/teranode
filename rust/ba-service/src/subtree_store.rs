//! Read subtree transaction sets from the (filesystem) subtree blob store — the
//! same store the Go BA reads via `subtreeStore.GetIoReader(hash, FileTypeSubtree)`.
//!
//! Layout (matching stores/blob/file + pkg/fileformat):
//!   - filename = reverse(hash) hex + ".subtree"  (ReverseAndHexEncodeSlice)
//!   - file = 8-byte magic header ("S-1.0   ") + Subtree::serialize body
//!
//! (A configurable hash-prefix subdirectory exists in Go; the default flat
//! layout is used here and is the integration point to match the live config.)

use std::path::PathBuf;

use ba_subtree_bench::hash::Hash;
use ba_subtree_bench::subtree::Subtree;

/// Length of the fileformat magic header prefixed to every blob.
const HEADER_LEN: usize = 8;

/// The 8-byte fileformat magic for subtree blobs. Byte-identical to Go's
/// `fileformat.NewHeader(FileTypeSubtree).Bytes()` — `magicSubtree` in
/// `pkg/fileformat/header.go:51`: ASCII `"S-1.0   "` (S '-' '1' '.' '0' then
/// three spaces). Go's `Header.Write` emits these 8 bytes and nothing else.
pub const SUBTREE_MAGIC: [u8; 8] = *b"S-1.0   ";

pub struct FsSubtreeStore {
    base: PathBuf,
}

impl FsSubtreeStore {
    pub fn new(base: impl Into<PathBuf>) -> Self {
        Self { base: base.into() }
    }

    pub(crate) fn path(&self, subtree_hash: &Hash) -> PathBuf {
        let mut rev = *subtree_hash;
        rev.reverse(); // ReverseAndHexEncodeSlice -> bitcoin display order
        self.base.join(format!("{}.subtree", hex::encode(rev)))
    }

    /// The transaction hashes contained in the given subtree.
    pub fn tx_hashes(&self, subtree_hash: &Hash) -> Result<Vec<Hash>, String> {
        let p = self.path(subtree_hash);
        let bytes = std::fs::read(&p).map_err(|e| format!("read {}: {e}", p.display()))?;
        if bytes.len() < HEADER_LEN {
            return Err(format!("{} shorter than header", p.display()));
        }
        let st = Subtree::deserialize(&bytes[HEADER_LEN..])?;
        Ok(st.tx_hashes())
    }

    /// The full deserialized subtree (nodes with fee/size + conflicting list) for
    /// `root`. Same read path as `tx_hashes` (8-byte header skip +
    /// `Subtree::deserialize`) but returns the whole structure — moveBack needs
    /// the persisted fee/size to re-add orphaned txs.
    pub fn subtree(&self, root: &Hash) -> Result<Subtree, String> {
        let p = self.path(root);
        let bytes = std::fs::read(&p).map_err(|e| format!("read {}: {e}", p.display()))?;
        if bytes.len() < HEADER_LEN {
            return Err(format!("{} shorter than header", p.display()));
        }
        Subtree::deserialize(&bytes[HEADER_LEN..])
    }

    /// Write `subtree` to the reverse-hash `.subtree` path under `base`, in Go's
    /// exact on-disk format: the 8-byte `SUBTREE_MAGIC` header followed by
    /// `Subtree::serialize()`. This is the byte-for-byte inverse of `tx_hashes`'
    /// read path (header skip + `Subtree::deserialize`) and matches what Go's
    /// `subtreeStore.Set(hash, FileTypeSubtree, ...)` writes.
    pub fn write_subtree(&self, root: &Hash, subtree: &mut Subtree) -> Result<(), String> {
        let body = subtree.serialize();
        let mut bytes = Vec::with_capacity(HEADER_LEN + body.len());
        bytes.extend_from_slice(&SUBTREE_MAGIC);
        bytes.extend_from_slice(&body);
        self.write_bytes(root, &bytes)
    }

    /// Write pre-serialized blob `bytes` (header + body) verbatim to the
    /// reverse-hash `.subtree` path for `key`, creating `base` if needed.
    pub(crate) fn write_bytes(&self, key: &Hash, bytes: &[u8]) -> Result<(), String> {
        std::fs::create_dir_all(&self.base)
            .map_err(|e| format!("mkdir {}: {e}", self.base.display()))?;
        let p = self.path(key);
        std::fs::write(&p, bytes).map_err(|e| format!("write {}: {e}", p.display()))
    }
}

#[cfg(test)]
mod write_tests {
    use super::*;
    use ba_subtree_bench::hash::sha256d;
    use ba_subtree_bench::subtree::Subtree;

    #[test]
    fn write_then_read_round_trips() {
        let dir = std::env::temp_dir().join(format!("ba_subtree_wt_{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let store = FsSubtreeStore::new(&dir);
        let mut st = Subtree::new();
        let txs: Vec<[u8; 32]> = (0u32..4).map(|i| sha256d(&i.to_le_bytes())).collect();
        for (i, h) in txs.iter().enumerate() {
            st.add_node(*h, i as u64, 10);
        }
        let root = st.root_hash().unwrap();
        store.write_subtree(&root, &mut st).unwrap();
        let got = store.tx_hashes(&root).unwrap();
        assert_eq!(got, txs);
    }
}

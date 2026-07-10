//! `Subtree` + serialization, byte-identical to `go-subtree/subtree.go`.
//!
//! Serialize layout (all integers little-endian):
//!   rootHash[32] ‖ fees(u64) ‖ sizeInBytes(u64) ‖ nNodes(u64) ‖
//!   { hash[32] ‖ fee(u64) ‖ sizeInBytes(u64) } * nNodes ‖
//!   nConflicting(u64) ‖ hash[32] * nConflicting

use crate::hash::Hash;
use crate::merkle;

/// The coinbase placeholder hash, byte-identical to go-subtree's
/// `CoinbasePlaceholder` (`go-subtree/coinbase_placeholder.go`): 32 bytes of
/// `0xFF`. It occupies node 0 of the FIRST subtree in a block; `Block.Valid`
/// check #7 (`model/Block.go`) requires `SubtreeSlices[0].Nodes[0] == this`.
/// At submit, `ReplaceRootNode(index 0)` swaps it for the real coinbase txid.
pub const COINBASE_PLACEHOLDER: Hash = [0xFF; 32];

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Node {
    pub hash: Hash,
    pub fee: u64,
    pub size_in_bytes: u64,
}

#[derive(Clone, Default)]
pub struct Subtree {
    /// Aggregate fee field. `add_node` accumulates each node's fee into this, byte-
    /// for-byte matching go-subtree `Subtree.AddNode` (`Fees += fee`). The persisted
    /// blob carries this sum; `model.Block.checkBlockRewardAndFees` reads it.
    pub fees: u64,
    /// Aggregate size field. `add_node` accumulates each node's size, matching
    /// go-subtree `Subtree.AddNode` (`SizeInBytes += sizeInBytes`).
    pub size_in_bytes: u64,
    pub nodes: Vec<Node>,
    pub conflicting_nodes: Vec<Hash>,
    root_cache: Option<Hash>,
}

impl Subtree {
    pub fn new() -> Self {
        Self::default()
    }

    /// Mirrors `AddNode`: append a node, accumulate its fee/size into the subtree
    /// aggregates (`Fees += fee`, `SizeInBytes += sizeInBytes`), and invalidate the
    /// cached root.
    pub fn add_node(&mut self, hash: Hash, fee: u64, size_in_bytes: u64) {
        self.nodes.push(Node {
            hash,
            fee,
            size_in_bytes,
        });
        self.fees += fee;
        self.size_in_bytes += size_in_bytes;
        self.root_cache = None;
    }

    /// Mirrors go-subtree `AddCoinbaseNode`: seed node 0 of an EMPTY subtree with
    /// the coinbase placeholder (`COINBASE_PLACEHOLDER`, fee 0, size 0). Aggregate
    /// fees/size stay 0 (the placeholder contributes nothing). Panics if the
    /// subtree is non-empty, matching Go's `ErrSubtreeNotEmpty`.
    pub fn add_coinbase_node(&mut self) {
        assert!(
            self.nodes.is_empty(),
            "AddCoinbaseNode on a non-empty subtree"
        );
        self.nodes.push(Node {
            hash: COINBASE_PLACEHOLDER,
            fee: 0,
            size_in_bytes: 0,
        });
        self.fees = 0;
        self.size_in_bytes = 0;
        self.root_cache = None;
    }

    /// Mirrors go-subtree `ReplaceRootNode`: substitute the node at `index`
    /// (the coinbase placeholder, normally 0) with the real coinbase, fee 0.
    /// Invalidates the cached root.
    pub fn replace_root_node(&mut self, hash: Hash, index: usize, size_in_bytes: u64) {
        if index < self.nodes.len() {
            self.nodes[index] = Node {
                hash,
                fee: 0,
                size_in_bytes,
            };
            self.root_cache = None;
        }
    }

    /// Remove the first node matching `hash` (invalidating the cached root).
    /// Returns true if a node was removed.
    pub fn remove_first(&mut self, hash: &Hash) -> bool {
        if let Some(pos) = self.nodes.iter().position(|n| &n.hash == hash) {
            self.nodes.remove(pos);
            self.root_cache = None;
            true
        } else {
            false
        }
    }

    pub fn len(&self) -> usize {
        self.nodes.len()
    }

    pub fn is_empty(&self) -> bool {
        self.nodes.is_empty()
    }

    /// Mirrors `RootHash`: merkle root over node hashes, cached. `None` if empty.
    pub fn root_hash(&mut self) -> Option<Hash> {
        if self.root_cache.is_none() {
            let leaves: Vec<Hash> = self.nodes.iter().map(|n| n.hash).collect();
            self.root_cache = merkle::root(&leaves);
        }
        self.root_cache
    }

    /// Inverse of `serialize` — reconstruct a Subtree from its blob-store bytes.
    /// Layout: rootHash[32] ‖ fees ‖ size ‖ nNodes ‖ {hash,fee,size}* ‖
    ///         nConflicting ‖ hash*  (all ints little-endian u64).
    pub fn deserialize(bytes: &[u8]) -> Result<Subtree, String> {
        let mut p = 0usize;
        let need = |p: usize, n: usize, b: &[u8]| -> Result<(), String> {
            if p + n > b.len() {
                Err(format!("subtree truncated at {p}+{n} > {}", b.len()))
            } else {
                Ok(())
            }
        };
        let rd_u64 = |p: &mut usize, b: &[u8]| -> Result<u64, String> {
            need(*p, 8, b)?;
            let v = u64::from_le_bytes(b[*p..*p + 8].try_into().unwrap());
            *p += 8;
            Ok(v)
        };
        let rd_hash = |p: &mut usize, b: &[u8]| -> Result<Hash, String> {
            need(*p, 32, b)?;
            let mut h = [0u8; 32];
            h.copy_from_slice(&b[*p..*p + 32]);
            *p += 32;
            Ok(h)
        };

        let _root = rd_hash(&mut p, bytes)?; // checksum field, not retained
        let fees = rd_u64(&mut p, bytes)?;
        let size_in_bytes = rd_u64(&mut p, bytes)?;
        let n_nodes = rd_u64(&mut p, bytes)? as usize;
        let mut nodes = Vec::with_capacity(n_nodes);
        for _ in 0..n_nodes {
            let hash = rd_hash(&mut p, bytes)?;
            let fee = rd_u64(&mut p, bytes)?;
            let sz = rd_u64(&mut p, bytes)?;
            nodes.push(Node {
                hash,
                fee,
                size_in_bytes: sz,
            });
        }
        let n_conf = rd_u64(&mut p, bytes)? as usize;
        let mut conflicting_nodes = Vec::with_capacity(n_conf);
        for _ in 0..n_conf {
            conflicting_nodes.push(rd_hash(&mut p, bytes)?);
        }

        Ok(Subtree {
            fees,
            size_in_bytes,
            nodes,
            conflicting_nodes,
            root_cache: None,
        })
    }

    /// The transaction hashes in this subtree (node order preserved).
    pub fn tx_hashes(&self) -> Vec<Hash> {
        self.nodes.iter().map(|n| n.hash).collect()
    }

    /// Mirrors `Serialize`. Requires at least one node (root hash is written).
    pub fn serialize(&mut self) -> Vec<u8> {
        let root = self
            .root_hash()
            .expect("serialize requires a non-empty subtree");
        let mut out = Vec::with_capacity(
            32 + 8 + 8 + 8 + self.nodes.len() * 48 + 8 + self.conflicting_nodes.len() * 32,
        );
        out.extend_from_slice(&root);
        out.extend_from_slice(&self.fees.to_le_bytes());
        out.extend_from_slice(&self.size_in_bytes.to_le_bytes());
        out.extend_from_slice(&(self.nodes.len() as u64).to_le_bytes());
        for n in &self.nodes {
            out.extend_from_slice(&n.hash);
            out.extend_from_slice(&n.fee.to_le_bytes());
            out.extend_from_slice(&n.size_in_bytes.to_le_bytes());
        }
        out.extend_from_slice(&(self.conflicting_nodes.len() as u64).to_le_bytes());
        for h in &self.conflicting_nodes {
            out.extend_from_slice(h);
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::Subtree;
    use crate::hash::sha256d;

    #[test]
    fn serialize_deserialize_round_trip() {
        let mut st = Subtree::new();
        for i in 0u32..4 {
            st.add_node(sha256d(&i.to_le_bytes()), i as u64, (i * 10) as u64);
        }
        st.fees = 1234;
        st.size_in_bytes = 5678;
        st.conflicting_nodes = vec![
            sha256d(&100u32.to_le_bytes()),
            sha256d(&200u32.to_le_bytes()),
        ];

        let bytes = st.serialize();
        let mut back = Subtree::deserialize(&bytes).expect("deserialize");
        assert_eq!(back.fees, 1234);
        assert_eq!(back.size_in_bytes, 5678);
        assert_eq!(back.nodes.len(), 4);
        assert_eq!(back.conflicting_nodes.len(), 2);
        assert_eq!(back.tx_hashes(), st.tx_hashes());
        assert_eq!(back.serialize(), bytes, "re-serialize must reproduce bytes");
    }

    #[test]
    fn add_node_accumulates_fee_and_size() {
        // go-subtree AddNode accumulates Fees/SizeInBytes per node.
        let mut st = Subtree::new();
        let fees = [3u64, 5, 7];
        let sizes = [11u64, 13, 17];
        for (i, (&fee, &size)) in fees.iter().zip(sizes.iter()).enumerate() {
            st.add_node(sha256d(&(i as u32).to_le_bytes()), fee, size);
        }
        assert_eq!(st.fees, 15, "fees must equal sum of node fees");
        assert_eq!(
            st.size_in_bytes,
            sizes.iter().sum::<u64>(),
            "size_in_bytes must equal sum of node sizes"
        );
    }

    #[test]
    fn add_coinbase_node_keeps_aggregates_zero() {
        // The coinbase placeholder contributes fee 0 / size 0, leaving the
        // aggregates at 0 (matching go-subtree AddCoinbaseNode).
        let mut st = Subtree::new();
        st.add_coinbase_node();
        assert_eq!(st.fees, 0);
        assert_eq!(st.size_in_bytes, 0);
    }

    #[test]
    fn replace_root_node_swaps_index_zero() {
        let mut st = Subtree::new();
        for i in 0u32..4 {
            st.add_node(sha256d(&i.to_le_bytes()), i as u64, 10);
        }
        let coinbase = sha256d(b"coinbase");
        st.replace_root_node(coinbase, 0, 99);
        assert_eq!(st.nodes[0].hash, coinbase);
        assert_eq!(st.nodes[0].size_in_bytes, 99);
        assert_eq!(st.nodes[0].fee, 0, "coinbase node carries 0 fee");
    }
}

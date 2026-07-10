//! Coinbase BUMP (BSV Unified Merkle Path, BRC-74) — mirrors Go
//! `util/bump.ComputeCoinbaseBUMP` + `ConvertToBUMP` + `Format.EncodeBinary`
//! (util/bump/coinbase_bump.go, util/bump/format.go) as invoked by the submit
//! path (Server.go:1438-1443): the proof runs from the coinbase (subtree 0,
//! index 0) through the coinbase-replaced subtree to the block merkle root,
//! over the SAME lifted top-level roots `createMerkleTreeFromSubtrees` uses.
//!
//! Binary layout (BRC-74):
//!   varint(blockHeight) ‖ u8(treeHeight) ‖ per level: varint(nodeCount) ‖
//!   per node: varint(offset) ‖ flag ‖ [hash32 if flag ∈ {0x00 data, 0x02 txid}]
//!
//! For the coinbase (global leaf offset 0) every path sibling sits at offset 1
//! of its level, and level 0 additionally carries the coinbase txid itself at
//! offset 0 with the txid flag (prepended — even offset means the txid is the
//! left node). Hashes are written in internal (little-endian) byte order, the
//! same order [`Hash`] holds.

use crate::block_merkle::{merkle_height, root_hash_padded};
use crate::hash::Hash;
use crate::merkle;
use crate::subtree::Subtree;
use crate::tx::varint;

const FLAG_DATA: u8 = 0x00;
const FLAG_TXID: u8 = 0x02;

/// Compute the coinbase BUMP for a block built from `subtrees` (the job's
/// subtree clones) and `coinbase_txid`, at `block_height` (the NEW block's
/// height, Go `currentHeight+1`).
///
/// Mirrors the Go submit flow exactly:
///   1. replace node 0 of subtree 0 with the coinbase txid
///      (`ReplaceRootNode`, Server.go:1407),
///   2. within-subtree proof of index 0 (`Subtree.GetMerkleProof(0)`),
///   3. top-level roots with the final-subtree height-lift
///      (`createMerkleTreeFromSubtrees`, Server.go:1541-1552),
///   4. block-level proof of subtree-root 0
///      (`merkleproof.GenerateBlockMerkleProof`),
///   5. BRC-74 binary encoding (`ConvertToBUMP` + `EncodeBinary`).
///
/// Errors on no subtrees (Go's caller skips the call — coinbase-only blocks
/// carry no BUMP) and on an internally-empty subtree.
pub fn coinbase_bump(
    subtrees: &mut [Subtree],
    coinbase_txid: &Hash,
    block_height: u32,
) -> Result<Vec<u8>, String> {
    if subtrees.is_empty() {
        return Err("no subtrees".to_string());
    }

    subtrees[0].replace_root_node(*coinbase_txid, 0, 0);

    let leaves0 = subtrees[0].tx_hashes();
    if leaves0.is_empty() {
        return Err("empty subtree".to_string());
    }
    let subtree_proof = merkle::merkle_proof(&leaves0, 0);

    let first_len = subtrees[0].len();
    let last_idx = subtrees.len() - 1;
    let last_len = subtrees[last_idx].len();

    let mut roots: Vec<Hash> = Vec::with_capacity(subtrees.len());
    for s in subtrees.iter_mut() {
        roots.push(s.root_hash().ok_or("empty subtree")?);
    }

    // Final-subtree height-lift, identical to block_merkle_root (Server.go:1545).
    if subtrees.len() > 1 && last_len < first_len {
        let first_height = merkle_height(first_len);
        roots[last_idx] = root_hash_padded(roots[last_idx], last_len, first_height);
    }

    // Block-level proof of subtree 0. A single subtree yields an empty proof
    // (GenerateBlockMerkleProof's len==1 early return == merkle_proof's no-op).
    let block_proof = merkle::merkle_proof(&roots, 0);

    let mut path: Vec<Hash> = subtree_proof;
    path.extend_from_slice(&block_proof);

    encode(block_height, coinbase_txid, &path)
}

/// BRC-74 binary encoding of the coinbase path (all siblings at offset 1; the
/// txid node at offset 0 on level 0). Mirrors `Format.EncodeBinary`.
fn encode(block_height: u32, coinbase_txid: &Hash, path: &[Hash]) -> Result<Vec<u8>, String> {
    if path.len() > u8::MAX as usize {
        return Err(format!("BUMP path too long: {} levels", path.len()));
    }

    let mut out = Vec::with_capacity(2 + path.len() * 35 + 34);
    varint::append(&mut out, block_height as u64);
    out.push(path.len() as u8);

    for (level, sibling) in path.iter().enumerate() {
        if level == 0 {
            // Level 0 carries [txid(offset 0, flag 0x02), sibling(offset 1)] —
            // the txid is prepended because its (even) offset puts it on the left.
            varint::append(&mut out, 2);
            varint::append(&mut out, 0);
            out.push(FLAG_TXID);
            out.extend_from_slice(coinbase_txid);
        } else {
            varint::append(&mut out, 1);
        }
        varint::append(&mut out, 1);
        out.push(FLAG_DATA);
        out.extend_from_slice(sibling);
    }

    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::block_merkle::try_block_merkle_root;
    use crate::hash::{hash_pair, sha256d};

    fn subtree_of(range: std::ops::Range<u32>) -> Subtree {
        let mut s = Subtree::new();
        for i in range {
            s.add_node(sha256d(&i.to_le_bytes()), i as u64, 10);
        }
        s
    }

    /// Parse the BUMP and fold the coinbase txid up the path; the result must be
    /// the block merkle root `try_block_merkle_root` computes for the same input.
    fn assert_bump_folds_to_root(mut subtrees: Vec<Subtree>, coinbase: Hash, height: u32) {
        let mut for_root = subtrees.clone();
        let want_root = try_block_merkle_root(&mut for_root, &coinbase).expect("root");

        let bump = coinbase_bump(&mut subtrees, &coinbase, height).expect("bump");

        // Parse: varint height, u8 treeHeight, then per-level nodes.
        let mut off = 0usize;
        let h = varint::read(&bump, &mut off).unwrap();
        assert_eq!(h, height as u64, "block height");
        let tree_height = bump[off] as usize;
        off += 1;

        let mut acc = coinbase;
        for level in 0..tree_height {
            let node_count = varint::read(&bump, &mut off).unwrap();
            let expected_nodes = if level == 0 { 2 } else { 1 };
            assert_eq!(node_count, expected_nodes, "level {level} node count");

            let mut sibling: Option<Hash> = None;
            for _ in 0..node_count {
                let offset = varint::read(&bump, &mut off).unwrap();
                let flag = bump[off];
                off += 1;
                let mut hash = [0u8; 32];
                hash.copy_from_slice(&bump[off..off + 32]);
                off += 32;
                match flag {
                    FLAG_TXID => {
                        assert_eq!(offset, 0, "txid offset");
                        assert_eq!(hash, coinbase, "txid node carries the coinbase");
                    }
                    FLAG_DATA => {
                        assert_eq!(offset, 1, "sibling offset");
                        sibling = Some(hash);
                    }
                    f => panic!("unexpected flag {f:#x}"),
                }
            }
            acc = hash_pair(&acc, &sibling.expect("sibling at every level"));
        }
        assert_eq!(off, bump.len(), "no trailing bytes");
        assert_eq!(acc, want_root, "BUMP folds to the block merkle root");
    }

    #[test]
    fn single_subtree_four_leaves_exact_bytes() {
        let coinbase = sha256d(b"cb");
        let mut sts = vec![subtree_of(0..4)];
        let bump = coinbase_bump(&mut sts, &coinbase, 840_000).expect("bump");

        // Hand-built expectation: leaves after replacement = [cb, l1, l2, l3].
        let l1 = sha256d(&1u32.to_le_bytes());
        let l2 = sha256d(&2u32.to_le_bytes());
        let l3 = sha256d(&3u32.to_le_bytes());
        let h23 = hash_pair(&l2, &l3);

        let mut want = Vec::new();
        varint::append(&mut want, 840_000); // 0xfe + 4 bytes — exercises varint
        want.push(2); // tree height: 2 in-subtree levels, no block level
        want.extend_from_slice(&[2, 0, FLAG_TXID]); // level 0: 2 nodes; txid@0
        want.extend_from_slice(&coinbase);
        want.extend_from_slice(&[1, FLAG_DATA]); // sibling@1
        want.extend_from_slice(&l1);
        want.extend_from_slice(&[1, 1, FLAG_DATA]); // level 1: 1 node @1
        want.extend_from_slice(&h23);

        assert_eq!(bump, want, "exact BRC-74 bytes");
    }

    #[test]
    fn bump_folds_to_block_root_across_shapes() {
        let coinbase = sha256d(b"cb");
        // 1 subtree; 2 equal; 3 (odd top level); lift case (last shorter than first).
        assert_bump_folds_to_root(vec![subtree_of(0..4)], coinbase, 1);
        assert_bump_folds_to_root(vec![subtree_of(0..4), subtree_of(4..8)], coinbase, 150);
        assert_bump_folds_to_root(
            vec![subtree_of(0..4), subtree_of(4..8), subtree_of(8..12)],
            coinbase,
            70_000,
        );
        assert_bump_folds_to_root(
            vec![subtree_of(0..4), subtree_of(4..8), subtree_of(8..9)],
            coinbase,
            840_000,
        );
    }

    #[test]
    fn single_node_single_subtree_is_height_zero() {
        // One subtree holding only the (replaced) coinbase: empty path. Go emits
        // varint(height) + 0x00 and no txid node (ConvertToBUMP only adds it when
        // the path is non-empty).
        let coinbase = sha256d(b"cb");
        let mut sts = vec![subtree_of(0..1)];
        let bump = coinbase_bump(&mut sts, &coinbase, 7).expect("bump");
        assert_eq!(bump, vec![7u8, 0u8]);
    }

    #[test]
    fn no_subtrees_is_an_error() {
        let coinbase = sha256d(b"cb");
        let err = coinbase_bump(&mut [], &coinbase, 1).unwrap_err();
        assert!(err.contains("no subtrees"), "got: {err}");
    }
}

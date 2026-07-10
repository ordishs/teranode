//! Bitcoin merkle root over subtree leaves, byte-identical to
//! `go-subtree/merkle_tree.go` (`BuildMerkleTreeStoreFromBytes` + `calcMerkle`).
//!
//! Algorithm: pad the leaves up to the next power of two with the zero hash, then
//! fold pairwise to the root, where a parent is:
//!   - zero            if the left child is zero (empty propagates up),
//!   - sha256d(L ‖ L)  if the right child is zero (odd → duplicate left),
//!   - sha256d(L ‖ R)  otherwise.
//!
//! A single leaf is its own root (the Bitcoin one-tx exception).

use crate::hash::{hash_pair, Hash};
use crate::power_of_two::next_power_of_two;

const ZERO: Hash = [0u8; 32];

/// Parent-node hash from a (possibly zero) left/right child pair.
fn calc_merkle(left: &Hash, right: &Hash) -> Hash {
    if *left == ZERO {
        ZERO
    } else if *right == ZERO {
        hash_pair(left, left)
    } else {
        hash_pair(left, right)
    }
}

/// Merkle root of the given leaf hashes. Mirrors `Subtree.RootHash()`:
/// returns `None` for an empty input, the leaf itself for a single leaf.
pub fn root(leaves: &[Hash]) -> Option<Hash> {
    match leaves.len() {
        0 => None,
        1 => Some(leaves[0]),
        len => {
            let n = next_power_of_two(len);
            let mut level: Vec<Hash> = Vec::with_capacity(n);
            level.extend_from_slice(leaves);
            level.resize(n, ZERO); // pad phantom slots with the zero sentinel
            while level.len() > 1 {
                let mut next = Vec::with_capacity(level.len() / 2);
                let mut i = 0;
                while i < level.len() {
                    next.push(calc_merkle(&level[i], &level[i + 1]));
                    i += 2;
                }
                level = next;
            }
            Some(level[0])
        }
    }
}

/// Sibling-hash branch from `index` up to the root, mirroring go-subtree's
/// `Subtree.GetMerkleProof` (Bitcoin merkle: odd nodes duplicate the last). Pairs
/// are hashed `sha256d(left || right)`. Returns the siblings bottom-up so the
/// verifier fold `acc = sha256d(acc || sibling)` reconstructs the root — matching
/// `util.BuildMerkleRootFromCoinbase`.
pub fn merkle_proof(leaves: &[Hash], index: usize) -> Vec<Hash> {
    if leaves.is_empty() {
        return vec![];
    }

    let mut level: Vec<Hash> = leaves.to_vec();
    let mut idx = index;
    let mut proof = Vec::new();

    while level.len() > 1 {
        let sibling = if idx.is_multiple_of(2) {
            // even: sibling is right; duplicate self if there is no right node
            // (Bitcoin odd-node rule, identical to calc_merkle's H(L,L)).
            if idx + 1 < level.len() {
                level[idx + 1]
            } else {
                level[idx]
            }
        } else {
            level[idx - 1]
        };

        proof.push(sibling);

        let mut next = Vec::with_capacity(level.len().div_ceil(2));
        let mut i = 0;

        while i < level.len() {
            let l = level[i];
            let r = if i + 1 < level.len() { level[i + 1] } else { level[i] };
            next.push(hash_pair(&l, &r));
            i += 2;
        }

        idx /= 2;
        level = next;
    }

    proof
}

#[cfg(test)]
mod tests {
    use super::root;
    use crate::hash::{hash_pair, sha256d};

    #[test]
    fn single_leaf_is_its_own_root() {
        let leaf = sha256d(b"a");
        assert_eq!(root(&[leaf]), Some(leaf));
    }

    #[test]
    fn two_leaves_hash_pair() {
        let a = sha256d(b"a");
        let b = sha256d(b"b");
        assert_eq!(root(&[a, b]), Some(hash_pair(&a, &b)));
    }

    #[test]
    fn three_leaves_duplicate_odd() {
        let a = sha256d(b"a");
        let b = sha256d(b"b");
        let c = sha256d(b"c");
        // level0: [ab, cc]; root: hash_pair(ab, cc)
        let ab = hash_pair(&a, &b);
        let cc = hash_pair(&c, &c);
        assert_eq!(root(&[a, b, c]), Some(hash_pair(&ab, &cc)));
    }

    #[test]
    fn empty_is_none() {
        assert_eq!(root(&[]), None);
    }
}

#[cfg(test)]
mod proof_tests {
    use super::*;
    use crate::hash::sha256d;

    fn fold(leaf: Hash, proof: &[Hash]) -> Hash {
        let mut acc = leaf;
        for sib in proof {
            let mut buf = [0u8; 64];
            buf[..32].copy_from_slice(&acc);
            buf[32..].copy_from_slice(sib);
            acc = sha256d(&buf);
        }
        acc
    }

    #[test]
    fn proof_folds_back_to_root() {
        let leaves: Vec<Hash> = (0u32..8).map(|i| sha256d(&i.to_le_bytes())).collect();
        let root = root(&leaves).unwrap();
        let proof = merkle_proof(&leaves, 0);
        assert_eq!(fold(leaves[0], &proof), root, "leaf 0 must reconstruct root");
    }

    #[test]
    fn proof_folds_back_for_index_zero() {
        // The coinbase always sits at index 0 (the left-most leaf), so the
        // `sha256d(acc || sibling)` fold reconstructs the root for any leaf count.
        for count in [1u32, 2, 3, 5, 7, 13, 1024] {
            let leaves: Vec<Hash> = (0..count).map(|i| sha256d(&i.to_le_bytes())).collect();
            let root = root(&leaves).unwrap();
            let proof = merkle_proof(&leaves, 0);
            assert_eq!(
                fold(leaves[0], &proof),
                root,
                "count {count} index 0 must reconstruct root"
            );
        }
    }
}

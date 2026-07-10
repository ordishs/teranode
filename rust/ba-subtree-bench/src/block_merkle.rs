//! Block-level merkle: combine the coinbase with the assembled subtrees into the
//! block merkle root, and the coinbase's merkle branch — mirroring Go
//! `createMerkleTreeFromSubtrees` (Server.go:1533) and
//! `subtree.GetMerkleProofForCoinbase` (BlockAssembler.go:1178 / merkle_tree.go:13).

use crate::hash::{hash_pair, Hash};
use crate::merkle;
use crate::subtree::Subtree;

/// Merkle height of a subtree with `length` leaves: `ceil(log2(length))`, computed
/// exactly like go-subtree's `bits.Len(uint(length-1))`. `length` must be >= 1.
pub(crate) fn merkle_height(length: usize) -> u32 {
    debug_assert!(length >= 1);
    (usize::BITS) - (length - 1).leading_zeros()
}

/// Lift a subtree's natural root up to `target_height` by repeatedly self-hashing
/// `H(root, root)` per level — mirrors go-subtree `Subtree.RootHashPadded`. This
/// makes a short final subtree compose with the top tree as if it had been padded
/// to the first subtree's height. Returns the natural root when already at height.
pub(crate) fn root_hash_padded(root: Hash, length: usize, target_height: u32) -> Hash {
    let actual_height = merkle_height(length);
    let mut r = root;
    for _ in 0..target_height.saturating_sub(actual_height) {
        r = hash_pair(&r, &r);
    }
    r
}

/// Block merkle root, byte-identical to Go `createMerkleTreeFromSubtrees`:
///   1. replace node 0 of subtree-0 with the coinbase txid (recomputing its root),
///   2. take each subtree's root,
///   3. height-lift the LAST subtree's root to the FIRST subtree's height when it
///      is shorter (so it composes like a single flat tree),
///   4. merkle the (possibly lifted) subtree roots — with a CVE-2012-2459 duplicate
///      guard — into the block merkle root.
///
/// Coinbase-only (no subtrees) → the coinbase txid itself.
///
/// # Panics
/// Panics if two top-level subtree roots collide (the CVE-2012-2459 guard) — Go
/// returns an error here; assembly must never emit such a block.
pub fn block_merkle_root(subtrees: &mut [Subtree], coinbase_txid: &Hash) -> Hash {
    if subtrees.is_empty() {
        return *coinbase_txid;
    }

    subtrees[0].replace_root_node(*coinbase_txid, 0, 0);

    let first_len = subtrees[0].len();
    let last_idx = subtrees.len() - 1;
    let last_len = subtrees[last_idx].len();

    let mut roots: Vec<Hash> = subtrees
        .iter_mut()
        .map(|s| s.root_hash().expect("non-empty subtree"))
        .collect();

    // Height-lift the final subtree to the first subtree's height when shorter,
    // mirroring Server.go:1545-1552 (last.RootHashPadded(first.Height)).
    if subtrees.len() > 1 && last_len < first_len {
        let first_height = merkle_height(first_len);
        roots[last_idx] = root_hash_padded(roots[last_idx], last_len, first_height);
    }

    // CVE-2012-2459-style duplicate detection over the top-level roots.
    for i in 0..roots.len() {
        for j in (i + 1)..roots.len() {
            assert_ne!(
                roots[i], roots[j],
                "duplicate subtree root hash in top-level merkle tree"
            );
        }
    }

    merkle::root(&roots).expect("non-empty roots")
}

/// Non-panicking variant of [`block_merkle_root`] for callers that must surface a
/// `Result` instead of aborting (e.g. a gRPC handler). Returns `Err` with a
/// human-readable reason when the panicking version would abort: a duplicate
/// top-level subtree root (the CVE-2012-2459 guard) or an internally-empty
/// subtree. The coinbase-only (no subtrees) case returns the coinbase txid, like
/// [`block_merkle_root`]. On success the result is byte-identical to
/// [`block_merkle_root`].
pub fn try_block_merkle_root(
    subtrees: &mut [Subtree],
    coinbase_txid: &Hash,
) -> Result<Hash, String> {
    if subtrees.is_empty() {
        return Ok(*coinbase_txid);
    }

    subtrees[0].replace_root_node(*coinbase_txid, 0, 0);

    let first_len = subtrees[0].len();
    let last_idx = subtrees.len() - 1;
    let last_len = subtrees[last_idx].len();

    let mut roots: Vec<Hash> = Vec::with_capacity(subtrees.len());
    for s in subtrees.iter_mut() {
        roots.push(s.root_hash().ok_or("empty subtree")?);
    }

    if subtrees.len() > 1 && last_len < first_len {
        let first_height = merkle_height(first_len);
        roots[last_idx] = root_hash_padded(roots[last_idx], last_len, first_height);
    }

    for i in 0..roots.len() {
        for j in (i + 1)..roots.len() {
            if roots[i] == roots[j] {
                return Err("duplicate subtree root hash in top-level merkle tree".to_string());
            }
        }
    }

    merkle::root(&roots).ok_or_else(|| "empty roots".to_string())
}

/// The coinbase merkle branch, byte-identical to Go `GetMerkleProofForCoinbase`:
/// the in-subtree-0 branch for index 0, then the top-tree branch over the subtree
/// roots for subtree index 0. The top-tree roots are the RAW subtree roots (no
/// coinbase substitution, no height-lift) — exactly what Go computes from the
/// candidate subtrees, where index 0 is the coinbase placeholder.
pub fn coinbase_merkle_proof(subtrees: &mut [Subtree]) -> Vec<Hash> {
    if subtrees.is_empty() {
        return vec![];
    }

    let leaves0: Vec<Hash> = subtrees[0].tx_hashes();
    let mut proof = merkle::merkle_proof(&leaves0, 0);

    if subtrees.len() > 1 {
        let roots: Vec<Hash> = subtrees
            .iter_mut()
            .map(|s| s.root_hash().expect("non-empty subtree"))
            .collect();
        proof.extend(merkle::merkle_proof(&roots, 0));
    }

    proof
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::hash::sha256d;
    use crate::subtree::Subtree;

    fn subtree_of(range: std::ops::Range<u32>) -> Subtree {
        let mut s = Subtree::new();
        for i in range {
            s.add_node(sha256d(&i.to_le_bytes()), i as u64, 10);
        }
        s
    }

    #[test]
    fn single_subtree_root_and_proof_consistent() {
        let mut sts = vec![subtree_of(0..4)];
        let coinbase = sha256d(b"cb");
        let proof = coinbase_merkle_proof(&mut sts);
        let root = block_merkle_root(&mut sts, &coinbase);
        let mut acc = coinbase;
        for sib in &proof {
            let mut buf = [0u8; 64];
            buf[..32].copy_from_slice(&acc);
            buf[32..].copy_from_slice(sib);
            acc = sha256d(&buf);
        }
        assert_eq!(acc, root);
    }

    #[test]
    fn coinbase_only_block_root_is_coinbase() {
        let mut empty: Vec<Subtree> = vec![];
        let coinbase = sha256d(b"cb");
        assert_eq!(block_merkle_root(&mut empty, &coinbase), coinbase);
    }

    #[test]
    fn try_block_merkle_root_matches_panicking_on_valid_input() {
        let coinbase = sha256d(b"cb");
        let mut a = vec![subtree_of(0..4), subtree_of(4..8)];
        let mut b = a.clone();
        let got = try_block_merkle_root(&mut a, &coinbase).expect("valid");
        let want = block_merkle_root(&mut b, &coinbase);
        assert_eq!(got, want);
    }

    #[test]
    fn try_block_merkle_root_coinbase_only() {
        let mut empty: Vec<Subtree> = vec![];
        let coinbase = sha256d(b"cb");
        assert_eq!(try_block_merkle_root(&mut empty, &coinbase), Ok(coinbase));
    }

    #[test]
    fn try_block_merkle_root_errors_on_duplicate_root() {
        // Two identical subtrees → identical top-level roots after the coinbase is
        // placed only in subtree 0... so make subtree 1 equal to the (post-coinbase)
        // subtree 0 is hard; instead use two subtrees whose RAW roots collide.
        let coinbase = sha256d(b"cb");
        // subtree 0 gets the coinbase swapped in; to force a top-level duplicate we
        // build subtree 1 to equal subtree 0 AFTER substitution. Easiest: single-node
        // subtrees where node 0 of st0 becomes the coinbase, and st1's only node is
        // also the coinbase txid.
        let mut st0 = Subtree::new();
        st0.add_node(sha256d(b"placeholder"), 0, 10);
        let mut st1 = Subtree::new();
        st1.add_node(coinbase, 0, 10);
        let mut sts = vec![st0, st1];
        let err = try_block_merkle_root(&mut sts, &coinbase).unwrap_err();
        assert!(err.contains("duplicate"), "got: {err}");
    }

    #[test]
    fn merkle_height_matches_bits_len() {
        // bits.Len(uint(length-1))
        assert_eq!(merkle_height(1), 0);
        assert_eq!(merkle_height(2), 1);
        assert_eq!(merkle_height(3), 2);
        assert_eq!(merkle_height(4), 2);
        assert_eq!(merkle_height(404), 9);
        assert_eq!(merkle_height(1024), 10);
    }
}

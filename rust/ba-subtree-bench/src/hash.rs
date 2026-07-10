//! 32-byte hash + Bitcoin double-SHA256, matching `chainhash.Hash` semantics.

use sha2::{Digest, Sha256};

/// A 32-byte hash (txid / merkle node), matching `chainhash.Hash`'s byte layout.
pub type Hash = [u8; 32];

/// Bitcoin double-SHA256: `sha256(sha256(data))`.
pub fn sha256d(data: &[u8]) -> Hash {
    let first = Sha256::digest(data);
    let second = Sha256::digest(first);
    let mut out = [0u8; 32];
    out.copy_from_slice(&second);
    out
}

/// Single SHA-256 — the UTXO-hash digest (`chainhash.HashH`). Distinct from
/// `sha256d` (txid / merkle), which double-hashes.
pub fn sha256(data: &[u8]) -> Hash {
    let mut out = [0u8; 32];
    out.copy_from_slice(&Sha256::digest(data));
    out
}

/// Concatenate two 32-byte hashes and double-SHA256 — the merkle inner-node hash.
pub fn hash_pair(left: &Hash, right: &Hash) -> Hash {
    let mut buf = [0u8; 64];
    buf[..32].copy_from_slice(left);
    buf[32..].copy_from_slice(right);
    sha256d(&buf)
}

#[cfg(test)]
mod tests {
    use super::sha256d;

    #[test]
    fn sha256d_empty() {
        assert_eq!(
            hex::encode(sha256d(&[])),
            "5df6e0e2761359d30a8275058e299fcc0381534545f55cf43e41983f5d4c9456"
        );
    }

    #[test]
    fn sha256d_abc() {
        assert_eq!(
            hex::encode(sha256d(b"abc")),
            "4f8b42c22dd3729b519ba6f68d2da7cc5b2d606d05daed5ad5128cc03e6c6358"
        );
    }

    #[test]
    fn sha256_single_abc() {
        assert_eq!(
            hex::encode(super::sha256(b"abc")),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }
}

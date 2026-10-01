//! 32-byte hash + Bitcoin double-SHA256, matching `chainhash.Hash` semantics.

use sha2::{Digest, Sha256};

/// A 32-byte hash (txid / merkle node), matching `chainhash.Hash`'s byte layout.
pub type Hash = [u8; 32];

/// Hash as Go `chainhash.Hash.String()` prints it: byte-reversed hex.
pub fn display_hex(hash: &Hash) -> String {
    hash.iter().rev().map(|b| format!("{b:02x}")).collect()
}

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
    #[test]
    fn display_hex_reverses_bytes_like_chainhash_string() {
        // Mainnet genesis: stored little-endian, printed big-endian.
        let mut h = [0u8; 32];
        hex_decode(
            "6fe28c0ab6f1b372c1a6a246ae63f74f931e8365e15a089c68d6190000000000",
            &mut h,
        );
        assert_eq!(
            super::display_hex(&h),
            "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f"
        );
    }

    fn hex_decode(s: &str, out: &mut [u8]) {
        for (i, b) in out.iter_mut().enumerate() {
            *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap();
        }
    }

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

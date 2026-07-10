//! Bitcoin block-header construction + hashing (implemented natively in-process).
//!
//! The 80-byte header and its double-SHA256 hash are the real formulas.
//! Block merkle root computation is delegated to the engine
//! (`ba_subtree_bench::block_merkle`); the local stand-in was removed in A5.1.

use ba_subtree_bench::hash::{sha256d, Hash};

/// Standard 80-byte Bitcoin block header (all little-endian).
pub fn build_header(
    version: u32,
    prev: &Hash,
    merkle_root: &Hash,
    time: u32,
    n_bits: u32,
    nonce: u32,
) -> [u8; 80] {
    let mut h = [0u8; 80];
    h[0..4].copy_from_slice(&version.to_le_bytes());
    h[4..36].copy_from_slice(prev);
    h[36..68].copy_from_slice(merkle_root);
    h[68..72].copy_from_slice(&time.to_le_bytes());
    h[72..76].copy_from_slice(&n_bits.to_le_bytes());
    h[76..80].copy_from_slice(&nonce.to_le_bytes());
    h
}

/// Block hash = double-SHA256 of the 80-byte header.
pub fn header_hash(header: &[u8; 80]) -> Hash {
    sha256d(header)
}

/// Decode a compact "nBits" target into the full 256-bit target, as a big-endian
/// 32-byte array. nBits compact format: high byte = base-256 exponent, low 3
/// bytes = mantissa; target = mantissa * 256^(exponent - 3). Returns all-zero
/// (an impossible-to-meet target) for a zero mantissa or an out-of-range exponent.
fn target_from_nbits(n_bits: u32) -> [u8; 32] {
    let exponent = (n_bits >> 24) as usize;
    let mantissa = n_bits & 0x00ff_ffff;

    let mut target = [0u8; 32];
    if mantissa == 0 {
        return target;
    }

    // The mantissa occupies 3 bytes ending at byte position `exponent` (1-indexed
    // from the least-significant end), i.e. its most-significant byte sits at
    // offset (exponent - 1) from the LSB. Place the 3 mantissa bytes big-endian
    // into the big-endian target array, guarding against overflow/underflow.
    let mant_bytes = mantissa.to_be_bytes(); // [0, b2, b1, b0]
                                             // exponent is the byte-length of the value; the MSB of the mantissa lives at
                                             // big-endian index 32 - exponent.
    if exponent == 0 || exponent > 32 {
        return target;
    }
    // Big-endian index where the mantissa's most-significant byte (mant_bytes[1])
    // is written.
    let msb_index = 32usize.checked_sub(exponent);
    let Some(msb_index) = msb_index else {
        return target;
    };

    for (k, &b) in mant_bytes[1..4].iter().enumerate() {
        let idx = msb_index + k;
        if idx < 32 {
            target[idx] = b;
        } else if b != 0 {
            // A non-zero mantissa byte would fall off the high end → target
            // overflows 256 bits; treat as the maximum (all 0xff) target so any
            // hash meets it (mirrors how oversized targets behave in practice).
            return [0xff; 32];
        }
    }

    target
}

/// Proof-of-work check: interpret `header_hash` as a 256-bit LITTLE-ENDIAN integer
/// (Bitcoin block hashes are displayed big-endian but stored little-endian) and
/// return true iff `hash <= target_from_nbits(n_bits)`.
pub fn meets_target(header_hash: &Hash, n_bits: u32) -> bool {
    let target = target_from_nbits(n_bits);

    // Compare the hash (little-endian) to the target (big-endian) most-significant
    // byte first. The hash's most-significant byte is its LAST byte.
    for i in 0..32 {
        let hash_byte = header_hash[31 - i];
        let target_byte = target[i];
        if hash_byte < target_byte {
            return true;
        }
        if hash_byte > target_byte {
            return false;
        }
    }
    // Equal → meets the target.
    true
}

#[cfg(test)]
mod pow_tests {
    use super::*;

    #[test]
    fn regtest_target_decodes() {
        // 0x207fffff: exponent 0x20 (32), mantissa 0x7fffff. MSB at big-endian
        // index 0 → target = 7fffff0000...00.
        let t = target_from_nbits(0x207f_ffff);
        assert_eq!(t[0], 0x7f);
        assert_eq!(t[1], 0xff);
        assert_eq!(t[2], 0xff);
        assert!(t[3..].iter().all(|&b| b == 0));
    }

    #[test]
    fn mainnet_difficulty_one_target_decodes() {
        // 0x1d00ffff: exponent 0x1d (29), mantissa 0x00ffff. MSB byte (0x00) at
        // big-endian index 32-29=3, so bytes [3]=0x00, [4]=0xff, [5]=0xff.
        let t = target_from_nbits(0x1d00_ffff);
        assert_eq!(t[3], 0x00);
        assert_eq!(t[4], 0xff);
        assert_eq!(t[5], 0xff);
        assert!(t[0..3].iter().all(|&b| b == 0));
        assert!(t[6..].iter().all(|&b| b == 0));
    }

    #[test]
    fn hash_below_regtest_target_meets() {
        // A hash whose most-significant byte (LE: last byte) is below 0x7f meets
        // the 0x207fffff target easily.
        let mut h = [0xffu8; 32];
        h[31] = 0x00; // most-significant byte = 0 → way below target
        assert!(meets_target(&h, 0x207f_ffff));
    }

    #[test]
    fn hash_above_regtest_target_fails() {
        // Most-significant byte = 0x80 > 0x7f → exceeds the regtest target.
        let mut h = [0x00u8; 32];
        h[31] = 0x80;
        assert!(!meets_target(&h, 0x207f_ffff));
    }

    #[test]
    fn hash_equal_to_target_meets() {
        // hash (LE) numerically equal to the target (BE) meets (<=).
        let mut h = [0u8; 32];
        // target = 7fffff00..00 (BE). LE hash equal to it: byte 31=0x7f, 30=0xff, 29=0xff.
        h[31] = 0x7f;
        h[30] = 0xff;
        h[29] = 0xff;
        assert!(meets_target(&h, 0x207f_ffff));
    }

    #[test]
    fn zero_mantissa_target_is_unmeetable() {
        // Target all-zero: only an all-zero hash would "meet" it (<=), any nonzero fails.
        let mut h = [0u8; 32];
        h[0] = 1;
        assert!(!meets_target(&h, 0x1d00_0000));
    }
}

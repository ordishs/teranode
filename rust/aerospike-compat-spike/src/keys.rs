//! Port of Teranode's Aerospike key-source computation.
//!
//! Mirrors `util/uaerospike/client.go` (`CalculateKeySource` /
//! `CalculateKeySourceInternal`) in the Go repo. The key source is the raw byte
//! slice handed to `aerospike.NewKey(namespace, set, keySource)`; the Aerospike
//! client then computes the record digest over it. For the Rust client to resolve
//! the same records, it must produce the identical key-source bytes.

/// Mirrors `uaerospike.CalculateKeySourceInternal`.
///
/// Record 0 (the master record) keys on the bare 32-byte hash. Pagination
/// records (`num > 0`) key on the 32-byte hash followed by `num` as a
/// little-endian `u32` (36 bytes total).
pub fn calculate_key_source_internal(hash: &[u8; 32], num: u32) -> Vec<u8> {
    if num == 0 {
        return hash.to_vec();
    }
    let mut ks = Vec::with_capacity(36);
    ks.extend_from_slice(hash);
    ks.extend_from_slice(&num.to_le_bytes());
    ks
}

/// Mirrors `uaerospike.CalculateKeySource`: maps a `vout` to its pagination
/// record index via `vout / batch_size`. Returns `None` when `batch_size == 0`
/// (the Go code returns nil).
pub fn calculate_key_source(hash: &[u8; 32], vout: u32, batch_size: u32) -> Option<Vec<u8>> {
    if batch_size == 0 {
        return None;
    }
    Some(calculate_key_source_internal(hash, vout / batch_size))
}

#[cfg(test)]
mod tests {
    use super::{calculate_key_source, calculate_key_source_internal};

    #[test]
    fn num_zero_returns_32_byte_hash() {
        let hash = [0xABu8; 32];
        let ks = calculate_key_source_internal(&hash, 0);
        assert_eq!(ks.len(), 32);
        assert_eq!(ks, hash.to_vec());
    }

    #[test]
    fn num_nonzero_appends_le_u32() {
        let hash = [0x11u8; 32];
        let ks = calculate_key_source_internal(&hash, 1);
        assert_eq!(ks.len(), 36);
        assert_eq!(&ks[0..32], &hash);
        assert_eq!(&ks[32..36], &[1u8, 0, 0, 0]); // little-endian 1
    }

    #[test]
    fn key_source_maps_vout_to_record_index() {
        let hash = [0x11u8; 32];
        // batch size 20_000: vout 0 -> record 0 (32 bytes), vout 25_000 -> record 1 (36 bytes).
        assert_eq!(calculate_key_source(&hash, 0, 20_000).unwrap().len(), 32);
        let r1 = calculate_key_source(&hash, 25_000, 20_000).unwrap();
        assert_eq!(r1.len(), 36);
        assert_eq!(&r1[32..36], &[1u8, 0, 0, 0]);
    }

    #[test]
    fn zero_batch_size_returns_none() {
        assert!(calculate_key_source(&[0u8; 32], 5, 0).is_none());
    }
}

//! Coinbase tx construction, mirroring model.CreateCoinbase / GetCoinbaseParts.

/// Block subsidy in satoshis: 50 BTC halved every `interval` blocks.
pub fn block_subsidy(height: u32, interval: u32) -> u64 {
    let halvings = height / interval;
    if halvings >= 64 {
        return 0;
    }
    5_000_000_000u64 >> halvings
}

/// Decode a base58check BSV address to its locking script.
/// P2PKH (version 0x00 mainnet / 0x6f testnet): DUP HASH160 <20> EQUALVERIFY CHECKSIG.
/// P2SH  (version 0x05 mainnet / 0xc4 testnet): HASH160 <20> EQUAL.
/// Mirrors model.AddressToScript (model/GetCoinbaseParts.go).
pub fn address_to_script(address: &str) -> Result<Vec<u8>, String> {
    let raw = bs58::decode(address)
        .with_check(None)
        .into_vec()
        .map_err(|e| format!("base58check {address}: {e}"))?;
    if raw.len() != 21 {
        return Err(format!("address payload {} != 21 bytes", raw.len()));
    }
    let (version, h160) = (raw[0], &raw[1..21]);
    match version {
        0x00 | 0x6f => {
            let mut s = vec![0x76, 0xa9, 0x14];
            s.extend_from_slice(h160);
            s.extend_from_slice(&[0x88, 0xac]);
            Ok(s)
        }
        0x05 | 0xc4 => {
            let mut s = vec![0xa9, 0x14];
            s.extend_from_slice(h160);
            s.push(0x87);
            Ok(s)
        }
        v => Err(format!("unsupported address version {v:#x}")),
    }
}

/// Build a coinbase tx (bytes), mirroring model.CreateCoinbase / GetCoinbaseParts.
/// scriptSig = 0x03 || height_LE3 (BIP34) || arbitrary || extranonce(12).
/// The scriptSig length varint covers arbitrary + the 12-byte extranonce space.
/// Outputs split `coinbase_value` across addresses (first gets the remainder).
/// Sequence 0xffffffff, locktime 0. Go truncates `0x03||height||arbitrary` so the
/// whole coinbase data fits in 100 bytes (leaving 12 for the extranonce).
pub fn create_coinbase(
    height: u32,
    coinbase_value: u64,
    arbitrary: &[u8],
    addresses: &[String],
    extranonce: [u8; 12],
) -> Result<Vec<u8>, String> {
    if addresses.is_empty() {
        return Err("no wallet addresses provided".to_string());
    }

    const SPACE_FOR_EXTRA_NONCE: usize = 12;

    // arbitraryData = 0x03 || height_LE3 || arbitrary text (matches makeCoinbase1).
    let mut arbitrary_data = vec![0x03];
    arbitrary_data.extend_from_slice(&height.to_le_bytes()[..3]);
    arbitrary_data.extend_from_slice(arbitrary);

    // Truncate so arbitrary_data + extranonce fits in 100 bytes (Go behaviour).
    if arbitrary_data.len() > 100 - SPACE_FOR_EXTRA_NONCE {
        arbitrary_data.truncate(100 - SPACE_FOR_EXTRA_NONCE);
    }

    let mut tx = Vec::new();
    tx.extend_from_slice(&1u32.to_le_bytes()); // version
    tx.push(0x01); // input count
    tx.extend_from_slice(&[0u8; 32]); // null prevout txid
    tx.extend_from_slice(&[0xff, 0xff, 0xff, 0xff]); // prevout index

    // scriptSig length covers arbitrary_data + the 12-byte extranonce.
    push_varint(
        &mut tx,
        (arbitrary_data.len() + SPACE_FOR_EXTRA_NONCE) as u64,
    );
    tx.extend_from_slice(&arbitrary_data);
    tx.extend_from_slice(&extranonce);

    tx.extend_from_slice(&[0xff, 0xff, 0xff, 0xff]); // sequence

    let n = addresses.len() as u64;
    let per = coinbase_value / n;
    let rem = coinbase_value % n;
    push_varint(&mut tx, n);
    for (i, addr) in addresses.iter().enumerate() {
        let value = if i == 0 { per + rem } else { per };
        let script = address_to_script(addr)?;
        tx.extend_from_slice(&value.to_le_bytes());
        push_varint(&mut tx, script.len() as u64);
        tx.extend_from_slice(&script);
    }
    tx.extend_from_slice(&0u32.to_le_bytes()); // locktime
    Ok(tx)
}

fn push_varint(out: &mut Vec<u8>, v: u64) {
    if v < 0xfd {
        out.push(v as u8);
    } else if v <= 0xffff {
        out.push(0xfd);
        out.extend_from_slice(&(v as u16).to_le_bytes());
    } else if v <= 0xffff_ffff {
        out.push(0xfe);
        out.extend_from_slice(&(v as u32).to_le_bytes());
    } else {
        out.push(0xff);
        out.extend_from_slice(&v.to_le_bytes());
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn subsidy_halves() {
        assert_eq!(block_subsidy(0, 210_000), 5_000_000_000);
        assert_eq!(block_subsidy(210_000, 210_000), 2_500_000_000);
        assert_eq!(block_subsidy(420_000, 210_000), 1_250_000_000);
        assert_eq!(block_subsidy(210_000 * 64, 210_000), 0);
    }

    #[test]
    fn p2pkh_script_shape() {
        // 1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2 (well-known) → DUP HASH160 <20> EQUALVERIFY CHECKSIG
        let s = address_to_script("1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2").unwrap();
        assert_eq!(s[0], 0x76); // OP_DUP
        assert_eq!(s[1], 0xa9); // OP_HASH160
        assert_eq!(s[2], 0x14); // push 20
        assert_eq!(s.len(), 25);
        assert_eq!(s[23], 0x88); // OP_EQUALVERIFY
        assert_eq!(s[24], 0xac); // OP_CHECKSIG
    }
}

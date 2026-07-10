//! Consensus-faithful port of go-bt transaction deserialization (standard +
//! extended format) plus the canonical txid (double-SHA256) and per-input UTXO
//! hash (single-SHA256). See go-bt@v2.6.5 tx.go/input.go/output.go and
//! teranode util/utxo_hash.go. Read-only structural parsing; no script/sig logic.

use crate::hash::{sha256, sha256d, Hash};

// Smallest possible on-wire input: 32 (prev txid) + 4 (vout) + 1 (min varint len) + 4 (sequence).
const MIN_INPUT_WIRE_BYTES: u64 = 41;
// Smallest possible on-wire output: 8 (satoshis) + 1 (min varint len).
const MIN_OUTPUT_WIRE_BYTES: u64 = 9;

#[derive(Debug, PartialEq, Eq)]
pub enum TxError {
    UnexpectedEof {
        field: &'static str,
        need: usize,
        have: usize,
    },
    NonExtendedInput {
        index: usize,
    },
}

pub mod varint {
    use super::TxError;

    /// Decode a Bitcoin CompactSize at `*off`, advancing `*off`.
    pub fn read(buf: &[u8], off: &mut usize) -> Result<u64, TxError> {
        let first = *read_n(buf, off, 1, "varint")?.first().unwrap();
        match first {
            0xFF => Ok(u64::from_le_bytes(read_arr8(buf, off)?)),
            0xFE => {
                let b = read_n(buf, off, 4, "varint")?;
                Ok(u32::from_le_bytes([b[0], b[1], b[2], b[3]]) as u64)
            }
            0xFD => {
                let b = read_n(buf, off, 2, "varint")?;
                Ok(u16::from_le_bytes([b[0], b[1]]) as u64)
            }
            n => Ok(n as u64),
        }
    }

    /// Encode `v` as CompactSize, appending to `out`.
    pub fn append(out: &mut Vec<u8>, v: u64) {
        if v < 0xFD {
            out.push(v as u8);
        } else if v <= 0xFFFF {
            out.push(0xFD);
            out.extend_from_slice(&(v as u16).to_le_bytes());
        } else if v <= 0xFFFF_FFFF {
            out.push(0xFE);
            out.extend_from_slice(&(v as u32).to_le_bytes());
        } else {
            out.push(0xFF);
            out.extend_from_slice(&v.to_le_bytes());
        }
    }

    fn read_n<'a>(
        buf: &'a [u8],
        off: &mut usize,
        n: usize,
        field: &'static str,
    ) -> Result<&'a [u8], TxError> {
        if n > buf.len().saturating_sub(*off) {
            return Err(TxError::UnexpectedEof {
                field,
                need: n,
                have: buf.len().saturating_sub(*off),
            });
        }
        let s = &buf[*off..*off + n];
        *off += n;
        Ok(s)
    }

    fn read_arr8(buf: &[u8], off: &mut usize) -> Result<[u8; 8], TxError> {
        let s = read_n(buf, off, 8, "varint")?;
        let mut a = [0u8; 8];
        a.copy_from_slice(s);
        Ok(a)
    }
}

#[derive(Debug, Clone)]
pub struct TxInput {
    pub prev_txid: Hash,
    pub vout: u32,
    pub unlocking_script: Vec<u8>,
    pub sequence: u32,
    pub extended: bool,
    pub prev_satoshis: u64,
    pub prev_script: Vec<u8>,
}

#[derive(Debug, Clone)]
pub struct TxOutput {
    pub satoshis: u64,
    pub locking_script: Vec<u8>,
}

#[derive(Debug, Clone)]
pub struct Tx {
    pub version: u32,
    pub inputs: Vec<TxInput>,
    pub outputs: Vec<TxOutput>,
    pub locktime: u32,
    pub extended: bool,
}

fn take<'a>(
    buf: &'a [u8],
    off: &mut usize,
    n: usize,
    field: &'static str,
) -> Result<&'a [u8], TxError> {
    if n > buf.len().saturating_sub(*off) {
        return Err(TxError::UnexpectedEof {
            field,
            need: n,
            have: buf.len().saturating_sub(*off),
        });
    }
    let s = &buf[*off..*off + n];
    *off += n;
    Ok(s)
}

fn u32_le(buf: &[u8], off: &mut usize, field: &'static str) -> Result<u32, TxError> {
    let s = take(buf, off, 4, field)?;
    Ok(u32::from_le_bytes([s[0], s[1], s[2], s[3]]))
}

fn u64_le(buf: &[u8], off: &mut usize, field: &'static str) -> Result<u64, TxError> {
    let s = take(buf, off, 8, field)?;
    let mut a = [0u8; 8];
    a.copy_from_slice(s);
    Ok(u64::from_le_bytes(a))
}

fn read_script(buf: &[u8], off: &mut usize, field: &'static str) -> Result<Vec<u8>, TxError> {
    let n = varint::read(buf, off)? as usize;
    Ok(take(buf, off, n, field)?.to_vec())
}

impl Tx {
    /// Deserialize a transaction (standard or extended) from `buf`. Faithful port
    /// of go-bt `Tx.ReadFromWithArena` incl. the extended-marker state machine.
    pub fn from_bytes(buf: &[u8]) -> Result<Tx, TxError> {
        let mut off = 0usize;
        let version = u32_le(buf, &mut off, "version")?;
        let mut input_count = varint::read(buf, &mut off)?;
        let mut extended = false;
        let mut output_count = 0u64;

        if input_count == 0 {
            output_count = varint::read(buf, &mut off)?;
            if output_count == 0 {
                let four = take(buf, &mut off, 4, "locktime/marker")?;
                let be = u32::from_be_bytes([four[0], four[1], four[2], four[3]]);
                if be != 0xEF {
                    return Ok(Tx {
                        version,
                        inputs: Vec::new(),
                        outputs: Vec::new(),
                        locktime: u32::from_le_bytes([four[0], four[1], four[2], four[3]]),
                        extended: false,
                    });
                }
                extended = true;
                input_count = varint::read(buf, &mut off)?;
            }
        }

        let cap = input_count.min(buf.len() as u64 / MIN_INPUT_WIRE_BYTES) as usize;
        let mut inputs = Vec::with_capacity(cap);
        for _ in 0..input_count {
            let prev = take(buf, &mut off, 32, "prevTxID")?;
            let mut prev_txid = [0u8; 32];
            prev_txid.copy_from_slice(prev);
            let vout = u32_le(buf, &mut off, "vout")?;
            let unlocking_script = read_script(buf, &mut off, "unlockingScript")?;
            let sequence = u32_le(buf, &mut off, "sequence")?;
            let (mut prev_satoshis, mut prev_script) = (0u64, Vec::new());
            if extended {
                prev_satoshis = u64_le(buf, &mut off, "prevSatoshis")?;
                prev_script = read_script(buf, &mut off, "prevTxScript")?;
            }
            inputs.push(TxInput {
                prev_txid,
                vout,
                unlocking_script,
                sequence,
                extended,
                prev_satoshis,
                prev_script,
            });
        }

        if input_count > 0 || extended {
            output_count = varint::read(buf, &mut off)?;
        }

        let cap = output_count.min(buf.len() as u64 / MIN_OUTPUT_WIRE_BYTES) as usize;
        let mut outputs = Vec::with_capacity(cap);
        for _ in 0..output_count {
            let satoshis = u64_le(buf, &mut off, "satoshis")?;
            let locking_script = read_script(buf, &mut off, "lockingScript")?;
            outputs.push(TxOutput {
                satoshis,
                locking_script,
            });
        }

        let locktime = u32_le(buf, &mut off, "locktime")?;
        Ok(Tx {
            version,
            inputs,
            outputs,
            locktime,
            extended,
        })
    }

    /// Standard (non-extended) serialization — the preimage the txid hashes.
    pub fn standard_bytes(&self) -> Vec<u8> {
        let mut h = Vec::new();
        h.extend_from_slice(&self.version.to_le_bytes());
        varint::append(&mut h, self.inputs.len() as u64);
        for i in &self.inputs {
            h.extend_from_slice(&i.prev_txid);
            h.extend_from_slice(&i.vout.to_le_bytes());
            varint::append(&mut h, i.unlocking_script.len() as u64);
            h.extend_from_slice(&i.unlocking_script);
            h.extend_from_slice(&i.sequence.to_le_bytes());
        }
        varint::append(&mut h, self.outputs.len() as u64);
        for o in &self.outputs {
            h.extend_from_slice(&o.satoshis.to_le_bytes());
            varint::append(&mut h, o.locking_script.len() as u64);
            h.extend_from_slice(&o.locking_script);
        }
        h.extend_from_slice(&self.locktime.to_le_bytes());
        h
    }

    /// Canonical txid = double-SHA256 of the standard serialization (raw wire
    /// byte order; display reverses).
    pub fn txid(&self) -> Hash {
        sha256d(&self.standard_bytes())
    }

    /// Per-input UTXO hash = SHA256( prev_txid ‖ VarInt(vout) ‖ prev_script ‖
    /// VarInt(prev_satoshis) ) — single SHA-256, matching util/utxo_hash.go.
    /// Errors when the input lacks the extended prev-output section (the faithful
    /// analogue of `PreviousTxScript == nil`).
    pub fn utxo_hash_from_input(&self, i: usize) -> Result<Hash, TxError> {
        let inp = self
            .inputs
            .get(i)
            .ok_or(TxError::NonExtendedInput { index: i })?;
        if !inp.extended {
            return Err(TxError::NonExtendedInput { index: i });
        }
        let mut pre = Vec::with_capacity(32 + 9 + inp.prev_script.len() + 9);
        pre.extend_from_slice(&inp.prev_txid);
        varint::append(&mut pre, inp.vout as u64);
        pre.extend_from_slice(&inp.prev_script);
        varint::append(&mut pre, inp.prev_satoshis);
        Ok(sha256(&pre))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn varint_roundtrip_all_classes() {
        for v in [
            0u64,
            0xFC,
            0xFD,
            0xFFFF,
            0x1_0000,
            0xFFFF_FFFF,
            0x1_0000_0000,
            u64::MAX,
        ] {
            let mut out = Vec::new();
            varint::append(&mut out, v);
            let mut off = 0;
            assert_eq!(varint::read(&out, &mut off).unwrap(), v, "value {v:#x}");
            assert_eq!(off, out.len(), "consumed all bytes for {v:#x}");
        }
    }

    #[test]
    fn varint_truncated_is_eof_not_panic() {
        let mut off = 0;
        assert!(matches!(
            varint::read(&[0xFE, 0x01], &mut off),
            Err(TxError::UnexpectedEof { .. })
        ));
    }

    // Helper byte builders (LE) for hand-rolled fixtures.
    fn push_u32(v: &mut Vec<u8>, x: u32) {
        v.extend_from_slice(&x.to_le_bytes());
    }
    fn push_u64(v: &mut Vec<u8>, x: u64) {
        v.extend_from_slice(&x.to_le_bytes());
    }

    /// Build a minimal STANDARD tx: 1 input, 1 output, locktime 7.
    fn standard_tx_bytes() -> Vec<u8> {
        let mut b = Vec::new();
        push_u32(&mut b, 2); // version
        varint::append(&mut b, 1); // input count
        b.extend_from_slice(&[0x11; 32]); // prev txid
        push_u32(&mut b, 3); // vout
        varint::append(&mut b, 2);
        b.extend_from_slice(&[0xAA, 0xBB]); // scriptSig
        push_u32(&mut b, 0xFFFF_FFFF); // sequence
        varint::append(&mut b, 1); // output count
        push_u64(&mut b, 50_000); // satoshis
        varint::append(&mut b, 1);
        b.push(0x6A); // locking script (OP_RETURN)
        push_u32(&mut b, 7); // locktime
        b
    }

    #[test]
    fn parse_standard_tx() {
        let b = standard_tx_bytes();
        let tx = Tx::from_bytes(&b).unwrap();
        assert!(!tx.extended);
        assert_eq!(tx.version, 2);
        assert_eq!(tx.locktime, 7);
        assert_eq!(tx.inputs.len(), 1);
        assert_eq!(tx.inputs[0].prev_txid, [0x11; 32]);
        assert_eq!(tx.inputs[0].vout, 3);
        assert_eq!(tx.inputs[0].unlocking_script, vec![0xAA, 0xBB]);
        assert!(!tx.inputs[0].extended);
        assert_eq!(tx.outputs.len(), 1);
        assert_eq!(tx.outputs[0].satoshis, 50_000);
        // round-trip: standard_bytes reproduces the input bytes exactly
        assert_eq!(tx.standard_bytes(), b);
    }

    /// Same logical tx as standard_tx_bytes but EXTENDED: marker after version, and
    /// the input carries prev_satoshis + prev_script.
    fn extended_tx_bytes() -> Vec<u8> {
        let mut b = Vec::new();
        push_u32(&mut b, 2); // version
        b.extend_from_slice(&[0x00, 0x00, 0x00, 0x00, 0x00, 0xEF]); // extended marker
        varint::append(&mut b, 1); // input count
        b.extend_from_slice(&[0x11; 32]); // prev txid
        push_u32(&mut b, 3); // vout
        varint::append(&mut b, 2);
        b.extend_from_slice(&[0xAA, 0xBB]); // scriptSig
        push_u32(&mut b, 0xFFFF_FFFF); // sequence
        push_u64(&mut b, 99_000); // prev satoshis
        varint::append(&mut b, 3);
        b.extend_from_slice(&[0x76, 0xA9, 0x88]); // prev script
        varint::append(&mut b, 1); // output count
        push_u64(&mut b, 50_000); // satoshis
        varint::append(&mut b, 1);
        b.push(0x6A);
        push_u32(&mut b, 7); // locktime
        b
    }

    #[test]
    fn parse_extended_tx() {
        let tx = Tx::from_bytes(&extended_tx_bytes()).unwrap();
        assert!(tx.extended);
        assert_eq!(tx.inputs.len(), 1);
        let i0 = &tx.inputs[0];
        assert!(i0.extended);
        assert_eq!(i0.prev_satoshis, 99_000);
        assert_eq!(i0.prev_script, vec![0x76, 0xA9, 0x88]);
        // standard_bytes of an extended tx == the standard tx bytes (no marker, no prev fields)
        assert_eq!(tx.standard_bytes(), standard_tx_bytes());
    }

    #[test]
    fn genuine_empty_tx_not_mistaken_for_extended() {
        // version, inputCount=0, outputCount=0, locktime bytes 00 00 00 01 (BE != 0xEF)
        let mut b = Vec::new();
        push_u32(&mut b, 1);
        varint::append(&mut b, 0);
        varint::append(&mut b, 0);
        b.extend_from_slice(&[0x00, 0x00, 0x00, 0x01]); // locktime; BE = 0x01000000 != 0xEF
        let tx = Tx::from_bytes(&b).unwrap();
        assert!(!tx.extended);
        assert!(tx.inputs.is_empty());
        assert!(tx.outputs.is_empty());
        assert_eq!(tx.locktime, 0x0100_0000);
    }

    #[test]
    fn truncated_input_is_eof_not_panic() {
        let mut b = standard_tx_bytes();
        b.truncate(10); // cut mid-input
        assert!(matches!(
            Tx::from_bytes(&b),
            Err(TxError::UnexpectedEof { .. })
        ));
    }

    #[test]
    fn oversized_script_len_is_eof_not_panic() {
        let mut b = Vec::new();
        push_u32(&mut b, 1);
        varint::append(&mut b, 1);
        b.extend_from_slice(&[0x00; 32]);
        push_u32(&mut b, 0);
        b.push(0xFF);
        b.extend_from_slice(&u64::MAX.to_le_bytes()); // script length = u64::MAX
        assert!(matches!(
            Tx::from_bytes(&b),
            Err(TxError::UnexpectedEof { .. })
        ));
    }

    #[test]
    fn utxo_hash_matches_manual_preimage() {
        let tx = Tx::from_bytes(&extended_tx_bytes()).unwrap();
        // manual preimage: prev_txid ‖ varint(vout) ‖ prev_script ‖ varint(satoshis), single sha256
        let mut pre = Vec::new();
        pre.extend_from_slice(&[0x11; 32]);
        varint::append(&mut pre, 3);
        pre.extend_from_slice(&[0x76, 0xA9, 0x88]);
        varint::append(&mut pre, 99_000);
        assert_eq!(
            tx.utxo_hash_from_input(0).unwrap(),
            crate::hash::sha256(&pre)
        );
    }

    #[test]
    fn utxo_hash_non_extended_input_errors() {
        let tx = Tx::from_bytes(&standard_tx_bytes()).unwrap();
        assert_eq!(
            tx.utxo_hash_from_input(0),
            Err(TxError::NonExtendedInput { index: 0 })
        );
    }

    #[test]
    fn utxo_hash_extended_empty_prev_script_ok() {
        // extended input with a zero-length prev script must NOT error (go-bt yields
        // a non-nil empty bscript, so UTXOHash proceeds).
        let mut b = Vec::new();
        push_u32(&mut b, 1);
        b.extend_from_slice(&[0x00, 0x00, 0x00, 0x00, 0x00, 0xEF]);
        varint::append(&mut b, 1);
        b.extend_from_slice(&[0x22; 32]);
        push_u32(&mut b, 0);
        varint::append(&mut b, 0); // empty scriptSig
        push_u32(&mut b, 0);
        push_u64(&mut b, 1); // prev satoshis
        varint::append(&mut b, 0); // empty prev script
        varint::append(&mut b, 0); // 0 outputs
        push_u32(&mut b, 0); // locktime
        let tx = Tx::from_bytes(&b).unwrap();
        assert!(tx.utxo_hash_from_input(0).is_ok());
    }
}

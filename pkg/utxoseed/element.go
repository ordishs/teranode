// Package utxoseed defines the frozen canonical byte layout of a UTXO as fed
// into the MuHash set commitment. This layout MUST NOT change without a format
// version bump, because it changes every commitment value.
package utxoseed

import (
	"encoding/binary"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
)

// CommitmentVersion identifies this frozen commitment construction: the Element
// byte layout below together with the MuHash3072 element mapping. Bump it on ANY
// change to either. A signed checkpoint carries this value, so a consumer built
// for a different version refuses the seed up front instead of silently failing
// the set-hash check.
const CommitmentVersion uint32 = 1

// Element serializes a single UTXO into its canonical commitment bytes:
//
//	txid(32) | vout(4 LE) | (height<<1 | coinbase)(4 LE) | value(8 LE) | scriptLen(4 LE) | script
//
// txid is written in chainhash internal byte order. coinbase occupies the
// least-significant bit of the height word, so height must be < 2^31; higher
// values would overflow the shift and alias distinct UTXOs to the same element.
func Element(txid chainhash.Hash, vout, height uint32, coinbase bool, value uint64, script []byte) []byte {
	buf := make([]byte, 0, 32+4+4+8+4+len(script))
	buf = append(buf, txid[:]...)
	buf = binary.LittleEndian.AppendUint32(buf, vout)

	heightWord := height << 1
	if coinbase {
		heightWord |= 1
	}

	buf = binary.LittleEndian.AppendUint32(buf, heightWord)

	buf = binary.LittleEndian.AppendUint64(buf, value)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(script)))
	buf = append(buf, script...)

	return buf
}

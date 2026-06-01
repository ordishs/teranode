// Package seedcheckpoint signs and verifies the compact UTXO-set commitment
// checkpoint (height, blockHash, setHash) that a new miner trusts when verifying
// a downloaded seed.
package seedcheckpoint

import (
	"bytes"
	"encoding/binary"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/teranode/errors"
)

// FormatVersion is the signed-checkpoint serialization version.
const FormatVersion uint32 = 1

const (
	pubKeyLen       = 33
	signedHeaderLen = 4 + 4 + 32 + 32 + pubKeyLen + 2 // version, height, blockHash, setHash, pubkey, sigLen = 107
	maxSigLen       = 80                              // DER secp256k1 signatures are <= ~72 bytes
)

// Checkpoint is the signed triple committing to the UTXO set at a block.
type Checkpoint struct {
	Height    uint32
	BlockHash chainhash.Hash
	SetHash   [32]byte
}

// digest returns the 32-byte message hash that is signed: the double-SHA256 of
// height(4 LE) | blockHash(32) | setHash(32). This layout is frozen.
func (c Checkpoint) digest() []byte {
	msg := make([]byte, 0, 4+32+32)
	msg = binary.LittleEndian.AppendUint32(msg, c.Height)
	msg = append(msg, c.BlockHash[:]...)
	msg = append(msg, c.SetHash[:]...)

	return chainhash.DoubleHashB(msg)
}

// SignedCheckpoint is a checkpoint plus the signer's compressed public key and
// the DER-encoded secp256k1 signature over the checkpoint digest.
type SignedCheckpoint struct {
	Checkpoint Checkpoint
	PubKey     [pubKeyLen]byte
	Sig        []byte
}

// Sign produces a SignedCheckpoint for c using priv (secp256k1, RFC6979).
func Sign(priv *bec.PrivateKey, c Checkpoint) (*SignedCheckpoint, error) {
	sig, err := priv.Sign(c.digest())
	if err != nil {
		return nil, errors.NewProcessingError("error signing checkpoint", err)
	}

	var pub [pubKeyLen]byte
	copy(pub[:], priv.PubKey().Compressed())

	return &SignedCheckpoint{Checkpoint: c, PubKey: pub, Sig: sig.Serialize()}, nil
}

// Verify checks that the signature is valid for the embedded public key and
// checkpoint. It does NOT establish that the public key is a trusted authority.
func (sc *SignedCheckpoint) Verify() error {
	pub, err := bec.ParsePubKey(sc.PubKey[:])
	if err != nil {
		return errors.NewProcessingError("invalid checkpoint public key", err)
	}

	sig, err := bec.ParseDERSignature(sc.Sig)
	if err != nil {
		return errors.NewProcessingError("invalid checkpoint signature encoding", err)
	}

	if !sig.Verify(sc.Checkpoint.digest(), pub) {
		return errors.NewProcessingError("checkpoint signature does not verify")
	}

	return nil
}

// VerifyWithKey checks the signature AND that it was made by trustedPubKey
// (a 33-byte compressed key). This is the check a seed consumer performs.
func (sc *SignedCheckpoint) VerifyWithKey(trustedPubKey []byte) error {
	if !bytes.Equal(sc.PubKey[:], trustedPubKey) {
		return errors.NewProcessingError("checkpoint signed by untrusted key")
	}

	return sc.Verify()
}

// Serialize encodes the signed checkpoint as:
//
//	version(4 LE) | height(4 LE) | blockHash(32) | setHash(32) | pubkey(33) | sigLen(2 LE) | sig
func (sc *SignedCheckpoint) Serialize() []byte {
	out := make([]byte, 0, signedHeaderLen+len(sc.Sig))
	out = binary.LittleEndian.AppendUint32(out, FormatVersion)
	out = binary.LittleEndian.AppendUint32(out, sc.Checkpoint.Height)
	out = append(out, sc.Checkpoint.BlockHash[:]...)
	out = append(out, sc.Checkpoint.SetHash[:]...)
	out = append(out, sc.PubKey[:]...)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(sc.Sig)))
	out = append(out, sc.Sig...)

	return out
}

// ParseSignedCheckpoint decodes and structurally validates a signed checkpoint.
func ParseSignedCheckpoint(b []byte) (*SignedCheckpoint, error) {
	if len(b) < signedHeaderLen {
		return nil, errors.NewProcessingError("signed checkpoint too short: %d bytes", len(b))
	}

	version := binary.LittleEndian.Uint32(b[0:4])
	if version != FormatVersion {
		return nil, errors.NewProcessingError("unsupported signed checkpoint version %d", version)
	}

	sc := &SignedCheckpoint{}
	sc.Checkpoint.Height = binary.LittleEndian.Uint32(b[4:8])
	copy(sc.Checkpoint.BlockHash[:], b[8:40])
	copy(sc.Checkpoint.SetHash[:], b[40:72])
	copy(sc.PubKey[:], b[72:105])

	sigLen := int(binary.LittleEndian.Uint16(b[105:107]))
	if sigLen == 0 || sigLen > maxSigLen {
		return nil, errors.NewProcessingError("invalid checkpoint signature length %d", sigLen)
	}

	if len(b) != signedHeaderLen+sigLen {
		return nil, errors.NewProcessingError("signed checkpoint length %d, expected %d", len(b), signedHeaderLen+sigLen)
	}

	sc.Sig = make([]byte, sigLen)
	copy(sc.Sig, b[signedHeaderLen:])

	return sc, nil
}

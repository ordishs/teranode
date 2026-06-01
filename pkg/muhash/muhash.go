// Package muhash implements MuHash3072, a deterministic, order-independent
// (multiset) hash of a set of byte strings. Elements can be added and removed
// in any order; the Digest depends only on the multiset of current elements.
package muhash

import (
	"fmt"
	"math/big"
)

// MuHash3072 is a multiset hash over the group of integers modulo a 3072-bit
// prime. It is NOT safe for concurrent use; callers must synchronize access.
type MuHash3072 struct {
	numerator   *big.Int // product of added elements
	denominator *big.Int // product of removed elements
}

// New returns an accumulator representing the empty set.
func New() *MuHash3072 {
	return &MuHash3072{numerator: big.NewInt(1), denominator: big.NewInt(1)}
}

// Add inserts data into the multiset.
func (m *MuHash3072) Add(data []byte) {
	m.numerator = mulMod(m.numerator, elementToNum(data))
}

// Remove deletes data from the multiset. Removing an element that was never
// added is well-defined (it becomes a denominator factor) and is exactly
// cancelled by a later Add of the same element.
func (m *MuHash3072) Remove(data []byte) {
	m.denominator = mulMod(m.denominator, elementToNum(data))
}

// Digest returns the 32-byte commitment: SHA256 of the little-endian encoding
// of numerator * denominator^-1 mod modulus.
func (m *MuHash3072) Digest() [32]byte {
	inv := new(big.Int).ModInverse(m.denominator, modulus)
	res := mulMod(m.numerator, inv)

	return sha256Sum(numToBytes(res))
}

// Bytes serializes the accumulator state as numerator||denominator, each a
// fixed numBytes little-endian integer (2*numBytes total).
func (m *MuHash3072) Bytes() []byte {
	out := make([]byte, 0, 2*numBytes)
	out = append(out, numToBytes(m.numerator)...)
	out = append(out, numToBytes(m.denominator)...)

	return out
}

// FromBytes restores an accumulator previously produced by Bytes.
func FromBytes(b []byte) (*MuHash3072, error) {
	if len(b) != 2*numBytes {
		return nil, fmt.Errorf("muhash: expected %d bytes, got %d", 2*numBytes, len(b))
	}

	return &MuHash3072{
		numerator:   bytesToNum(b[:numBytes]),
		denominator: bytesToNum(b[numBytes:]),
	}, nil
}

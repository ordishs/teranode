package muhash

import "math/big"

// numBytes is the byte length of a 3072-bit group element (3072 / 8).
const numBytes = 384

// modulus is the 3072-bit prime M = 2^3072 - 1103717. The accumulator operates
// in the multiplicative group of integers modulo M.
var modulus = func() *big.Int {
	m := new(big.Int).Lsh(big.NewInt(1), 3072)
	return m.Sub(m, big.NewInt(1103717))
}()

// mulMod returns (a * b) mod modulus.
func mulMod(a, b *big.Int) *big.Int {
	r := new(big.Int).Mul(a, b)
	return r.Mod(r, modulus)
}

// numToBytes serializes x as a fixed-width little-endian byte slice of numBytes.
func numToBytes(x *big.Int) []byte {
	be := x.FillBytes(make([]byte, numBytes)) // big-endian, left zero-padded
	le := make([]byte, numBytes)
	for i := 0; i < numBytes; i++ {
		le[i] = be[numBytes-1-i]
	}
	return le
}

// bytesToNum interprets a little-endian byte slice as an integer reduced mod modulus.
func bytesToNum(le []byte) *big.Int {
	be := make([]byte, len(le))
	for i := range le {
		be[len(le)-1-i] = le[i]
	}
	x := new(big.Int).SetBytes(be)
	return x.Mod(x, modulus)
}

package muhash

import (
	"crypto/sha256"
	"math/big"

	"golang.org/x/crypto/chacha20"
)

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

// elementToNum maps arbitrary data to a group element in [0, modulus).
// Construction: key = SHA256(data); generate a numBytes-long ChaCha20 keystream
// under that key with an all-zero 12-byte nonce and counter 0; interpret the
// keystream as a little-endian integer reduced mod modulus. This construction
// is frozen — changing it changes every commitment.
func elementToNum(data []byte) *big.Int {
	key := sha256.Sum256(data)

	var nonce [12]byte

	c, err := chacha20.NewUnauthenticatedCipher(key[:], nonce[:])
	if err != nil {
		// key is always 32 bytes and nonce 12 bytes, so this cannot happen.
		panic(err)
	}

	buf := make([]byte, numBytes)
	c.XORKeyStream(buf, buf) // buf is zero-filled, so output is the raw keystream

	return bytesToNum(buf)
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

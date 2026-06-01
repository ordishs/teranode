package muhash

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModulusValue(t *testing.T) {
	// M = 2^3072 - 1103717
	expected := new(big.Int).Lsh(big.NewInt(1), 3072)
	expected.Sub(expected, big.NewInt(1103717))
	require.Equal(t, 0, modulus.Cmp(expected))
}

func TestMulModCommutes(t *testing.T) {
	a := big.NewInt(123456789)
	b := big.NewInt(987654321)
	require.Equal(t, 0, mulMod(a, b).Cmp(mulMod(b, a)))
}

func TestNumBytesRoundTrip(t *testing.T) {
	x := big.NewInt(0)
	x.SetString("123456789012345678901234567890", 10)
	le := numToBytes(x)
	require.Len(t, le, numBytes)
	got := bytesToNum(le)
	require.Equal(t, 0, x.Cmp(got))
}

func TestNumToBytesIsLittleEndian(t *testing.T) {
	le := numToBytes(big.NewInt(1))
	require.Equal(t, byte(1), le[0])
	for i := 1; i < numBytes; i++ {
		require.Equal(t, byte(0), le[i])
	}
}

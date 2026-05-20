package txmetacache

import (
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/cespare/xxhash/v2"
	"github.com/stretchr/testify/require"
)

func hashN(n uint64) *chainhash.Hash {
	var h chainhash.Hash
	for i := 0; i < 8; i++ {
		h[i] = byte(n >> (i * 8))
	}

	return &h
}

func TestPointerCache_SetGetRoundTrip(t *testing.T) {
	c, err := NewPointerCache(64 * 1024 * 1024)
	require.NoError(t, err)

	h := hashN(42)
	d := &meta.Data{Fee: 1234, SizeInBytes: 250, IsCoinbase: false}

	require.NoError(t, c.Set(h, d))

	got, ok := c.Get(*h)
	require.True(t, ok)
	require.Same(t, d, got, "pointer cache must return the same pointer that was stored")
}

func TestPointerCache_MissReturnsFalse(t *testing.T) {
	c, err := NewPointerCache(64 * 1024 * 1024)
	require.NoError(t, err)

	got, ok := c.Get(*hashN(99))
	require.False(t, ok)
	require.Nil(t, got)
}

func TestPointerCache_OverwriteDoesNotConsumeRingSlot(t *testing.T) {
	c, err := NewPointerCache(64 * 1024 * 1024)
	require.NoError(t, err)

	h := hashN(7)
	d1 := &meta.Data{Fee: 1}
	d2 := &meta.Data{Fee: 2}

	require.NoError(t, c.Set(h, d1))
	require.NoError(t, c.Set(h, d2))

	got, ok := c.Get(*h)
	require.True(t, ok)
	require.Same(t, d2, got)

	b := c.bucketFor(xxhash.Sum64(h[:]))
	require.Equal(t, uint64(1), b.added.Load(),
		"overwriting an existing key must not increment insertion count")
}

func TestPointerCache_FIFOEvictionWhenRingFull(t *testing.T) {
	// Force tiny per-bucket capacity by sizing maxBytes so that
	// maxBytes / BucketsCount / pointerCacheAvgEntryBytes == 1.
	maxBytes := BucketsCount * pointerCacheAvgEntryBytes
	c, err := NewPointerCache(maxBytes)
	require.NoError(t, err)

	// Find two distinct keys that collide on the same bucket so we can
	// observe eviction deterministically.
	var first, second *chainhash.Hash

	target := xxhash.Sum64(hashN(0)[:]) % BucketsCount

	for n := uint64(1); n < 1_000_000; n++ {
		h := hashN(n)
		if xxhash.Sum64(h[:])%BucketsCount != target {
			continue
		}

		if first == nil {
			first = h
		} else {
			second = h
			break
		}
	}

	require.NotNil(t, first)
	require.NotNil(t, second)

	d1 := &meta.Data{Fee: 100}
	d2 := &meta.Data{Fee: 200}

	require.NoError(t, c.Set(hashN(0), &meta.Data{Fee: 1})) // primes bucket
	require.NoError(t, c.Set(first, d1))                    // either evicts the prime or coexists, depending on bucket
	require.NoError(t, c.Set(second, d2))                   // capacity is 1; second must evict whatever's there

	// d2 must still be present
	got, ok := c.Get(*second)
	require.True(t, ok)
	require.Same(t, d2, got)

	// The bucket now has at most one entry
	b := c.bucketFor(xxhash.Sum64(second[:]))
	b.mu.RLock()
	require.LessOrEqual(t, len(b.m), 1, "ring size is 1; bucket must not exceed one live entry")
	b.mu.RUnlock()
	require.GreaterOrEqual(t, b.evicted.Load(), uint64(1))
}

func TestPointerCache_Del(t *testing.T) {
	c, err := NewPointerCache(64 * 1024 * 1024)
	require.NoError(t, err)

	h := hashN(5)
	_ = c.Set(h, &meta.Data{Fee: 9})
	c.Del(h[:])

	_, ok := c.Get(*h)
	require.False(t, ok)
}

func TestPointerCache_Reset(t *testing.T) {
	c, err := NewPointerCache(64 * 1024 * 1024)
	require.NoError(t, err)

	for n := uint64(0); n < 100; n++ {
		require.NoError(t, c.Set(hashN(n), &meta.Data{Fee: n}))
	}

	c.Reset()

	for n := uint64(0); n < 100; n++ {
		_, ok := c.Get(*hashN(n))
		require.False(t, ok, "Reset must clear every entry; key %d still present", n)
	}
}

func TestPointerCache_UpdateStats(t *testing.T) {
	c, err := NewPointerCache(64 * 1024 * 1024)
	require.NoError(t, err)

	for n := uint64(0); n < 50; n++ {
		require.NoError(t, c.Set(hashN(n), &meta.Data{Fee: n}))
	}

	var s Stats
	c.UpdateStats(&s)
	require.Equal(t, uint64(50), s.ValidEntriesCount)
	require.Equal(t, uint64(50), s.TotalElementsAdded)
}

func TestPointerCache_ConcurrentReadersAndWriters(t *testing.T) {
	c, err := NewPointerCache(64 * 1024 * 1024)
	require.NoError(t, err)

	const writers = 4
	const readers = 16
	const keysPerWriter = 1000

	var wg sync.WaitGroup
	wg.Add(writers)

	for w := 0; w < writers; w++ {
		w := w
		go func() {
			defer wg.Done()
			base := uint64(w) * keysPerWriter

			for i := uint64(0); i < keysPerWriter; i++ {
				_ = c.Set(hashN(base+i), &meta.Data{Fee: base + i})
			}
		}()
	}

	wg.Wait()

	// All writes complete before readers start, so every key must be present
	// (capacity is huge — no eviction).
	wg.Add(readers)

	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()

			for w := 0; w < writers; w++ {
				base := uint64(w) * keysPerWriter
				for i := uint64(0); i < keysPerWriter; i++ {
					d, ok := c.Get(*hashN(base + i))
					require.True(t, ok)
					require.NotNil(t, d)
					require.Equal(t, base+i, d.Fee)
				}
			}
		}()
	}

	wg.Wait()
}

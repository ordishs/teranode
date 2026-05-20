package txmetacache

import (
	"sync"
	"sync/atomic"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/cespare/xxhash/v2"
)

// pointerCacheAvgEntryBytes is the heap-cost estimate per cached entry used to
// translate the configured MB budget into a fixed per-bucket capacity. It must
// account for the meta.Data struct, average TxInpoints payload, map and ring
// overhead. Refined empirically by Phase B benchmarks; today's value is a
// deliberately conservative placeholder.
const pointerCacheAvgEntryBytes = 300

// Compile-time check that *PointerCache satisfies the cacheBackend contract.
var _ cacheBackend = (*PointerCache)(nil)

// pointerBucket holds one shard of the PointerCache: a Go map of pointer-valued
// entries plus a fixed FIFO ring of inserted keys for eviction.
type pointerBucket struct {
	mu      sync.RWMutex
	m       map[uint64]*meta.Data
	ring    []uint64
	head    int
	full    bool
	added   atomic.Uint64
	evicted atomic.Uint64
}

// PointerCache is a sharded, FIFO-evicting cache that stores *meta.Data
// pointers directly — no serialization on write, no deserialization on read.
// Trade-off: every live entry is a reachable Go heap allocation, scanned by GC.
//
// Sharding mirrors ImprovedCache (BucketsCount buckets selected by xxhash mod).
// Each bucket has its own RWMutex; the ring is per-bucket so eviction is
// strictly per-shard FIFO (not global FIFO).
//
// Set replaces an existing entry in place (no new ring slot); inserting a
// new key when the ring is full evicts the oldest key in that bucket.
//
// Caller contract: once handed to Set (or via SetFromBytes), the *meta.Data
// must not be mutated — readers may obtain the same pointer concurrently.
type PointerCache struct {
	buckets [BucketsCount]*pointerBucket
}

// NewPointerCache constructs a PointerCache sized to fit roughly maxBytes of
// live entries, distributed evenly across BucketsCount shards. The capacity
// per shard is fixed at construction (FIFO ring is preallocated).
func NewPointerCache(maxBytes int) (*PointerCache, error) {
	if maxBytes <= 0 {
		return nil, errors.NewServiceError("maxBytes must be greater than 0; got %d", maxBytes)
	}

	perBucketEntries := maxBytes / BucketsCount / pointerCacheAvgEntryBytes
	if perBucketEntries < 1 {
		perBucketEntries = 1
	}

	var c PointerCache
	for i := range c.buckets {
		c.buckets[i] = &pointerBucket{
			m:    make(map[uint64]*meta.Data, perBucketEntries),
			ring: make([]uint64, perBucketEntries),
		}
	}

	return &c, nil
}

func (c *PointerCache) bucketFor(k uint64) *pointerBucket {
	return c.buckets[k%BucketsCount]
}

// insert is the shared insertion path used by Set (pointer-native) and
// SetFromBytes (byte adapter). The caller has already hashed the key.
func (c *PointerCache) insert(k uint64, data *meta.Data) {
	b := c.bucketFor(k)

	b.mu.Lock()

	if _, exists := b.m[k]; exists {
		b.m[k] = data
		b.mu.Unlock()
		return
	}

	if b.full {
		evict := b.ring[b.head]
		delete(b.m, evict)
		b.evicted.Add(1)
	}

	b.ring[b.head] = k
	b.head++

	if b.head >= len(b.ring) {
		b.head = 0
		b.full = true
	}

	b.m[k] = data
	b.added.Add(1)
	b.mu.Unlock()
}

// Set is the pointer-native insert — the caller already has a *meta.Data and
// the cache stores the pointer without serialisation. Implements cacheBackend.
func (c *PointerCache) Set(hash *chainhash.Hash, data *meta.Data) error {
	c.insert(xxhash.Sum64(hash[:]), data)
	return nil
}

// Get returns the cached *meta.Data for hash, or (nil, false) if absent.
// The returned pointer is shared with every other concurrent reader and the
// cache itself — callers must treat it as read-only.
// Implements cacheBackend.
func (c *PointerCache) Get(hash chainhash.Hash) (*meta.Data, bool) {
	k := xxhash.Sum64(hash[:])
	b := c.bucketFor(k)

	b.mu.RLock()
	d, ok := b.m[k]
	b.mu.RUnlock()

	return d, ok
}

// SetFromBytes is the byte-oriented bridge: deserialises value into a fresh
// *meta.Data and inserts it. Implements cacheBackend.
func (c *PointerCache) SetFromBytes(key, value []byte) error {
	hash, err := chainhash.NewHash(key)
	if err != nil {
		return errors.NewProcessingError("invalid cache key", err)
	}

	data := &meta.Data{}
	if err := meta.NewMetaDataFromBytes(value, data); err != nil {
		return errors.NewProcessingError("failed to unmarshal txMeta bytes for pointer cache", err)
	}

	c.insert(xxhash.Sum64(hash[:]), data)

	return nil
}

// SetMultiFromBytes applies SetFromBytes to each (key, value) pair.
// Implements cacheBackend.
func (c *PointerCache) SetMultiFromBytes(keys, values [][]byte) error {
	if len(keys) != len(values) {
		return errors.NewProcessingError("keys and values length mismatch; got %d keys and %d values", len(keys), len(values))
	}

	for i := range keys {
		if err := c.SetFromBytes(keys[i], values[i]); err != nil {
			return err
		}
	}

	return nil
}

// SetMultiFromBytesSequential is functionally identical to SetMultiFromBytes
// — PointerCache's batch path is already serial. Provided so the
// cacheBackend dispatch can stay uniform across backends.
// Implements cacheBackend.
func (c *PointerCache) SetMultiFromBytesSequential(keys, values [][]byte) error {
	return c.SetMultiFromBytes(keys, values)
}

// SetMultiFromBytesSequentialWithHashes ignores the supplied hashes — the
// pointer cache hashes the raw bytes itself via the same xxhash call in
// SetFromBytes, and the perf delta of reusing the wire hash here is below
// noise for the pointer path (which is allocation-free regardless).
// Implements cacheBackend.
func (c *PointerCache) SetMultiFromBytesSequentialWithHashes(keys, values [][]byte, _ []uint64) error {
	return c.SetMultiFromBytes(keys, values)
}

// Del removes the entry under key. The ring slot is not reclaimed eagerly —
// it will be passed over on its next eviction turn (bounded by ring size).
// Implements cacheBackend.
func (c *PointerCache) Del(key []byte) {
	k := xxhash.Sum64(key)
	b := c.bucketFor(k)

	b.mu.Lock()
	delete(b.m, k)
	b.mu.Unlock()
}

// Reset clears every bucket and resets the FIFO state.
func (c *PointerCache) Reset() {
	for _, b := range c.buckets {
		b.mu.Lock()

		ringLen := len(b.ring)
		b.m = make(map[uint64]*meta.Data, ringLen)

		for i := range b.ring {
			b.ring[i] = 0
		}

		b.head = 0
		b.full = false
		b.mu.Unlock()
	}
}

// UpdateStats populates s with current PointerCache totals so it can be
// reported through the existing txmetacache Prometheus pipeline. Fields that
// do not map cleanly onto FIFO-ring semantics (TrimCount, PreviousGenEntries)
// are left untouched.
// Implements cacheBackend.
func (c *PointerCache) UpdateStats(s *Stats) {
	var entries, totalAdded uint64

	for _, b := range c.buckets {
		b.mu.RLock()
		entries += uint64(len(b.m))
		b.mu.RUnlock()
		totalAdded += b.added.Load()
	}

	s.ValidEntriesCount += entries
	s.TotalMapSize += entries
	s.TotalElementsAdded += totalAdded
	s.CurrentGenEntries += entries
}

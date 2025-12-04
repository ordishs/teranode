package subtreeprocessor

import (
	"sync"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
	"github.com/dolthub/swiss"
)

type SplitSwissMap struct {
	m           map[uint16]*txmap.SwissMap
	nrOfBuckets uint16
}

var _ txmap.TxHashMap = (*SplitSwissMap)(nil)

func NewSplitSwissMap(nrOfBuckets uint16, length uint32) *SplitSwissMap {
	m := make(map[uint16]*txmap.SwissMap, nrOfBuckets)
	for i := uint16(0); i < nrOfBuckets; i++ {
		m[i] = txmap.NewSwissMap(length / uint32(nrOfBuckets))
	}

	return &SplitSwissMap{
		m:           m,
		nrOfBuckets: nrOfBuckets,
	}
}

func (s SplitSwissMap) Delete(hash chainhash.Hash) error {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Delete(hash)
}

func (s SplitSwissMap) Exists(hash chainhash.Hash) bool {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Exists(hash)
}

func (s SplitSwissMap) Get(hash chainhash.Hash) (uint64, bool) {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Get(hash)
}

func (s SplitSwissMap) Keys() []chainhash.Hash {
	keys := make([]chainhash.Hash, 0, 1024)

	for _, swissMap := range s.m {
		keys = append(keys, swissMap.Keys()...)
	}

	return keys
}

func (s SplitSwissMap) Length() int {
	length := 0

	for _, swissMap := range s.m {
		length += swissMap.Length()
	}

	return length
}

func (s SplitSwissMap) Put(hash chainhash.Hash) error {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Put(hash)
}

func (s SplitSwissMap) PutMulti(hashes []chainhash.Hash) error {
	for _, hash := range hashes {
		if err := s.Put(hash); err != nil {
			return err
		}
	}

	return nil
}

func (s SplitSwissMap) Set(hash chainhash.Hash) error {
	return s.Put(hash)
}

func (s SplitSwissMap) Iter(f func(hash chainhash.Hash, value uint64) bool) {
	for _, swissMap := range s.m {
		swissMap.Iter(f)
	}
}

type SplitTxInpointsMap struct {
	m           map[uint16]*SwissMap
	nrOfBuckets uint16
}

func NewSplitTxInpointsMap(size uint32, nrOfBuckets uint16) *SplitTxInpointsMap {
	m := make(map[uint16]*SwissMap, nrOfBuckets)
	for i := uint16(0); i < nrOfBuckets; i++ {
		m[i] = NewSwissMap(size)
	}

	return &SplitTxInpointsMap{
		m:           m,
		nrOfBuckets: nrOfBuckets,
	}
}

func (s *SplitTxInpointsMap) Delete(hash chainhash.Hash) bool {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Delete(hash)
}

func (s *SplitTxInpointsMap) Exists(hash chainhash.Hash) bool {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Exists(hash)
}

func (s *SplitTxInpointsMap) Get(hash chainhash.Hash) (*subtreepkg.TxInpoints, bool) {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Get(hash)
}

func (s *SplitTxInpointsMap) Length() int {
	length := 0

	for _, syncedMap := range s.m {
		length += syncedMap.Length()
	}

	return length
}

func (s *SplitTxInpointsMap) Set(hash chainhash.Hash, inpoints *subtreepkg.TxInpoints) {
	s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Set(hash, inpoints)
}

func (s *SplitTxInpointsMap) SetIfNotExists(hash chainhash.Hash, inpoints *subtreepkg.TxInpoints) (*subtreepkg.TxInpoints, bool) {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].SetIfNotExists(hash, inpoints)
}

func (s *SplitTxInpointsMap) Clear() {
	for _, syncedMap := range s.m {
		syncedMap.Clear()
	}
}

// SwissMap is a simple concurrent-safe map that uses the swiss package
type SwissMap struct {
	mu     sync.RWMutex
	m      *swiss.Map[chainhash.Hash, *subtreepkg.TxInpoints]
	length int
}

// NewSwissMap creates a new SwissMap with the specified initial length.
// The length is used to preallocate the map size for better performance.
// It is not a hard limit, but a hint to the underlying swiss map.
//
// Params:
//   - length: The initial length of the map, used for preallocation.
//
// Returns:
//   - *SwissMap: A pointer to the newly created SwissMap instance.
//
// Considerations: The length is not enforced, and the map can grow beyond this size.
func NewSwissMap(length uint32) *SwissMap {
	return &SwissMap{
		m: swiss.NewMap[chainhash.Hash, *subtreepkg.TxInpoints](length),
	}
}

// Exists checks if the given hash exists in the map.
// It returns true if the hash is found, false otherwise.
//
// Params:
//   - hash: The hash to check for existence in the map.
//
// Returns:
//   - bool: True if the hash exists in the map, false otherwise.
func (s *SwissMap) Exists(hash chainhash.Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.m.Get(hash)

	return ok
}

// Get retrieves the value associated with the given hash from the map.
// It always returns 0 and a boolean indicating whether the hash was found.
//
// Params:
//   - hash: The hash to retrieve from the map.
//
// Returns:
//   - *subtreepkg.TxInpoints: Always returns nil in this implementation.
//   - bool: True if the hash was found in the map, false otherwise.
func (s *SwissMap) Get(hash chainhash.Hash) (*subtreepkg.TxInpoints, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.m.Get(hash)
}

// Set adds a new hash to the map. It increments the length of the map.
//
// Params:
//   - hash: The hash to add to the map.
//
// Returns:
//   - error: always returns nil, as this map does not have any constraints on adding hashes.
func (s *SwissMap) Set(hash chainhash.Hash, value *subtreepkg.TxInpoints) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.length++

	s.m.Put(hash, value)
}

// SetIfNotExists adds a new hash to the map only if it does not already exist.
// It increments the length of the map if the hash was added.
//
// Params:
//   - hash: The hash to add to the map.
//   - value: The value associated with the hash.
//
// Returns:
//   - *subtreepkg.TxInpoints: The existing value if the hash already exists, or nil if it was added.
//   - bool: True if the hash was added, false if it already existed.
func (s *SwissMap) SetIfNotExists(hash chainhash.Hash, value *subtreepkg.TxInpoints) (*subtreepkg.TxInpoints, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existingValue, exists := s.m.Get(hash)
	if exists {
		return existingValue, false
	}

	s.m.Put(hash, value)
	s.length++

	return nil, true
}

// PutMulti adds multiple hashes to the map. It increments the length of the map for each hash added.
//
// Params:
//   - hashes: A slice of hashes to add to the map.
//
// Returns:
//   - error: always returns nil, as this map does not have any constraints on adding hashes.
func (s *SwissMap) PutMulti(hashes []chainhash.Hash, values []*subtreepkg.TxInpoints) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for idx, hash := range hashes {
		s.m.Put(hash, values[idx])

		s.length++
	}

	return nil
}

// Delete removes a hash from the map. It decrements the length of the map.
//
// Params:
//   - hash: The hash to remove from the map.
//
// Returns:
//   - error: always returns nil, as this map does not have any constraints on deleting hashes.
func (s *SwissMap) Delete(hash chainhash.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.length--

	s.m.Delete(hash)

	return true
}

// Length returns the current number of hashes in the map.
//
// Returns:
//   - int: The number of hashes currently stored in the map.
func (s *SwissMap) Length() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.length
}

// Keys returns a slice of all hashes currently stored in the map.
// It iterates over the map and collects the keys.
// The order of keys is not guaranteed.
//
// Returns:
//   - []chainhash.Hash: A slice containing all the hashes in the map.
func (s *SwissMap) Keys() []chainhash.Hash {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]chainhash.Hash, 0, s.length)

	s.m.Iter(func(k chainhash.Hash, _ *subtreepkg.TxInpoints) (stop bool) {
		keys = append(keys, k)
		return false
	})

	return keys
}

// Iter iterates over all key-value pairs in the map and applies the provided function to each pair.
// Stops iterating if the function returns true.
//
// Params:
//   - f: A function that takes a hash and its associated value (always 0 in this map).
func (s *SwissMap) Iter(f func(hash chainhash.Hash, value *subtreepkg.TxInpoints) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	s.m.Iter(func(k chainhash.Hash, v *subtreepkg.TxInpoints) (stop bool) {
		return f(k, v)
	})
}

// Clear removes all entries from the map and resets its length to zero.
func (s *SwissMap) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.m = swiss.NewMap[chainhash.Hash, *subtreepkg.TxInpoints](1024 * 1024)
	s.length = 0
}

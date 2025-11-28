package subtreeprocessor

import (
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
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
	m           map[uint16]*txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints]
	nrOfBuckets uint16
}

func NewSplitTxInpointsMap(nrOfBuckets uint16) *SplitTxInpointsMap {
	m := make(map[uint16]*txmap.SyncedMap[chainhash.Hash, subtreepkg.TxInpoints], nrOfBuckets)
	for i := uint16(0); i < nrOfBuckets; i++ {
		m[i] = txmap.NewSyncedMap[chainhash.Hash, subtreepkg.TxInpoints]()
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

func (s *SplitTxInpointsMap) Get(hash chainhash.Hash) (subtreepkg.TxInpoints, bool) {
	return s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Get(hash)
}

func (s *SplitTxInpointsMap) Length() int {
	length := 0

	for _, syncedMap := range s.m {
		length += syncedMap.Length()
	}

	return length
}

func (s *SplitTxInpointsMap) Set(hash chainhash.Hash, inpoints subtreepkg.TxInpoints) {
	s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Set(hash, inpoints)
}

func (s *SplitTxInpointsMap) SetIfNotExists(hash chainhash.Hash, inpoints subtreepkg.TxInpoints) (subtreepkg.TxInpoints, bool) {
	if existingValue, ok := s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Get(hash); ok {
		return existingValue, false
	}

	s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Set(hash, inpoints)

	return inpoints, true
}

func (s *SplitTxInpointsMap) Clear() {
	for _, syncedMap := range s.m {
		syncedMap.Clear()
	}
}

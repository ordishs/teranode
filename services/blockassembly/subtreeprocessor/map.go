package subtreeprocessor

import (
	"sync"
	"sync/atomic"

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
	m           map[uint16]*sync.Map
	l           map[uint16]*atomic.Uint64
	nrOfBuckets uint16
}

func NewSplitTxInpointsMap(nrOfBuckets uint16) *SplitTxInpointsMap {
	m := make(map[uint16]*sync.Map, nrOfBuckets)
	l := make(map[uint16]*atomic.Uint64, nrOfBuckets)

	for i := uint16(0); i < nrOfBuckets; i++ {
		m[i] = &sync.Map{}
		l[i] = &atomic.Uint64{}
	}

	return &SplitTxInpointsMap{
		m:           m,
		l:           l,
		nrOfBuckets: nrOfBuckets,
	}
}

func (s *SplitTxInpointsMap) Delete(hash chainhash.Hash) bool {
	s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Delete(hash)
	s.l[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Add(^uint64(0))
	return true
}

func (s *SplitTxInpointsMap) Exists(hash chainhash.Hash) bool {
	_, ok := s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Load(hash)

	return ok
}

func (s *SplitTxInpointsMap) Get(hash chainhash.Hash) (subtreepkg.TxInpoints, bool) {
	v, ok := s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Load(hash)

	if !ok {
		return v.(subtreepkg.TxInpoints), false
	}

	return v.(subtreepkg.TxInpoints), true
}

func (s *SplitTxInpointsMap) Length() int {
	length := 0

	for i := uint16(0); i < s.nrOfBuckets; i++ {
		length += int(s.l[i].Load())
	}

	return length
}

func (s *SplitTxInpointsMap) Set(hash chainhash.Hash, inpoints subtreepkg.TxInpoints) {
	s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Store(hash, inpoints)
	s.l[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Add(1)
}

func (s *SplitTxInpointsMap) SetIfNotExists(hash chainhash.Hash, inpoints subtreepkg.TxInpoints) (subtreepkg.TxInpoints, bool) {
	if v, ok := s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Load(hash); ok {
		return v.(subtreepkg.TxInpoints), false
	}

	s.m[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Store(hash, inpoints)
	s.l[txmap.Bytes2Uint16Buckets(hash, s.nrOfBuckets)].Add(1)

	return inpoints, true
}

func (s *SplitTxInpointsMap) Clear() {
	for i := uint16(0); i < s.nrOfBuckets; i++ {
		s.m[i] = &sync.Map{}
		s.l[i] = &atomic.Uint64{}
	}

}

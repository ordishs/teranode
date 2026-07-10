// Command gen emits golden vectors from the real go-subtree library so the Rust
// port can be validated byte-identical. Part of the Gate 1 throwaway benchmark;
// isolated nested module, reads go-subtree only (does not modify Teranode).
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"time"

	bt "github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	base58 "github.com/bsv-blockchain/go-sdk/compat/base58"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/util/bump"
)

// leaf(i) = double-SHA256(little-endian uint32 i). Deterministic, non-zero, and
// trivially reproducible on the Rust side.
func leaf(i int) chainhash.Hash {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(i))
	return chainhash.DoubleHashH(b)
}

func merkleRootHex(count int) string {
	st, err := subtree.NewIncompleteTreeByLeafCount(count)
	if err != nil {
		panic(err)
	}
	for i := 0; i < count; i++ {
		if err := st.AddNode(leaf(i), uint64(i), uint64(i)); err != nil {
			panic(err)
		}
	}
	root := st.RootHash()
	// Forward byte order (NOT chainhash.String() which reverses for display).
	return hex.EncodeToString(root[:])
}

func main() {
	mode := "merkle"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if err := os.MkdirAll("../fixtures/golden", 0o755); err != nil {
		panic(err)
	}

	switch mode {
	case "merkle":
		counts := []int{1, 2, 3, 4, 7, 1000, 1024}
		f, err := os.Create("../fixtures/golden/merkle.txt")
		if err != nil {
			panic(err)
		}
		defer f.Close()
		for _, c := range counts {
			fmt.Fprintf(f, "%d %s\n", c, merkleRootHex(c))
		}
		fmt.Printf("wrote merkle golden vectors for counts %v\n", counts)
	case "serialize":
		writeSerializeVectors()
		fmt.Println("wrote subtree + inpoints serialize golden vectors")
	case "ingest":
		writeIngestVectors(5000, 1024)
		fmt.Println("wrote ingest golden vectors")
	case "bench":
		n := argInt(2, 2_000_000)
		cap := argInt(3, 65536)
		benchIngest(n, cap)
	case "candidate":
		writeCandidateVectors(5000, 1024)
		fmt.Println("wrote candidate golden vectors")
	case "incomplete-candidate":
		// PARTIAL set: placeholder + k leaves, k < cap → no completed subtree.
		// The candidate publishes the incomplete subtree.
		writeIncompleteCandidateVectors(404, 1024)
		fmt.Println("wrote incomplete-candidate golden vectors")
	case "blockmerkle":
		// N=4500, cap=1024 -> subtrees [1024,1024,1024,1024,404]; the final
		// subtree (404 leaves, height 9) is SHORTER than the first (1024, height
		// 10), exercising RootHashPadded height-lifting.
		writeBlockMerkleVectors(4500, 1024)
		fmt.Println("wrote block merkle golden vectors")
	case "bump":
		writeBumpVectors()
		fmt.Println("wrote coinbase BUMP golden vectors")
	case "coinbase":
		writeCoinbaseVectors()
		fmt.Println("wrote coinbase golden vectors")
	case "subtreeblob":
		writeSubtreeBlobVectors(4)
		fmt.Println("wrote subtree blob golden vectors")
	case "txhash":
		writeTxHashVectors()
		fmt.Println("wrote tx hash golden vectors")
	case "spendsfortx":
		writeSpendsForTxVectors()
		fmt.Println("wrote spendsForTx golden vectors")
	case "reorg-golden":
		// small, fixed params — must match the Rust golden test
		writeReorgGolden(reorgParams{u: 10_000, block: 2_000, dBack: 2, dFwd: 3, conflictStride: 100, cap: 1024})
		fmt.Println("wrote reorg golden vectors")
	case "reorg-golden-noconflict":
		// D-a structural reorg: conflicts DISABLED (conflictStride 0). Must
		// match the Rust no-conflict golden test.
		writeReorgGoldenNoConflict(reorgParams{u: 10_000, block: 2_000, dBack: 2, dFwd: 3, conflictStride: 0, cap: 1024})
		fmt.Println("wrote no-conflict reorg golden vectors")
	case "reorg-bench":
		p := reorgParams{
			u:              uint32(argInt(2, 500_000)),
			block:          uint32(argInt(3, 50_000)),
			dBack:          uint32(argInt(4, 3)),
			dFwd:           uint32(argInt(5, 4)),
			conflictStride: 1000,
			cap:            65536,
		}
		benchReorg(p)
	default:
		panic("unknown mode: " + mode)
	}
}

type reorgParams struct {
	u, block, dBack, dFwd, conflictStride uint32
	cap                                   int
}

// reorgReconcile mirrors src/reorg.rs reorg_reconcile EXACTLY: same leaf(i), same
// set operations, same deterministic sort + chunk + RootHash.
func reorgReconcile(p reorgParams) []chainhash.Hash {
	unmined := make(map[chainhash.Hash]struct{}, p.u)
	for i := uint32(0); i < p.u; i++ {
		unmined[leaf(int(i))] = struct{}{}
	}

	mb := p.dBack * p.block
	movedBack := make([]uint32, 0, mb)
	for i := p.u; i < p.u+mb; i++ {
		unmined[leaf(int(i))] = struct{}{}
		movedBack = append(movedBack, i)
	}

	// conflictStride == 0 disables conflicts (D-a structural-only path):
	// no reverse cascade, no losing-conflict eviction.
	reverseCascade := make(map[chainhash.Hash]struct{})
	if p.conflictStride != 0 {
		for _, i := range movedBack {
			if i%p.conflictStride == 0 {
				h := leaf(int(i))
				delete(unmined, h)
				reverseCascade[h] = struct{}{}
			}
		}
	}

	fb := p.dFwd * p.block
	if fb > p.u {
		fb = p.u
	}
	for i := uint32(0); i < fb; i++ {
		delete(unmined, leaf(int(i)))
	}

	losing := make([]chainhash.Hash, 0)
	if p.conflictStride != 0 {
		for i := fb; i < p.u; i += p.conflictStride {
			h := leaf(int(i))
			if _, ok := unmined[h]; ok {
				delete(unmined, h)
				losing = append(losing, h)
			}
		}
	}

	markOnChain := make([]chainhash.Hash, 0, len(movedBack))
	for _, i := range movedBack {
		h := leaf(int(i))
		if _, ok := reverseCascade[h]; !ok {
			markOnChain = append(markOnChain, h)
		}
	}
	markFalse := make([]chainhash.Hash, 0, len(unmined)+len(losing))
	for h := range unmined {
		markFalse = append(markFalse, h)
	}
	markFalse = append(markFalse, losing...)
	_, _ = markOnChain, markFalse

	remaining := make([]chainhash.Hash, 0, len(unmined))
	for h := range unmined {
		remaining = append(remaining, h)
	}
	sort.Slice(remaining, func(a, b int) bool {
		return bytes.Compare(remaining[a][:], remaining[b][:]) < 0
	})

	var roots []chainhash.Hash
	for start := 0; start < len(remaining); start += p.cap {
		end := start + p.cap
		if end > len(remaining) {
			end = len(remaining)
		}
		chunk := remaining[start:end]
		st, err := subtree.NewIncompleteTreeByLeafCount(len(chunk))
		if err != nil {
			panic(err)
		}
		for j, h := range chunk {
			if err := st.AddNode(h, uint64(j), uint64(j)); err != nil {
				panic(err)
			}
		}
		roots = append(roots, *st.RootHash())
	}
	return roots
}

func writeReorgGolden(p reorgParams) {
	roots := reorgReconcile(p)
	f, err := os.Create("../fixtures/golden/reorg.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "%d %d %d %d %d %d %d\n", p.u, p.block, p.dBack, p.dFwd, p.conflictStride, p.cap, len(roots))
	for _, r := range roots {
		fmt.Fprintf(f, "%s\n", hex.EncodeToString(r[:]))
	}
}

// writeReorgGoldenNoConflict emits the D-a structural-only reorg end-state
// (conflictStride 0) to a dedicated golden file.
func writeReorgGoldenNoConflict(p reorgParams) {
	roots := reorgReconcile(p)
	f, err := os.Create("../fixtures/golden/reorg_noconflict.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "%d %d %d %d %d %d %d\n", p.u, p.block, p.dBack, p.dFwd, p.conflictStride, p.cap, len(roots))
	for _, r := range roots {
		fmt.Fprintf(f, "%s\n", hex.EncodeToString(r[:]))
	}
}

func benchReorg(p reorgParams) {
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	start := time.Now()
	roots := reorgReconcile(p)
	elapsed := time.Since(start)
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	fmt.Printf("go    reorg u=%d block=%d d_back=%d d_fwd=%d subtrees=%d elapsed=%.4fs gc_pause_ns=%d num_gc=%d alloc_bytes=%d\n",
		p.u, p.block, p.dBack, p.dFwd, len(roots), elapsed.Seconds(),
		m1.PauseTotalNs-m0.PauseTotalNs, m1.NumGC-m0.NumGC, m1.TotalAlloc-m0.TotalAlloc)
}

func argInt(idx, def int) int {
	if len(os.Args) > idx {
		if v, err := strconv.Atoi(os.Args[idx]); err == nil {
			return v
		}
	}
	return def
}

// benchIngest mirrors the Rust replay binary: pre-build the workload (not timed),
// then time the tuned ingest loop (dedup map + go-subtree AddNode + RootHash on
// each completed subtree), reporting tx/s and GC pressure.
func benchIngest(n, capSize int) {
	type tx struct {
		h         chainhash.Hash
		fee, size uint64
	}
	txs := make([]tx, n)
	for i := 0; i < n; i++ {
		txs[i] = tx{leaf(i), uint64(i), uint64(i)}
	}

	newTree := func() *subtree.Subtree {
		st, err := subtree.NewIncompleteTreeByLeafCount(capSize)
		if err != nil {
			panic(err)
		}
		return st
	}

	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	start := time.Now()

	seen := make(map[chainhash.Hash]struct{}, n)
	var chained []*subtree.Subtree
	cur := newTree()
	count := 0
	for _, t := range txs {
		if _, ok := seen[t.h]; ok {
			continue
		}
		seen[t.h] = struct{}{}
		_ = cur.AddNode(t.h, t.fee, t.size)
		count++
		if count == capSize {
			_ = cur.RootHash()
			chained = append(chained, cur)
			cur = newTree()
			count = 0
		}
	}
	elapsed := time.Since(start)

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	tps := float64(n) / elapsed.Seconds()
	fmt.Printf("go    n=%d cap=%d completed_subtrees=%d elapsed=%.3fs tx/s=%.0f gc_pause_ns=%d num_gc=%d alloc_bytes=%d\n",
		n, capSize, len(chained), elapsed.Seconds(), tps,
		m1.PauseTotalNs-m0.PauseTotalNs, m1.NumGC-m0.NumGC, m1.TotalAlloc-m0.TotalAlloc)
}

// writeBlockMerkleVectors emits the block merkle root + coinbase merkle proof
// golden, mirroring Go createMerkleTreeFromSubtrees (Server.go:1533) and
// GetMerkleProofForCoinbase (merkle_tree.go:13). Tx set = leaf(i) for i in 0..n,
// chunked by capSize so the LAST subtree is shorter than the first (exercising
// RootHashPadded height-lifting). Fixed coinbase txid = leaf(0xC0FFEE).
//
// Fixture format (fixtures/golden/blockmerkle.txt):
//
//	blockmerkle <n> <cap> <n_subtrees>
//	<block_merkle_root_hex>
//	<proof_len>
//	<proof_hash_hex>   # one per line, proof_len lines
func writeBlockMerkleVectors(n, capSize int) {
	newTree := func() *subtree.Subtree {
		st, err := subtree.NewIncompleteTreeByLeafCount(capSize)
		if err != nil {
			panic(err)
		}
		return st
	}

	// Build subtrees: subtree 0 starts with the coinbase placeholder at node 0
	// (mirroring SubtreeProcessor), then leaf(i) chunked by capSize. Identical to
	// the Rust test. The placeholder occupies the slot the coinbase substitution
	// later overwrites via ReplaceRootNode(coinbase, 0, 0).
	var subtrees []*subtree.Subtree
	cur := newTree()
	if err := cur.AddCoinbaseNode(); err != nil {
		panic(err)
	}
	count := 1
	for i := 0; i < n; i++ {
		if err := cur.AddNode(leaf(i), uint64(i), 10); err != nil {
			panic(err)
		}
		count++
		if count == capSize {
			subtrees = append(subtrees, cur)
			cur = newTree()
			count = 0
		}
	}
	if count > 0 {
		subtrees = append(subtrees, cur)
	}

	coinbaseTxID := leaf(0xC0FFEE)

	// --- Coinbase merkle proof: GetMerkleProofForCoinbase over the RAW subtrees
	// (index 0 still carries leaf(0), the placeholder position). ---
	coinbaseProof, err := subtree.GetMerkleProofForCoinbase(subtrees)
	if err != nil {
		panic(err)
	}

	// --- Block merkle root: createMerkleTreeFromSubtrees ---
	// Duplicate subtree-0 and replace its root node with the coinbase txid; take
	// each subtree's root into subtreeHashes.
	subtreesInJob := make([]*subtree.Subtree, len(subtrees))
	subtreeHashes := make([]chainhash.Hash, len(subtrees))
	for i, st := range subtrees {
		if i == 0 {
			subtreesInJob[i] = st.Duplicate()
			subtreesInJob[i].ReplaceRootNode(&coinbaseTxID, 0, 0)
		} else {
			subtreesInJob[i] = st
		}
		subtreeHashes[i] = *subtreesInJob[i].RootHash()
	}

	// Height-lift the final subtree to the first subtree's height when shorter.
	if len(subtreesInJob) > 1 {
		first := subtreesInJob[0]
		last := subtreesInJob[len(subtreesInJob)-1]
		if last.Length() < first.Length() {
			liftedRoot, err := last.RootHashPadded(first.Height)
			if err != nil {
				panic(err)
			}
			subtreeHashes[len(subtreeHashes)-1] = *liftedRoot
		}
	}

	// Top tree over the (possibly lifted) subtree hashes, with CVE dup guard.
	topTree, err := subtree.NewTreeByLeafCount(subtree.CeilPowerOfTwo(len(subtreesInJob)))
	if err != nil {
		panic(err)
	}
	seen := make(map[chainhash.Hash]struct{}, len(subtreeHashes))
	for _, h := range subtreeHashes {
		if _, dup := seen[h]; dup {
			panic("duplicate subtree root hash in top-level merkle tree")
		}
		seen[h] = struct{}{}
		if err := topTree.AddNode(h, 1, 0); err != nil {
			panic(err)
		}
	}

	var blockMerkleRoot chainhash.Hash
	if len(subtreesInJob) == 0 {
		blockMerkleRoot = coinbaseTxID
	} else {
		blockMerkleRoot = *topTree.RootHash()
	}

	f, err := os.Create("../fixtures/golden/blockmerkle.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "blockmerkle %d %d %d\n", n, capSize, len(subtrees))
	fmt.Fprintf(f, "%s\n", hex.EncodeToString(blockMerkleRoot[:]))
	fmt.Fprintf(f, "%d\n", len(coinbaseProof))
	for _, h := range coinbaseProof {
		fmt.Fprintf(f, "%s\n", hex.EncodeToString(h[:]))
	}
}

// computeBumpHex builds the submit-shaped data for (n leaves + coinbase
// placeholder, chunked by capSize) and computes the coinbase BUMP via the REAL
// teranode util/bump — the exact flow Server.go:1399-1443 runs: duplicate
// subtree-0, ReplaceRootNode(coinbase), height-lift the final subtree's hash,
// bump.ComputeCoinbaseBUMP(subtree0, hashes, height).
func computeBumpHex(n, capSize int, height uint32) string {
	newTree := func() *subtree.Subtree {
		st, err := subtree.NewIncompleteTreeByLeafCount(capSize)
		if err != nil {
			panic(err)
		}
		return st
	}

	var subtrees []*subtree.Subtree
	cur := newTree()
	if err := cur.AddCoinbaseNode(); err != nil {
		panic(err)
	}
	count := 1
	for i := 0; i < n; i++ {
		if err := cur.AddNode(leaf(i), uint64(i), 10); err != nil {
			panic(err)
		}
		count++
		if count == capSize {
			subtrees = append(subtrees, cur)
			cur = newTree()
			count = 0
		}
	}
	if count > 0 {
		subtrees = append(subtrees, cur)
	}

	coinbaseTxID := leaf(0xC0FFEE)

	subtreesInJob := make([]*subtree.Subtree, len(subtrees))
	subtreeHashes := make([]chainhash.Hash, len(subtrees))
	for i, st := range subtrees {
		if i == 0 {
			subtreesInJob[i] = st.Duplicate()
			subtreesInJob[i].ReplaceRootNode(&coinbaseTxID, 0, 0)
		} else {
			subtreesInJob[i] = st
		}
		subtreeHashes[i] = *subtreesInJob[i].RootHash()
	}

	if len(subtreesInJob) > 1 {
		first := subtreesInJob[0]
		last := subtreesInJob[len(subtreesInJob)-1]
		if last.Length() < first.Length() {
			liftedRoot, err := last.RootHashPadded(first.Height)
			if err != nil {
				panic(err)
			}
			subtreeHashes[len(subtreeHashes)-1] = *liftedRoot
		}
	}

	hashPtrs := make([]*chainhash.Hash, len(subtreeHashes))
	for i := range subtreeHashes {
		hashPtrs[i] = &subtreeHashes[i]
	}

	bumpBytes, err := bump.ComputeCoinbaseBUMP(subtreesInJob[0], hashPtrs, height)
	if err != nil {
		panic(err)
	}

	return hex.EncodeToString(bumpBytes)
}

// Fixture format (fixtures/golden/bump.txt):
//
//	bump <numCases>
//	<n> <cap> <height> <bumpHex>   (one line per case)
func writeBumpVectors() {
	cases := []struct {
		n, cap int
		height uint32
	}{
		{0, 8, 7},        // single subtree, coinbase only → height-0 BUMP
		{5, 8, 42},       // single PARTIAL subtree (6 nodes, non-pow2)
		{7, 8, 150},      // single full subtree
		{15, 8, 300},     // two full subtrees (0xfd height varint)
		{19, 8, 840_000}, // 2 full + 1 of 4 → final-subtree lift (0xfe varint)
		{16, 8, 1},       // 2 full + 1 single node → deepest lift
		{23, 8, 100_000}, // 3 full → odd top level
	}

	f, err := os.Create("../fixtures/golden/bump.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	fmt.Fprintf(f, "bump %d\n", len(cases))
	for _, c := range cases {
		fmt.Fprintf(f, "%d %d %d %s\n", c.n, c.cap, c.height, computeBumpHex(c.n, c.cap, c.height))
	}
}

// subtreeMagic is the 8-byte fileformat header that
// `fileformat.NewHeader(FileTypeSubtree).Bytes()` writes — `magicSubtree` in
// pkg/fileformat/header.go:51, ASCII "S-1.0   " (S '-' '1' '.' '0' + 3 spaces).
// The gen module is standalone (it does not import teranode pkg/fileformat), so
// these bytes mirror the authoritative source byte-for-byte, like the coinbase
// gen mirrors model. Header.Write emits exactly these 8 bytes and nothing else.
var subtreeMagic = [8]byte{'S', '-', '1', '.', '0', ' ', ' ', ' '}

// writeSubtreeBlobVectors emits the byte-exact on-disk subtree blob:
// fileformat header || Subtree.Serialize(). The subtree mirrors the live engine
// seeding: node 0 is the coinbase placeholder (AddCoinbaseNode), followed by
// `leaves` real txs leaf(i). Every node uses fee 0 / size 0 so the aggregate
// Fees/SizeInBytes fields stay 0 on both sides (Go's AddNode accumulates them;
// the Rust port leaves them untouched — fee/size 0 makes them agree). The Rust
// golden test builds the identical subtree and asserts SUBTREE_MAGIC||serialize
// == this hex.
//
// Fixture format (fixtures/golden/subtreeblob.txt):
//
//	subtreeblob <leaves>
//	<header_plus_body_hex>
func writeSubtreeBlobVectors(leaves int) {
	st, err := subtree.NewIncompleteTreeByLeafCount(leaves + 1)
	if err != nil {
		panic(err)
	}
	if err := st.AddCoinbaseNode(); err != nil {
		panic(err)
	}
	for i := 0; i < leaves; i++ {
		if err := st.AddNode(leaf(i), 0, 0); err != nil {
			panic(err)
		}
	}

	body, err := st.Serialize()
	if err != nil {
		panic(err)
	}

	blob := make([]byte, 0, len(subtreeMagic)+len(body))
	blob = append(blob, subtreeMagic[:]...)
	blob = append(blob, body...)

	f, err := os.Create("../fixtures/golden/subtreeblob.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "subtreeblob %d\n", leaves)
	fmt.Fprintf(f, "%s\n", hex.EncodeToString(blob))
}

// writeSerializeVectors emits Subtree.Serialize() and TxInpoints.Serialize()
// golden hex for fixed, reproducible inputs.
func writeSerializeVectors() {
	// Subtree: 4 nodes (leaf i, fee=i, size=i*10), aggregate fees/size set,
	// 2 conflicting nodes.
	st, err := subtree.NewIncompleteTreeByLeafCount(4)
	if err != nil {
		panic(err)
	}
	for i := 0; i < 4; i++ {
		if err := st.AddNode(leaf(i), uint64(i), uint64(i*10)); err != nil {
			panic(err)
		}
	}
	st.Fees = 1234
	st.SizeInBytes = 5678
	st.ConflictingNodes = []chainhash.Hash{leaf(100), leaf(200)}
	stBytes, err := st.Serialize()
	if err != nil {
		panic(err)
	}

	// TxInpoints: parent0 has 1 vout [5], parent1 has 2 vouts [7,9].
	parents := []chainhash.Hash{leaf(0), leaf(1)}
	voutIdxs := []uint32{1, 5, 2, 7, 9}
	tip := subtree.NewTxInpointsFromPacked(parents, voutIdxs)
	tipBytes, err := tip.Serialize()
	if err != nil {
		panic(err)
	}

	f, err := os.Create("../fixtures/golden/serialize.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "subtree %s\n", hex.EncodeToString(stBytes))
	fmt.Fprintf(f, "inpoints %s\n", hex.EncodeToString(tipBytes))
}

// writeIngestVectors replicates the processor's ingest behavior over a workload
// of `n` txs (leaf i) with the given `capSize`, emitting the completed-subtree
// merkle roots. The Rust processor must reproduce these for the same workload.
func writeIngestVectors(n, capSize int) {
	var roots []chainhash.Hash
	newTree := func() *subtree.Subtree {
		st, err := subtree.NewIncompleteTreeByLeafCount(capSize)
		if err != nil {
			panic(err)
		}
		return st
	}
	// Subtree 0 is seeded with the coinbase placeholder at node 0 (mirroring
	// SubtreeProcessor init); it consumes one slot so subtree 0 holds capSize-1
	// real txs. Subsequent subtrees carry no placeholder.
	st := newTree()
	if err := st.AddCoinbaseNode(); err != nil {
		panic(err)
	}
	count := 1
	for i := 0; i < n; i++ {
		if err := st.AddNode(leaf(i), uint64(i), uint64(i)); err != nil {
			panic(err)
		}
		count++
		if count == capSize {
			roots = append(roots, *st.RootHash())
			st = newTree()
			count = 0
		}
	}

	f, err := os.Create("../fixtures/golden/ingest.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "%d %d %d\n", capSize, n, len(roots))
	for _, r := range roots {
		fmt.Fprintf(f, "%s\n", hex.EncodeToString(r[:]))
	}
}

// writeCandidateVectors emits the §10 candidate-parity golden: a deterministic
// set of N txs { txid = leaf(i), fee = i, size = i, createdAt = N - i } sorted by
// createdAt ASCENDING (deliberately the REVERSE of index, so the sort actually
// reorders), fed to go-subtree in that order. The Rust offline parity test must
// reproduce these exact completed-subtree roots and aggregates after running its
// real plan_unmined_load createdAt sort over the same set.
//
// Fixture format (fixtures/golden/candidate.txt):
//
//	candidate <cap> <num_txs> <num_roots> <coinbase_value> <size_without_coinbase>
//	<root_hex>            # one per completed subtree, forward byte order
//	...
//
// coinbase_value      = sum(fee)  = sum(i for i in 0..N)
// size_without_coinbase = sum(size) = sum(i for i in 0..N)
func writeCandidateVectors(n, capSize int) {
	type tx struct {
		h         chainhash.Hash
		fee, size uint64
		createdAt int64
	}
	txs := make([]tx, n)
	var coinbaseValue, sizeWithoutCoinbase uint64
	for i := 0; i < n; i++ {
		txs[i] = tx{
			h:         leaf(i),
			fee:       uint64(i),
			size:      uint64(i),
			createdAt: int64(n - i),
		}
		coinbaseValue += uint64(i)
		sizeWithoutCoinbase += uint64(i)
	}

	// Sort by createdAt ascending (stable) — exactly what plan_unmined_load does.
	sort.SliceStable(txs, func(a, b int) bool {
		return txs[a].createdAt < txs[b].createdAt
	})

	var roots []chainhash.Hash
	newTree := func() *subtree.Subtree {
		st, err := subtree.NewIncompleteTreeByLeafCount(capSize)
		if err != nil {
			panic(err)
		}
		return st
	}
	// Seed subtree 0 with the coinbase placeholder at node 0, like the live
	// SubtreeProcessor. The placeholder carries fee 0 / size 0, so it does not
	// affect coinbase_value (sum fee) or size_without_coinbase (sum size).
	st := newTree()
	if err := st.AddCoinbaseNode(); err != nil {
		panic(err)
	}
	count := 1
	for _, t := range txs {
		if err := st.AddNode(t.h, t.fee, t.size); err != nil {
			panic(err)
		}
		count++
		if count == capSize {
			roots = append(roots, *st.RootHash())
			st = newTree()
			count = 0
		}
	}

	f, err := os.Create("../fixtures/golden/candidate.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "candidate %d %d %d %d %d\n",
		capSize, n, len(roots), coinbaseValue, sizeWithoutCoinbase)
	for _, r := range roots {
		fmt.Fprintf(f, "%s\n", hex.EncodeToString(r[:]))
	}
}

// writeIncompleteCandidateVectors emits the incomplete-subtree candidate golden:
// a PARTIAL set of k leaves (k < cap) added to a single subtree seeded with the
// coinbase placeholder at node 0 — exactly Go's `createIncompleteSubtreeCopy`
// shape (placeholder + current.Nodes[1:], carrying Fees). No subtree completes, so
// the candidate publishes this one incomplete subtree. Records its root, the
// carried fee sum (coinbase_value − subsidy), and the coinbase merkle proof over
// the single subtree, so the Rust test can assert byte-parity without hardcoding.
//
// Tx set: leaf(i) for i in 0..k, fee = i+1, size = 10. Fee sum = Σ(i+1) for i<k.
//
// Fixture format (fixtures/golden/incomplete_candidate.txt):
//
//	incomplete-candidate <cap> <k> <fee_sum>
//	<incomplete_subtree_root_hex>
//	<proof_len>
//	<proof_hash_hex>   # one per line, proof_len lines
func writeIncompleteCandidateVectors(k, capSize int) {
	if k >= capSize {
		panic("incomplete-candidate: k must be < cap (no subtree may complete)")
	}

	st, err := subtree.NewIncompleteTreeByLeafCount(capSize)
	if err != nil {
		panic(err)
	}
	if err := st.AddCoinbaseNode(); err != nil {
		panic(err)
	}

	var feeSum uint64
	for i := 0; i < k; i++ {
		fee := uint64(i + 1)
		if err := st.AddNode(leaf(i), fee, 10); err != nil {
			panic(err)
		}
		feeSum += fee
	}

	root := st.RootHash()

	// Coinbase merkle proof over the single incomplete subtree (index 0 still the
	// placeholder position), mirroring GetMerkleProofForCoinbase.
	proof, err := subtree.GetMerkleProofForCoinbase([]*subtree.Subtree{st})
	if err != nil {
		panic(err)
	}

	f, err := os.Create("../fixtures/golden/incomplete_candidate.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "incomplete-candidate %d %d %d\n", capSize, k, feeSum)
	fmt.Fprintf(f, "%s\n", hex.EncodeToString(root[:]))
	fmt.Fprintf(f, "%d\n", len(proof))
	for _, h := range proof {
		fmt.Fprintf(f, "%s\n", hex.EncodeToString(h[:]))
	}
}

// --- coinbase golden ---
//
// Faithful replication of model.GetCoinbaseParts / makeCoinbase1 / makeCoinbase2 /
// makeCoinbaseOutputTransactions (model/GetCoinbaseParts.go) and the assembly in
// model.CreateCoinbase (model/mining_candidate.go), but with a FIXED 12-byte
// extranonce instead of rand.Read so the vector is deterministic. The teranode
// `model` package is NOT imported here (the gen is a standalone module); this
// mirrors the authoritative Go source byte-for-byte. The Rust golden test is the
// gate that proves the Rust port matches this output.

func cbVarInt(i uint64) []byte {
	b := make([]byte, 9)
	if i < 0xfd {
		b[0] = byte(i)
		return b[:1]
	}
	if i < 0x10000 {
		b[0] = 0xfd
		binary.LittleEndian.PutUint16(b[1:3], uint16(i))
		return b[:3]
	}
	if i < 0x100000000 {
		b[0] = 0xfe
		binary.LittleEndian.PutUint32(b[1:5], uint32(i))
		return b[:5]
	}
	b[0] = 0xff
	binary.LittleEndian.PutUint64(b[1:9], i)
	return b
}

// addressToScript mirrors model.AddressToScript exactly: base58.Decode yields the
// 25-byte payload (1 version + 20 hash + 4 checksum); the script uses bytes [1:21].
func addressToScript(address string) ([]byte, error) {
	decoded, err := base58.Decode(address)
	if err != nil {
		return nil, err
	}
	if len(decoded) != 25 {
		return nil, fmt.Errorf("invalid address length for '%s'", address)
	}
	switch decoded[0] {
	case 0x00, 0x6f: // P2PKH (mainnet / testnet)
		pubkey := decoded[1 : len(decoded)-4]
		ret := []byte{0x76, 0xa9, 0x14} // OP_DUP OP_HASH160 push20
		ret = append(ret, pubkey...)
		ret = append(ret, 0x88, 0xac) // OP_EQUALVERIFY OP_CHECKSIG
		return ret, nil
	case 0x05, 0xc4: // P2SH (mainnet / testnet)
		hash := decoded[1 : len(decoded)-4]
		ret := []byte{0xa9, 0x14} // OP_HASH160 push20
		ret = append(ret, hash...)
		ret = append(ret, 0x87) // OP_EQUAL
		return ret, nil
	default:
		return nil, fmt.Errorf("address %s is not supported", address)
	}
}

func makeCoinbaseOutputs(coinbaseValue uint64, walletAddresses []string) ([]byte, error) {
	numberOfOutputs := uint64(len(walletAddresses))
	if numberOfOutputs == 0 {
		return nil, fmt.Errorf("no wallet addresses provided")
	}
	outputValue := coinbaseValue / numberOfOutputs
	outputChange := coinbaseValue % numberOfOutputs

	buf := make([]byte, 0)
	buf = append(buf, cbVarInt(numberOfOutputs)...)
	for i, walletAddress := range walletAddresses {
		lockingScript, err := addressToScript(walletAddress)
		if err != nil {
			return nil, err
		}
		outputBytes := make([]byte, 8)
		if i == 0 {
			binary.LittleEndian.PutUint64(outputBytes[0:], outputValue+outputChange)
		} else {
			binary.LittleEndian.PutUint64(outputBytes[0:], outputValue)
		}
		outputBytes = append(outputBytes, cbVarInt(uint64(len(lockingScript)))...)
		outputBytes = append(outputBytes, lockingScript...)
		buf = append(buf, outputBytes...)
	}
	return buf, nil
}

func makeCoinbase1(height uint32, coinbaseText string) []byte {
	spaceForExtraNonce := 12

	blockHeightBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(blockHeightBytes, height)

	arbitraryData := []byte{}
	arbitraryData = append(arbitraryData, 0x03)
	arbitraryData = append(arbitraryData, blockHeightBytes[:3]...)
	arbitraryData = append(arbitraryData, []byte(coinbaseText)...)

	if len(arbitraryData) > (100 - spaceForExtraNonce) {
		arbitraryData = arbitraryData[:100-spaceForExtraNonce]
	}

	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, 1) // Version

	buf = append(buf, 0x01)
	buf = append(buf, make([]byte, 32)...)
	buf = append(buf, 0xff, 0xff, 0xff, 0xff)
	buf = append(buf, cbVarInt(uint64(len(arbitraryData)+spaceForExtraNonce))...)
	buf = append(buf, arbitraryData...)
	return buf
}

func makeCoinbase2(ot []byte) []byte {
	sq := []byte{0xff, 0xff, 0xff, 0xff}
	lt := make([]byte, 4)
	out := append(sq, ot...)
	out = append(out, lt...)
	return out
}

func writeCoinbaseVectors() {
	const (
		height       = uint32(100)
		coinbaseVal  = uint64(5_000_000_000)
		coinbaseText = "rust"
		address      = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"
	)
	// FIXED 12-byte extranonce (deterministic substitute for rand.Read).
	extranonce := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb}

	c1 := makeCoinbase1(height, coinbaseText)
	ot, err := makeCoinbaseOutputs(coinbaseVal, []string{address})
	if err != nil {
		panic(err)
	}
	c2 := makeCoinbase2(ot)

	// CreateCoinbase assembly: a = c1 || extranonce || c2.
	cb := make([]byte, 0, len(c1)+len(extranonce)+len(c2))
	cb = append(cb, c1...)
	cb = append(cb, extranonce...)
	cb = append(cb, c2...)

	f, err := os.Create("../fixtures/golden/coinbase.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "coinbase %d %d %s\n", height, coinbaseVal, hex.EncodeToString(extranonce))
	fmt.Fprintf(f, "%s\n", hex.EncodeToString(cb))
}

// p2pkhScript builds a P2PKH-shaped locking script: OP_DUP OP_HASH160 <20-byte
// push> OP_EQUALVERIFY OP_CHECKSIG, with all 20 hash bytes set to fill.
func p2pkhScript(fill byte) *bscript.Script {
	b := make([]byte, 0, 25)
	b = append(b, 0x76, 0xa9, 0x14)
	for i := 0; i < 20; i++ {
		b = append(b, fill)
	}
	b = append(b, 0x88, 0xac)
	return bscript.NewFromBytes(b)
}

type txHashInput struct {
	UTXOHashRawHex string `json:"utxo_hash_raw_hex"`
}

type txHashVector struct {
	ExtendedHex string        `json:"extended_hex"`
	StandardHex string        `json:"standard_hex"`
	TxIDRawHex  string        `json:"txid_raw_hex"`
	Inputs      []txHashInput `json:"inputs"`
}

// utxoHashRaw replicates teranode util/utxo_hash.go UTXOHashInto EXACTLY, using
// go-bt's own bt.VarInt and chainhash.HashH (single SHA-256), so the golden is
// byte-identical to teranode's UTXO hash by construction (no teranode import):
//
//	pre := prevTxid[:]
//	pre = bt.VarInt(index).AppendTo(pre)
//	pre = append(pre, lockingScript...)
//	pre = bt.VarInt(satoshis).AppendTo(pre)
//	h := chainhash.HashH(pre)
func utxoHashRaw(prevTxid *chainhash.Hash, index uint32, lockingScript []byte, satoshis uint64) chainhash.Hash {
	pre := make([]byte, 0, 32+9+len(lockingScript)+9)
	pre = append(pre, prevTxid[:]...)
	pre = bt.VarInt(index).AppendTo(pre)
	pre = append(pre, lockingScript...)
	pre = bt.VarInt(satoshis).AppendTo(pre)
	return chainhash.HashH(pre)
}

// deterministicExtendedTx builds THE deterministic EXTENDED transaction shared
// by the `txhash` and `spendsfortx` golden modes so both prove against the same
// inputs. Two extended inputs (leaf(1) vout 0, leaf(2) vout 5), two outputs.
func deterministicExtendedTx() *bt.Tx {
	tx := bt.NewTx()
	tx.Version = 2
	tx.LockTime = 500000

	in0Prev := leaf(1)
	in0PrevScript := p2pkhScript(0xAB)
	tx.Inputs = append(tx.Inputs, &bt.Input{
		PreviousTxOutIndex: 0,
		UnlockingScript:    bscript.NewFromBytes([]byte{0x51}),
		SequenceNumber:     0xFFFFFFFF,
		PreviousTxSatoshis: 111111,
		PreviousTxScript:   in0PrevScript,
	})
	if err := tx.Inputs[0].PreviousTxIDAdd(&in0Prev); err != nil {
		panic(err)
	}

	in1Prev := leaf(2)
	in1PrevScript := bscript.NewFromBytes([]byte{0x6a, 0x04, 0xde, 0xad, 0xbe, 0xef})
	tx.Inputs = append(tx.Inputs, &bt.Input{
		PreviousTxOutIndex: 5,
		UnlockingScript:    bscript.NewFromBytes([]byte{0x52, 0x53}),
		SequenceNumber:     0xFFFFFFFE,
		PreviousTxSatoshis: 222222,
		PreviousTxScript:   in1PrevScript,
	})
	if err := tx.Inputs[1].PreviousTxIDAdd(&in1Prev); err != nil {
		panic(err)
	}

	tx.Outputs = append(tx.Outputs,
		&bt.Output{Satoshis: 100000, LockingScript: p2pkhScript(0xAB)},
		&bt.Output{Satoshis: 0, LockingScript: bscript.NewFromBytes([]byte{0x6a})},
	)

	tx.SetExtended(true)

	// Round-trip sanity: ExtendedBytes() must parse back as an extended tx.
	rt, err := bt.NewTxFromBytes(tx.ExtendedBytes())
	if err != nil {
		panic(err)
	}
	if !rt.IsExtended() {
		panic("expected re-parsed tx to be extended")
	}

	return tx
}

// writeTxHashVectors builds a deterministic EXTENDED transaction with go-bt and
// emits the standard/extended serialization, the raw txid, and the per-input
// UTXO hash (computed via the util/utxo_hash.go formula above). The Rust tx
// deserializer must reproduce all of these byte-for-byte.
func writeTxHashVectors() {
	tx := deterministicExtendedTx()
	in0PrevScript := tx.Inputs[0].PreviousTxScript
	in1PrevScript := tx.Inputs[1].PreviousTxScript

	txid := tx.TxIDChainHash()
	h0 := utxoHashRaw(tx.Inputs[0].PreviousTxIDChainHash(), tx.Inputs[0].PreviousTxOutIndex, *in0PrevScript, tx.Inputs[0].PreviousTxSatoshis)
	h1 := utxoHashRaw(tx.Inputs[1].PreviousTxIDChainHash(), tx.Inputs[1].PreviousTxOutIndex, *in1PrevScript, tx.Inputs[1].PreviousTxSatoshis)

	vec := txHashVector{
		ExtendedHex: hex.EncodeToString(tx.ExtendedBytes()),
		StandardHex: hex.EncodeToString(tx.Bytes()),
		TxIDRawHex:  hex.EncodeToString(txid[:]),
		Inputs: []txHashInput{
			{UTXOHashRawHex: hex.EncodeToString(h0[:])},
			{UTXOHashRawHex: hex.EncodeToString(h1[:])},
		},
	}

	out, err := json.MarshalIndent(vec, "", "  ")
	if err != nil {
		panic(err)
	}
	out = append(out, '\n')
	if err := os.WriteFile("../fixtures/golden/txhash.json", out, 0o644); err != nil {
		panic(err)
	}
}

type spendsForTxRecord struct {
	TxID         string `json:"tx_id"`          // input.PreviousTxID raw hex
	Vout         uint32 `json:"vout"`           // input.PreviousTxOutIndex
	UTXOHash     string `json:"utxo_hash"`      // util/utxo_hash.go formula, raw hex
	SpendingTxID string `json:"spending_tx_id"` // tx.TxIDChainHash() raw hex
	Vin          int    `json:"vin"`            // input index
}

type spendsForTxVector struct {
	ExtendedHex string              `json:"extended_hex"`
	Spends      []spendsForTxRecord `json:"spends"`
}

// writeSpendsForTxVectors emits the Go `spendsForTx` output
// (stores/utxo/process_conflicting.go:612) for the SAME deterministic extended
// tx as the `txhash` mode: one record per input with the prev outpoint, the
// per-input UTXO hash (util/utxo_hash.go formula), the spender (this tx's txid),
// and the input index. The Rust `spends_for_tx` must reproduce these
// byte-for-byte. `extended_hex` is included so the Rust test is self-contained.
func writeSpendsForTxVectors() {
	tx := deterministicExtendedTx()
	txid := tx.TxIDChainHash()

	spends := make([]spendsForTxRecord, len(tx.Inputs))
	for i, input := range tx.Inputs {
		// Go spendsForTx: UTXOHash via util.UTXOHashFromInput (process_conflicting.go:616).
		h := utxoHashRaw(input.PreviousTxIDChainHash(), input.PreviousTxOutIndex, *input.PreviousTxScript, input.PreviousTxSatoshis)
		prevTxID := input.PreviousTxIDChainHash()
		spends[i] = spendsForTxRecord{
			TxID:         hex.EncodeToString(prevTxID[:]),
			Vout:         input.PreviousTxOutIndex,
			UTXOHash:     hex.EncodeToString(h[:]),
			SpendingTxID: hex.EncodeToString(txid[:]),
			Vin:          i,
		}
	}

	vec := spendsForTxVector{
		ExtendedHex: hex.EncodeToString(tx.ExtendedBytes()),
		Spends:      spends,
	}

	out, err := json.MarshalIndent(vec, "", "  ")
	if err != nil {
		panic(err)
	}
	out = append(out, '\n')
	if err := os.WriteFile("../fixtures/golden/spendsfortx.json", out, 0o644); err != nil {
		panic(err)
	}
}

// Stage "full forward-mining" validation over gRPC: columnar ingest -> candidate
// (with chain context) -> submit solution -> chain advances + assembly resets ->
// generate blocks. Exercises the whole cycle against the native Rust service.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/blockassembly/blockassembly_api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func leaf(i int) chainhash.Hash {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(i))
	return chainhash.DoubleHashH(b)
}

func main() {
	conn, err := grpc.NewClient("127.0.0.1:18089", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	c := blockassembly_api.NewBlockAssemblyAPIClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1) Columnar ingest of 5000 txs.
	const n = 5000
	packed := make([]byte, 0, n*32)
	fees := make([]uint64, n)
	sizes := make([]uint64, n)
	for i := 0; i < n; i++ {
		h := leaf(i)
		packed = append(packed, h[:]...)
		fees[i] = uint64(i)
		sizes[i] = uint64(i)
	}
	if _, err := c.AddTxBatchColumnar(ctx, &blockassembly_api.AddTxBatchColumnarRequest{
		TxidsPacked: packed, Fees: fees, Sizes: sizes,
	}); err != nil {
		log.Fatalf("AddTxBatchColumnar: %v", err)
	}

	// 2) Candidate at height 1, prev = genesis zeros.
	mc, err := c.GetMiningCandidate(ctx, &blockassembly_api.GetMiningCandidateRequest{})
	if err != nil {
		log.Fatal(err)
	}
	zeros := make([]byte, 32)
	if mc.Height != 1 || !bytes.Equal(mc.PreviousHash, zeros) || mc.SubtreeCount != 4 {
		log.Fatalf("candidate#1 wrong: height=%d subtreeCount=%d prevZero=%v", mc.Height, mc.SubtreeCount, bytes.Equal(mc.PreviousHash, zeros))
	}
	fmt.Printf("candidate#1: height=%d subtrees=%d numTxs=%d prev=%s...\n", mc.Height, mc.SubtreeCount, mc.NumTxs, hex.EncodeToString(mc.PreviousHash[:4]))

	// 3) Submit a solution -> chain advances, assembly resets.
	if _, err := c.SubmitMiningSolution(ctx, &blockassembly_api.SubmitMiningSolutionRequest{
		Id: mc.Id, Nonce: 42, CoinbaseTx: []byte("coinbase-block-1"),
	}); err != nil {
		log.Fatalf("SubmitMiningSolution: %v", err)
	}

	// 4) Candidate#2: height 2, prev = block1 hash (non-zero), assembly empty.
	mc2, err := c.GetMiningCandidate(ctx, &blockassembly_api.GetMiningCandidateRequest{})
	if err != nil {
		log.Fatal(err)
	}
	if mc2.Height != 2 || bytes.Equal(mc2.PreviousHash, zeros) || mc2.SubtreeCount != 0 {
		log.Fatalf("candidate#2 wrong: height=%d subtreeCount=%d prevZero=%v", mc2.Height, mc2.SubtreeCount, bytes.Equal(mc2.PreviousHash, zeros))
	}
	fmt.Printf("candidate#2: height=%d subtrees=%d prev=%s... (block1 hash)\n", mc2.Height, mc2.SubtreeCount, hex.EncodeToString(mc2.PreviousHash[:4]))

	// 5) GenerateBlocks(3) -> height should reach 5.
	if _, err := c.GenerateBlocks(ctx, &blockassembly_api.GenerateBlocksRequest{Count: 3}); err != nil {
		log.Fatalf("GenerateBlocks: %v", err)
	}
	mc3, _ := c.GetMiningCandidate(ctx, &blockassembly_api.GetMiningCandidateRequest{})
	if mc3.Height != 5 {
		log.Fatalf("after GenerateBlocks(3): expected height 5, got %d", mc3.Height)
	}
	st, _ := c.GetBlockAssemblyState(ctx, &blockassembly_api.EmptyMessage{})
	fmt.Printf("after GenerateBlocks(3): height=%d  state.txCount=%d state.subtreeCount=%d\n", mc3.Height, st.TxCount, st.SubtreeCount)
	fmt.Println("PASS: full forward-mining cycle works over gRPC (ingest -> candidate -> submit -> advance -> generate)")
}

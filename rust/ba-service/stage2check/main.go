// Stage 2 end-to-end validation: add 5000 txs over gRPC to the Rust service,
// then GetMiningCandidate and verify subtree_hashes match the Gate 1 go-subtree
// ingest golden (same workload: leaf(i), cap=1024).
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"os"
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
	conn, err := grpc.NewClient("127.0.0.1:18088", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	c := blockassembly_api.NewBlockAssemblyAPIClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const n = 5000
	for i := 0; i < n; i++ {
		h := leaf(i)
		if _, err := c.AddTx(ctx, &blockassembly_api.AddTxRequest{
			Txid: h[:], Fee: uint64(i), Size: uint64(i),
		}); err != nil {
			log.Fatalf("AddTx %d: %v", i, err)
		}
	}

	mc, err := c.GetMiningCandidate(ctx, &blockassembly_api.GetMiningCandidateRequest{})
	if err != nil {
		log.Fatalf("GetMiningCandidate: %v", err)
	}
	fmt.Printf("candidate: subtreeCount=%d numTxs=%d coinbaseValue=%d\n", mc.SubtreeCount, mc.NumTxs, mc.CoinbaseValue)

	// Load golden roots.
	f, err := os.Open("../../ba-subtree-bench/fixtures/golden/ingest.txt")
	if err != nil {
		log.Fatalf("open golden: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header line: cap n numRoots
	var golden []string
	for sc.Scan() {
		golden = append(golden, sc.Text())
	}

	if len(mc.SubtreeHashes) != len(golden) {
		log.Fatalf("subtree count mismatch: service=%d golden=%d", len(mc.SubtreeHashes), len(golden))
	}
	for i, h := range mc.SubtreeHashes {
		got := hex.EncodeToString(h)
		if got != golden[i] {
			log.Fatalf("subtree #%d mismatch:\n  got  %s\n  want %s", i, got, golden[i])
		}
	}
	fmt.Printf("PASS: all %d subtree hashes match the go-subtree golden (gRPC AddTx -> rust engine -> GetMiningCandidate)\n", len(golden))
}

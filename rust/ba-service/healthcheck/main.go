// Proves the existing Go gRPC client can talk to the native Rust ba-service
// (the strangler seam). Uses the canonical generated blockassembly_api client.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockassembly/blockassembly_api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	// Default matches the settings.conf-resolved address in dev contexts
	// (blockassembly_grpcListenAddress -> localhost:${BLOCK_ASSEMBLY_GRPC_PORT}=8085).
	addr := flag.String("addr", "localhost:8085", "ba-service gRPC address")
	flag.Parse()

	conn, err := grpc.NewClient(*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := blockassembly_api.NewBlockAssemblyAPIClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.HealthGRPC(ctx, &blockassembly_api.EmptyMessage{})
	if err != nil {
		log.Fatalf("HealthGRPC failed against %s: %v", *addr, err)
	}
	fmt.Printf("GO CLIENT -> RUST SERVER (%s): ok=%v details=%q\n", *addr, resp.Ok, resp.Details)
}

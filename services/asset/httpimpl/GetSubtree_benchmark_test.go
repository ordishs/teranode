package httpimpl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/asset/repository"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

const (
	benchmarkSubtreeSize = 1024 * 1024 // 1 million items
)

// TestGetSubtreeBenchmark benchmarks the GetSubtree HTTP handler with a large subtree.
// This test is skipped by default and can be run with: go test -v -run TestGetSubtreeBenchmark
// It creates a subtree with 1 million items, writes it to an in-memory store, and benchmarks
// the GetSubtree handler processing. CPU and memory profiles are written to /tmp.
func TestGetSubtreeBenchmark(t *testing.T) {
	t.Skip("Skipping benchmark test. Set RUN_SUBTREE_BENCHMARK=true to run.")

	initPrometheusMetrics()

	cpuProfile := "/tmp/getsubtree_cpu.prof"
	memProfile := "/tmp/getsubtree_mem.prof"

	fmt.Printf("GetSubtree HTTP Handler Benchmark\n")
	fmt.Printf("==================================\n")
	fmt.Printf("Subtree Size:     %d items\n", benchmarkSubtreeSize)
	fmt.Printf("CPU Cores:        %d\n", runtime.NumCPU())
	fmt.Printf("GOMAXPROCS:       %d\n", runtime.GOMAXPROCS(0))
	fmt.Println()

	ctx := context.Background()
	benchStartTime := time.Now()

	// Create in-memory blob store for subtrees
	fmt.Printf("[%s] Setting up in-memory blob store...\n", time.Since(benchStartTime))
	subtreeStore := memory.New()

	// Create a large subtree with 1M items
	fmt.Printf("[%s] Creating subtree with %d items...\n", time.Since(benchStartTime), benchmarkSubtreeSize)
	largeSubtree, err := subtreepkg.NewTreeByLeafCount(benchmarkSubtreeSize)
	require.NoError(t, err)

	// Pre-generate all hashes in parallel for faster setup
	fmt.Printf("[%s] Pre-generating %d transaction hashes using %d workers...\n",
		time.Since(benchStartTime), benchmarkSubtreeSize, runtime.NumCPU())

	txHashes := make([]chainhash.Hash, benchmarkSubtreeSize)
	numWorkers := runtime.NumCPU()
	itemsPerWorker := benchmarkSubtreeSize / numWorkers

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			start := workerID * itemsPerWorker
			end := start + itemsPerWorker
			if workerID == numWorkers-1 {
				end = benchmarkSubtreeSize
			}
			for i := start; i < end; i++ {
				txHashes[i] = chainhash.HashH([]byte(fmt.Sprintf("tx-%d", i)))
			}
		}(w)
	}
	wg.Wait()
	fmt.Printf("[%s] Hash pre-generation complete.\n", time.Since(benchStartTime))

	// Add nodes to subtree
	fmt.Printf("[%s] Adding %d nodes to subtree...\n", time.Since(benchStartTime), benchmarkSubtreeSize)
	addStartTime := time.Now()
	for i := 0; i < benchmarkSubtreeSize; i++ {
		err := largeSubtree.AddNode(txHashes[i], uint64(i%10000), uint64(250))
		require.NoError(t, err)

		if (i+1)%100000 == 0 {
			fmt.Printf("  [%s] Added %d/%d nodes (%.1f%%)\n",
				time.Since(benchStartTime), i+1, benchmarkSubtreeSize,
				100.0*float64(i+1)/float64(benchmarkSubtreeSize))
		}
	}
	addDuration := time.Since(addStartTime)
	fmt.Printf("[%s] Node addition complete in %.2fs (%.0f nodes/sec)\n",
		time.Since(benchStartTime), addDuration.Seconds(), float64(benchmarkSubtreeSize)/addDuration.Seconds())

	// Serialize the subtree
	fmt.Printf("[%s] Serializing subtree...\n", time.Since(benchStartTime))
	serializeStartTime := time.Now()
	subtreeBytes, err := largeSubtree.Serialize()
	require.NoError(t, err)
	serializeDuration := time.Since(serializeStartTime)
	fmt.Printf("[%s] Serialization complete in %.2fs (%.2f MB, %.2f MB/sec)\n",
		time.Since(benchStartTime), serializeDuration.Seconds(),
		float64(len(subtreeBytes))/(1024*1024),
		float64(len(subtreeBytes))/(1024*1024)/serializeDuration.Seconds())

	// Get the subtree root hash
	rootHash := largeSubtree.RootHash()
	require.NotNil(t, rootHash)
	fmt.Printf("[%s] Subtree root hash: %s\n", time.Since(benchStartTime), rootHash.String())

	// Store the subtree in the blob store
	fmt.Printf("[%s] Writing subtree to blob store...\n", time.Since(benchStartTime))
	storeStartTime := time.Now()
	err = subtreeStore.Set(ctx, rootHash.CloneBytes(), fileformat.FileTypeSubtree, subtreeBytes)
	require.NoError(t, err)
	storeDuration := time.Since(storeStartTime)
	fmt.Printf("[%s] Store write complete in %.2fs\n", time.Since(benchStartTime), storeDuration.Seconds())

	// Create a repository with the subtree store
	repo := &repository.Repository{
		SubtreeStore: subtreeStore,
	}

	// Create HTTP handler
	httpServer := &HTTP{
		logger:     ulogger.TestLogger{},
		settings:   &settings.Settings{},
		repository: repo,
		e:          echo.New(),
		startTime:  time.Now(),
	}

	fmt.Printf("[%s] Setup complete. Starting profiled benchmark...\n\n", time.Since(benchStartTime))

	// Start CPU profiling
	cpuFile, err := os.Create(cpuProfile)
	require.NoError(t, err)
	err = pprof.StartCPUProfile(cpuFile)
	require.NoError(t, err)

	// Run the benchmark
	const iterations = 10
	fmt.Printf("[%s] Running %d iterations of GetSubtree (BINARY_STREAM mode)...\n", time.Since(benchStartTime), iterations)

	var totalBinaryDuration time.Duration
	var totalBinaryBytes int64

	for i := 0; i < iterations; i++ {
		req := httptest.NewRequest(http.MethodGet, "/subtree/"+rootHash.String(), nil)
		rec := httptest.NewRecorder()
		c := httpServer.e.NewContext(req, rec)
		c.SetPath("/subtree/:hash")
		c.SetParamNames("hash")
		c.SetParamValues(rootHash.String())

		iterStart := time.Now()
		err = httpServer.GetSubtree(BINARY_STREAM)(c)
		iterDuration := time.Since(iterStart)

		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code)

		responseBytes := rec.Body.Len()
		totalBinaryDuration += iterDuration
		totalBinaryBytes += int64(responseBytes)

		fmt.Printf("  [%s] Iteration %d: %.2fs, %d bytes (%.2f MB/sec)\n",
			time.Since(benchStartTime), i+1, iterDuration.Seconds(), responseBytes,
			float64(responseBytes)/(1024*1024)/iterDuration.Seconds())
	}

	avgBinaryDuration := totalBinaryDuration / iterations
	avgBinaryBytes := totalBinaryBytes / iterations
	fmt.Printf("\n[%s] BINARY_STREAM Results:\n", time.Since(benchStartTime))
	fmt.Printf("  Average Duration: %.3fs\n", avgBinaryDuration.Seconds())
	fmt.Printf("  Average Response Size: %.2f MB\n", float64(avgBinaryBytes)/(1024*1024))
	fmt.Printf("  Average Throughput: %.2f MB/sec\n", float64(avgBinaryBytes)/(1024*1024)/avgBinaryDuration.Seconds())

	// Run HEX mode benchmark
	fmt.Printf("\n[%s] Running %d iterations of GetSubtree (HEX mode)...\n", time.Since(benchStartTime), iterations)

	var totalHexDuration time.Duration
	var totalHexBytes int64

	for i := 0; i < iterations; i++ {
		req := httptest.NewRequest(http.MethodGet, "/subtree/"+rootHash.String()+"/hex", nil)
		rec := httptest.NewRecorder()
		c := httpServer.e.NewContext(req, rec)
		c.SetPath("/subtree/:hash")
		c.SetParamNames("hash")
		c.SetParamValues(rootHash.String())

		iterStart := time.Now()
		err = httpServer.GetSubtree(HEX)(c)
		iterDuration := time.Since(iterStart)

		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code)

		responseBytes := rec.Body.Len()
		totalHexDuration += iterDuration
		totalHexBytes += int64(responseBytes)

		fmt.Printf("  [%s] Iteration %d: %.2fs, %d bytes (%.2f MB/sec)\n",
			time.Since(benchStartTime), i+1, iterDuration.Seconds(), responseBytes,
			float64(responseBytes)/(1024*1024)/iterDuration.Seconds())
	}

	avgHexDuration := totalHexDuration / iterations
	avgHexBytes := totalHexBytes / iterations
	fmt.Printf("\n[%s] HEX Results:\n", time.Since(benchStartTime))
	fmt.Printf("  Average Duration: %.3fs\n", avgHexDuration.Seconds())
	fmt.Printf("  Average Response Size: %.2f MB\n", float64(avgHexBytes)/(1024*1024))
	fmt.Printf("  Average Throughput: %.2f MB/sec\n", float64(avgHexBytes)/(1024*1024)/avgHexDuration.Seconds())

	// Stop CPU profiling
	pprof.StopCPUProfile()
	err = cpuFile.Close()
	require.NoError(t, err)

	// Write memory profile
	runtime.GC()
	memFile, err := os.Create(memProfile)
	require.NoError(t, err)
	err = pprof.WriteHeapProfile(memFile)
	require.NoError(t, err)
	err = memFile.Close()
	require.NoError(t, err)

	// Print summary
	totalDuration := time.Since(benchStartTime)
	fmt.Printf("\n========================================\n")
	fmt.Printf("Benchmark Summary\n")
	fmt.Printf("========================================\n")
	fmt.Printf("Total Duration:          %.2fs\n", totalDuration.Seconds())
	fmt.Printf("Subtree Size:            %d items\n", benchmarkSubtreeSize)
	fmt.Printf("Serialized Size:         %.2f MB\n", float64(len(subtreeBytes))/(1024*1024))
	fmt.Printf("BINARY_STREAM Avg Time:  %.3fs\n", avgBinaryDuration.Seconds())
	fmt.Printf("HEX Mode Avg Time:       %.3fs\n", avgHexDuration.Seconds())
	fmt.Printf("\n")
	fmt.Printf("Profiles written to:\n")
	fmt.Printf("  CPU:    %s\n", cpuProfile)
	fmt.Printf("  Memory: %s\n", memProfile)
	fmt.Printf("\nTo analyze profiles:\n")
	fmt.Printf("  go tool pprof -http=:8080 %s\n", cpuProfile)
	fmt.Printf("  go tool pprof -http=:8081 %s\n", memProfile)
}

// TestGetSubtreeJSONBenchmark benchmarks the GetSubtree JSON handler specifically.
// JSON mode requires full deserialization, so it's more expensive.
func TestGetSubtreeJSONBenchmark(t *testing.T) {
	t.Skip("Skipping benchmark test. Set RUN_SUBTREE_BENCHMARK=true to run.")

	initPrometheusMetrics()

	// Use a smaller size for JSON since it's much slower due to full deserialization
	// Must be a power of 2 for subtree creation
	const jsonSubtreeSize = 131072 // 128K items for JSON mode (2^17)

	cpuProfile := "/tmp/getsubtree_json_cpu.prof"
	memProfile := "/tmp/getsubtree_json_mem.prof"

	fmt.Printf("GetSubtree JSON Handler Benchmark\n")
	fmt.Printf("=================================\n")
	fmt.Printf("Subtree Size:     %d items\n", jsonSubtreeSize)
	fmt.Printf("CPU Cores:        %d\n", runtime.NumCPU())
	fmt.Printf("GOMAXPROCS:       %d\n", runtime.GOMAXPROCS(0))
	fmt.Println()

	benchStartTime := time.Now()

	// Create subtree
	fmt.Printf("[%s] Creating subtree with %d items...\n", time.Since(benchStartTime), jsonSubtreeSize)
	largeSubtree, err := subtreepkg.NewTreeByLeafCount(jsonSubtreeSize)
	require.NoError(t, err)

	// Add nodes
	for i := 0; i < jsonSubtreeSize; i++ {
		hash := chainhash.HashH([]byte(fmt.Sprintf("tx-%d", i)))
		err := largeSubtree.AddNode(hash, uint64(i%10000), uint64(250))
		require.NoError(t, err)

		if (i+1)%16384 == 0 {
			fmt.Printf("  [%s] Added %d/%d nodes (%.1f%%)\n",
				time.Since(benchStartTime), i+1, jsonSubtreeSize,
				100.0*float64(i+1)/float64(jsonSubtreeSize))
		}
	}

	// Serialize and store
	subtreeBytes, err := largeSubtree.Serialize()
	require.NoError(t, err)

	rootHash := largeSubtree.RootHash()
	require.NotNil(t, rootHash)
	fmt.Printf("[%s] Subtree root hash: %s\n", time.Since(benchStartTime), rootHash.String())

	// Create a mock that returns the subtree for JSON mode
	// Note: Mock's GetSubtree ignores context, only uses the hash parameter
	mockRepo := &repository.Mock{}
	mockRepo.On("GetSubtree", rootHash).Return(largeSubtree, nil)
	mockRepo.On("GetSubtreeTxIDsReader", rootHash).Return(
		io.NopCloser(bytes.NewReader(subtreeBytes)), nil)

	// Create HTTP handler
	httpServer := &HTTP{
		logger:     ulogger.TestLogger{},
		settings:   &settings.Settings{},
		repository: mockRepo,
		e:          echo.New(),
		startTime:  time.Now(),
	}

	fmt.Printf("[%s] Setup complete. Starting profiled benchmark...\n\n", time.Since(benchStartTime))

	// Start CPU profiling
	cpuFile, err := os.Create(cpuProfile)
	require.NoError(t, err)
	err = pprof.StartCPUProfile(cpuFile)
	require.NoError(t, err)

	// Run JSON benchmark
	const iterations = 5
	fmt.Printf("[%s] Running %d iterations of GetSubtree (JSON mode)...\n", time.Since(benchStartTime), iterations)

	var totalDuration time.Duration
	var totalBytes int64

	for i := 0; i < iterations; i++ {
		req := httptest.NewRequest(http.MethodGet, "/subtree/"+rootHash.String()+"/json", nil)
		rec := httptest.NewRecorder()
		c := httpServer.e.NewContext(req, rec)
		c.SetPath("/subtree/:hash")
		c.SetParamNames("hash")
		c.SetParamValues(rootHash.String())

		iterStart := time.Now()
		err = httpServer.GetSubtree(JSON)(c)
		iterDuration := time.Since(iterStart)

		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code)

		responseBytes := rec.Body.Len()
		totalDuration += iterDuration
		totalBytes += int64(responseBytes)

		fmt.Printf("  [%s] Iteration %d: %.2fs, %d bytes (%.2f MB/sec)\n",
			time.Since(benchStartTime), i+1, iterDuration.Seconds(), responseBytes,
			float64(responseBytes)/(1024*1024)/iterDuration.Seconds())
	}

	// Stop CPU profiling
	pprof.StopCPUProfile()
	err = cpuFile.Close()
	require.NoError(t, err)

	// Write memory profile
	runtime.GC()
	memFile, err := os.Create(memProfile)
	require.NoError(t, err)
	err = pprof.WriteHeapProfile(memFile)
	require.NoError(t, err)
	err = memFile.Close()
	require.NoError(t, err)

	// Print results
	avgDuration := totalDuration / iterations
	avgBytes := totalBytes / iterations
	fmt.Printf("\n[%s] JSON Results:\n", time.Since(benchStartTime))
	fmt.Printf("  Average Duration: %.3fs\n", avgDuration.Seconds())
	fmt.Printf("  Average Response Size: %.2f MB\n", float64(avgBytes)/(1024*1024))
	fmt.Printf("  Average Throughput: %.2f MB/sec\n", float64(avgBytes)/(1024*1024)/avgDuration.Seconds())
	fmt.Printf("\nProfiles written to:\n")
	fmt.Printf("  CPU:    %s\n", cpuProfile)
	fmt.Printf("  Memory: %s\n", memProfile)
}

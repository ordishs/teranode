package teranodecli

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
)

type benchmarkResult struct {
	TotalTxs        int
	Elapsed         time.Duration
	TxPerSec        float64
	ProcessedTxs    uint64
	SubtreesCreated int64
	QueueRemaining  int64
}

// createBenchmarkSettings creates a settings object configured for benchmarking.
func createBenchmarkSettings(subtreeSize int) *settings.Settings {
	s := settings.NewSettings()

	// Use a temporary directory that won't interfere with anything
	s.DataFolder = os.TempDir() + "/subtreebench"

	// Create a copy of RegressionNetParams to avoid race conditions
	chainParams := chaincfg.RegressionNetParams
	chainParams.CoinbaseMaturity = 1
	s.ChainCfgParams = &chainParams
	s.GlobalBlockHeightRetention = 10
	s.BlockValidation.OptimisticMining = false

	// Configure block assembly settings for benchmarking
	s.BlockAssembly.InitialMerkleItemsPerSubtree = subtreeSize
	s.BlockAssembly.SubtreeProcessorBatcherSize = 1024 * 1024 // Large batcher to avoid blocking

	return s
}

func runSubtreeBenchmark(subtreeSize, producers, iterations, duration int, cpuProfile, memProfile string) error {
	fmt.Printf("SubtreeProcessor Throughput Benchmark\n")
	fmt.Printf("=====================================\n")
	fmt.Printf("Subtree Size:     %d\n", subtreeSize)
	fmt.Printf("Producers:        %d\n", producers)
	fmt.Printf("CPU Cores:        %d\n", runtime.NumCPU())
	fmt.Printf("GOMAXPROCS:       %d\n", runtime.GOMAXPROCS(0))
	fmt.Println()

	// Start CPU profiling
	cpuFile, err := os.Create(cpuProfile)
	if err != nil {
		return fmt.Errorf("failed to create CPU profile: %w", err)
	}
	defer cpuFile.Close()

	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		return fmt.Errorf("failed to start CPU profile: %w", err)
	}
	defer pprof.StopCPUProfile()

	// Run the benchmark
	result := runBenchmarkCore(subtreeSize, producers, iterations, duration)

	// Stop CPU profiling
	pprof.StopCPUProfile()
	cpuFile.Close()

	// Write memory profile
	memFile, err := os.Create(memProfile)
	if err != nil {
		return fmt.Errorf("failed to create memory profile: %w", err)
	}
	defer memFile.Close()

	runtime.GC() // Force GC before memory profile
	if err := pprof.WriteHeapProfile(memFile); err != nil {
		return fmt.Errorf("failed to write memory profile: %w", err)
	}

	// Print results
	fmt.Printf("\nBenchmark Results\n")
	fmt.Printf("=================\n")
	fmt.Printf("Total Transactions:  %d\n", result.TotalTxs)
	fmt.Printf("Elapsed Time:        %.2fs\n", result.Elapsed.Seconds())
	fmt.Printf("Throughput:          %.2f tx/sec\n", result.TxPerSec)
	fmt.Printf("Processed Txs:       %d\n", result.ProcessedTxs)
	fmt.Printf("Subtrees Created:    %d\n", result.SubtreesCreated)
	fmt.Printf("Queue Remaining:     %d\n", result.QueueRemaining)
	fmt.Printf("Avg per Producer:    %.2f tx/sec\n", result.TxPerSec/float64(producers))
	fmt.Println()
	fmt.Printf("Profiles written to: %s, %s\n", cpuProfile, memProfile)

	return nil
}

func runBenchmarkCore(subtreeSize, producers, iterations, duration int) benchmarkResult {
	// Setup
	newSubtreeChan := make(chan subtreeprocessor.NewSubtreeRequest, 100)
	subtreeCount := atomic.Int64{}

	// Consume subtrees in background to prevent blocking
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		for {
			select {
			case req := <-newSubtreeChan:
				subtreeCount.Add(1)
				if req.ErrChan != nil {
					req.ErrChan <- nil
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Create settings
	settings := createBenchmarkSettings(subtreeSize)

	// Create subtree processor
	stp, err := subtreeprocessor.NewSubtreeProcessor(context.Background(), ulogger.TestLogger{}, settings, nil, nil, nil, newSubtreeChan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create subtree processor: %v\n", err)
		os.Exit(1)
	}
	defer stp.Stop(ctx)

	// Determine number of iterations
	numTxs := iterations
	if duration > 0 {
		// Estimate based on duration - we'll stop after time elapses
		fmt.Printf("Running for %d seconds (iteration count will be determined by throughput)\n\n", duration)
		numTxs = 100_000_000 // Large number, will be stopped by timer
	} else {
		fmt.Printf("Running for %d iterations\n\n", numTxs)
	}

	// Pre-generate transaction hashes to avoid crypto overhead in benchmark
	fmt.Printf("Pre-generating %d transaction hashes...\n", numTxs)
	txHashes := make([]chainhash.Hash, numTxs)
	parentHashes := make([]chainhash.Hash, numTxs)
	for i := 0; i < numTxs; i++ {
		txid := make([]byte, 32)
		_, _ = rand.Read(txid)
		txHashes[i] = chainhash.Hash(txid)

		parentID := make([]byte, 32)
		_, _ = rand.Read(parentID)
		parentHashes[i] = chainhash.Hash(parentID)
	}
	fmt.Printf("Pre-generation complete.\n\n")

	// Measure metrics
	var opsCompleted atomic.Int64
	startTime := time.Now()

	// Setup timer if duration-based
	var timeoutChan <-chan time.Time
	if duration > 0 {
		timeoutChan = time.After(time.Duration(duration) * time.Second)
	}

	// Run benchmark with multiple producer goroutines
	var wg sync.WaitGroup
	var stopped atomic.Bool
	itemsPerGoroutine := numTxs / producers
	const batchSize = 1024

	fmt.Printf("Starting benchmark with %d producers (batch size: %d)...\n", producers, batchSize)

	for g := 0; g < producers; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			start := goroutineID * itemsPerGoroutine
			end := start + itemsPerGoroutine
			if goroutineID == producers-1 {
				end = numTxs // Last goroutine handles remainder
			}

			// Pre-allocate batch slices
			nodes := make([]subtreepkg.Node, 0, batchSize)
			inpoints := make([]subtreepkg.TxInpoints, 0, batchSize)

			for i := start; i < end; i++ {
				// Check if we should stop (duration-based)
				if stopped.Load() {
					break
				}

				// Check timeout
				select {
				case <-timeoutChan:
					stopped.Store(true)
					// Flush remaining batch before returning
					if len(nodes) > 0 {
						stp.AddBatch(nodes, inpoints)
						opsCompleted.Add(int64(len(nodes)))
					}
					return
				default:
				}

				// Add to batch
				nodes = append(nodes, subtreepkg.Node{
					Hash: txHashes[i],
					Fee:  uint64(i % 10000),
				})
				inpoints = append(inpoints, subtreepkg.TxInpoints{
					ParentTxHashes: []chainhash.Hash{parentHashes[i]},
				})

				// Submit batch when full
				if len(nodes) >= batchSize {
					stp.AddBatch(nodes, inpoints)
					opsCompleted.Add(int64(len(nodes)))
					// Reset slices (reuse backing array)
					nodes = nodes[:0]
					inpoints = inpoints[:0]
				}
			}

			// Flush remaining batch
			if len(nodes) > 0 {
				stp.AddBatch(nodes, inpoints)
				opsCompleted.Add(int64(len(nodes)))
			}
		}(g)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	// Wait for queue to drain
	fmt.Printf("\nWaiting for queue to drain...\n")
	drainStart := time.Now()
	for stp.QueueLength() > 0 && time.Since(drainStart) < 30*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	drainTime := time.Since(drainStart)
	fmt.Printf("Queue drained in %.2fs\n", drainTime.Seconds())

	totalCompleted := int(opsCompleted.Load())
	txPerSec := float64(totalCompleted) / elapsed.Seconds()
	queueRemaining := stp.QueueLength()
	processedTxs := stp.TxCount()
	subtreesCreated := subtreeCount.Load()

	return benchmarkResult{
		TotalTxs:        totalCompleted,
		Elapsed:         elapsed,
		TxPerSec:        txPerSec,
		ProcessedTxs:    processedTxs,
		SubtreesCreated: subtreesCreated,
		QueueRemaining:  queueRemaining,
	}
}

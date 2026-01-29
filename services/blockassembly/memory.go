package blockassembly

import (
	"github.com/shirou/gopsutil/v4/mem"
)

// CalculateMaxTransactions calculates the maximum number of transactions based on system memory.
// It returns the calculated limit based on the formula:
// (TotalMemory * memoryPercent / 100) / bytesPerTx
//
// Parameters:
//   - bytesPerTx: Estimated memory footprint per transaction in bytes
//   - memoryPercent: Percentage of total system memory to use (0-100)
//
// Returns:
//   - int64: Calculated maximum transaction count
//   - error: Any error encountered while detecting system memory
func CalculateMaxTransactions(bytesPerTx int, memoryPercent int) (int64, error) {
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}

	usableMemory := (vmStat.Total * uint64(memoryPercent)) / 100
	maxTx := int64(usableMemory) / int64(bytesPerTx)

	return maxTx, nil
}

// GetTotalSystemMemory returns the total system memory in bytes.
// This is useful for logging and debugging purposes.
//
// Returns:
//   - uint64: Total system memory in bytes
//   - error: Any error encountered while detecting system memory
func GetTotalSystemMemory() (uint64, error) {
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}

	return vmStat.Total, nil
}

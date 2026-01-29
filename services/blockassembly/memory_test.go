package blockassembly

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCalculateMaxTransactions(t *testing.T) {
	maxTx, err := CalculateMaxTransactions(300, 80)
	require.NoError(t, err)
	require.Greater(t, maxTx, int64(0), "calculated max should be positive")
}

func TestGetTotalSystemMemory(t *testing.T) {
	totalMem, err := GetTotalSystemMemory()
	require.NoError(t, err)
	require.Greater(t, totalMem, uint64(0), "system memory should be positive")
}

func TestCalculateMaxTransactions_VariousSettings(t *testing.T) {
	tests := []struct {
		name          string
		bytesPerTx    int
		memoryPercent int
	}{
		{"default settings", 300, 80},
		{"smaller tx estimate", 150, 80},
		{"larger tx estimate", 600, 80},
		{"lower memory percent", 300, 50},
		{"higher memory percent", 300, 90},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			maxTx, err := CalculateMaxTransactions(tc.bytesPerTx, tc.memoryPercent)
			require.NoError(t, err)
			require.Greater(t, maxTx, int64(0), "calculated max should be positive")
		})
	}
}

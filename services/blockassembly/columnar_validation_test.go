package blockassembly

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidatePackedOffsets locks the per-batch offset-table invariants.
// A panic-on-malformed-input regression in AddTxBatchColumnar exposes the
// block-assembly process to any peer that can issue gRPC, so each rejection
// path is its own subtest.
func TestValidatePackedOffsets(t *testing.T) {
	t.Run("well-formed", func(t *testing.T) {
		require.NoError(t, validatePackedOffsets([]uint32{0, 3, 5, 8}, 8))
	})

	t.Run("single zero-length batch is well-formed", func(t *testing.T) {
		// txCount=1, no entries → offsets [0, 0]; bufferLen 0.
		require.NoError(t, validatePackedOffsets([]uint32{0, 0}, 0))
	})

	t.Run("first offset must be zero", func(t *testing.T) {
		err := validatePackedOffsets([]uint32{1, 3, 5, 8}, 8)
		require.Error(t, err)
		require.Contains(t, err.Error(), "first offset must be 0")
	})

	t.Run("last offset must equal buffer length", func(t *testing.T) {
		err := validatePackedOffsets([]uint32{0, 3, 5, 7}, 8)
		require.Error(t, err)
		require.Contains(t, err.Error(), "last offset")
	})

	t.Run("non-monotonic offsets rejected", func(t *testing.T) {
		err := validatePackedOffsets([]uint32{0, 5, 3, 8}, 8)
		require.Error(t, err)
		require.Contains(t, err.Error(), "monotonically non-decreasing")
	})

	t.Run("too-few entries rejected", func(t *testing.T) {
		err := validatePackedOffsets([]uint32{0}, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "at least 2 entries")
	})

	t.Run("offset exceeding buffer length rejected", func(t *testing.T) {
		// Offsets monotonic and start at 0, but last exceeds bufferLen.
		err := validatePackedOffsets([]uint32{0, 1 << 31}, 100)
		require.Error(t, err)
		require.Contains(t, err.Error(), "last offset")
	})
}

// TestValidatePackedVouts locks the per-tx count-prefixed payload contract.
// subtree.NewTxInpointsFromPacked aliases without validating; any malformed
// shape that slips past this check panics later inside GetParentVoutsAtIndex.
func TestValidatePackedVouts(t *testing.T) {
	t.Run("well-formed two-parent tx", func(t *testing.T) {
		// parent 0: 2 vouts {7, 8}; parent 1: 1 vout {9}
		require.NoError(t, validatePackedVouts([]uint32{2, 7, 8, 1, 9}, 2))
	})

	t.Run("empty slice with zero parents", func(t *testing.T) {
		require.NoError(t, validatePackedVouts(nil, 0))
		require.NoError(t, validatePackedVouts([]uint32{}, 0))
	})

	t.Run("each parent can have zero vouts", func(t *testing.T) {
		// Two parents, each with zero vouts: [0, 0]
		require.NoError(t, validatePackedVouts([]uint32{0, 0}, 2))
	})

	t.Run("missing count word rejected", func(t *testing.T) {
		// numParents=2 but slice is empty
		err := validatePackedVouts(nil, 2)
		require.Error(t, err)
		require.Contains(t, err.Error(), "ran out of bytes")
	})

	t.Run("count exceeds remaining buffer rejected", func(t *testing.T) {
		// count=10 but only 1 entry available
		err := validatePackedVouts([]uint32{10, 7}, 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "declares count=10")
	})

	t.Run("trailing garbage rejected", func(t *testing.T) {
		// parent 0: 1 vout {7}; then a stray uint32 with no parent claiming it
		err := validatePackedVouts([]uint32{1, 7, 99}, 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "trailing bytes")
	})

	t.Run("max-uint32 count caught early", func(t *testing.T) {
		err := validatePackedVouts([]uint32{1<<32 - 1, 0}, 1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "declares count=")
	})
}

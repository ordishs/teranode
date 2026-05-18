package pruner

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNormaliseAddDeletedChildrenResponse covers both response shapes that the
// addDeletedChildren mod-teranode op may return: the UDF path's
// map[interface{}]interface{} and the native-op path's map[string]interface{}.
// The pruner's per-record handler dereferences `respMap["status"]` so both
// shapes must round-trip into a single interface-keyed map.
func TestNormaliseAddDeletedChildrenResponse(t *testing.T) {
	t.Run("interface-keyed map passes through unchanged", func(t *testing.T) {
		in := map[interface{}]interface{}{
			"status":    "OK",
			"errorCode": "",
		}

		out, ok := normaliseAddDeletedChildrenResponse(in)
		require.True(t, ok)
		require.Equal(t, "OK", out["status"])
		require.Equal(t, "", out["errorCode"])
	})

	t.Run("string-keyed map is rekeyed to interface-keyed", func(t *testing.T) {
		in := map[string]interface{}{
			"status":    "ERROR",
			"errorCode": "TX_NOT_FOUND",
		}

		out, ok := normaliseAddDeletedChildrenResponse(in)
		require.True(t, ok)
		require.Equal(t, "ERROR", out["status"])
		require.Equal(t, "TX_NOT_FOUND", out["errorCode"])
	})

	t.Run("unrecognised shape returns false", func(t *testing.T) {
		_, ok := normaliseAddDeletedChildrenResponse("not-a-map")
		require.False(t, ok)

		_, ok = normaliseAddDeletedChildrenResponse(nil)
		require.False(t, ok)

		_, ok = normaliseAddDeletedChildrenResponse(42)
		require.False(t, ok)
	})
}

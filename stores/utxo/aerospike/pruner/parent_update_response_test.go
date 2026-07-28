package pruner

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAddDeletedChildrenStatus covers both response shapes that the
// addDeletedChildren mod-teranode op may return — the UDF path's
// map[interface{}]interface{} and the native-op path's map[string]interface{} —
// and the classification the pruner's per-record handler keys on. In particular
// the ERROR/TX_NOT_FOUND case is the branch that decides whether a missing parent
// is treated as a benign skip or a hard cleanup failure, so it is asserted for
// both shapes (the native path's TX_NOT_FOUND handling had no direct coverage).
func TestAddDeletedChildrenStatus(t *testing.T) {
	t.Run("interface-keyed OK", func(t *testing.T) {
		status, errCode, errMsg, ok := addDeletedChildrenStatus(map[interface{}]interface{}{
			"status":    "OK",
			"errorCode": "",
		})
		require.True(t, ok)
		require.Equal(t, "OK", status)
		require.Empty(t, errCode)
		require.Empty(t, errMsg)
	})

	t.Run("string-keyed OK (native-op shape)", func(t *testing.T) {
		status, errCode, _, ok := addDeletedChildrenStatus(map[string]interface{}{
			"status":    "OK",
			"errorCode": "",
		})
		require.True(t, ok)
		require.Equal(t, "OK", status)
		require.Empty(t, errCode)
	})

	t.Run("interface-keyed ERROR TX_NOT_FOUND", func(t *testing.T) {
		status, errCode, _, ok := addDeletedChildrenStatus(map[interface{}]interface{}{
			"status":    "ERROR",
			"errorCode": "TX_NOT_FOUND",
		})
		require.True(t, ok)
		require.Equal(t, "ERROR", status)
		require.Equal(t, "TX_NOT_FOUND", errCode)
	})

	t.Run("string-keyed ERROR TX_NOT_FOUND (native-op shape)", func(t *testing.T) {
		status, errCode, _, ok := addDeletedChildrenStatus(map[string]interface{}{
			"status":    "ERROR",
			"errorCode": "TX_NOT_FOUND",
		})
		require.True(t, ok)
		require.Equal(t, "ERROR", status)
		require.Equal(t, "TX_NOT_FOUND", errCode)
	})

	t.Run("string-keyed ERROR with message", func(t *testing.T) {
		status, errCode, errMsg, ok := addDeletedChildrenStatus(map[string]interface{}{
			"status":       "ERROR",
			"errorCode":    "SOME_FAILURE",
			"errorMessage": "boom",
		})
		require.True(t, ok)
		require.Equal(t, "ERROR", status)
		require.Equal(t, "SOME_FAILURE", errCode)
		require.Equal(t, "boom", errMsg)
	})

	t.Run("map without a string status reports not-ok (caller falls through to success)", func(t *testing.T) {
		_, _, _, ok := addDeletedChildrenStatus(map[interface{}]interface{}{"errorCode": "X"})
		require.False(t, ok)

		_, _, _, ok = addDeletedChildrenStatus(map[string]interface{}{"status": 42})
		require.False(t, ok)
	})

	t.Run("unrecognised shape returns false", func(t *testing.T) {
		_, _, _, ok := addDeletedChildrenStatus("not-a-map")
		require.False(t, ok)

		_, _, _, ok = addDeletedChildrenStatus(nil)
		require.False(t, ok)

		_, _, _, ok = addDeletedChildrenStatus(42)
		require.False(t, ok)
	})
}

package utxo

import (
	"strings"
	"testing"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// Store-backed wedge and retry coverage is in tests.ConflictRecoveryDescendants
// and tests.ConflictRecoveryFailures; they cannot retain erased spend links.
func TestRollback_CapsJoinedErrors(t *testing.T) {
	var failures []error
	for i := 0; i < 30; i++ {
		failures = append(failures, errors.NewProcessingError("failure %d", i))
	}
	err := capRollbackFailures(failures)
	require.Error(t, err)
	require.Equal(t, 20, strings.Count(err.Error(), "; "))
	require.Contains(t, err.Error(), "10 more rollback failures omitted")
}

package aerospike

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	spendpkg "github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// TestResolveSpendCompletionsReportsAllSuccessfulSpends pins the input to the
// rollback decision: resolveSpendCompletions must report every spend that
// succeeded, whatever the error class of the ones that failed. Spend then rolls
// that whole set back unconditionally — partial spends are never left behind for
// a hoped-for retry, because a surviving spend names a spender whose record is
// never created (#1214).
//
// Error classes here deliberately exclude ErrTxNotFound, which takes the
// "already blessed" store lookup path; the missing-parent case is covered
// end-to-end by tests.PartialSpendRollback against a real store.
func TestResolveSpendCompletionsReportsAllSuccessfulSpends(t *testing.T) {
	store := &Store{logger: ulogger.TestLogger{}}

	newSpend := func(vout uint32, err error) *utxo.Spend {
		return &utxo.Spend{TxID: &chainhash.Hash{}, Vout: vout, Err: err}
	}

	tests := []struct {
		name          string
		failure       error
		expectSuccess int
	}{
		{name: "locked parent", failure: errors.NewTxLockedError("locked"), expectSuccess: 2},
		{name: "double spend", failure: errors.NewUtxoSpentError(chainhash.Hash{}, 0, chainhash.Hash{}, spendpkg.NewSpendingData(&chainhash.Hash{}, 0)), expectSuccess: 2},
		{name: "conflicting tx", failure: errors.NewTxConflictingError("conflicting"), expectSuccess: 2},
		{name: "frozen utxo", failure: errors.NewUtxoFrozenError("frozen"), expectSuccess: 2},
		{name: "hash mismatch", failure: errors.NewUtxoHashMismatchError("mismatch"), expectSuccess: 2},
		{name: "device overload", failure: errors.NewStorageError("DEVICE_OVERLOAD"), expectSuccess: 2},
		{name: "service unavailable", failure: errors.NewServiceUnavailableError("circuit breaker open"), expectSuccess: 2},
		{name: "every spend failed", failure: errors.NewStorageError("DEVICE_OVERLOAD"), expectSuccess: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			items := make([]*batchSpend, 0, 3)

			for i := 0; i < 3; i++ {
				err := tc.failure
				if i < tc.expectSuccess {
					err = nil
				}

				items = append(items, &batchSpend{spend: newSpend(uint32(i), err)}) // nolint:gosec
			}

			result := store.resolveSpendCompletions(context.Background(), bt.NewTx(), items, false)
			require.Len(t, result.spentSpends, tc.expectSuccess,
				"every successful spend must be reported so Spend can roll it back")
		})
	}
}

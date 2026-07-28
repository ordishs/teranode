package aerospike

import (
	"strings"
	"testing"

	as "github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/aerospike-client-go/v8/types"
	"github.com/bsv-blockchain/teranode/stores/utxo"
)

func TestDescribeAerospikeBatchRecordIncludesResponseShape(t *testing.T) {
	batchRecord := &as.BatchWrite{}
	batchRecord.ResultCode = types.OK
	batchRecord.Record = &as.Record{
		Bins: as.BinMap{
			LuaSuccess.String(): nil,
			"other":             []byte{1, 2, 3},
		},
	}

	got := describeAerospikeBatchRecord(batchRecord)

	for _, want := range []string{
		"resultCode=OK",
		"inDoubt=false",
		"SUCCESS:<nil>",
		"other:[]uint8(len=3)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostics %q missing %q", got, want)
		}
	}
}

func TestDescribeAerospikeBatchRecordIncludesBatchError(t *testing.T) {
	batchRecord := &as.BatchWrite{}
	batchRecord.ResultCode = types.PARAMETER_ERROR
	batchRecord.Err = as.ErrInvalidParam

	got := describeAerospikeBatchRecord(batchRecord)

	for _, want := range []string{
		"resultCode=PARAMETER_ERROR",
		"recordErr=ResultCode: PARAMETER_ERROR",
		"record=<nil>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostics %q missing %q", got, want)
		}
	}
}

func TestDiagnosticHelpersHandleNilInputs(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "nil batch record", got: describeAerospikeBatchRecord(nil), want: "batchRecord=<nil>"},
		{name: "nil record", got: describeAerospikeRecord(nil), want: "record=<nil>"},
		{name: "nil hash", got: describeChainHash(nil), want: "<nil>"},
		{name: "nil spend", got: describeUTXOSpend(nil), want: "<nil>"},
		{name: "nil txid spend", got: describeUTXOSpend(&utxo.Spend{}), want: "<nil>:0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestCreateSpendErrorHandlesNilBatchItem(t *testing.T) {
	s := &Store{}

	err := s.createSpendError(LuaErrorInfo{
		ErrorCode: LuaErrorCodeSpent,
		Message:   "spent",
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "nil batch item") {
		t.Fatalf("error %q missing nil batch item context", err.Error())
	}
}

package aerospike

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
)

func describeAerospikeBatchRecord(batchRecord aerospike.BatchRecordIfc) string {
	if batchRecord == nil {
		return "batchRecord=<nil>"
	}

	rec := batchRecord.BatchRec()
	if rec == nil {
		return "batchRecord.BatchRec=<nil>"
	}

	parts := []string{
		fmt.Sprintf("resultCode=%s", rec.ResultCode.String()),
		fmt.Sprintf("inDoubt=%t", rec.InDoubt),
	}

	if rec.Key != nil {
		parts = append(parts, fmt.Sprintf("key=%s", rec.Key.String()))
	}

	if rec.Err != nil {
		parts = append(parts, fmt.Sprintf("recordErr=%s", rec.Err.Error()))
	}

	if rec.Record == nil {
		parts = append(parts, "record=<nil>")
		return strings.Join(parts, ", ")
	}

	parts = append(parts, describeAerospikeRecord(rec.Record))
	return strings.Join(parts, ", ")
}

func describeAerospikeRecord(record *aerospike.Record) string {
	if record == nil {
		return "record=<nil>"
	}

	if record.Bins == nil {
		return "bins=<nil>"
	}

	return fmt.Sprintf("bins=%s", describeAerospikeBins(record.Bins))
}

func describeAerospikeBins(bins aerospike.BinMap) string {
	if len(bins) == 0 {
		return "{}"
	}

	keys := make([]string, 0, len(bins))
	for key := range bins {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	const maxBins = 8
	limit := len(keys)
	if limit > maxBins {
		limit = maxBins
	}

	parts := make([]string, 0, limit+1)
	for idx, key := range keys {
		if idx == maxBins {
			parts = append(parts, fmt.Sprintf("...+%d more", len(keys)-idx))
			break
		}
		parts = append(parts, fmt.Sprintf("%s:%s", key, describeAerospikeValue(bins[key])))
	}

	return "{" + strings.Join(parts, ", ") + "}"
}

func describeAerospikeValue(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return fmt.Sprintf("%T(len=%d)", value, len(v))
	case []interface{}:
		return fmt.Sprintf("%T(len=%d)", value, len(v))
	case map[interface{}]interface{}:
		return fmt.Sprintf("%T(len=%d)", value, len(v))
	case map[string]interface{}:
		return fmt.Sprintf("%T(len=%d)", value, len(v))
	case aerospike.OpResults:
		return fmt.Sprintf("%T(len=%d)", value, len(v))
	default:
		return fmt.Sprintf("%T", value)
	}
}

func describeChainHash(hash *chainhash.Hash) string {
	if hash == nil {
		return "<nil>"
	}
	return hash.String()
}

func describeUTXOSpend(spend *utxo.Spend) string {
	if spend == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s:%d", describeChainHash(spend.TxID), spend.Vout)
}

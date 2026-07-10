//go:build aerospike

package aerospike

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// sumNamespaceStat reads a numeric Aerospike namespace statistic, summed across
// every node in the cluster.
func sumNamespaceStat(t *testing.T, db *Store, stat string) int64 {
	t.Helper()

	cmd := "namespace/" + db.namespace

	var total int64

	nodes := db.client.Cluster().GetNodes()
	require.NotEmpty(t, nodes, "cluster must expose at least one node")

	infoPolicy := aerospike.NewInfoPolicy()

	for _, nd := range nodes {
		info, err := nd.RequestInfo(infoPolicy, cmd)
		require.NoError(t, err)

		for _, kv := range strings.Split(info[cmd], ";") {
			name, value, found := strings.Cut(kv, "=")
			if !found || name != stat {
				continue
			}

			v, perr := strconv.ParseInt(value, 10, 64)
			require.NoError(t, perr)

			total += v
		}
	}

	return total
}

// TestNativeOp_AddDeletedChildren_Integration verifies against a real Teraspike
// server (the BSV fork of aerospike-server with native operate-path support,
// wire op 200) that the pruner's addDeletedChildren parent-update — the op that
// PR #1269 re-routed onto the native builder in the combined cleanup path — runs
// as a NATIVE batch write (zero batch_sub_udf) and correctly mutates the parent's
// deletedChildren map.
//
// It builds the record via exactly the call GetPrunerService's
// BuildAddDeletedChildrenRecord uses, so it exercises the shared native path the
// pruner relies on. Skips cleanly on a stock Aerospike image (native-op probe
// fails); point at a Teraspike build with AEROSPIKE_CONTAINER_IMAGE to run it.
func TestNativeOp_AddDeletedChildren_Integration(t *testing.T) {
	InitPrometheusMetrics()

	logger := ulogger.NewErrorTestLogger(t)
	ctx := context.Background()

	container, err := runAerospikeTestContainer(ctx)
	if err != nil {
		t.Skipf("Skipping: Aerospike container not available (%v)", err)
	}

	t.Cleanup(func() { require.NoError(t, container.Terminate(ctx)) })

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.ServicePort(ctx)
	require.NoError(t, err)

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.Aerospike.UseNativeTeranodeOps = true

	aeroURL, err := url.Parse(fmt.Sprintf("aerospike://%s:%d/test?set=test&externalStore=file://./data/externalStore", host, port))
	require.NoError(t, err)

	db, err := New(ctx, logger, tSettings, aeroURL)
	require.NoError(t, err)

	db.SetExternalStore(memory.New())

	if !db.useNativeTeranodeOps {
		t.Skipf("native ops not enabled (image %q is not a Teraspike build); set AEROSPIKE_CONTAINER_IMAGE to a native-op server", os.Getenv("AEROSPIKE_CONTAINER_IMAGE"))
	}

	// Seed a parent record so addDeletedChildren has a record to update
	// (the op returns TX_NOT_FOUND for a missing record and mutates nothing).
	var parentHash chainhash.Hash
	parentHash[0] = 0xAB

	key, err := aerospike.NewKey(db.namespace, db.setName, parentHash[:])
	require.NoError(t, err)
	require.NoError(t, db.client.PutBins(nil, key, aerospike.NewBin("seed", 1)))

	var childHash chainhash.Hash
	childHash[0] = 0xCD

	childList := []interface{}{childHash.String()}

	udfBefore := sumNamespaceStat(t, db, "batch_sub_udf_complete")

	// Build the record exactly as GetPrunerService.BuildAddDeletedChildrenRecord does.
	rec := db.teranodeBatchRecord(aerospike.NewBatchUDFPolicy(), LuaPackage, key, subOpAddDeletedChildren, "addDeletedChildren", childList)

	_, builtAsUDF := rec.(*aerospike.BatchUDF)
	require.False(t, builtAsUDF, "a native-op store must build addDeletedChildren as a BatchWrite, not a NewBatchUDF")

	require.NoError(t, db.client.BatchOperate(nil, []aerospike.BatchRecordIfc{rec}))
	require.NoError(t, rec.BatchRec().Err, "native addDeletedChildren batch operation must succeed")

	// The parent's deletedChildren map must now contain the child hash — proof
	// the native subOpAddDeletedChildren dispatcher executed server-side.
	parent, err := db.client.Get(nil, key, fields.DeletedChildren.String())
	require.NoError(t, err)
	require.NotNil(t, parent)

	deletedChildren, ok := parent.Bins[fields.DeletedChildren.String()].(map[interface{}]interface{})
	require.Truef(t, ok, "deletedChildren must be a map, got %T", parent.Bins[fields.DeletedChildren.String()])
	require.Equal(t, true, deletedChildren[childHash.String()], "child hash must be recorded in the parent's deletedChildren map")

	udfAfter := sumNamespaceStat(t, db, "batch_sub_udf_complete")

	// The decisive check: the parent was mutated (native dispatcher ran) yet the
	// server's Lua-UDF sub-transaction counter did not move at all. The native
	// wire-op-200 path is deliberately not counted under batch_sub_udf.
	require.Equalf(t, udfBefore, udfAfter, "addDeletedChildren must NOT invoke a Lua UDF on a native-op store (batch_sub_udf_complete went %d -> %d)", udfBefore, udfAfter)
}

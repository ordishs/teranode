// Package aerospike — native operate-path support for mod-teranode.
//
// Each mod-teranode UDF function (spend, setMined, freeze, ...) can be
// invoked through one of two paths:
//
//  1. UDF path (legacy):  aerospike.NewBatchUDF(..., "<fn>", args...)
//  2. Native path:        aerospike.NewBatchWrite(..., TeranodeModifyOp("result", payload))
//
// The native path bypasses the UDF executor and runs under the same
// lock as native ops like LIST_APPEND. It requires both:
//
//   - A server with the BSV-forked aerospike-server-private (which
//     adds wire opcodes 200/201 and the SUBOP_TABLE dispatcher).
//   - The setting `AerospikeSettings.UseNativeTeranodeOps = true`.
//
// If either is missing, calls fall back transparently to the UDF
// path. The decision is made once at store init and cached.
//
// Call sites should not branch themselves — they use the wrappers
// teranodeBatchRecord / executeTeranodeOp here, which hide the
// path selection.

package aerospike

import (
	"context"
	"fmt"
	"time"

	aerospike "github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/aerospike-client-go/v8/types"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/vmihailenco/msgpack/v5"
)

// Sub-op IDs are wire contract — frozen, must match the SUBOP_TABLE
// in modules/mod-teranode/src/main/mod_teranode_native_op.c on the
// server-private fork. Never renumber.
const (
	subOpSpend                  uint8 = 1
	subOpSpendMulti             uint8 = 2
	subOpUnspend                uint8 = 3
	subOpSetMined               uint8 = 4
	subOpFreeze                 uint8 = 5
	subOpUnfreeze               uint8 = 6
	subOpReassign               uint8 = 7
	subOpSetConflicting         uint8 = 8
	subOpPreserveUntil          uint8 = 9
	subOpSetLocked              uint8 = 10
	subOpIncrementSpentExtraRec uint8 = 11
	subOpSetDeleteAtHeight      uint8 = 12
	subOpAddDeletedChildren     uint8 = 13
)

// nativeOpResultBin is the bin name used by the native-op result
// message. The dispatcher writes the result map into this bin in the
// response; the request bin name is echoed back unchanged so callers
// can read result by name.
const nativeOpResultBin = "result"

// encodeNativeOpPayload serializes a sub-op invocation onto the wire
// as MessagePack `[sub_op_id, [args...]]` matching the dispatcher's
// expected format.
func encodeNativeOpPayload(subOp uint8, args []any) ([]byte, error) {
	return msgpack.Marshal([]any{subOp, args})
}

// teranodeBatchRecord builds a BatchRecord that invokes a mod-teranode
// function. The record uses either the native operate-path (when the
// store has determined both the setting + capability allow it) or the
// legacy UDF path. Args must be plain Go values (int64, []byte, bool,
// string, []any, map[any]any, ...) that round-trip through both
// MessagePack and aerospike.NewValue.
//
// udfPolicy / udfPackage are used only when falling back to the UDF
// path. Most call sites pass LuaPackage as udfPackage; setMined
// optionally uses LuaPackageMined; spendMulti optionally uses one of
// s.spendLuaPackages. Pass nil for udfPolicy to get default.
func (s *Store) teranodeBatchRecord(
	udfPolicy *aerospike.BatchUDFPolicy,
	udfPackage string,
	key *aerospike.Key,
	subOp uint8,
	udfFnName string,
	args ...any,
) aerospike.BatchRecordIfc {
	if s.useNativeTeranodeOps {
		payload, err := encodeNativeOpPayload(subOp, args)
		if err != nil {
			// Fallback: encoding shouldn't fail for plain Go types.
			// Log once and use UDF.
			s.logger.Warnf("[teranodeBatchRecord] msgpack encode failed for sub_op=%d: %v; falling back to UDF",
				subOp, err)
			return s.teranodeBatchUDFRecord(udfPolicy, udfPackage, key, udfFnName, args)
		}
		return aerospike.NewBatchWrite(
			aerospike.NewBatchWritePolicy(),
			key,
			aerospike.TeranodeModifyOp(nativeOpResultBin, payload),
		)
	}
	return s.teranodeBatchUDFRecord(udfPolicy, udfPackage, key, udfFnName, args)
}

// teranodeBatchUDFRecord is the UDF-path branch of teranodeBatchRecord.
// Split out so it can also be called explicitly when a fallback is
// needed mid-flight (e.g. after the native path returned an error
// the call site classifies as transient).
func (s *Store) teranodeBatchUDFRecord(
	udfPolicy *aerospike.BatchUDFPolicy,
	udfPackage string,
	key *aerospike.Key,
	udfFnName string,
	args []any,
) aerospike.BatchRecordIfc {
	if udfPolicy == nil {
		udfPolicy = aerospike.NewBatchUDFPolicy()
	}
	if udfPackage == "" {
		udfPackage = LuaPackage
	}
	udfArgs := make([]aerospike.Value, len(args))
	for i, a := range args {
		udfArgs[i] = aerospike.NewValue(a)
	}
	return aerospike.NewBatchUDF(udfPolicy, key, udfPackage, udfFnName, udfArgs...)
}

// executeTeranodeOp issues a single-record (non-batch) mod-teranode
// invocation against the store. Returns the same shape of result as
// client.Execute would, regardless of which path was taken: an
// interface{} carrying the result map (or nil if the function
// returned no value).
func (s *Store) executeTeranodeOp(
	udfPolicy *aerospike.WritePolicy,
	key *aerospike.Key,
	subOp uint8,
	udfFnName string,
	args ...any,
) (any, aerospike.Error) {
	if s.useNativeTeranodeOps {
		payload, err := encodeNativeOpPayload(subOp, args)
		if err != nil {
			s.logger.Warnf("[executeTeranodeOp] msgpack encode failed for sub_op=%d: %v; falling back to UDF",
				subOp, err)
			return s.executeTeranodeUDF(udfPolicy, key, udfFnName, args)
		}
		writePolicy := aerospike.NewWritePolicy(0, 0)
		rec, opErr := s.client.Operate(writePolicy, key, aerospike.TeranodeModifyOp(nativeOpResultBin, payload))
		if opErr != nil {
			return nil, opErr
		}
		if rec == nil {
			return nil, nil
		}
		return rec.Bins[nativeOpResultBin], nil
	}
	return s.executeTeranodeUDF(udfPolicy, key, udfFnName, args)
}

func (s *Store) executeTeranodeUDF(
	udfPolicy *aerospike.WritePolicy,
	key *aerospike.Key,
	udfFnName string,
	args []any,
) (any, aerospike.Error) {
	udfArgs := make([]aerospike.Value, len(args))
	for i, a := range args {
		udfArgs[i] = aerospike.NewValue(a)
	}
	return s.client.Execute(udfPolicy, key, LuaPackage, udfFnName, udfArgs...)
}

// detectNativeTeranodeOpSupport probes the cluster to confirm it
// understands AS_MSG_OP_TERANODE_MODIFY (wire op 200). Returns true
// only when the probe got a response shape consistent with our
// dispatcher (PARAMETER_ERROR for malformed payload, or OK for a
// valid one). Anything else — connection error, timeout, an
// unexpected result code — biases toward false-negative so the
// fallback never runs against a server that doesn't support the op.
//
// Called once during store construction; the result is cached in
// s.useNativeTeranodeOps.
func (s *Store) detectNativeTeranodeOpSupport(ctx context.Context) bool {
	if !s.settings.Aerospike.UseNativeTeranodeOps {
		return false
	}

	// Probe key — chosen to never collide with real txid keys (always
	// 32 bytes / chainhash). The bin we read back from the response
	// also doesn't pollute the namespace because we never commit; the
	// dispatcher fails fast on the malformed payload.
	probeKey, err := aerospike.NewKey(s.namespace, s.setName, "_teranode-native-op-probe")
	if err != nil {
		s.logger.Warnf("[teranode-native-op] probe key creation failed: %v; falling back to UDF path", err)
		return false
	}

	// Intentionally malformed payload: msgpack `nil` (0xc0). The
	// dispatcher's first step is as_unpack_list_header_element_count;
	// nil isn't a list header, so we expect AS_ERR_PARAMETER (=4).
	probeOp := aerospike.TeranodeModifyOp(nativeOpResultBin, []byte{0xc0})

	policy := aerospike.NewWritePolicy(0, 0)
	policy.TotalTimeout = 2 * time.Second

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = probeCtx // reserved for future cancellation plumbing into the client

	_, opErr := s.client.Operate(policy, probeKey, probeOp)
	if opErr == nil {
		// Unexpected — we sent garbage; getting OK means the dispatcher
		// is even more permissive than we thought. Treat as supported.
		return true
	}

	// Aerospike Error wraps a result code; PARAMETER_ERROR (=4) is
	// what our dispatcher returns for the malformed payload.
	var aerr aerospike.Error
	if errors.As(opErr, &aerr) {
		switch aerr.Matches(types.PARAMETER_ERROR) {
		case true:
			return true
		}
		s.logger.Warnf("[teranode-native-op] probe got unexpected error code (%v); "+
			"falling back to UDF path", aerr)
		return false
	}

	s.logger.Warnf("[teranode-native-op] probe failed with non-aerospike error (%v); "+
		"falling back to UDF path", opErr)
	return false
}

// initNativeTeranodeOps caches the native-op decision on the store.
// Idempotent; safe to call from store construction.
func (s *Store) initNativeTeranodeOps(ctx context.Context) {
	supported := s.detectNativeTeranodeOpSupport(ctx)
	s.useNativeTeranodeOps = supported

	if s.settings.Aerospike.UseNativeTeranodeOps && !supported {
		s.logger.Infof("[teranode-native-op] setting requested native ops but server " +
			"capability probe rejected; using UDF path")
	} else if supported {
		s.logger.Infof("[teranode-native-op] enabled (op type 200, sub_op_id wire format)")
	}

	// Sanity: if a future caller introduces a sub_op id higher than
	// the table can hold, panic at startup rather than at first use.
	if subOpAddDeletedChildren > 13 {
		panic(fmt.Sprintf("sub_op_id %d exceeds 13; SUBOP_TABLE on the server "+
			"only has 14 entries (0..13)", subOpAddDeletedChildren))
	}
}

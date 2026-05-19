// Package aerospike — native operate-path support for mod-teranode.
//
// Each mod-teranode UDF function (spend, setMined, freeze, ...) can be
// invoked through one of two paths:
//
//  1. UDF path (legacy):  aerospike.NewBatchUDF(..., "<fn>", args...)
//  2. Native path:        aerospike.NewBatchWrite(..., TeranodeModifyOp("SUCCESS", payload))
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
	"time"

	aerospike "github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/aerospike-client-go/v8/types"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
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

// nativeOpResultBin is the bin name used by the native-op result message.
// Keep this aligned with LuaSuccess so existing batch response parsing works
// for both UDF and native paths.
const nativeOpResultBin = string(LuaSuccess)

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
		// Reuse the Store's shared BatchWritePolicy (allocated once in
		// initNativeTeranodeOps). The Aerospike client only reads from the
		// policy during BatchOperate, and no caller mutates it after init.
		return aerospike.NewBatchWrite(
			s.nativeOpBatchWritePolicy,
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
		// Reuse the caller's WritePolicy so timeouts/retries/commit-level/TTL stay
		// consistent with the UDF path. Only synthesise a default when the caller
		// passed nil (the UDF path's client.Execute tolerates a nil policy, but
		// client.Operate does not).
		writePolicy := udfPolicy
		if writePolicy == nil {
			writePolicy = aerospike.NewWritePolicy(0, 0)
		}
		rec, opErr := s.client.Operate(writePolicy, key, aerospike.TeranodeModifyOp(nativeOpResultBin, payload))
		if opErr != nil {
			return nil, opErr
		}
		if rec == nil || rec.Bins == nil {
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
// only when the server accepts a valid native sub-op, returns a parseable
// response, and applies the expected record mutation. Anything else biases
// toward false-negative so the fallback never runs against a server that
// doesn't support the op.
//
// Called once during store construction; the result is cached in
// s.useNativeTeranodeOps.
func (s *Store) detectNativeTeranodeOpSupport(ctx context.Context) bool {
	if !s.settings.Aerospike.UseNativeTeranodeOps {
		return false
	}

	// Probe key — chosen to never collide with real txid keys (always
	// 32 bytes / chainhash). Create a short-lived record and run a valid
	// setLocked sub-op via TeranodeModifyOp: a patched server recognises wire
	// op 200, executes the sub-op, and returns a structured response; an
	// unpatched server rejects the unknown opcode with PARAMETER_ERROR.
	probeKey, err := aerospike.NewKey(s.namespace, s.setName, "_teranode-native-op-probe")
	if err != nil {
		s.logger.Warnf("[teranode-native-op] probe key creation failed: %v; falling back to UDF path", err)
		return false
	}

	payload, encErr := encodeNativeOpPayload(subOpSetLocked, []any{true})
	if encErr != nil {
		s.logger.Warnf("[teranode-native-op] probe payload encode failed: %v; falling back to UDF path", encErr)
		return false
	}

	probeOp := aerospike.TeranodeModifyOp(nativeOpResultBin, payload)

	policy := aerospike.NewWritePolicy(0, 0)
	policy.TotalTimeout = 2 * time.Second
	policy.Expiration = 60

	if putErr := s.client.PutBins(policy, probeKey, aerospike.NewBin("_nativeProbe", true)); putErr != nil {
		s.logger.Warnf("[teranode-native-op] probe record setup failed: %v; falling back to UDF path", putErr)
		return false
	}
	defer func() {
		_, _ = s.client.Delete(policy, probeKey)
	}()

	rec, opErr := s.client.Operate(policy, probeKey, probeOp)
	if opErr == nil {
		if rec == nil || rec.Bins == nil || rec.Bins[nativeOpResultBin] == nil {
			s.logger.Warnf("[teranode-native-op] probe returned no %q bin; %s; falling back to UDF path", nativeOpResultBin, describeAerospikeRecord(rec))
			return false
		}

		rawResponse := rec.Bins[nativeOpResultBin]
		res, parseErr := s.ParseLuaMapResponse(rawResponse)
		if parseErr != nil {
			s.logger.Warnf("[teranode-native-op] probe returned unparsable response bin %q (value %s); %s: %v; falling back to UDF path", nativeOpResultBin, describeAerospikeValue(rawResponse), describeAerospikeRecord(rec), parseErr)
			return false
		}

		if res.Status != LuaStatusOK {
			s.logger.Warnf("[teranode-native-op] probe returned non-OK response (%+v); falling back to UDF path", res)
			return false
		}

		readPolicy := aerospike.NewPolicy()
		readPolicy.TotalTimeout = 2 * time.Second

		probeRecord, readErr := s.client.Get(readPolicy, probeKey, fields.Locked.String())
		if readErr != nil {
			s.logger.Warnf("[teranode-native-op] probe verification failed: %v; falling back to UDF path", readErr)
			return false
		}
		if probeRecord == nil || probeRecord.Bins == nil {
			s.logger.Warnf("[teranode-native-op] probe verification returned no record; falling back to UDF path")
			return false
		}
		if locked, ok := probeRecord.Bins[fields.Locked.String()].(bool); !ok || !locked {
			s.logger.Warnf("[teranode-native-op] probe did not set %q=true; falling back to UDF path", fields.Locked.String())
			return false
		}

		return true
	}

	var aerr aerospike.Error
	if errors.As(opErr, &aerr) {
		if aerr.Matches(types.PARAMETER_ERROR) {
			s.logger.Infof("[teranode-native-op] server rejected native-op probe; falling back to UDF path")
			return false
		}
		s.logger.Warnf("[teranode-native-op] probe got unexpected error code (%v); "+
			"falling back to UDF path", aerr)
		return false
	}

	s.logger.Warnf("[teranode-native-op] probe failed with non-aerospike error (%v); "+
		"falling back to UDF path", opErr)
	return false
}

// initNativeTeranodeOps caches the native-op decision on the store and
// allocates the shared BatchWritePolicy used by every record the native-op
// path emits. Idempotent; safe to call from store construction.
//
// nativeOpBatchWritePolicy is shared across every NewBatchWrite the native
// path constructs (one allocation per Store, not per record). The Aerospike
// client only reads from this policy during BatchOperate, so concurrent reads
// from many goroutines are safe. Do NOT mutate the policy after this call.
func (s *Store) initNativeTeranodeOps(ctx context.Context) {
	supported := s.detectNativeTeranodeOpSupport(ctx)
	s.useNativeTeranodeOps = supported
	s.nativeOpBatchWritePolicy = aerospike.NewBatchWritePolicy()

	if s.settings.Aerospike.UseNativeTeranodeOps && !supported {
		s.logger.Infof("[teranode-native-op] setting requested native ops but server " +
			"capability probe rejected; using UDF path")
	} else if supported {
		s.logger.Infof("[teranode-native-op] enabled (op type 200, sub_op_id wire format)")
	}
}

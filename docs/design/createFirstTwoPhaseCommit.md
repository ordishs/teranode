# Create-first two-phase commit: protocol design

Status: draft for design review. It documents PR #1355 (`feat/create-first-utxo-2pc`) as it stands at head `5403765a4`.

## Read this first

- **Citations.** Every `path:line` in this document points into the reference checkout of PR #1355 at `5403765a4`. `origin/main` does not contain this code. Line numbers do not resolve on `main`.
- **Short names.** A bare file name means this file: `create.go`, `spend.go`, `get.go`, `delete.go`, `set_mined.go`, `finalize.go`, `create_first_rollback.go`, `aerospike.go`, `teranode.go`, `teranode.lua`, `spend_expressions.go`, `set_mined_expressions.go`, `unmined_iterator.go`, `un_spend.go`, `native_op.go`, `creating_query.go`, `README.md` are in `stores/utxo/aerospike/`. `spend_and_create.go` is the generic driver `stores/utxo/spend_and_create.go`. `creating_sweep.go` and `worker.go` are in `services/pruner/`. `Validator.go` is in `services/validator/`. `data.go` is `stores/utxo/meta/data.go`. A path with a directory is exact. A line that starts with `:` continues the file cited before it in the same cell.
- **Scope.** The Aerospike store is the only store that implements the protocol. The generic driver is `stores/utxo/spend_and_create.go`.
- **Method.** Each statement comes from the code. A statement that comes only from a comment says so. Where the code cannot answer, the text says `UNVERIFIED: <what to check>`.
- **Flag.** `utxostore_useCreateFirstOrder` (default `false`). The store reads it on every `SpendAndCreate` call (`stores/utxo/aerospike/spend_and_create.go:15`).
- **Terms.** `H` means `utxostore_spendRollbackTimeout` (default 120 s, `settings/utxostore_settings.go:47`). The Aerospike store converts it to whole seconds in `holdSeconds` (`stores/utxo/aerospike/create_first_rollback.go:24`).

## 0. What the protocol does

The old order was spend the inputs, then create the record. A crash between the two left a parent output spent by a record that never landed (issue 1214).

The new order is:

1. Create the record in the tentative `creating` state. Its outputs are unspendable.
2. Spend the inputs.
3. Finalize: clear `creating`. Its outputs become spendable.

A crash after step 1 leaves a record that a healer can finish (roll forward) or undo (roll back). The cost is a set of identity and ownership rules between actors that may run on different hosts. Those rules are the subject of this document.

Actors that touch a record:

| Actor | Entry point | Role |
|---|---|---|
| Creator | `sequentialCreateFirst` (`stores/utxo/spend_and_create.go:1163`) | Creates, spends, finalizes, or rolls back its own attempt |
| Retry | same function, `ErrTxExists` branch (`:1236`) | Rolls an abandoned `creating` record forward |
| setMined pre-flight | `rollForwardCreatingBeforeSetMined` (`stores/utxo/aerospike/set_mined.go:802`) | Rolls forward before a block marks the record mined |
| Pruner sweep | `sweepCreatingTxs` (`services/pruner/creating_sweep.go:43`) | Rolls forward, rolls back, finishes rollbacks, deletes expired holds |
| Spend-first caller | `SequentialSpendAndCreate` loop (`stores/utxo/spend_and_create.go:408`) | Flag-off path. Meets holds and rollbacks left by create-first |
| Validator | `Validator.go:1230-1255` | Consumes `ErrTxExists`, `IsRolledForward`, `IsAlreadyFinal` |

## 1. The state machine

### 1.1 States

A state is a combination of bins on the master record (key = txid).

| State | Bins on the master | Meaning |
|---|---|---|
| `absent` | no record | Nothing stored for the txid |
| `creating` | `creating=true`, `attempt=<token>`, `spenders={lease:until}` | Tentative. Outputs gated |
| `finalized` | no `creating` bin | Ordinary record. `attempt` now holds the finalizer's lease (`finalize.go:355-370`) |
| `rolling-back` (open) | `creating`, `rollingBack=true`, `busy=true`, `holdUntil` | One caller owns the reversal of spends |
| `rolling-back` (reversed) | open bins plus `reversed=true` | Spends are undone. Cleanup runs |
| `hold` | `creating`, `rollingBack`, `hold=true`, `holdUntil`, `attempt`, `unminedSince`. No `busy`, no `reversed`. Aerospike TTL = `H` | Placeholder that keeps the txid taken. No outputs, no tx data |

A record may also carry `creating` with no `attempt` bin. That is a legacy record: a spend-first multi-record create whose clear failed. It is not part of this state machine. Section 1.4 covers it.

### 1.2 Diagram

```
                      Create(WithCreating)                  FinalizeTransaction
              ┌───────────────────────────────┐        ┌────────────────────────────┐
              │ T1: CREATE_ONLY write         ▼        │ T2: filter creating,       │
         ┌────┴───┐                      ┌──────────┐  │     !rollingBack, attempt  │
         │ absent │                      │ creating │──┘                            │
         └────────┘◄──────┐              └────┬─────┘                               ▼
              ▲           │                   │ T3: BeginCreateFirstRollback  ┌───────────┐
              │           │                   │  filter: T2 filter AND        │ finalized │
              │           │                   │  no live spend lease          └─────┬─────┘
              │           │                   ▼                                     │
              │           │       ┌──────────────────────┐                          │ T8: DeleteComplete
              │           │       │ rolling-back (open)  │                          │  (filter: no live lease)
              │           │       └──────────┬───────────┘                          │ or DAH pruning
              │           │                  │ T4: markReversed                     ▼
              │           │                  ▼                                  absent
              │           │       ┌──────────────────────┐
              │           │       │ rolling-back         │
              │           │       │ (reversed)           │
              │           │       └──────────┬───────────┘
              │           │                  │ T5: convertToHold (REPLACE_ONLY)
              │           │                  ▼
              │           │            ┌──────────┐
              │           └────────────│   hold   │
              │   T6: TTL expiry or    └──────────┘
              │   ClearCreateFirstHold
              └──────────────────────────────┘
```

### 1.3 Legal transitions

| # | From → To | Function that performs it | Store operation that enforces it |
|---|---|---|---|
| T1 | `absent` → `creating` | `Store.Create` (`create.go:205`) with `WithCreating(true)`. Caller: `sequentialCreateFirst` (`spend_and_create.go:1181`) or `claimAndRollBack` (`:691`) | Server: `RecordExistsAction = CREATE_ONLY` (`create.go:507`, `:1532`). Any existing record makes the write fail with `KEY_EXISTS`, so a hold, a rollback and a final record all refuse it |
| T2 | `creating` → `finalized` | `Store.FinalizeTransaction` (`finalize.go:75`), batched in `sendFinalizeBatch` (`:179`). Multi-record: `clearCreatingFlag` (`create.go:1910`) | Server: `UPDATE_ONLY` write with `finalizeFilter` (`finalize.go:329`): `creating` present, `rollingBack` absent, `attempt` equals token. Multi-record: children first, master last |
| T3 | `creating` → `rolling-back` (open) | `Store.BeginCreateFirstRollback` (`create_first_rollback.go:217`). Caller: `RollBackCreating` (`spend_and_create.go:900`) through `beginAfterSpendLeases` (`:1058`) | Server: `UPDATE_ONLY` write filtered by `finalizeFilter` AND `noLiveSpendLease(now)` (`create_first_rollback.go:225`, `:261`). Sets `rollingBack`, `busy`, `holdUntil` |
| T4 | `rolling-back` (open) → `rolling-back` (reversed) | `markReversed` (`create_first_rollback.go:362`), first write of `EndCreateFirstRollback` (`:297`) | Server: filter `rollingBack` present, `hold` absent, `notBusyOrStale(now)`, attempt equals token |
| T5 | `rolling-back` (reversed) → `hold` | `convertToHold` (`create_first_rollback.go:410`), after `removeRolledBackData` (`:340`) | Server: `REPLACE_ONLY` write filtered by `holdWritePolicy` (`:463`): `rollingBack`, `reversed`, no `hold`, attempt. TTL = `H`. A namespace that forbids TTL gets the hold with no TTL (`:428-432`) |
| T6 | `hold` → `absent` | Server TTL, or `Store.ClearCreateFirstHold` (`create_first_rollback.go:627`). Callers: `settle`, `sequentialCreateFirst` (`:1223`), `Validator.go:1421`, `creating_sweep.go:123` | Server: delete filtered by `hold` plus `holdUntil` equality (without `adopt`) or `notBusyOrStale(now)` (with `adopt`). The expiry decision is made in Go from the caller's clock (`:661-666`) |
| T7 | `hold` → `creating` (new attempt) | Not direct. T6, then T1 | T1's `CREATE_ONLY` refuses while the hold exists. `resolve` (`spend_and_create.go:566`) and `sequentialCreateFirst` clear the hold first and loop |
| T8 | `finalized` → `absent` | `DeleteComplete` (`delete.go:208`), `deleteMasterWithNoSpender` (`:265`) | Server: delete filtered by `noLiveSpendLease(now)`. A refusal is an error, record kept |
| T9 | `creating` → `creating` (same attempt, more spenders) | `TakeSpendLease` (`create_first_rollback.go:77`), `ReleaseSpendLease` (`:117`) | Server: `UPDATE_ONLY`; take is filtered by `finalizeFilter` |
| T10 | `rolling-back` / `hold` → same state, `busy` set or cleared | `TakeRollbackHold` (`:509`), `ReleaseRollbackHold` (`:584`) | Server: filter `rollingBack`, `notBusyOrStale(now)`, token, plus `!reversed && !hold` (open rollback) or `hold` (hold) |

### 1.4 Legacy variants outside the machine

| Variant | Function | Enforcement |
|---|---|---|
| `creating` with no `attempt`, cleared by a duplicate create | `clearCreatingFlag(..., nil, nil)` at `create.go:1644` | `attemptFilter(nil)` matches a record with no `attempt` bin (`finalize.go:339-342`) |
| Same record, cleared by setMined pre-flight | `clearCreatingWithoutAttempt` (`set_mined.go:886`) | Server filter `finalizeFilter(nil)` (`:895`) |
| Same record, cleared by the sweep | `FinalizeTransaction(ctx, tx, nil, nil)` (`creating_sweep.go:199`) | Same filter |
| Extra (pagination) records of a mined tx | `clearLockedOnRecordsMulti` (`set_mined.go:713`) | **No filter.** The write deletes `creating` on every extra record unconditionally (`:729`, `:741`) |

### 1.5 Illegal transitions

Each row says which mechanism refuses it. "No write exists" means the code has no write that removes the bin.

| Illegal transition | Why it cannot happen | Mechanism |
|---|---|---|
| `finalized` → `creating` | T1 is `CREATE_ONLY`. T2 and T3 need the `creating` bin | Server |
| `finalized` → `rolling-back` | T3 filter needs `creating` | Server |
| `rolling-back` → `finalized` | T2 filter needs no `rollingBack` bin | Server |
| `rolling-back` → `creating` (clear the marker) | No write removes `rollingBack`, `reversed` or `hold`. `grep` over `stores/utxo/aerospike/*.go` finds none outside tests | No write exists. An absence of code, not a filter |
| `hold` → `finalized` | T2 filter needs no `rollingBack` bin. A hold has it | Server |
| `hold` → `rolling-back` (open) again | T3 filter needs no `rollingBack` bin | Server |
| `creating` (attempt A) → `finalized` by attempt B | T2 filter compares the token | Server |
| `absent` → `finalized` | T2 is `UPDATE_ONLY`. The result maps to `ErrTxNotFound` (`finalize.go:317-320`) | Server |
| `creating` → `rolling-back` while a spend lease is live | T3 filter `noLiveSpendLease` | Server, but the `now` operand comes from the caller's clock. See section 3, row L5 |
| Two callers both own the rollback | T3 write sets the marker. T3's filter needs it absent | Server |
| Reversal of spends by a caller that has not taken the hold | `TakeRollbackHold` is a Go call convention, not a store gate on `Unspend` | **Go convention only.** The Lua `unspend` checks spend ownership by txid and vin, not by attempt (`un_spend.go:221-237`). See residual window R1 |

## 2. Identity and ownership primitives

| Primitive | Problem it solves | Who writes it | Who reads it | Expiry | If it goes wrong |
|---|---|---|---|---|---|
| `attempt` token (16 random bytes, `create.go:1234`) | Two attempts at one txid look alike. A spend record holds only txid and vin, so a reversal cannot tell them apart | `Create` (master and every pagination child, `create.go:690`, `:1550`). Duplicate create aligns child tokens to the master (`:1265`, `:1333`). A finalizer overwrites the master's `attempt` with its lease (`finalize.go:363`) | Every filter via `attemptFilter` (`finalize.go:339`). Actors read it from the record (`Get`, `GetMeta`) | Lives as long as the record | A wrong token on a child makes the finalize refuse the child (`childFilteredOutResult`, `create.go:2118`). The master stays `creating`. After finalize, the field no longer names the attempt |
| spend `lease` in `spenders` (map lease-id → `until`) | A rollback must not reverse spends while a spender may still write one | Creator: its own write puts `attempt` as its lease (`create_first_rollback.go:40`). Roll-forward: `TakeSpendLease` adds a random id. `until = writer_now + H` (`:34`) | `noLiveSpendLease` (server filter, `:261`), `SpendLeasesLive` (Go, `:149`) | Live until `until + H`: the filter counts leases with `until >= reader_now - H` (`:265`). `TakeSpendLease` drops leases with `until < now - H` (`:96`) | Stale too early (reader clock ahead): rollback starts under a running spender. Stale too late: rollback waits, liveness only. An unknown spend outcome keeps the lease (`spendOutcomeUnknown`, `spend_and_create.go:1314`) |
| `rollingBack` | Stops finalize and new spenders. Marks one rollback owner | T3 only | `finalizeFilter` (server), readers via `GetMeta` | Never cleared. Leaves with the record (T6) | None found. No path unsets it |
| `busy` + `holdUntil` | One actor at a time reverses spends or cleans up. Called the "hold" in code comments | T3, `markReversed`, `takeHold` (`:544`) set both. `holdUntil = writer_now + H` | `notBusyOrStale(now)` (`:485`): free if `busy` absent, or `holdUntil <= reader_now - H` | Stale after `H` past `holdUntil`. `ReleaseRollbackHold` clears `busy` (`:584`) | Stale too early: two actors reverse together. Stale too late: sweep waits |
| `hold` + `holdUntil` + TTL | Keeps the txid taken while children and blobs are removed. Refuses a new create | `convertToHold` (`:410`). `TakeRollbackHold` renews TTL to `2H` (`:527`) | `ClearCreateFirstHold`, readers (`get.go:883`, `:1884`) | Two clocks. Aerospike TTL uses the **server** clock. `holdUntil` uses **host** clocks | Hold gone early: a new attempt may start while a straggler still reverses without proof (R1). Hold kept late: the txid is unavailable. Namespace with no TTL: hold stays until the sweep deletes it |
| `reversed` | Lets a recovery finish a rollback after the tx blob is gone | `markReversed` (`:362`) | `TakeRollbackHold` refuses a reversed record (`:517`). Sweep calls `EndCreateFirstRollback` (`creating_sweep.go:145`) | With the record | If missing after a reversal, a recovery needs tx data that may be gone. The sweep then cannot finish (`creating_sweep.go:166-173`) |
| `spenders` bin | Storage for leases | `spendLeaseOp`, `TakeSpendLease`, `finalizeMasterOps` (removes caller's lease) | `noLiveSpendLease`, `SpendLeasesLive` | Bin removed with the last lease (`removeSpendLeaseOps`, `:48`) | A bin of unexpected type counts as a live lease (`:177-181`). Leases of other spenders stay after finalize. `DeleteComplete` refuses (T8) |

### Who may write what

- A writer must be the attempt that owns the token, or hold a lease on it. The store checks the token (server filter). The store does **not** check that the caller really holds a lease when it spends. A spender that ignores the protocol is not stopped by the store. See ledger row L6.
- The reader of a time value is not the writer. Every time comparison in the protocol crosses hosts. Section 5 gives the consequence.

## 3. The invariant ledger

Enforcement classes:

- **SF**: server-side Aerospike filter expression. The server evaluates it inside the write. Atomic with the write.
- **SF\***: SF whose time operand is a literal that the *calling host* computed from its own clock. The filter is atomic. The time value in it is not server-owned.
- **LUA**: Lua body (UDF), runs on the server.
- **GO**: client-side Go check. Not atomic with any write.
- **DOC**: doc comment only. Nothing enforces it.

| ID | Invariant | Enforced by | Where | What breaks if enforcement is removed |
|---|---|---|---|---|
| L1 | Outputs of a record that has `creating` cannot be spent. A rolling-back record and a hold carry `creating`, so the same gate covers them | LUA + SF | Lua `teranode.lua:377`. Expression `stores/utxo/aerospike/spend_expressions.go:140-145` | Outputs spendable before inputs are spent. The bug the whole protocol fixes |
| L1n | The third spend path, native sub-op `spendMulti`, also refuses `creating` | **UNVERIFIED** | `useNativeForSubOp` (`native_op.go:154`) sends sub-ops to an out-of-tree server module. This repo has no source for it. Check the module | Same as L1, on nodes that run native ops |
| L2 | Only a write that carries the right token clears `creating` | SF | `finalizeFilter` / `attemptFilter` (`finalize.go:329`, `:339`) | A stale finalize of attempt A clears attempt B. B's outputs spendable before B's inputs are spent |
| L3 | Exactly one finalize returns nil and owns the hand-off to block assembly | SF write + GO read-back | Write stamps the caller's lease into `attempt` (`finalize.go:355-370`). Read-back in `finalizeFilteredOutResult` (`:378`) | Two instances both announce, or both unwind. A creator deletes a record another instance delivered |
| L4 | A record being rolled back cannot be finalized | SF | `finalizeFilter` needs `rollingBack` absent. Multi-record: Go pre-read `refuseForeignMaster` (`create.go:2178`) plus the same filter per write | A roll-forward completes a record whose spends the rollback reverses |
| L5 | A rollback does not begin while a spender of the attempt may still write | SF\* | `noLiveSpendLease(time.Now())` in `BeginCreateFirstRollback` (`create_first_rollback.go:225`, `:261`) | A spend lands after the reversal. The parent output is spent by a record that no longer exists. Issue 1214 returns |
| L5b | A spender cannot start once a rollback has begun | SF | `TakeSpendLease` write uses `finalizeFilter` (`:90`), which needs `rollingBack` absent | A new spend starts under a rollback |
| L6 | A spender writes no spend after the time its lease names | GO | Spend context deadline (`spend_and_create.go:1254`), `splitSpendsByDeadline` (`spend.go:1233`), `BatchOperateBefore` (`util/uaerospike/client.go:600`). The README adds that the server does not start a command whose timeout has passed (`README.md:522-523`). That is a server behaviour. **UNVERIFIED:** check Aerospike batch `total_timeout` semantics | L5 loses its premise. A lease expires while its write is still queued |
| L7 | A lease is kept when a spend outcome is unknown | GO | `spendOutcomeUnknown` (`spend_and_create.go:1314`), used at `:1260` and `:1514` | A late write lands after a rollback began |
| L8 | Only the holder of the hold reverses spends | SF + GO | Take: `takeHold` filter with `notBusyOrStale(now)` (SF\*, `:544-551`). The reversal call itself is not gated by the store: the Go callers follow the convention (`settle`, `FinishRollBackCreating`, `RollBackCreating`, `partialSpendRollbackDecision`) | Two actors reverse together. One clears the spends of a fresh attempt |
| L9 | The cleanup (children, blobs, hold conversion) has one owner | SF\* | `markReversed` filter (`:362-370`) | Two cleanups. One deletes the blobs of a replacement |
| L10 | A new attempt cannot create while a rollback or hold stands | SF + GO | `CREATE_ONLY` (`create.go:507`, `:1532`). Multi-record pre-check `masterRollingBack` (`create.go:2147`) | A new attempt writes children that the rollback then deletes |
| L11 | A hold is deleted only if it expired and no live holder is busy | GO decision + SF guard | Decision `ClearCreateFirstHold` (`:661-666`) uses the caller's clock. Server filter checks `holdUntil` equality (`:670`) or `notBusyOrStale` | Hold removed under a live holder. A new attempt starts early |
| L12 | A hold's TTL ends it | Server TTL | `holdWritePolicy` (`:463-470`), server clock | Namespace with no TTL: hold stays. The sweep deletes it (`creating_sweep.go:122-138`) |
| L13 | `DeleteComplete` does not delete a record that a lease holder may still spend for | SF\* | `deleteMasterWithNoSpender` (`delete.go:265`) | Shed unwind deletes a record, unspends inputs, then a late roll-forward spend names a txid with no record |
| L14 | A reversal clears only spends that this txid owns | LUA | `unspend` Lua (`teranode.lua:603`), `unspendLua` (`un_spend.go:204-237`). The key is txid plus vin. It carries **no attempt** | A reversal clears a rival's spend |
| L15 | A reversal never clears the spends of a **later attempt of the same txid** | **DOC** | `RollBackCreating` comment (`spend_and_create.go:894-899`). Only ordering conventions (hold, token proof, sweep age) help | Window R1 (issue 1291) |
| L16 | A roll-forward finalizes only if every input is spent | GO | `AllInputsSpent` (`spend_and_create.go:1408`), used at `:1521` | A record is finalized with unspent inputs. Issue 1214 returns |
| L17 | The validator does not treat a `creating` record as validated | GO | `Validator.go:1255` | A block accepts a tx whose parent outputs are unspent |
| L18 | The first spend of a fresh attempt does not accept "parent missing, spender record exists" as proof | GO | `RequireParentOutputs` (`spend_and_create.go:1251-1252`), read at `spend.go:1072` | The tentative record proves a missing parent. A bad input passes |
| L19 | setMined does not make a `creating` record's outputs spendable | LUA + SF + GO | Lua `teranode.lua:791` (clear removed). Expression write `set_mined_expressions.go:754-758` (clear removed). Pre-flight `set_mined.go:226`, `:802` | Mining clears the gate while inputs are unspent |
| L19n | The native setMined path keeps the `creating` bin | **UNVERIFIED** | `set_mined.go:217-220`: the comment states the behaviour is "unknown from this repo". The code relies on L19's pre-flight instead | The pre-flight is the only guard on this path |
| L20 | The pre-flight and the mined write cannot be separated by a new `creating` record | **GO, not atomic** | Read at `set_mined.go:817`, write later. Extra-record clear is unfiltered (`:729-741`). **UNVERIFIED:** whether a create can land between them for the same txid | A `creating` master is marked mined. Its extras lose `creating` and `locked` |
| L21 | Block assembly does not restore a `creating` record | GO | `unmined_iterator.go` skip, `unmined_iterator_raw.go` skip, `BlockAssembler.go` parent check | A child of an unconfirmed parent enters a template |
| L22 | The sweep sees only `creating` records | SF | `QueryStaleCreatingTxs` (`creating_query.go:24`): index range on `unminedSince`, filter `creating` present | Sweep wastes work or touches final records |
| L23 | Every actor that reverses finishes within `H` | GO | `rollbackContext` (`spend_and_create.go:787`) | A reversal outlives its hold and its lease margin |
| L24 | **Hosts that share a store agree on wall-clock time to within `H` minus the in-flight time of a write** | **DOC / assumed** | `create_first_rollback.go:71-73`, `README.md:541-545`. No check exists. Section 5 | L5, L8, L9, L11 and L13 weaken. See section 5 |
| L25 | The sweep age (blocks) outlasts `H` (seconds) | **DOC** | `settings/pruner_settings.go:28` ("The age must outlast ..."), `spend_and_create.go:896-898`. No code compares blocks to seconds | The sweep takes over a rollback owner that is still running. Mitigated by L8's `busy` and `holdUntil` |
| L26 | The flag takes effect only on a store that implements the protocol | GO | `stores/utxo/aerospike/spend_and_create.go:15`, startup warning `stores/utxo/factory/utxo.go:143` | A store that ignores `Creating` runs create-first with no gate (L1) |
| L27 | Old binaries do not misread the new flag bits | GO (format) | `stores/utxo/meta/data.go:150`, `:189`: bit 5 `Creating`, bit 6 `RollingBack` | Old node reads a tentative record as final. **UNVERIFIED** on a real old binary. The diff shows `main` documents bits 0 to 4 only |
| L28 | A duplicate **create** does not clear a tentative record | GO, **flag-dependent** | `create.go:1644`: the clear is skipped only when a token exists **and** the flag is on. With the flag off, the duplicate create clears a tentative record. It does not check that inputs are spent | See window R4 |

### Reading the ledger

- Server-atomic ordering holds for L2, L4, L5b, L10 and the T1 to T6 writes. Those rest on single-record filter expressions and need no clock.
- Every safety argument that depends on **liveness of a lease or a busy marker** (L5, L8, L9, L11, L13) rests on SF\*. The filter is atomic. Its time operand is a client clock. No filter uses a server clock. `grep` for `ExpVoidTime`, `ExpTTL`, `ExpLastUpdate` and `ExpSinceUpdate` in `stores/utxo/aerospike/` finds none.
- The claim "ordering is decided server-side" is true for the *order of writes*. It is not true for *whether a lease counts as live*. The code and README say the order "does not depend on clocks" (`README.md:544-545`). That holds for T3 against `TakeSpendLease` once the marker exists. It does not hold for the check "no live lease" at T3.
- L15, L24 and L25 are the three rows that rest on convention or assumption. A reviewer who wants to find a design defect should start there.

## 4. Known residual windows

"Flag on?" says whether `utxostore_useCreateFirstOrder` must be true on the node that causes the window. Metric names are copied from `promauto` definitions in the code. "None in code" means the grep found no metric. Prometheus names use the form `<namespace>_<subsystem>_<name>`.

| ID | Window | Interleaving that reaches it | Flag on? | Detectable? | Metric |
|---|---|---|---|---|---|
| R1 | **Attempt-identity gap** (issue 1291). A spend record is txid plus vin. A reversal cannot tell attempt A from attempt B of the same txid (`spend_and_create.go:894-899`, `README.md:692-694`) | Owner A begins a rollback, then stalls past `H` and past the sweep age. The hold expires. Attempt B creates and spends the same inputs. A resumes and unspends. B's spends are cleared. B is then finalized with unspent inputs, or B's roll-forward re-spends | Yes (a rollback owner exists only in create-first). A flag-off spend-first caller has a related reversal path (`reverseCarried`, `:634`) | Not directly. A parent output shows spent-by absent for a finalized record | None in code |
| R2 | **Clock skew** beyond the bound in section 5 | Spender host S writes a lease. Rollback host R has a clock ahead of S by more than about `H` minus the in-flight time. R sees the lease as stale and begins. S's write lands after R reverses | Yes | No. Looks like R1 | None in code |
| R3 | **Finalize unreachable**. Three tries fail and the record may already be final (`README.md:565-567`) | Store times out on the reply to a finalize write that did commit | Yes | Yes, partly | `teranode_utxo_create_first_finalize_failed_total` (`spend_and_create.go:204`). The counter increments on any failure that is not already-final, so it also counts failures where the write did not commit |
| R4 | **Duplicate create clears a tentative record with the flag off** (`create.go:1644`, `README.md:709-712`) | A flag-on node leaves a tentative multi-record master. A flag-off caller of `Create` with `CreateOnly` (no spend phase in the same call) finds all records present. The create clears `creating` with no check of inputs | **Flag off on the clearing node.** A tentative record from earlier flag-on use or another node must exist | Partly: the warn log "creating flag cleanup failed" only on failure | None in code. **UNVERIFIED:** which `CreateOnly` callers can meet a multi-record tentative master |
| R5 | **Native setMined path** handles `creating` in unknown ways (`set_mined.go:217-220`) | Native ops on, mined write via the sub-op | No | No | None in code |
| R6 | **Pre-flight is read then write** (`set_mined.go:817` then the write). Extras' `creating` cleared unfiltered (`:741`) | A `creating` master appears between the read and the write | No | No | None in code. **UNVERIFIED** reachability |
| R7 | **Multi-record clear races a rollback marker**: the marker set between the child pass and the master write leaves children cleared while the master is `creating` and `rolling-back` (`create.go:1901` comment: "a window this function does not close") | A finalizer that holds no lease clears children. A rollback begins before its master write | Possible with the flag off (R4 path: lease is nil) | No | None in code |
| R8 | **Spend-first beside a rollback gives up** after `2 * rollbackBound`: "they are left in place" (`spend_and_create.go:594-596`, `:610`) | A spend-first call waits at a record whose hold stays busy past twice the bound | No (this is the flag-off path) | Log line only | None in code. `teranode_aerospike_utxo_spend_rollback_failed` counts the store's own partial-spend rollback, not this path |
| R9 | **Earlier-round spends not reversed** after a later spend fails (`spend_and_create.go:666`, `:673`) | `reverseCarried` fails in `settle` or `claimAndRollBack` | No | Error log only | None in code |
| R10 | **Rollback cannot start while a lease is live**. An unknown-outcome spend keeps its lease until `until + H` | Spend times out. Rollback waits `spendLeaseWait` (30 s default, `:1028`). The record stays `creating` | Yes | Yes | `teranode_pruner_creating_sweep_failures_total` (increments in the "may still hold a lease" branch, `creating_sweep.go:232-237`). `teranode_pruner_creating_backlog` rises |
| R11 | **Sweep backlog or stuck page**: cap `pruner_creatingTxSweepMaxPerPass`, locked parent waits ten times the age (`creating_sweep.go:310`) | Burst of crashes. A parent that stays locked | Healers run either way | Yes | `teranode_pruner_creating_backlog`, `teranode_pruner_creating_sweep_failures_total` (`services/pruner/metrics.go:147`, `:156`) |
| R12 | **setMined fails closed** on a rolling-back or hold record. The whole `SetMinedMulti` call returns an error (`set_mined.go:859-865`) | A block's tx list contains a txid whose record is a hold | Healers run either way | Yes (block validation retries) | None specific. **UNVERIFIED** whether any existing validator metric covers it |
| R13 | **Shed of a rolled-forward record** is kept finalized and locked for the unmined reload (`Validator.go:1726`) | Block assembly queue full on a rolled-forward record | Yes | Yes | `teranode_validator_shed_rolled_forward_kept_total` (`services/validator/metrics.go:604`) |
| R14 | **Spend "already blessed" fallback** can accept a rival's take of a pruned parent output (`spend.go:1084-1097` comment) | A parent is pruned and a rival spent the same output | Spend-first has the same exposure | No | None in code |
| R15 | **Hold with no TTL** (namespace forbids TTL) relies on the sweep to delete it (`create_first_rollback.go:428-432`) | Namespace without record expiry, pruner sweep disabled (`pruner_creatingTxSweepMinAgeBlocks = 0`) | Yes | Yes (txid stays refused) | `teranode_pruner_creating_backlog` is **not** a reliable signal here: it counts records in the `unminedSince` query range only |

R4 does not touch the combined spend-first call. That call spends its own inputs before it creates, so a clear after a duplicate leaves the inputs spent. The gap is a `CreateOnly` caller that spends in a later call.

R1 is the only window the code concedes by issue number. R4 to R7 follow from the code but no comment ties them to an issue.

## 5. The clock requirement

### What the code depends on

Three kinds of time value cross hosts:

| Value | Written by | Read by | Compare |
|---|---|---|---|
| Lease `until` | Spender host, `writer_now + H` (`create_first_rollback.go:34`) | Rollback host, in the T3 filter (`:261-268`), in `SpendLeasesLive` (`:149`), in `deleteMasterWithNoSpender` | Live if `until >= reader_now - H` |
| `holdUntil` with `busy` | Holder host, `writer_now + H` (`:230`, `:375`, `:555`) | Any host, in `notBusyOrStale` (`:485`) and `ClearCreateFirstHold` (`:661`) | Free if `holdUntil <= reader_now - H` |
| Hold TTL | Server, from the TTL written by `convertToHold` | Server | Server clock |

### The bound

A lease is stamped `writer_now + H`. A reader treats it as live while `reader_now < writer_stamp + 2H`. The spender must finish by `writer_stamp` on its own clock (`spend_and_create.go:1254`). Its last write may still be in flight after that. Safety needs the reader to keep treating the lease as live until that write ends:

```
reader_clock - writer_clock  <  H - (longest in-flight time of a write that started before the lease time)
```

The code states a looser bound: "the hosts of one store must agree on the time to within that setting" (`create_first_rollback.go:71-73`). That is an upper limit, not the margin. The margin is smaller by the in-flight time. The code does not state or bound the in-flight time. **UNVERIFIED:** the batch policy timeouts in `util.GetAerospikeBatchPolicy` fix it.

A reader clock that is *behind* the writer's clock only delays rollback. It costs liveness, not safety.

### Quote

The setting is `utxostore_spendRollbackTimeout`, default `120s`. The code comment reads:

> Lease times are the unix clocks of the hosts that write and read them. A lease is treated as live for a further utxostore_spendRollbackTimeout past its time, so the hosts of one store must agree on the time to within that setting. The same holds for the busy marker.

(`stores/utxo/aerospike/create_first_rollback.go:71-73`.)

### Enforced or assumed?

**Assumed.** No code compares host clocks. No startup check, no runtime check, no metric. The only clock check in the package is the Aerospike server health probe for `clock_skew_stop_writes` (`aerospike.go:1073`). That probe covers skew between **Aerospike server nodes**. It does not cover skew between Teranode hosts.

### What an operator must do

These steps follow from the bound. They are not in the code.

1. Run time synchronisation (NTP or chrony) on **every host** that runs a service which uses the store: validator, block validation, subtree validation, block assembly, pruner, legacy sync.
2. Alert on clock offset. Keep the worst offset between any two such hosts well below `H` minus the in-flight time. With the default `H = 120 s`, an offset of a few seconds is safe. An offset near 100 s is not.
3. Lowering `H` tightens the clock bound. Raising `H` loosens it but lengthens holds, lease waits, and the time a rejected double-spend keeps its txid refused.
4. Treat a step in time (leap, VM resume) as a crash of every spender that holds a lease at that moment.

## 6. Rollout ordering

Derived from the code. Each step names its reason.

### Before the flag is set anywhere

1. **Upgrade every node past this change.** The tentative state rides in the on-disk flags byte, bit 5 `Creating` and bit 6 `RollingBack` (`stores/utxo/meta/data.go:150`). `main` documents bits 0 to 4 only. An old node decodes a tentative record as final. Source: `settings/utxostore_settings.go:76`. The old-binary behaviour is inferred from the diff. **UNVERIFIED** by running an old binary.
2. **Understand that the Lua change reaches the whole fleet at the first new start, whatever the flag says.** `teranode.lua` changes (the `setMined` function no longer clears `creating`, `teranode.lua:791`). `LuaPackage` is `"teranode_v66"` on both `origin/main` and the PR head (`stores/utxo/aerospike/teranode.go:82`). The PR does not change the constant. The new binary re-registers a UDF whose registered body differs from the embedded body (`registerLuaIfNecessary`, `teranode.go:193`; `registeredUDFContent`, `:149`). The calls cover `LuaPackage`, every `LuaPackage_<n>` spend module, and `LuaPackageMined` (`aerospike.go:567`, `:574`, `:582`). A UDF is server-side, so every node of the fleet, old binaries included, runs the new body once the first new node has started.
   - The old code only checks that the filename exists (the removed lines in the diff). An old node that restarts does not overwrite the new body. A downgrade keeps the new body.
   - Effect on old nodes: their `setMined` stops clearing `creating`. A legacy record with no `attempt` that an old node mines stays gated, because only the new pre-flight clears it (`clearCreatingWithoutAttempt`). **UNVERIFIED:** how often legacy `creating` records occur.
3. **Check the native op module.** If `useNativeTeranodeOps` is on, `setMined` and `spendMulti` may run in the out-of-tree module (`native_op.go:154`). Its handling of `creating` is unknown from this repo (rows L1n, L19n). Do not enable the flag on a fleet that uses native `setMined` or `spendMulti` until the module is checked.
4. **Run the pruner with the sweep on.** Set `pruner_creatingTxSweepMinAgeBlocks` above 0 (default 2). The sweep is the only healer for a hold in a namespace that forbids TTL (R15) and for a record no block has reached.
5. **Prove clock agreement** (section 5) across all hosts.
6. **Choose `H` and the sweep age together.** The age is in blocks and `H` is in seconds. The code does not relate them (L25). Block times are random.

### Setting the flag

- The flag is a per-node value. It is read on each call from the settings struct. The settings `longdesc` says "read once at startup" (`settings/utxostore_settings.go:76`). Nothing in this study shows a reload path. Restart after a change.
- A flag-off node copes with holds and rollbacks left by flag-on nodes. The spend-first loop has `rollbackInTheWay` for that (`spend_and_create.go:517-612`). A mixed-flag fleet is therefore supported by the code. R4 is the known gap in that mix.
- The flag is ignored with a startup warning on a store that does not support it (`stores/utxo/factory/utxo.go:143`).

### Turning the flag off

- The setMined pre-flight and the sweep run regardless of the flag (`set_mined.go:225-229`, `worker.go:327-337`). Records left `creating` are still healed, as long as the pruner runs and the record has `unminedSince` in range (R15) and the sweep is enabled.
- A duplicate multi-record create with the flag off clears a tentative record without spending (R4). Keep the flag on until no tentative multi-record records remain, or confirm that no `CreateOnly` caller can meet one.

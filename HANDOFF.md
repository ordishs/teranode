# HANDOFF: create-first design doc and doc-accuracy audit (Split brief C)

Branch `split/1355-design-doc`, based on `origin/main`. Nothing is pushed.

- Design doc: `docs/design/createFirstTwoPhaseCommit.md`. The repo has no `docs/design/` directory. The closest convention is `docs/topics/architecture/`. I followed the brief and created `docs/design/`. Move it if you prefer the other place.
- **Citations.** Every `file:line` below, and in the design doc, points into the reference checkout `/Users/ordishs/dev/teranode-pr1355` at `5403765a4`. None resolves on `origin/main`.
- Bare file names follow the short-name rule in the design doc. `README.md` means `stores/utxo/aerospike/README.md`.
- Verdicts: `MATCHES`, `MISLEADING`, `WRONG`. A claim that the code cannot settle is listed apart in "Claims I cannot verify".

## 1. The two checks the brief named

### 1a. "Lua package bump to v63"

**WRONG.** The code refutes it.

| Evidence | Result |
|---|---|
| `LuaPackage` on PR head, `stores/utxo/aerospike/teranode.go:82` | `"teranode_v66"` |
| `LuaPackage` on `origin/main` | `"teranode_v66"` (`git show origin/main:stores/utxo/aerospike/teranode.go`, line 77) |
| `git diff origin/main -- stores/utxo/aerospike/teranode.go` | Contains no change to the constant |
| `git grep v63` over `*.go`, `*.md`, `*.lua`, `*.conf`, `*.yaml`, `*.yml` | No match that is a package name |
| PR body, line "Rollout gates" | Says "bump to v63" |
| PR body, "What changed" | Says "Lua package `teranode_v66`" in the same text. The body contradicts itself |

The settings `longdesc` (`settings/utxostore_settings.go:76`) says instead that the store re-registers on a content mismatch. That is what the code does (`registerLuaIfNecessary`, `teranode.go:193`). The two accounts differ. The code supports the `longdesc`.

The real rollout fact is in design doc section 6. The Lua body changes (`teranode.lua:791` no longer clears `creating` in `setMined`) with no version bump. The first new node that starts re-registers it for the whole fleet, flag on or off. Old nodes then run the new body. The PR description is outside my scope. I did not touch it.

### 1b. "Healers run regardless of this flag"

**MISLEADING.** The claim holds in a narrow sense and fails in three ways.

| Part | Verdict | Evidence |
|---|---|---|
| The setMined pre-flight has no flag check | Holds | `stores/utxo/aerospike/set_mined.go:225-229` |
| The pre-flight runs "on every call" (README:476-477) | Not quite | Skipped when `UnsetMined` is true (`set_mined.go:225`) |
| The pruner sweep has no flag check | Holds | `services/pruner/worker.go:327-337` |
| The sweep always runs | Not quite | Needs `pruner_creatingTxSweepMinAgeBlocks > 0` and `blockHeight > minAge` (`creating_sweep.go:44-47`). Needs the pruner service to run. Selects only records with `unminedSince` in range (`creating_query.go:24`) |
| "Turning the flag off does not strand records" | Misleading | With the flag off, a duplicate multi-record `Create` clears a tentative record with no check that its inputs are spent (`create.go:1644`, `README.md:709-712`). That is a hazard the flag-off path adds. See design doc R4 |
| Pre-flight on a rolling-back or hold record | Fails closed for the whole block | `set_mined.go:859-865` returns an error for the whole `SetMinedMulti` call |

## 2. Findings table

| # | file:line | Claim | What the code does | Verdict |
|---|---|---|---|---|
| 1 | `settings/utxostore_settings.go:76` | Flag enables create-first on stores that support it. Others warn at startup and ignore it | `stores/utxo/aerospike/spend_and_create.go:15` and `stores/utxo/factory/utxo.go:143` do this | MATCHES |
| 2 | `settings/utxostore_settings.go:76` | "The tentative state is carried in the on-disk flags byte (bit 5)" | Bit 5 is `Creating`. Bit 6 is `RollingBack` (`stores/utxo/meta/data.go:150`). `Hold`, `Reversed`, `Attempt` are not serialized | MISLEADING |
| 3 | `settings/utxostore_settings.go:76` | An old binary decodes a `creating` record as fully created | `main` documents bits 0 to 4 only (diff of `meta/data.go`). Not run against an old binary | MATCHES (by reading the format; see section 4) |
| 4 | `settings/utxostore_settings.go:76` | "The value is read once at startup" | The store reads `s.settings.UtxoStore.UseCreateFirstOrder` on every call (`stores/utxo/aerospike/spend_and_create.go:15`). The struct is set in `NewSettings`. I found no reload path | MATCHES |
| 5 | `settings/utxostore_settings.go:76` | "Healers run regardless of this flag ... turning the flag off does not strand records" | See 1b | MISLEADING |
| 6 | `settings/utxostore_settings.go:76` | "The Lua package must have been re-registered by the new build before enabling; the store re-registers automatically on a content mismatch" | `teranode.go:193`, `:149`. True. Omits that the new body also reaches old nodes at the first new start, flag off or on | MATCHES |
| 7 | PR body, "Rollout gates" | "Lua package bump to v63" | `LuaPackage` is `teranode_v66` on both sides. See 1a | WRONG |
| 8 | `settings/utxostore_settings.go:47` | `desc`: "Timeout for rolling back the partial spends of a failed spend batch". `longdesc` describes only the partial-spend rollback and caller latency | The PR changed this setting's role without changing the text. It is now also the spend lease length (`create_first_rollback.go:34`), the hold TTL (`:463-470`), the busy-marker staleness margin (`:485`), the sweep hold time, and the cross-host clock bound (`:71-73`) | MISLEADING |
| 9 | `settings/pruner_settings.go:28` | "A locked parent is not bypassed; the tx is retried on a later pass" | True until the record is ten times older than the sweep age. Then the sweep rolls it back (`creating_sweep.go:228`, `:310`). The `longdesc` never mentions this | MISLEADING |
| 10 | `settings/pruner_settings.go:28` | "The age must outlast `utxostore_spendRollbackTimeout` in wall-clock time" | The age is in blocks. `H` is in seconds. No code relates them. A stated requirement that nothing enforces. `busy`/`holdUntil` protect a live owner instead | MATCHES (requirement stated; not enforced; see section 3) |
| 11 | `settings/pruner_settings.go:28` | Sweep "not gated on `utxostore_useCreateFirstOrder`" and "stores without create-first support return nothing" | `worker.go:327-337`. `stores/utxo/nullstore/nullstore.go:58-67` returns nothing. I did not read the SQL store's version | MATCHES |
| 12 | `settings/pruner_settings.go:28` | "A hold whose time has passed is deleted by the sweep" | `creating_sweep.go:122-138` via `ClearCreateFirstHold(..., false)` | MATCHES |
| 13 | `stores/utxo/aerospike/README.md:99` | "The plain single-record path ... does not set the bin" | True for a plain create. A create-first create (`WithCreating(true)`) sets the bin on a single record (`create.go:1079`) | MISLEADING |
| 14 | `README.md:110` | The Lua `spend` refuses a record with `creating` and returns `CREATING` | `teranode.lua:377` | MATCHES |
| 15 | `README.md:123` | "`setMined` deletes the `creating` bin from the master record and from every extra record that exists" | The Lua and expression `setMined` do **not** clear it (`teranode.lua:791`, `set_mined_expressions.go:754-758`). The master of a legacy record is cleared by the pre-flight. Extra records are cleared with no filter (`set_mined.go:729`, `:741`). The same paragraph later says the Lua function does not touch the bin | WRONG (first sentence); the paragraph contradicts itself |
| 16 | `README.md:437` | "There is no native Aerospike TTL on creating records" | A hold carries `creating` and is written with a TTL of `H` (`create_first_rollback.go:463-470`). `README.md:630-632` says so | MISLEADING |
| 17 | `README.md:443-446` | A retry on a still-creating record rolls it forward and returns `ErrTxExists` that `IsRolledForward` recognises | `spend_and_create.go:1236-1248` | MATCHES |
| 18 | `README.md:476-479` | The pre-flight "runs on every call, whatever the flag says" | Skipped when `UnsetMined` (`set_mined.go:225`) | MISLEADING |
| 19 | `README.md:481` | "The healers ... are not gated on the flag" | See 1b | MISLEADING |
| 20 | `README.md:496-499` | The marker write and the lease take are single-record writes with the same base filter. The server orders them. "While a spender holds a lease no rollback begins, and once a rollback has begun no spender starts" | Order of the two writes: true (`create_first_rollback.go:90`, `:225`). Whether a lease **counts as live** uses a literal built from the caller's clock (`:261-268`) | MISLEADING (second half holds only inside the clock bound) |
| 21 | `README.md:509-511` | "No lease is granted on a record that is final" | `TakeSpendLease` uses `finalizeFilter`, which needs `creating` (`create_first_rollback.go:90`, `finalize.go:329`) | MATCHES |
| 22 | `README.md:520-523` | "the server does not start a command whose timeout has passed" | A server behaviour. The Go client sets the timeout (`util/uaerospike/client.go:600`) | Cannot verify (section 4) |
| 23 | `README.md:541-545` | "The margin is one `utxostore_spendRollbackTimeout` ... hosts must agree on the time to within that setting" | The margin is `H` minus the in-flight time of a write (design doc section 5). Nothing checks it | MISLEADING |
| 24 | `README.md:544-545` | "The order of a lease and a rollback claim does not depend on clocks" | Whether a lease blocks a claim depends on the caller's clock (`create_first_rollback.go:225`, `:261`) | MISLEADING |
| 25 | `README.md:549-567` | One finalize owns the hand-off. Caller recorded in the `attempt` bin | `finalize.go:355-370`, `:378-403` | MATCHES |
| 26 | `README.md:565-567` | Three failed tries leave the record possibly final. The tx waits for the unmined reload | `finalizeWithRetry` (`spend_and_create.go:1612-1639`) | MATCHES |
| 27 | `README.md:577` | Rollback retries its claim for `utxostore_spendWaitTimeout` | `spendLeaseWait`, `beginAfterSpendLeases` (`spend_and_create.go:1040`, `:1058`) | MATCHES |
| 28 | `README.md:676` | Sweep rolls back a record older than ten times `pruner_creatingTxSweepMinAgeBlocks` | `creating_sweep.go:310` | MATCHES |
| 29 | `README.md:679-683` | Hold expiry rules | `create_first_rollback.go:627-694` | MATCHES |
| 30 | `README.md:692-694` | Gap: a stalled rollback owner can unspend after a new attempt spent. Closing needs the attempt in the spend record (issue 1291) | `spend_and_create.go:894-899`. The spend record is txid plus vin (`teranode.lua:603`, `un_spend.go:221-237`) | MATCHES |
| 31 | `README.md:709-712` | With the flag on, a duplicate create leaves `creating`. With it off, the legacy cleanup clears it | `create.go:1644`. True. This is a documented hazard, not a safe default (R4) | MATCHES |
| 32 | `README.md:716-721` | Name carries `teranode_v66`. Body is read back and compared. On mismatch the store re-registers | `teranode.go:82`, `:149`, `:193` | MATCHES |
| 33 | `README.md:731-733` | Creating records are excluded from the unmined restore iterator and rejected as parents. The pruner-mode iterator still sees them | `unmined_iterator.go`, `unmined_iterator_raw.go`, `BlockAssembler.go` diffs | MATCHES |
| 34 | `README.md:734` | `teranode_utxo_create_first_finalize_failed_total` | `stores/utxo/spend_and_create.go:204-208`. It also increments when the failure is an unknown outcome, not only a confirmed failure | MATCHES |
| 35 | `stores/utxo/Interface.go:749` | Hold expires "one `utxostore_spendRollbackTimeout` after it was written or last taken" | A take renews `holdUntil` by `H` but sets the server TTL to `2H` (`create_first_rollback.go:527`). Two mechanisms, two lifetimes | MISLEADING |
| 36 | `stores/utxo/Interface.go:506` | `WithCreating`: "every spend of its outputs is rejected with `ErrTxCreating`" | Lua and expression paths do (`teranode.lua:377`, `spend_expressions.go:140-145`). The native sub-op path is unverified | MISLEADING |
| 37 | `stores/utxo/aerospike/aerospike.go:999` | "the creating bin gates both spend paths (expression filter and Lua)" | There is a third path, the native sub-op (`native_op.go:154`). Its gating is unknown from this repo | MISLEADING |
| 38 | `stores/utxo/Interface.go:675` | `FinalizeTransaction`: `ErrTxNotFound` for absent, rolling-back, or other attempt. nil only to the caller whose write cleared the state | `finalize.go:389-399` | MATCHES |
| 39 | `stores/utxo/Interface.go:743` | `BeginCreateFirstRollback` is exclusive, refuses on a live lease, reports false for absent, final, other attempt, already rolling back | `create_first_rollback.go:217-256` | MATCHES |
| 40 | `stores/utxo/aerospike/set_mined.go:217-220` | "how the native path treats [the bin] is unknown from this repo" | An honest statement of a gap, not a claim. Listed as UNVERIFIED in the design doc (L19n) | MATCHES |
| 41 | `services/pruner/creating_sweep.go:20-42` | Sweep rolls forward, rolls back a loser, finishes an abandoned rollback, deletes an expired hold, announces then unlocks | The function body does each in that order | MATCHES |
| 42 | `stores/utxo/spend_and_create.go:1114-1117` | The lease names a time one rollback bound after the create. The spend runs under a context that ends then | `:1179`, `:1254`. The store writes whole seconds | MATCHES |
| 43 | `docs/references/prometheusMetrics.md` | Documents the new metrics | Only `teranode_validator_shed_rolled_forward_kept_total` was added (line 393). `teranode_utxo_create_first_finalize_failed_total`, `teranode_pruner_creating_backlog`, `teranode_pruner_creating_sweep_failures_total` are not listed | MISLEADING (incomplete) |

## 3. The design doc's weakest rows (by enforcement class)

These are the ledger rows that rest on convention or assumption, not on a server filter. A reviewer who wants the design flaw should start here.

| Row | What it is |
|---|---|
| L15 | A reversal never clears a later attempt's spends. **DOC only.** The spend record has no attempt. Issue 1291 |
| L24 | Host clocks agree within `H` minus in-flight time. **Assumed.** No check |
| L25 | Sweep age in blocks outlasts `H` in seconds. **DOC only** |
| L28 / R4 | Flag-off duplicate create clears a tentative record. Found by reading the code. No comment names it as a window |

## 4. UNVERIFIED items (copied from the design doc, with what to check)

| ID | Item | What to check |
|---|---|---|
| U1 | Native sub-op `spendMulti` refuses `creating` (L1n) | The out-of-tree server module source |
| U2 | Native `setMined` keeps the `creating` bin (L19n, R5) | Same module |
| U3 | Aerospike server does not start a batch command whose timeout has passed (L6, README:522-523) | Aerospike batch policy and `total_timeout` semantics for the server build in use |
| U4 | In-flight time bound of a spend write (clock margin, section 5) | `util.GetAerospikeBatchPolicy` timeouts and the retry budget in `uaerospike.Client.BatchOperateBefore` |
| U5 | A create can land between the setMined pre-flight read and the mined write (L20, R6) | Callers of `SetMinedMulti`; whether block validation can create the same txid concurrently |
| U6 | Which `CreateOnly` callers can meet a tentative multi-record master (R4) | Callers of `Create` with `WithCreateOnly` |
| U7 | An old binary misreads bit 5 as final (L27, section 6 step 1) | Run an old binary against a tentative record, or read its decode path |
| U8 | How often legacy `creating` records with no token exist (section 6 step 2) | Production metrics or logs for "creating flag cleanup failed" |
| U9 | Whether any validator metric covers a `setMined` failure on a hold record (R12) | `services/validator/metrics.go`, `services/blockvalidation` |
| U10 | The settings struct has no hot reload (finding 4) | Search for code that rewrites `UtxoStore` settings after start |
| U11 | The SQL store's behaviour for the new interface methods (finding 11) | `stores/utxo/sql/sql.go` |

## 5. Assessment: is the mechanism set finished?

Still growing. The evidence is in the code and the history, not in an opinion. Each mechanism exists to close a window that the previous mechanism opened. The `attempt` token exists because a spend record holds no attempt. The `hold` exists because the token cannot stop an unspend. The spend `lease` exists because a hold has no transaction data. `busy` and `holdUntil` exist because two actors may take the hold. `reversed` exists because the blob may be gone before the cleanup ends. The latest commit (`5403765a4`, 2026-11-23) added another rule on top of this stack, the ten-times age rollback (`creating_sweep.go:310`). The PR has 27 commits over `origin/main`. Eight of them touch `create_first_rollback.go`, which first appeared on 2026-11-04 and gained the hold and attempt logic on 2026-11-09. The window that would stop the growth is not closed: the code names it (issue 1291, the attempt in the spend record) and does not implement it. Three ledger rows (L15, L24, L25) rest on a comment or an assumption. Two further windows (R4, R7) follow from the code, and no comment names them. R4, the duplicate create that clears a tentative record with the flag off, is the one I would raise first, because it defeats the "flag off is safe" claim in the settings text. I cannot say how many windows remain. I can say the present set was not derived from a model and that the document makes the model explicit so a reviewer can check it.

## 6. Verification run

| Check | Command | Result |
|---|---|---|
| Build | `go build ./...` in this worktree | exit 0, no output |
| Tracked code untouched | `git status --short` before commit | only `HANDOFF.md`, `docs/design/`, and the untracked `SPLIT_BRIEF.md` |
| Citations | A script extracted every `path:line` and `path:line-line` from both documents (213), resolved bare names by the short-name rule, and compared each line number with the length of the file in the reference checkout | 213 cites, 0 unresolved, 0 out of range |
| Reference checkout | `git -C /Users/ordishs/dev/teranode-pr1355 status --short` | clean. HEAD `5403765a4` |

The script checks that each cited line **exists**. It does not check that the line says what the sentence claims. I read the code at each cited function and block while I wrote the sentence. Continuation cites that start with `:` (for example `(:1236)`) are not matched by the script. I checked them by hand against the `grep -n` output of the same file.

Not run: the Go test suite. This change adds documentation only and touches no Go file. `go build ./...` confirms the tree still compiles.


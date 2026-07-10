//! Real Aerospike-backed UtxoStore using the upstream `aerospike` crate.
//! UDF/bin/response shapes mirror stores/utxo/aerospike + teranode.lua,
//! proven interoperable in rust/aerospike-compat-spike.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU32, Ordering};

use aerospike::operations::scalar::put;
use aerospike::query::Filter;
use aerospike::{
    BatchOperation, BatchPolicy, BatchUDFPolicy, BatchWritePolicy, Bin, Bins, Client, ClientPolicy,
    Key, PartitionFilter, QueryPolicy, ReadPolicy, Record, RecordExistsAction, Statement, Value,
    WritePolicy,
};
use ba_subtree_bench::hash::{sha256, Hash};
use ba_subtree_bench::subtree::COINBASE_PLACEHOLDER;
use ba_subtree_bench::tx::{varint, Tx, TxInput, TxOutput};
use futures::StreamExt;
use tonic::async_trait;

use super::{
    IgnoreFlags, MinedBlockInfo, Spend, SpendingData, StoreError, TxMeta, UnminedTx, UtxoStore,
};

/// Default `utxoBatchSize`. Go requires this to be a configured positive value
/// (stores/utxo/aerospike/aerospike.go:196) with no in-code default; production
/// deployments set it explicitly. The conflict-cascade key-source/offset math
/// only diverges from this when a tx has more outputs than one record holds
/// (pagination), so the value MUST match the deployment's
/// `UtxoStore.UtxoBatchSize` for `spend`/`unspend`/`set_conflicting` to resolve
/// the correct records. See `set_utxo_batch_size`.
const DEFAULT_UTXO_BATCH_SIZE: u32 = 20_000;

/// Coinbase maturity in blocks: `spendingHeight = blockHeight + COINBASE_MATURITY`
/// for coinbase records (Go `s.settings.ChainCfgParams.CoinbaseMaturity`). In the
/// pinned go-chaincfg v1.5.8 this is 100 for EVERY network (mainnet, stn, regtest,
/// testnet, teratestnet, tstn), so a constant is faithful across the dev rig.
const COINBASE_MATURITY: u32 = 100;

pub struct AeroUtxoStore {
    client: Client,
    namespace: String,
    set: String,
    udf_module: String,
    block_height_retention: u32,
    /// Mirrors Go `s.utxoBatchSize`: outputs-per-record for pagination. Drives
    /// `CalculateKeySource` (record selection) and `calculateOffsetForOutput`.
    utxo_batch_size: u32,
    /// Mirrors Go `s.blockHeight.Load()`: the current chain height passed to the
    /// spend/unspend/setConflicting UDFs as `currentBlockHeight`. Set via
    /// `set_block_height` as the chain advances.
    block_height: AtomicU32,
}

impl AeroUtxoStore {
    pub async fn connect(
        hosts: &str,
        namespace: &str,
        set: &str,
        udf_module: &str,
        block_height_retention: u32,
    ) -> Result<Self, StoreError> {
        let policy = ClientPolicy::default();
        let client = Client::new(&policy, &hosts.to_string())
            .await
            .map_err(|e| StoreError::Backend(format!("aerospike connect {hosts}: {e}")))?;

        Ok(Self {
            client,
            namespace: namespace.to_string(),
            set: set.to_string(),
            udf_module: udf_module.to_string(),
            block_height_retention,
            utxo_batch_size: DEFAULT_UTXO_BATCH_SIZE,
            block_height: AtomicU32::new(0),
        })
    }

    /// Override the `utxoBatchSize` to match the deployment's
    /// `UtxoStore.UtxoBatchSize`. MUST equal the value used when records were
    /// written, or the spend/unspend/setConflicting key-source math will target
    /// the wrong pagination record for multi-record txs.
    pub fn set_utxo_batch_size(&mut self, size: u32) {
        if size > 0 {
            self.utxo_batch_size = size;
        }
    }

    /// Update the current chain height (Go `s.blockHeight.Store`). Threaded into
    /// the spend/unspend/setConflicting UDF args as `currentBlockHeight`.
    pub fn set_block_height(&self, height: u32) {
        self.block_height.store(height, Ordering::Relaxed);
    }

    /// The master-record key is the raw 32-byte txid (mirrors Go
    /// `aerospike.NewKey(namespace, setName, txID[:])` in setMined /
    /// MarkTransactionsOnLongestChain).
    fn key(&self, txid: &Hash) -> Result<Key, StoreError> {
        Key::new(
            self.namespace.clone(),
            self.set.clone(),
            Value::Blob(txid.to_vec()),
        )
        .map_err(|e| StoreError::Backend(format!("key: {e}")))
    }

    /// Mirrors `uaerospike.CalculateKeySource` (util/uaerospike/client.go:498):
    /// the UTXO at (txid, vout) lives in pagination record `vout / batchSize`.
    /// Record 0 keys on the bare 32-byte txid; record N>0 keys on
    /// `txid[32] ‖ N(u32 LE)` (36 bytes). Matches the spike `keys.rs` helper.
    fn key_source(&self, txid: &Hash, vout: u32) -> Result<Key, StoreError> {
        let num = vout / self.utxo_batch_size;
        let ks: Vec<u8> = if num == 0 {
            txid.to_vec()
        } else {
            let mut v = Vec::with_capacity(36);
            v.extend_from_slice(txid);
            v.extend_from_slice(&num.to_le_bytes());
            v
        };

        Key::new(self.namespace.clone(), self.set.clone(), Value::Blob(ks))
            .map_err(|e| StoreError::Backend(format!("key_source: {e}")))
    }

    /// Mirrors Go `calculateOffsetForOutput`: the UTXO's slot within its
    /// pagination record is `vout % batchSize`.
    fn offset_for_output(&self, vout: u32) -> u32 {
        vout % self.utxo_batch_size
    }
}

#[async_trait]
impl UtxoStore for AeroUtxoStore {
    async fn set_mined_multi(
        &self,
        hashes: &[Hash],
        info: &MinedBlockInfo,
    ) -> Result<HashMap<Hash, Vec<u32>>, StoreError> {
        if hashes.is_empty() {
            return Ok(HashMap::new());
        }

        let bup = BatchUDFPolicy::default();

        // setMined(rec, blockID, blockHeight, subtreeIdx, currentBlockHeight,
        //          blockHeightRetention, onLongestChain, unsetMined)
        // Arg order/types verified against teranode.lua:558 and the proven spike
        // (rust/aerospike-compat-spike/tests/t7_udf_setmined.rs). All integers map
        // to Value::Int (the crate has no unsigned integer variant).
        let ops: Vec<BatchOperation> = hashes
            .iter()
            .map(|h| {
                let key = self.key(h)?;
                let args = vec![
                    Value::Int(i64::from(info.block_id)),
                    Value::Int(i64::from(info.block_height)),
                    Value::Int(i64::from(info.subtree_idx)),
                    // currentBlockHeight: Go passes s.blockHeight.Load() (the
                    // current chain tip height), NOT the mined block's height.
                    // This field defaults to 0 until set_block_height is called
                    // at boot; callers must invoke set_block_height before
                    // set_mined_multi for the correct value to propagate.
                    Value::Int(i64::from(self.block_height.load(Ordering::Relaxed))),
                    Value::Int(i64::from(self.block_height_retention)),
                    Value::Bool(info.on_longest_chain),
                    Value::Bool(info.unset_mined),
                ];

                Ok(BatchOperation::udf(
                    &bup,
                    key,
                    &self.udf_module,
                    "setMined",
                    Some(args),
                ))
            })
            .collect::<Result<_, StoreError>>()?;

        let results = self
            .client
            .batch(&BatchPolicy::default(), &ops)
            .await
            .map_err(|e| StoreError::Backend(format!("batch setMined: {e}")))?;

        let mut out = HashMap::new();

        for (h, rec) in hashes.iter().zip(results.iter()) {
            // On UDF success the returned Lua map lands in the "SUCCESS" bin
            // (Aerospike server convention; "FAILURE" on error). The map carries
            // the updated `blockIDs` list (teranode.lua FIELD_BLOCK_IDS = "blockIDs").
            let block_ids = rec
                .record
                .as_ref()
                .and_then(|r| r.bins.get("SUCCESS").or_else(|| r.bins.get("blockIDs")))
                .map(parse_block_ids)
                .unwrap_or_default();
            out.insert(*h, block_ids);
        }

        // Postcondition: every input hash present.
        for h in hashes {
            out.entry(*h).or_default();
        }

        Ok(out)
    }

    async fn unmined(&self) -> Result<Vec<UnminedTx>, StoreError> {
        // Select only the bins we need. The txid is read from the `txID` bin —
        // NOT the record key — mirroring Go's QueryOldUnminedTransactions, which
        // does `record.Bins[fields.TxID.String()]` (stores/utxo/aerospike/aerospike.go).
        // The user key is not returned unless writes set send_key, so the bin is
        // the reliable source.
        let mut stmt = Statement::new(
            &self.namespace,
            &self.set,
            Bins::Some(vec![
                "txID".into(),
                "fee".into(),
                "sizeInBytes".into(),
                "blockIDs".into(),
                "locked".into(),
                "createdAt".into(),
            ]),
        );

        // unminedSince is set (numeric) for unmined records; range over all
        // positive heights via the numeric secondary index. Go uses
        // NewRangeFilter(unminedSince, 1, cutoff); here we take the full range.
        // The `as_range!` macro is unusable from outside the crate at this
        // version (it expands to a pub(crate) Filter::new), so use the public
        // Filter::range constructor directly.
        stmt.add_filter(Filter::range("unminedSince", 1_i64, i64::MAX));

        let recordset = self
            .client
            .query(&QueryPolicy::default(), PartitionFilter::all(), stmt)
            .await
            .map_err(|e| StoreError::Backend(format!("unmined query: {e}")))?;

        let mut out = Vec::new();

        // With the async client, query results are consumed as a futures::Stream
        // (the blocking `Iterator for &Recordset` is gated behind the `sync`
        // feature, which we do not enable).
        let mut stream = recordset.into_stream();
        while let Some(rec) = stream.next().await {
            let rec = rec.map_err(|e| StoreError::Backend(format!("recordset: {e}")))?;

            // VERIFY-ON-RIG: txid is read from the `txID` bin (32-byte blob),
            // matching Go's QueryOldUnminedTransactions. Confirm the bin is
            // populated for seeded unmined records on the live rig.
            let txid_bytes = match rec.bins.get("txID") {
                Some(Value::Blob(b)) => b.clone(),
                _ => return Err(StoreError::Decode("record missing txID blob bin".into())),
            };
            let txid: Hash = txid_bytes
                .as_slice()
                .try_into()
                .map_err(|_| StoreError::Decode("txID bin not 32 bytes".into()))?;

            let fee = bin_u64(&rec, "fee");
            let size = bin_u64(&rec, "sizeInBytes");
            let block_ids = rec
                .bins
                .get("blockIDs")
                .map(parse_block_ids)
                .unwrap_or_default();
            let locked = matches!(rec.bins.get("locked"), Some(Value::Bool(true)));
            // VERIFY-ON-RIG: `createdAt` (fields.go:73, a UnixMilli int) is read
            // from the bin of the same name. Confirm seeded unmined records carry
            // it; Go's extractCreatedAt errors if the bin is absent, but here a
            // missing bin degrades to 0 so the boot load never aborts.
            let created_at = bin_i64(&rec, "createdAt");

            out.push(UnminedTx {
                txid,
                fee,
                size,
                block_ids,
                locked,
                created_at,
            });
        }

        Ok(out)
    }

    async fn mark_on_longest_chain(&self, hashes: &[Hash], on: bool) -> Result<(), StoreError> {
        if hashes.is_empty() {
            return Ok(());
        }

        // This is NOT a UDF. The Go path (stores/utxo/aerospike/longest_chain.go,
        // MarkTransactionsOnLongestChain) issues a batch write of the `unminedSince`
        // bin with RecordExistsAction = UPDATE_ONLY:
        //   - onLongestChain=true  -> clear unminedSince (Value::Nil): tx mined on
        //                             the main chain, so it is no longer "unmined".
        //   - onLongestChain=false -> set unminedSince = currentBlockHeight: the tx
        //                             is unmined again (re-discoverable by the sindex).
        let wp = BatchWritePolicy {
            record_exists_action: RecordExistsAction::UpdateOnly,
            ..BatchWritePolicy::default()
        };

        // VERIFY-ON-RIG: the Go path writes the *current block height* when
        // re-marking a tx as unmined (on=false). The Stage 3 trait does not yet
        // thread the live chain height into this call, and the store does not hold
        // it. We write `Value::Nil` for on=true (fully correct), and for on=false a
        // positive sentinel within the sindex range [1, cutoff] so the record stays
        // discoverable by `unmined()`. Confirm against the rig whether the precise
        // height is required by any consumer; if so, plumb current height through.
        let bin_value = if on { Value::Nil } else { Value::Int(1) };

        let ops: Vec<BatchOperation> = hashes
            .iter()
            .map(|h| {
                let key = self.key(h)?;
                let bin = Bin::new("unminedSince".to_string(), bin_value.clone());
                Ok(BatchOperation::write(&wp, key, vec![put(&bin)]))
            })
            .collect::<Result<_, StoreError>>()?;

        let results = self
            .client
            .batch(&BatchPolicy::default(), &ops)
            .await
            .map_err(|e| StoreError::Backend(format!("batch mark_on_longest_chain: {e}")))?;

        // Surface a per-record failure as a backend error (mirrors Go's per-record
        // result-code check, minus the fatal-on-missing escalation).
        for (h, rec) in hashes.iter().zip(results.iter()) {
            if let Some(code) = rec.result_code {
                if !matches!(code, aerospike::ResultCode::Ok) {
                    return Err(StoreError::Backend(format!(
                        "mark_on_longest_chain {}: result code {code:?}",
                        hex::encode(h)
                    )));
                }
            }
        }

        Ok(())
    }

    async fn set_locked(&self, hashes: &[Hash], locked: bool) -> Result<(), StoreError> {
        if hashes.is_empty() {
            return Ok(());
        }

        // VERIFY-ON-RIG: this writes the `locked` bin (teranode.lua BIN_LOCKED =
        // "locked", fields.go Locked = "locked") to match Go's SetLocked.
        //
        // DIVERGENCE FROM GO (documented, not faked): Go's SetLocked
        // (stores/utxo/aerospike/locked.go) is NOT a plain bin write — it invokes
        // the `setLocked` UDF (teranode.lua:1164) via BatchUDF. That UDF, beyond
        // setting `locked`, ALSO (a) clears `deleteAtHeight` when LOCKING
        // (setValue=true), and (b) recurses into child records (totalExtraRecs)
        // for multi-record (large) txs.
        //
        // We mirror mark_on_longest_chain's batch-write style: a UpdateOnly write
        // of the `locked` bin on the master record. This is FULLY faithful for the
        // Stage 3 boot use-case, which only ever UNLOCKS (locked=false): unlocking
        // does not touch deleteAtHeight, and child records carry their own `locked`
        // bin that is read independently. For the LOCK direction or large-tx child
        // fan-out, the UDF path must be used — confirm on the rig before relying on
        // set_locked(.., true) here.
        let wp = BatchWritePolicy {
            record_exists_action: RecordExistsAction::UpdateOnly,
            ..BatchWritePolicy::default()
        };

        let ops: Vec<BatchOperation> = hashes
            .iter()
            .map(|h| {
                let key = self.key(h)?;
                let bin = Bin::new("locked".to_string(), Value::Bool(locked));
                Ok(BatchOperation::write(&wp, key, vec![put(&bin)]))
            })
            .collect::<Result<_, StoreError>>()?;

        let results = self
            .client
            .batch(&BatchPolicy::default(), &ops)
            .await
            .map_err(|e| StoreError::Backend(format!("batch set_locked: {e}")))?;

        for (h, rec) in hashes.iter().zip(results.iter()) {
            if let Some(code) = rec.result_code {
                if !matches!(code, aerospike::ResultCode::Ok) {
                    return Err(StoreError::Backend(format!(
                        "set_locked {}: result code {code:?}",
                        hex::encode(h)
                    )));
                }
            }
        }

        Ok(())
    }

    async fn create(
        &self,
        tx: &Tx,
        block_height: u32,
        mined: Option<MinedBlockInfo>,
        locked: bool,
    ) -> Result<(), StoreError> {
        if tx.outputs.is_empty() {
            return Err(StoreError::Backend("create: tx has no outputs".into()));
        }
        // Single-record only. A tx whose outputs would span >1 pagination record
        // is the validator's responsibility (regular tx creation), not block
        // assembly's coinbase write. Reject rather than write a partial record.
        if tx.outputs.len() > self.utxo_batch_size as usize {
            return Err(StoreError::Backend(format!(
                "create: tx has {} outputs > utxoBatchSize {} (pagination unsupported)",
                tx.outputs.len(),
                self.utxo_batch_size
            )));
        }

        // createdAt is a UnixMilli int bin (fields.go CreatedAt). Real wall-clock
        // time — this is the live service, not a workflow script.
        let created_at = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis() as i64)
            .unwrap_or(0);

        let txid = tx.txid();
        let bins = build_create_bins(
            tx,
            &txid,
            block_height,
            mined.as_ref(),
            locked,
            COINBASE_MATURITY,
            created_at,
        );

        let key = self.key(&txid)?;
        // CREATE_ONLY: matches Go's batchWritePolicy.RecordExistsAction =
        // aerospike.CREATE_ONLY. A key-exists outcome is the benign ErrTxExists
        // case Go's processCoinbaseUtxos logs-and-skips.
        let wp = WritePolicy {
            record_exists_action: RecordExistsAction::CreateOnly,
            ..WritePolicy::default()
        };

        match self.client.put(&wp, &key, &bins).await {
            Ok(()) => Ok(()),
            Err(e) => {
                let m = format!("{e}").to_lowercase();
                if m.contains("key_exists")
                    || m.contains("key exists")
                    || m.contains("already exists")
                    || m.contains("record exists")
                {
                    // Idempotent: the coinbase UTXO is already present.
                    Ok(())
                } else {
                    Err(StoreError::Backend(format!(
                        "create put {}: {e}",
                        hex::encode(txid)
                    )))
                }
            }
        }
    }

    async fn spend(
        &self,
        spends: &[Spend],
        block_height: u32,
        ignore: IgnoreFlags,
    ) -> Result<Vec<Spend>, StoreError> {
        if spends.is_empty() {
            return Ok(Vec::new());
        }

        // Mirrors Go's spendMulti batch path (stores/utxo/aerospike/spend.go:643).
        // Go groups spends by (keySource, ignoreLocked) and sends ONE BatchUDF
        // per group whose first arg is a LIST of per-spend maps. We keep one
        // BatchUDF per spend (a degenerate one-element list) to avoid replicating
        // the grouping logic; functionally identical because the Lua `spendMulti`
        // iterates the list regardless of length. The arg order/types are taken
        // verbatim from createBatchRecords (spend.go:643) and the proven spike
        // (rust/aerospike-compat-spike t6 single-key `spend`, same encoding):
        //
        //   spendMulti(rec, spends[], ignoreConflicting, ignoreLocked,
        //              currentBlockHeight, blockHeightRetention)
        //
        // Each spend map (createSpendMapValue, spend.go:631):
        //   {idx, offset, vOut, utxoHash(blob), spendingData(36-byte blob)}
        //
        // VERIFY-ON-RIG: the round-trip (a seeded unspent UTXO becomes a 68-byte
        // spent entry carrying spendingData) is proven for single-key `spend` in
        // the spike but NOT for `spendMulti` via the Rust client at this crate
        // version. Run against the live rig before relying on this.
        let bup = BatchUDFPolicy::default();
        let retention = i64::from(self.block_height_retention);

        let mut ops: Vec<BatchOperation> = Vec::with_capacity(spends.len());

        for (idx, s) in spends.iter().enumerate() {
            let spending_data = s.spending_data.as_ref().ok_or_else(|| {
                StoreError::Backend(format!(
                    "spend {}:{}: spending_data is required",
                    hex::encode(s.tx_id),
                    s.vout
                ))
            })?;

            let key = self.key_source(&s.tx_id, s.vout)?;

            let mut item: HashMap<Value, Value> = HashMap::new();
            item.insert(Value::from("idx"), Value::Int(idx as i64));
            item.insert(
                Value::from("offset"),
                Value::Int(i64::from(self.offset_for_output(s.vout))),
            );
            item.insert(Value::from("vOut"), Value::Int(i64::from(s.vout)));
            item.insert(Value::from("utxoHash"), Value::Blob(s.utxo_hash.to_vec()));
            item.insert(
                Value::from("spendingData"),
                Value::Blob(spending_data.bytes().to_vec()),
            );

            let args = vec![
                Value::List(vec![Value::HashMap(item)]),
                Value::Bool(ignore.ignore_conflicting),
                Value::Bool(ignore.ignore_locked),
                Value::Int(i64::from(block_height)),
                Value::Int(retention),
            ];

            ops.push(BatchOperation::udf(
                &bup,
                key,
                &self.udf_module,
                "spendMulti",
                Some(args),
            ));
        }

        let results = self
            .client
            .batch(&BatchPolicy::default(), &ops)
            .await
            .map_err(|e| StoreError::Backend(format!("batch spendMulti: {e}")))?;

        // Surface a per-record UDF/transport failure. The Lua status (OK/ERROR)
        // lives in the SUCCESS-bin map; a hard conflict/locked rejection comes
        // back as an error status the caller must inspect. VERIFY-ON-RIG: confirm
        // the exact response shape (status/errors/signal) against teranode.lua.
        for (s, rec) in spends.iter().zip(results.iter()) {
            if let Some(code) = rec.result_code {
                if !matches!(code, aerospike::ResultCode::Ok) {
                    return Err(StoreError::Backend(format!(
                        "spend {}:{}: result code {code:?}",
                        hex::encode(s.tx_id),
                        s.vout
                    )));
                }
            }
        }

        // Go returns the input spends (now carrying the written SpendingData).
        Ok(spends.to_vec())
    }

    async fn unspend(&self, spends: &[Spend], _flag_as_locked: bool) -> Result<(), StoreError> {
        if spends.is_empty() {
            return Ok(());
        }

        // Mirrors Go `unspendLua` (un_spend.go:147) — a per-spend single-key
        // Execute (NOT a batch). Arg order/types verbatim:
        //   unspend(rec, offset, utxoHash(blob), expectedSpendingData(36-byte blob),
        //           currentBlockHeight, blockHeightRetention)
        // The expected spending data is a mandatory ownership check: the Lua only
        // clears the UTXO if the stored spender matches.
        //
        // DIVERGENCE: Go's `flagAsLocked` is handled inside the broader Unspend
        // wrapper (a separate SetLocked pass), not by the `unspend` UDF itself,
        // whose args carry no lock flag. We therefore drop `_flag_as_locked` here
        // and document that re-locking must be issued via `set_locked` by the
        // caller. VERIFY-ON-RIG: confirm no consumer relies on `unspend` alone to
        // re-lock.
        let policy = WritePolicy::default();
        let current_height = i64::from(self.block_height.load(Ordering::Relaxed));
        let retention = i64::from(self.block_height_retention);

        for s in spends {
            let spending_data = s.spending_data.as_ref().ok_or_else(|| {
                StoreError::Backend(format!(
                    "unspend {}:{}: spending_data is required",
                    hex::encode(s.tx_id),
                    s.vout
                ))
            })?;

            let key = self.key_source(&s.tx_id, s.vout)?;

            let args = vec![
                Value::Int(i64::from(self.offset_for_output(s.vout))),
                Value::Blob(s.utxo_hash.to_vec()),
                Value::Blob(spending_data.bytes().to_vec()),
                Value::Int(current_height),
                Value::Int(retention),
            ];

            self.client
                .execute_udf(&policy, &key, &self.udf_module, "unspend", Some(&args))
                .await
                .map_err(|e| {
                    StoreError::Backend(format!("unspend {}:{}: {e}", hex::encode(s.tx_id), s.vout))
                })?;
        }

        Ok(())
    }

    async fn set_conflicting(
        &self,
        tx_hashes: &[Hash],
        value: bool,
    ) -> Result<(Vec<Spend>, Vec<Hash>), StoreError> {
        if tx_hashes.is_empty() {
            return Ok((Vec::new(), Vec::new()));
        }

        // Mirrors Go `SetConflicting` (stores/utxo/aerospike/conflicting.go:37).
        // Go's flow per tx:
        //   1. get(Tx, ConflictingChildren, Utxos)        -- read the record
        //   2. updateParentConflictingChildren(tx)        -- list-append to parents
        //   3. setConflicting UDF on the tx               -- flip the flag
        //   4. affectedParentSpends   <- from tx.Inputs   -- one Spend per input
        //   5. spendingChildTxs       <- from tx.Outputs' stored spendingData
        //
        // The setConflicting UDF args (conflicting.go:88):
        //   setConflicting(rec, setValue, currentBlockHeight, blockHeightRetention)
        let bup = BatchUDFPolicy::default();
        let current_height = i64::from(self.block_height.load(Ordering::Relaxed));
        let retention = i64::from(self.block_height_retention);

        // I1: skip the coinbase placeholder (mirrors Go conflicting.go:52-55:
        // `if txHash.Equal(subtree.CoinbasePlaceholderHashValue) { continue }`).
        // Node 0 of subtree 0 is always [0xFF;32] — it is never a real Aerospike
        // record, so attempting to read or UDF-write it would either miss or
        // corrupt an unrelated record that happens to key on that blob.
        let effective_hashes = filter_coinbase_placeholder(tx_hashes);

        if effective_hashes.is_empty() {
            return Ok((Vec::new(), Vec::new()));
        }

        // Step 1: read each record's outputs (`utxos`) + inputs reference so we
        // can derive children (and, where possible, parents) BEFORE flipping the
        // flag. We read the bins GetSpend uses to recover stored spendingData.
        //
        // VERIFY-ON-RIG: the `utxos` bin is a LIST of blobs; an unspent output is
        // a 32-byte entry (just the utxoHash), a spent output is a 68-byte entry
        // (utxoHash[32] ‖ spendTxID[32] ‖ vin[4]) — see get.go:219. We extract
        // the spendTxID from every 68-byte entry as a spending child. Confirm the
        // bin name + entry layout on the rig; pagination (multi-record txs) only
        // surfaces record-0 outputs here — see I3 below.
        let read_policy = ReadPolicy::default();

        let mut affected_parent_spends: Vec<Spend> = Vec::new();
        let mut spending_child_txs: Vec<Hash> = Vec::new();

        for h in &effective_hashes {
            // C1 — VERIFY-ON-RIG / db2-prereq: updateParentConflictingChildren
            // Go's SetConflicting (conflicting.go:79) calls
            // `updateParentConflictingChildren(tx)` BEFORE the UDF batch.  That
            // helper (conflicting.go:151) iterates the tx's inputs and, for each
            // unique parent txid, appends this tx's hash to the parent record's
            // `conflictingChildren` bin via a ListAppendWithPolicyOp
            // (AddUnique|NoFail).  Doing this faithfully here requires:
            //   (a) a tx deserializer to extract the inputs from the stored `tx` bin;
            //   (b) access to the full extended-format tx body.
            // Neither exists in this crate yet. This step is deferred to the db2
            // tx-deserializer prerequisite. Until then, parent `conflictingChildren`
            // bins are NOT updated on conflict marking, which means
            // GetConflictingChildren queries on parents will miss this tx as a child.
            // This is NOT silent — do not remove this comment until the step is wired.

            // Read the master record's `utxos` (outputs' spend state). The
            // `inputs`/`tx` bins would be needed to derive affected parents with
            // their exact UTXO hashes (Go uses UTXOHashFromInput, which requires
            // the previous output's locking script + satoshis from the FULL tx
            // body). This crate has NO tx deserializer, so the parent UTXO hashes
            // cannot be recomputed offline — see the affected-parents note below.
            let key = self.key(h)?;
            let rec = self
                .client
                .get(&read_policy, &key, Bins::Some(vec!["utxos".into()]))
                .await
                .map_err(|e| {
                    StoreError::Backend(format!("set_conflicting get {}: {e}", hex::encode(h)))
                })?;

            // VERIFY-ON-RIG: parse the `utxos` list; collect 68-byte entries'
            // embedded spendTxID (bytes [32..64]) as spending children.
            //
            // I3 — VERIFY-ON-RIG (pagination): for large txs (> utxoBatchSize
            // outputs) the outputs span multiple pagination records (record N>0
            // keyed on txid[32] ‖ N[4LE]).  This read only fetches record 0 (the
            // master record), so spending children embedded in record-1+ are NOT
            // surfaced here.  Full pagination support requires reading all
            // totalExtraRecs child records; deferred to the rig-hardening pass.
            if let Some(Value::List(items)) = rec.bins.get("utxos") {
                for entry in items {
                    if let Value::Blob(b) = entry {
                        if b.len() == 68 {
                            let mut child = [0u8; 32];
                            child.copy_from_slice(&b[32..64]);
                            spending_child_txs.push(child);
                        }
                    }
                }
            }

            // AFFECTED PARENTS — DEFERRED (rig + tx-deserializer dependent):
            // Go derives one Spend per tx INPUT, each with UTXOHash =
            // UTXOHashFromInput (needs the prev output's script+satoshis from the
            // extended tx body) and SpendingData = (thisTx, inputIndex). We have
            // neither a tx deserializer nor the extended inputs in this crate, so
            // we CANNOT faithfully build these offline and do NOT fake them. The
            // BFS cascade does not depend on this list (it walks children only),
            // so leaving it empty is safe for `mark_conflicting_recursively`'s
            // correctness; the rollback path that consumes affected parents must
            // supply them another way until a tx deserializer lands.
            let _ = &mut affected_parent_spends;
        }

        // Step 3: flip the conflicting flag via the batch UDF.
        let ops: Vec<BatchOperation> = effective_hashes
            .iter()
            .map(|h| {
                let key = self.key(h)?;
                let args = vec![
                    Value::Bool(value),
                    Value::Int(current_height),
                    Value::Int(retention),
                ];
                Ok(BatchOperation::udf(
                    &bup,
                    key,
                    &self.udf_module,
                    "setConflicting",
                    Some(args),
                ))
            })
            .collect::<Result<_, StoreError>>()?;

        let results = self
            .client
            .batch(&BatchPolicy::default(), &ops)
            .await
            .map_err(|e| StoreError::Backend(format!("batch setConflicting: {e}")))?;

        // I4 — VERIFY-ON-RIG (Lua-response conflict/locked rejection): the
        // transport result_code only surfaces hard Aerospike errors; a Lua-level
        // rejection (e.g. the output is already spent, conflicting, or locked)
        // returns ResultCode::Ok but embeds a `status = "ERROR"` field inside the
        // SUCCESS-bin map.  This check only catches transport failures; parsing
        // the Lua status body to surface spend-conflict or locked rejections as
        // StoreError is deferred to the rig-hardening pass.
        for (h, rec) in effective_hashes.iter().zip(results.iter()) {
            if let Some(code) = rec.result_code {
                if !matches!(code, aerospike::ResultCode::Ok) {
                    return Err(StoreError::Backend(format!(
                        "set_conflicting {}: result code {code:?}",
                        hex::encode(h)
                    )));
                }
            }
        }

        Ok((affected_parent_spends, spending_child_txs))
    }

    async fn get_tx_meta(&self, hash: &Hash) -> Result<Option<TxMeta>, StoreError> {
        // Mirrors Go `Get(hash, fields.Tx, fields.Conflicting, fields.BlockIDs)`.
        // The `tx` bin holds the EXTENDED serialized tx body (fields.go:14-15),
        // `conflicting` a bool (fields.go:38-39), `blockIDs` a list of ints.
        //
        // VERIFY-ON-RIG: confirm the `tx` bin is a Blob (extended serialization)
        // and that an absent record surfaces as a key-not-found (mapped to
        // `Ok(None)`), not a transport error. The Aerospike Rust client returns
        // an error for a missing key; we treat the KeyNotFoundError result code
        // as `Ok(None)` to match Go's `errors.Is(err, ErrTxNotFound)` handling.
        let read_policy = ReadPolicy::default();
        let key = self.key(hash)?;

        let rec = match self
            .client
            .get(
                &read_policy,
                &key,
                Bins::Some(vec![
                    "tx".into(),
                    "conflicting".into(),
                    "blockIDs".into(),
                    "createdAt".into(),
                ]),
            )
            .await
        {
            Ok(rec) => rec,
            Err(e) => {
                // VERIFY-ON-RIG: the crate surfaces a missing record as an error
                // whose result code is KeyNotFoundError; map only that to None.
                if let aerospike::Error::ServerError(
                    aerospike::ResultCode::KeyNotFoundError,
                    _,
                    _,
                ) = e
                {
                    return Ok(None);
                }
                return Err(StoreError::Backend(format!(
                    "get_tx_meta {}: {e}",
                    hex::encode(hash)
                )));
            }
        };

        let tx_bytes = match rec.bins.get("tx") {
            Some(Value::Blob(b)) => b.clone(),
            // VERIFY-ON-RIG: a record with no `tx` bin is treated as absent
            // metadata for db2 purposes; the bytes are required by callers that
            // re-parse the tx (spends_for_tx). An empty body would fail the
            // deserializer downstream, so surface the missing bin as a decode
            // error rather than fabricating bytes.
            _ => {
                return Err(StoreError::Decode(format!(
                    "get_tx_meta {}: record missing tx blob bin",
                    hex::encode(hash)
                )))
            }
        };

        let conflicting = matches!(rec.bins.get("conflicting"), Some(Value::Bool(true)));
        let block_ids = rec
            .bins
            .get("blockIDs")
            .map(parse_block_ids)
            .unwrap_or_default();

        // VERIFY-ON-RIG: `createdAt` (fields.go:73, a UnixMilli int) is read as an
        // i64 via `bin_i64`, defaulting to 0 when the bin is absent. Go's
        // `Get(.., fields.CreatedAt)` returns the same UnixMilli value; db3
        // counter selection (is_older_counter) treats 0 as NEWEST.
        let created_at = bin_i64(&rec, "createdAt");

        Ok(Some(TxMeta {
            tx_bytes,
            conflicting,
            block_ids,
            created_at,
        }))
    }

    async fn get_spending_datas(
        &self,
        hash: &Hash,
    ) -> Result<Vec<Option<SpendingData>>, StoreError> {
        // Mirrors Go's read of the `utxos` bin (fields.go:50-51). Each entry is a
        // blob: 32 bytes (just the utxoHash) = UNSPENT; 68 bytes
        // (utxoHash[32] ‖ spendTxID[32] ‖ vin[4 LE]) = SPENT — see get.go:219-228.
        // Index = vout. Returns one slot per output of the master record.
        //
        // I3 — VERIFY-ON-RIG (pagination): for large txs (> utxoBatchSize outputs)
        // the outputs span multiple pagination records; this only reads record 0
        // (the master). Full pagination requires reading totalExtraRecs child
        // records (get.go:getAllExtraUTXOs); deferred to the rig-hardening pass.
        let read_policy = ReadPolicy::default();
        let key = self.key(hash)?;

        let rec = self
            .client
            .get(&read_policy, &key, Bins::Some(vec!["utxos".into()]))
            .await
            .map_err(|e| {
                StoreError::Backend(format!("get_spending_datas {}: {e}", hex::encode(hash)))
            })?;

        let mut out: Vec<Option<SpendingData>> = Vec::new();

        if let Some(Value::List(items)) = rec.bins.get("utxos") {
            for entry in items {
                match entry {
                    Value::Blob(b) if b.len() == 68 => {
                        let mut tx_id = [0u8; 32];
                        tx_id.copy_from_slice(&b[32..64]);
                        let vin = u32::from_le_bytes([b[64], b[65], b[66], b[67]]);
                        out.push(Some(SpendingData { tx_id, vin }));
                    }
                    // 32-byte (or any non-68 blob) = unspent.
                    _ => out.push(None),
                }
            }
        }

        Ok(out)
    }

    async fn get_conflicting_children_bin(&self, hash: &Hash) -> Result<Vec<Hash>, StoreError> {
        // Mirrors Go `processConflictingChildren` (get.go:1111-1127): the
        // `conflictingCs` bin (fields.go:41 — note the 15-char Aerospike bin-name
        // limit truncates "conflictingChildren" to "conflictingCs") is a LIST of
        // 32-byte blobs, each a child txid. Decoded as raw `chainhash.Hash`.
        //
        // VERIFY-ON-RIG: confirm the bin name is exactly "conflictingCs" on the
        // live rig and each element is a 32-byte blob.
        let read_policy = ReadPolicy::default();
        let key = self.key(hash)?;

        let rec = self
            .client
            .get(&read_policy, &key, Bins::Some(vec!["conflictingCs".into()]))
            .await
            .map_err(|e| {
                StoreError::Backend(format!(
                    "get_conflicting_children_bin {}: {e}",
                    hex::encode(hash)
                ))
            })?;

        let mut out: Vec<Hash> = Vec::new();

        if let Some(Value::List(items)) = rec.bins.get("conflictingCs") {
            for entry in items {
                if let Value::Blob(b) = entry {
                    let child: Hash = b.as_slice().try_into().map_err(|_| {
                        StoreError::Decode(format!(
                            "get_conflicting_children_bin {}: child not 32 bytes",
                            hex::encode(hash)
                        ))
                    })?;
                    out.push(child);
                }
            }
        }

        Ok(out)
    }
}

/// go-bt `Tx.IsCoinbase`: exactly one input whose previous outpoint is the null
/// point (all-zero txid, vout 0xFFFF_FFFF). Drives the `isCoinbase` /
/// `spendingHeight` bins, matching `util.TxMetaDataFromTx`.
fn is_coinbase_tx(tx: &Tx) -> bool {
    tx.inputs.len() == 1
        && tx.inputs[0].prev_txid == [0u8; 32]
        && tx.inputs[0].vout == 0xFFFF_FFFF
}

/// Port of `utxo.ShouldStoreOutputAsUTXO`: store any non-zero-value output, and
/// zero-value outputs only when they are NOT (OP_RETURN | OP_FALSE OP_RETURN).
/// OP_FALSE = 0x00, OP_RETURN = 0x6a.
fn should_store_output_as_utxo(out: &TxOutput) -> bool {
    if out.satoshis > 0 {
        return true;
    }
    let b = &out.locking_script;
    let op_return = !b.is_empty() && b[0] == 0x6a;
    let op_false_op_return = b.len() > 1 && b[0] == 0x00 && b[1] == 0x6a;
    !(op_return || op_false_op_return)
}

/// Per-output UTXO hash = single SHA-256 of `txid ‖ VarInt(vout) ‖ lockingScript
/// ‖ VarInt(satoshis)` (util/utxo_hash.go `UTXOHashInto`).
fn utxo_hash_from_output(txid: &Hash, vout: u32, locking_script: &[u8], satoshis: u64) -> Hash {
    let mut pre = Vec::with_capacity(32 + 9 + locking_script.len() + 9);
    pre.extend_from_slice(txid);
    varint::append(&mut pre, u64::from(vout));
    pre.extend_from_slice(locking_script);
    varint::append(&mut pre, satoshis);
    sha256(&pre)
}

/// Port of `appendOutputInto`: `satoshis(8 LE) ‖ VarInt(scriptLen) ‖ script`.
fn serialize_output(o: &TxOutput) -> Vec<u8> {
    let mut buf = Vec::with_capacity(8 + 9 + o.locking_script.len());
    buf.extend_from_slice(&o.satoshis.to_le_bytes());
    varint::append(&mut buf, o.locking_script.len() as u64);
    buf.extend_from_slice(&o.locking_script);
    buf
}

/// Port of `appendInputExtendedInto`: standard input bytes (`prevTxID(32) ‖
/// vout(4 LE) ‖ VarInt(unlockLen) ‖ unlock ‖ sequence(4 LE)`) followed by the
/// extended suffix (`prevSatoshis(8 LE) ‖ VarInt(prevScriptLen) ‖ prevScript`).
/// An empty unlocking/prev script serializes as `VarInt(0)` == a single 0x00,
/// identical to Go's nil-script branch.
fn serialize_input_extended(i: &TxInput) -> Vec<u8> {
    let mut buf = Vec::with_capacity(32 + 4 + 1 + i.unlocking_script.len() + 4 + 8 + 1 + i.prev_script.len());
    buf.extend_from_slice(&i.prev_txid);
    buf.extend_from_slice(&i.vout.to_le_bytes());
    varint::append(&mut buf, i.unlocking_script.len() as u64);
    buf.extend_from_slice(&i.unlocking_script);
    buf.extend_from_slice(&i.sequence.to_le_bytes());
    buf.extend_from_slice(&i.prev_satoshis.to_le_bytes());
    varint::append(&mut buf, i.prev_script.len() as u64);
    buf.extend_from_slice(&i.prev_script);
    buf
}

/// `len(VarInt(v))` without allocating into the output.
fn varint_len(v: u64) -> usize {
    if v < 0xFD {
        1
    } else if v <= 0xFFFF {
        3
    } else if v <= 0xFFFF_FFFF {
        5
    } else {
        9
    }
}

/// Port of `extendedTxSize`: standard size + 6-byte EF marker, plus per-input
/// `prevSatoshis(8)` and the prev-script `VarInt(len)+len` (empty == 1 byte).
/// `prev_txid` is always present in our parsed inputs (32 bytes counted in the
/// standard size), so the Go `-32` correction for an unset hash never applies.
fn extended_tx_size(tx: &Tx) -> usize {
    let mut size = tx.standard_bytes().len() + 6;
    for i in &tx.inputs {
        size += 8;
        size += varint_len(i.prev_script.len() as u64) + i.prev_script.len();
    }
    size
}

/// Build the single Aerospike record's bins for `tx` (the coinbase), byte-for-bin
/// faithful to `Store.GetBinsToStore` + `splitIntoBatches` for the one-record
/// (no-pagination) case. `txid` is passed in so the caller computes it once.
/// `created_at` is the UnixMilli value for the `createdAt` bin (injected so the
/// builder is deterministic for unit tests).
#[allow(clippy::too_many_arguments)]
fn build_create_bins(
    tx: &Tx,
    txid: &Hash,
    block_height: u32,
    mined: Option<&MinedBlockInfo>,
    locked: bool,
    coinbase_maturity: u32,
    created_at: i64,
) -> Vec<Bin> {
    let is_coinbase = is_coinbase_tx(tx);

    let size = tx.standard_bytes().len() as i64;
    let extended_size = extended_tx_size(tx) as i64;

    // fee: 0 for coinbase (Go GetFeesAndUtxoHashes short-circuits); otherwise
    // sum(prevSatoshis) - sum(outputSatoshis).
    let fee: i64 = if is_coinbase {
        0
    } else {
        let ins: u64 = tx.inputs.iter().map(|i| i.prev_satoshis).sum();
        let outs: u64 = tx.outputs.iter().map(|o| o.satoshis).sum();
        ins.saturating_sub(outs) as i64
    };

    // `utxos`: one entry per output — a 32-byte hash blob when stored, else Nil.
    let mut utxos: Vec<Value> = Vec::with_capacity(tx.outputs.len());
    let mut record_utxos: i64 = 0;
    for (i, o) in tx.outputs.iter().enumerate() {
        if should_store_output_as_utxo(o) {
            let h = utxo_hash_from_output(txid, i as u32, &o.locking_script, o.satoshis);
            utxos.push(Value::Blob(h.to_vec()));
            record_utxos += 1;
        } else {
            utxos.push(Value::Nil);
        }
    }
    let total_utxos = tx.outputs.len() as i64;

    let outputs_bin: Vec<Value> = tx
        .outputs
        .iter()
        .map(|o| Value::Blob(serialize_output(o)))
        .collect();
    let inputs_bin: Vec<Value> = tx
        .inputs
        .iter()
        .map(|i| Value::Blob(serialize_input_extended(i)))
        .collect();

    let mut bins: Vec<Bin> = vec![
        Bin::new("txID".into(), Value::Blob(txid.to_vec())),
        Bin::new("version".into(), Value::Int(i64::from(tx.version))),
        Bin::new("locktime".into(), Value::Int(i64::from(tx.locktime))),
        Bin::new("fee".into(), Value::Int(fee)),
        Bin::new("sizeInBytes".into(), Value::Int(size)),
        Bin::new("extendedSize".into(), Value::Int(extended_size)),
        Bin::new("spentUtxos".into(), Value::Int(0)),
        Bin::new("isCoinbase".into(), Value::Bool(is_coinbase)),
    ];

    if is_coinbase {
        bins.push(Bin::new(
            "spendingHeight".into(),
            Value::Int(i64::from(block_height + coinbase_maturity)),
        ));
    }

    bins.push(Bin::new("conflicting".into(), Value::Bool(false)));
    bins.push(Bin::new("locked".into(), Value::Bool(locked)));

    // splitIntoBatches: utxos + recordUtxos on the (single) record.
    bins.push(Bin::new("utxos".into(), Value::List(utxos)));
    bins.push(Bin::new("recordUtxos".into(), Value::Int(record_utxos)));

    // batch[0] additions.
    bins.push(Bin::new("totalExtraRecs".into(), Value::Int(0)));

    let (block_ids, block_heights, subtree_idxs): (Vec<Value>, Vec<Value>, Vec<Value>) =
        match mined {
            Some(m) => (
                vec![Value::Int(i64::from(m.block_id))],
                vec![Value::Int(i64::from(m.block_height))],
                vec![Value::Int(i64::from(m.subtree_idx))],
            ),
            None => (Vec::new(), Vec::new(), Vec::new()),
        };
    let mined_present = !block_ids.is_empty();
    bins.push(Bin::new("blockIDs".into(), Value::List(block_ids)));
    bins.push(Bin::new("blockHeights".into(), Value::List(block_heights)));
    bins.push(Bin::new("subtreeIdxs".into(), Value::List(subtree_idxs)));
    bins.push(Bin::new("totalUtxos".into(), Value::Int(total_utxos)));

    // UnminedSince only when no mined-block info (Go: len(blockIDs)==0 && ...).
    if !mined_present {
        bins.push(Bin::new(
            "unminedSince".into(),
            Value::Int(i64::from(block_height)),
        ));
    }

    bins.push(Bin::new("createdAt".into(), Value::Int(created_at)));

    // Non-external (single small record): store inputs + outputs inline.
    bins.push(Bin::new("inputs".into(), Value::List(inputs_bin)));
    bins.push(Bin::new("outputs".into(), Value::List(outputs_bin)));

    bins
}

/// Read a numeric bin as u64. Aerospike stores all integers as Value::Int(i64).
fn bin_u64(rec: &Record, name: &str) -> u64 {
    match rec.bins.get(name) {
        Some(Value::Int(i)) => *i as u64,
        _ => 0,
    }
}

/// Read a numeric bin as i64 (e.g. `createdAt`, a UnixMilli timestamp).
fn bin_i64(rec: &Record, name: &str) -> i64 {
    match rec.bins.get(name) {
        Some(Value::Int(i)) => *i,
        _ => 0,
    }
}

/// Parse a UDF response value (map with `blockIDs` list, or a bare list) into u32s.
fn parse_block_ids(v: &Value) -> Vec<u32> {
    fn list_to_u32(items: &[Value]) -> Vec<u32> {
        items
            .iter()
            .filter_map(|x| match x {
                Value::Int(i) => Some(*i as u32),
                _ => None,
            })
            .collect()
    }

    fn find_block_ids<'a, I>(entries: I) -> Vec<u32>
    where
        I: Iterator<Item = (&'a Value, &'a Value)>,
    {
        entries
            .into_iter()
            .find(|(k, _)| matches!(k, Value::String(s) if s == "blockIDs"))
            .map(|(_, val)| match val {
                Value::List(items) => list_to_u32(items),
                _ => vec![],
            })
            .unwrap_or_default()
    }

    match v {
        Value::List(items) => list_to_u32(items),
        Value::HashMap(m) => find_block_ids(m.iter()),
        // The aerospike crate may decode a server-side K-ordered map as
        // OrderedMap (BTreeMap) rather than HashMap. Some UDF responses return
        // the result map in this form, so parse it the same way (defensive).
        Value::OrderedMap(m) => find_block_ids(m.iter()),
        _ => vec![],
    }
}

/// Returns the subset of `tx_hashes` that are NOT the coinbase placeholder.
/// Extracted so the placeholder-skip logic can be unit-tested without a live
/// Aerospike connection (mirrors the filter applied in `set_conflicting`).
fn filter_coinbase_placeholder(tx_hashes: &[Hash]) -> Vec<Hash> {
    tx_hashes
        .iter()
        .copied()
        .filter(|h| *h != COINBASE_PLACEHOLDER)
        .collect()
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, HashMap};

    use aerospike::Value;
    use ba_subtree_bench::hash::Hash;
    use ba_subtree_bench::subtree::COINBASE_PLACEHOLDER;

    use super::{filter_coinbase_placeholder, parse_block_ids};

    #[test]
    fn parses_bare_list() {
        let v = Value::List(vec![Value::Int(1), Value::Int(2), Value::Int(3)]);
        assert_eq!(parse_block_ids(&v), vec![1, 2, 3]);
    }

    use std::collections::HashMap as StdHashMap;

    use ba_subtree_bench::tx::Tx;

    use super::{build_create_bins, is_coinbase_tx, utxo_hash_from_output};
    use crate::store::MinedBlockInfo;

    fn bins_map(bins: &[aerospike::Bin]) -> StdHashMap<String, Value> {
        bins.iter().map(|b| (b.name.clone(), b.value.clone())).collect()
    }

    /// The coinbase bin-builder is byte-faithful to Go's GetBinsToStore for the
    /// single-record (no-pagination) coinbase: coinbase flags, the maturity
    /// spendingHeight, a single 32-byte utxo hash, the mined-block bins (no
    /// unminedSince), zero fee, and the txID key bin.
    #[test]
    fn build_create_bins_for_coinbase_matches_go_schema() {
        // A real coinbase (height 12, 50 BTC) to the documented default P2PKH.
        let height = 12u32;
        let value = 50_0000_0000u64;
        let cb_bytes = crate::coinbase::create_coinbase(
            height,
            value,
            b"/rust-ba/",
            &[crate::config::DEFAULT_COINBASE_ADDRESS.to_string()],
            [0u8; 12],
        )
        .expect("build coinbase");
        let tx = Tx::from_bytes(&cb_bytes).expect("parse coinbase");
        assert!(is_coinbase_tx(&tx), "must detect coinbase");
        assert_eq!(tx.outputs.len(), 1, "single payout output");

        let txid = tx.txid();
        let mined = MinedBlockInfo {
            block_id: 7,
            block_height: height,
            subtree_idx: 0,
            on_longest_chain: true,
            unset_mined: false,
        };
        let created_at = 1_700_000_000_000i64;
        let bins = build_create_bins(&tx, &txid, height, Some(&mined), false, 100, created_at);
        let m = bins_map(&bins);

        // Key/identity bins.
        assert_eq!(m.get("txID"), Some(&Value::Blob(txid.to_vec())));
        assert_eq!(m.get("version"), Some(&Value::Int(i64::from(tx.version))));
        assert_eq!(m.get("locktime"), Some(&Value::Int(0)));
        assert_eq!(m.get("fee"), Some(&Value::Int(0)), "coinbase fee is 0");
        assert_eq!(m.get("spentUtxos"), Some(&Value::Int(0)));
        assert_eq!(m.get("isCoinbase"), Some(&Value::Bool(true)));
        assert_eq!(m.get("conflicting"), Some(&Value::Bool(false)));
        assert_eq!(m.get("locked"), Some(&Value::Bool(false)));

        // Sizes match the parsed tx's standard serialization.
        assert_eq!(
            m.get("sizeInBytes"),
            Some(&Value::Int(tx.standard_bytes().len() as i64))
        );

        // Coinbase maturity (height + 100).
        assert_eq!(
            m.get("spendingHeight"),
            Some(&Value::Int(i64::from(height + 100)))
        );

        // The single output's UTXO hash, computed independently.
        let expected_hash = utxo_hash_from_output(
            &txid,
            0,
            &tx.outputs[0].locking_script,
            tx.outputs[0].satoshis,
        );
        assert_eq!(
            m.get("utxos"),
            Some(&Value::List(vec![Value::Blob(expected_hash.to_vec())]))
        );
        assert_eq!(m.get("recordUtxos"), Some(&Value::Int(1)));
        assert_eq!(m.get("totalUtxos"), Some(&Value::Int(1)));
        assert_eq!(m.get("totalExtraRecs"), Some(&Value::Int(0)));

        // Mined-block bins present, single-element; unminedSince ABSENT.
        assert_eq!(m.get("blockIDs"), Some(&Value::List(vec![Value::Int(7)])));
        assert_eq!(
            m.get("blockHeights"),
            Some(&Value::List(vec![Value::Int(i64::from(height))]))
        );
        assert_eq!(m.get("subtreeIdxs"), Some(&Value::List(vec![Value::Int(0)])));
        assert!(
            !m.contains_key("unminedSince"),
            "mined coinbase must NOT carry unminedSince"
        );
        assert_eq!(m.get("createdAt"), Some(&Value::Int(created_at)));

        // Inputs/outputs stored inline (non-external single record).
        match m.get("inputs") {
            Some(Value::List(v)) => assert_eq!(v.len(), 1),
            other => panic!("inputs bin not a 1-elem list: {other:?}"),
        }
        match m.get("outputs") {
            Some(Value::List(v)) => assert_eq!(v.len(), 1),
            other => panic!("outputs bin not a 1-elem list: {other:?}"),
        }
    }

    /// With no mined-block info, the record is flagged unmined (unminedSince set,
    /// blockIDs empty) — the non-coinbase create-without-mining shape.
    #[test]
    fn build_create_bins_without_mined_sets_unmined_since() {
        let cb_bytes = crate::coinbase::create_coinbase(
            5,
            25_0000_0000,
            b"x",
            &[crate::config::DEFAULT_COINBASE_ADDRESS.to_string()],
            [0u8; 12],
        )
        .unwrap();
        let tx = Tx::from_bytes(&cb_bytes).unwrap();
        let txid = tx.txid();
        let bins = build_create_bins(&tx, &txid, 5, None, false, 100, 0);
        let m = bins_map(&bins);
        assert_eq!(m.get("blockIDs"), Some(&Value::List(vec![])));
        assert_eq!(m.get("unminedSince"), Some(&Value::Int(5)));
    }

    #[test]
    fn parses_hashmap_block_ids() {
        let mut m = HashMap::new();
        m.insert(
            Value::String("blockIDs".into()),
            Value::List(vec![Value::Int(7), Value::Int(9)]),
        );
        m.insert(Value::String("status".into()), Value::Int(0));
        assert_eq!(parse_block_ids(&Value::HashMap(m)), vec![7, 9]);
    }

    #[test]
    fn parses_ordered_map_block_ids() {
        // Same shape as the HashMap arm, but as an OrderedMap (BTreeMap) — the
        // form the crate may use for K-ordered server maps.
        let mut m = BTreeMap::new();
        m.insert(
            Value::String("blockIDs".into()),
            Value::List(vec![Value::Int(42)]),
        );
        m.insert(Value::String("status".into()), Value::Int(0));
        assert_eq!(parse_block_ids(&Value::OrderedMap(m)), vec![42]);
    }

    #[test]
    fn missing_block_ids_key_is_empty() {
        let mut m = BTreeMap::new();
        m.insert(Value::String("status".into()), Value::Int(0));
        assert!(parse_block_ids(&Value::OrderedMap(m)).is_empty());
    }

    // I1: coinbase-placeholder skip tests — no Aerospike connection required.

    #[test]
    fn placeholder_only_input_is_fully_skipped() {
        // A batch containing only the coinbase placeholder must yield an empty
        // effective list (no UDF calls, no reads). Mirrors Go conflicting.go:52-55.
        let result = filter_coinbase_placeholder(&[COINBASE_PLACEHOLDER]);
        assert!(
            result.is_empty(),
            "placeholder-only batch must yield empty effective list"
        );
    }

    #[test]
    fn placeholder_mixed_with_real_hashes_is_stripped() {
        let real_a: [u8; 32] = [0xAA; 32];
        let real_b: [u8; 32] = [0xBB; 32];
        let input = [real_a, COINBASE_PLACEHOLDER, real_b];

        let result = filter_coinbase_placeholder(&input);

        assert_eq!(result, vec![real_a, real_b], "only placeholder removed");
        assert!(
            !result.contains(&COINBASE_PLACEHOLDER),
            "placeholder must not appear in output"
        );
    }

    #[test]
    fn no_placeholder_passes_through_unchanged() {
        let real_a: [u8; 32] = [0x01; 32];
        let real_b: [u8; 32] = [0x02; 32];
        let input = [real_a, real_b];

        let result = filter_coinbase_placeholder(&input);

        assert_eq!(result, vec![real_a, real_b]);
    }

    #[test]
    fn empty_input_returns_empty() {
        let result = filter_coinbase_placeholder(&[]);
        assert!(result.is_empty());
    }

    // db2 raw getters — rig-deferred. These require a live Aerospike with a
    // seeded record; they cannot be proven offline. Marked `#[ignore]` so the
    // suite stays green without a rig, and carry a VERIFY-ON-RIG contract.

    #[tokio::test]
    #[ignore = "VERIFY-ON-RIG: requires live Aerospike + seeded tx record"]
    async fn rig_get_tx_meta_reads_tx_conflicting_blockids() {
        // VERIFY-ON-RIG: seed a record with the `tx` (extended blob),
        // `conflicting` (bool) and `blockIDs` (list) bins, then assert
        // get_tx_meta returns those exact values; assert a missing key -> None.
        // Decode shape verified against stores/utxo/fields/fields.go:14-39 and
        // get.go (conflicting bool).
    }

    #[tokio::test]
    #[ignore = "VERIFY-ON-RIG: requires live Aerospike + seeded utxos bin"]
    async fn rig_get_spending_datas_decodes_68_byte_entries() {
        // VERIFY-ON-RIG: seed the `utxos` bin with a mix of 32-byte (unspent)
        // and 68-byte (spent: utxoHash[32]‖spendTxID[32]‖vin[4LE]) blobs, then
        // assert get_spending_datas yields None / Some(SpendingData) per index.
        // Decode shape verified against get.go:219-228.
    }

    #[tokio::test]
    #[ignore = "VERIFY-ON-RIG: requires live Aerospike + seeded conflictingCs bin"]
    async fn rig_get_conflicting_children_bin_decodes_list_of_hashes() {
        // VERIFY-ON-RIG: seed the `conflictingCs` bin (note: 15-char bin-name
        // limit, fields.go:41) with a list of 32-byte child txid blobs, then
        // assert get_conflicting_children_bin returns them. Decode shape verified
        // against get.go:1111-1127 (processConflictingChildren).
    }

    // db2 ProcessConflicting forward cascade — rig-deferred. Exercises the full
    // 5-phase double-spend cascade against a live Aerospike, asserting the real
    // bins end in the displaced state. Cannot be proven offline (the Mem fake
    // covers the logic in process_conflicting.rs); this documents the on-rig
    // contract. Connection params come from AERO_* env vars (set by the rig).

    /// Minimal EXTENDED tx spending `(prev, vout)` with a 1-byte output script
    /// `out_script` (distinguishes the loser/winner bodies → distinct txids).
    /// Same byte shape as the offline Mem cascade tests.
    fn rig_ext_tx_out(prev: Hash, vout: u32, out_script: u8) -> Vec<u8> {
        let mut b = Vec::new();
        b.extend_from_slice(&2u32.to_le_bytes()); // version
        b.extend_from_slice(&[0, 0, 0, 0, 0, 0xEF]); // ext marker
        b.push(1); // input count
        b.extend_from_slice(&prev);
        b.extend_from_slice(&vout.to_le_bytes());
        b.push(0); // empty scriptSig
        b.extend_from_slice(&0xFFFF_FFFFu32.to_le_bytes()); // sequence
        b.extend_from_slice(&1000u64.to_le_bytes()); // prev satoshis
        b.push(3);
        b.extend_from_slice(&[0x76, 0xa9, 0x88]); // prev script
        b.push(1); // output count
        b.extend_from_slice(&500u64.to_le_bytes()); // satoshis
        b.push(1);
        b.push(out_script); // locking script
        b.extend_from_slice(&0u32.to_le_bytes()); // locktime
        b
    }

    #[tokio::test]
    #[ignore = "VERIFY-ON-RIG: requires live Aerospike rig — seeded parent+txa+txb"]
    async fn rig_process_conflicting_cascade_displaces_loser() {
        use ba_subtree_bench::tx::Tx;

        use super::super::{SpendingData, UtxoStore};
        use super::AeroUtxoStore;

        // Connection params from the rig's env (mirrors how main.rs would wire a
        // real AeroUtxoStore). On the rig these MUST be set to the seeded cluster.
        let hosts = std::env::var("AERO_HOSTS").unwrap_or_else(|_| "127.0.0.1:3000".into());
        let namespace = std::env::var("AERO_NAMESPACE").unwrap_or_else(|_| "test".into());
        let set = std::env::var("AERO_SET").unwrap_or_else(|_| "utxo".into());
        let udf = std::env::var("AERO_UDF_MODULE").unwrap_or_else(|_| "teranode".into());

        let store = AeroUtxoStore::connect(&hosts, &namespace, &set, &udf, 100)
            .await
            .expect("connect to the rig Aerospike cluster");
        store.set_block_height(100);

        // Build a REAL double-spend: parent P[0] spent by the valid loser txa;
        // winner txb (distinct body → distinct txid) also spends (P,0) and is
        // pre-flagged Conflicting=true by the caller. Both txids are canonical.
        let parent: Hash = [0x50; 32];
        let txa_body = rig_ext_tx_out(parent, 0, 0x6a);
        let txb_body = rig_ext_tx_out(parent, 0, 0x51);
        let txa = Tx::from_bytes(&txa_body).unwrap().txid();
        let txb = Tx::from_bytes(&txb_body).unwrap().txid();
        assert_ne!(txa, txb, "double-spend bodies must have distinct txids");

        // VERIFY-ON-RIG: seed the cluster so the pre-call reality holds:
        //   - parent P record with output 0 spent by txa (utxos[0] = txa,0),
        //   - txa record: tx=txa_body, conflicting=false, spends (P,0),
        //   - txb record: tx=txb_body, conflicting=true (caller pre-marked),
        //     spends (P,0).
        // The rig harness performs this seeding (Aerospike puts + UDF spend of
        // (P,0) by txa); there is no offline seeder for the real store.

        // Run the forward cascade against the real bins.
        crate::process_conflicting::process_conflicting(
            &store,
            100,
            &[txb],
            &std::collections::HashMap::new(),
        )
        .await
        .expect("forward cascade must succeed against the rig");

        // Assert the real bins ended in the displaced state:
        //   - winner txb cleared (Conflicting=false) and now spends P[0],
        //   - loser txa demoted (Conflicting=true),
        //   - parent P[0] spender == txb.
        let txb_meta = store
            .get_tx_meta(&txb)
            .await
            .unwrap()
            .expect("winner txb record must exist");
        assert!(
            !txb_meta.conflicting,
            "winner txb must end Conflicting=false (phase 4 cleared it)"
        );

        let txa_meta = store
            .get_tx_meta(&txa)
            .await
            .unwrap()
            .expect("loser txa record must exist");
        assert!(
            txa_meta.conflicting,
            "loser txa must end Conflicting=true (phase 1 marked it)"
        );

        let p = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(
            p[0],
            Some(SpendingData { tx_id: txb, vin: 0 }),
            "parent P[0] must now be spent by the winner txb"
        );
    }

    // db3 ReverseProcessConflicting reverse cascade — rig-deferred. Exercises the
    // live moveBack reverse against a real Aerospike: a previously-applied db2
    // swap is undone — the demoted winner D is re-marked Conflicting=true, its
    // inputs unspent, and the original counter C is restored as the canonical
    // spender (Conflicting=false). Cannot be proven offline (the Mem fake covers
    // the logic in process_conflicting.rs); this documents the on-rig contract.
    // Connection params come from AERO_* env vars (set by the rig).

    #[tokio::test]
    #[ignore = "VERIFY-ON-RIG: requires live Aerospike rig — seeded parent+D+C"]
    async fn rig_reverse_process_conflicting_restores_counter() {
        use ba_subtree_bench::tx::Tx;

        use super::super::{SpendingData, UtxoStore};
        use super::AeroUtxoStore;

        // Connection params from the rig's env (mirrors how main.rs would wire a
        // real AeroUtxoStore). On the rig these MUST be set to the seeded cluster.
        let hosts = std::env::var("AERO_HOSTS").unwrap_or_else(|_| "127.0.0.1:3000".into());
        let namespace = std::env::var("AERO_NAMESPACE").unwrap_or_else(|_| "test".into());
        let set = std::env::var("AERO_SET").unwrap_or_else(|_| "utxo".into());
        let udf = std::env::var("AERO_UDF_MODULE").unwrap_or_else(|_| "teranode".into());

        let store = AeroUtxoStore::connect(&hosts, &namespace, &set, &udf, 100)
            .await
            .expect("connect to the rig Aerospike cluster");
        store.set_block_height(100);

        // Build the db2 END-STATE that the reverse must undo: parent P[0] is
        // currently spent by the demoted winner D (Conflicting=false); the
        // original loser C (distinct body → distinct txid) is the counter — it is
        // listed in P.conflictingCs, is Conflicting=true, and spends (P,0). Both
        // txids are canonical.
        let parent: Hash = [0x50; 32];
        let d_body = rig_ext_tx_out(parent, 0, 0x51);
        let c_body = rig_ext_tx_out(parent, 0, 0x52);
        let d = Tx::from_bytes(&d_body).unwrap().txid();
        let c = Tx::from_bytes(&c_body).unwrap().txid();
        assert_ne!(d, c, "winner/counter bodies must have distinct txids");

        // VERIFY-ON-RIG: seed the cluster so the pre-call (post-db2) reality holds:
        //   - parent P record with output 0 spent by D (utxos[0] = D,0),
        //   - D record: tx=d_body, conflicting=false, spends (P,0),
        //   - C record: tx=c_body, conflicting=true, spends (P,0),
        //   - P.conflictingCs list contains C.
        // The rig harness performs this seeding (Aerospike puts + UDF spend of
        // (P,0) by D + conflictingCs append of C); there is no offline seeder for
        // the real store.

        // Run the reverse cascade against the real bins. D is the demoted winner
        // from the block being moved back.
        crate::process_conflicting::reverse_process_conflicting(&store, 100, &[d])
            .await
            .expect("reverse cascade must succeed against the rig");

        // Assert the real bins ended in the restored state:
        //   - demoted winner D re-marked Conflicting=true,
        //   - counter C restored as the canonical spender of P[0],
        //   - counter C cleared (Conflicting=false).
        let d_meta = store
            .get_tx_meta(&d)
            .await
            .unwrap()
            .expect("demoted winner D record must exist");
        assert!(
            d_meta.conflicting,
            "demoted winner D must end Conflicting=true (re-marked by reverse)"
        );

        let p = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(
            p[0],
            Some(SpendingData { tx_id: c, vin: 0 }),
            "parent P[0] must be restored to the counter C"
        );

        let c_meta = store
            .get_tx_meta(&c)
            .await
            .unwrap()
            .expect("counter C record must exist");
        assert!(
            !c_meta.conflicting,
            "counter C must end Conflicting=false (un-cascaded by reverse)"
        );
    }

    // db4 conflicting REORG cascade — rig-deferred. Exercises the UTXO-layer two
    // phases that `reconcile_reorg` (chain_grpc.rs) drives, against a live
    // Aerospike: a moveBack block whose subtree carries `conflicting_nodes` runs
    // `reverse_process_conflicting`, threading its restored/demoted hashes into a
    // shared `processed` map; a moveForward block whose subtree carries its own
    // `conflicting_nodes` then runs `process_conflicting` with that same map so
    // already-reversed hashes are NOT re-cascaded — exactly the processed-map
    // handoff in reconcile_reorg (chain_grpc.rs:553-603). Asserts the real bins
    // end reconciled: moveBack restores its counter, moveForward displaces its
    // loser.
    //
    // LIMITATION: full `reconcile_reorg` rig coverage (chain client + blob store
    // returning subtrees with conflicting_nodes + assembly `register_conflicting`
    // eviction) cannot be assembled against a bare `AeroUtxoStore` — `reconcile_
    // reorg` takes a `ChainClient`/blob/`AssemblyState`. This rig test therefore
    // proves the cascade PRIMITIVES + the processed-map handoff on the real store
    // (the only part that touches Aerospike); the chain/blob/assembly plumbing is
    // covered offline by the chain_grpc.rs Mem-store reorg-with-conflicts test.
    #[tokio::test]
    #[ignore = "VERIFY-ON-RIG: requires live Aerospike rig — seeded moveBack(P,D,C) + moveForward(Q,txa,txb)"]
    async fn rig_conflicting_reorg_reverse_then_forward_reconciles() {
        use std::collections::HashMap;

        use ba_subtree_bench::tx::Tx;

        use super::super::{SpendingData, UtxoStore};
        use super::AeroUtxoStore;

        // Connection params from the rig's env (mirrors how main.rs would wire a
        // real AeroUtxoStore). On the rig these MUST be set to the seeded cluster.
        let hosts = std::env::var("AERO_HOSTS").unwrap_or_else(|_| "127.0.0.1:3000".into());
        let namespace = std::env::var("AERO_NAMESPACE").unwrap_or_else(|_| "test".into());
        let set = std::env::var("AERO_SET").unwrap_or_else(|_| "utxo".into());
        let udf = std::env::var("AERO_UDF_MODULE").unwrap_or_else(|_| "teranode".into());

        let store = AeroUtxoStore::connect(&hosts, &namespace, &set, &udf, 100)
            .await
            .expect("connect to the rig Aerospike cluster");
        store.set_block_height(100);

        // --- moveBack block: parent P[0] currently spent by demoted winner D
        // (Conflicting=false); counter C is in P.conflictingCs, Conflicting=true,
        // spends (P,0). The orphaned block's subtree carried conflicting_nodes=[D].
        let parent_back: Hash = [0x50; 32];
        let d_body = rig_ext_tx_out(parent_back, 0, 0x51);
        let c_body = rig_ext_tx_out(parent_back, 0, 0x52);
        let d = Tx::from_bytes(&d_body).unwrap().txid();
        let c = Tx::from_bytes(&c_body).unwrap().txid();
        assert_ne!(d, c, "moveBack winner/counter must have distinct txids");

        // --- moveForward block: parent Q[0] spent by loser txa (Conflicting=false);
        // winner txb pre-flagged Conflicting=true also spends (Q,0). The adopted
        // block's subtree carries conflicting_nodes=[txb].
        let parent_fwd: Hash = [0x60; 32];
        let txa_body = rig_ext_tx_out(parent_fwd, 0, 0x6a);
        let txb_body = rig_ext_tx_out(parent_fwd, 0, 0x51);
        let txa = Tx::from_bytes(&txa_body).unwrap().txid();
        let txb = Tx::from_bytes(&txb_body).unwrap().txid();
        assert_ne!(
            txa, txb,
            "moveForward loser/winner must have distinct txids"
        );

        // VERIFY-ON-RIG: seed the cluster so the pre-call reality holds:
        //   moveBack (post-db2 end-state to undo):
        //     - P record: output 0 spent by D (utxos[0] = D,0),
        //     - D record: tx=d_body, conflicting=false, spends (P,0),
        //     - C record: tx=c_body, conflicting=true, spends (P,0),
        //     - P.conflictingCs list contains C.
        //   moveForward (pre-cascade double-spend):
        //     - Q record: output 0 spent by txa (utxos[0] = txa,0),
        //     - txa record: tx=txa_body, conflicting=false, spends (Q,0),
        //     - txb record: tx=txb_body, conflicting=true, spends (Q,0).
        // The rig harness performs this seeding; there is no offline seeder for
        // the real store.

        // Shared processed map — the exact handoff reconcile_reorg threads from
        // the moveBack reverse pass into the moveForward forward pass.
        let mut processed: HashMap<Hash, bool> = HashMap::new();

        // Phase 1 (moveBack): reverse the orphaned block's conflict resolution.
        // conflicting_nodes=[D]; D is not in any moveForwardTxSet here.
        let (cascaded, touched) =
            crate::process_conflicting::reverse_process_conflicting(&store, 100, &[d])
                .await
                .expect("moveBack reverse cascade must succeed against the rig");
        for h in [d].into_iter().chain(touched.into_iter()) {
            processed.insert(h, true);
        }
        let _ = cascaded;

        // Phase 2 (moveForward): forward cascade for the adopted block, threading
        // the SAME processed map (so the reversed moveBack hashes are skipped).
        crate::process_conflicting::process_conflicting(&store, 100, &[txb], &processed)
            .await
            .expect("moveForward forward cascade must succeed against the rig");

        // Assert moveBack restored its counter: P[0] back to C, D re-Conflicting,
        // C cleared.
        let d_meta = store
            .get_tx_meta(&d)
            .await
            .unwrap()
            .expect("moveBack demoted winner D must exist");
        assert!(
            d_meta.conflicting,
            "moveBack: D must end Conflicting=true (re-marked by reverse)"
        );
        let p_back = store.get_spending_datas(&parent_back).await.unwrap();
        assert_eq!(
            p_back[0],
            Some(SpendingData { tx_id: c, vin: 0 }),
            "moveBack: P[0] must be restored to counter C"
        );

        // Assert moveForward displaced its loser: Q[0] now spent by txb, txb
        // cleared (Conflicting=false), loser txa demoted (Conflicting=true).
        let txb_meta = store
            .get_tx_meta(&txb)
            .await
            .unwrap()
            .expect("moveForward winner txb must exist");
        assert!(
            !txb_meta.conflicting,
            "moveForward: txb must end Conflicting=false (cascade cleared it)"
        );
        let txa_meta = store
            .get_tx_meta(&txa)
            .await
            .unwrap()
            .expect("moveForward loser txa must exist");
        assert!(
            txa_meta.conflicting,
            "moveForward: txa must end Conflicting=true (cascade demoted it)"
        );
        let p_fwd = store.get_spending_datas(&parent_fwd).await.unwrap();
        assert_eq!(
            p_fwd[0],
            Some(SpendingData { tx_id: txb, vin: 0 }),
            "moveForward: Q[0] must now be spent by winner txb"
        );
    }
}

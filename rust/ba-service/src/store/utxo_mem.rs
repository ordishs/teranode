//! In-memory UtxoStore for unit tests and standalone runs.
use std::collections::HashMap;
use std::sync::Mutex;

use ba_subtree_bench::hash::Hash;
use tonic::async_trait;

use super::{
    IgnoreFlags, MinedBlockInfo, Spend, SpendingData, StoreError, TxMeta, UnminedTx, UtxoStore,
};

/// One recorded `create` invocation: `(txid, block_height, mined_info, locked)`.
type CreateCall = (Hash, u32, Option<MinedBlockInfo>, bool);

/// Stateful per-tx record modelling the observable Aerospike bins db2 reads.
/// `spending_datas` mirrors the `utxos` bin (index = vout, `None` = unspent);
/// `conflicting_children` mirrors the `conflictingCs` bin.
#[derive(Default, Clone)]
struct MemTxRecord {
    tx_bytes: Vec<u8>,
    conflicting: bool,
    block_ids: Vec<u32>,
    created_at: i64,
    spending_datas: Vec<Option<SpendingData>>,
    conflicting_children: Vec<Hash>,
}

#[derive(Default)]
pub struct MemUtxoStore {
    unmined: Vec<UnminedTx>,
    mined: Mutex<HashMap<Hash, Vec<u32>>>,
    /// Records set_locked calls so tests can assert the unlock path runs.
    locked_calls: Mutex<Vec<(Vec<Hash>, bool)>>,
    /// Records mark_on_longest_chain calls so the reorg test can assert the
    /// winners (on=true) / returned losers (on=false) marks.
    mark_calls: Mutex<Vec<(Vec<Hash>, bool)>>,
    /// Static conflict graph used to drive `set_conflicting` deterministically
    /// in BFS unit tests: maps each tx hash to the txids that spend its outputs
    /// (its "spending children"). `mark_conflicting_recursively` walks this.
    conflict_children: Mutex<HashMap<Hash, Vec<Hash>>>,
    /// Static parent-spends graph: per tx hash, the affected parent `Spend`s
    /// (one per input) that `set_conflicting` returns. Lets the BFS test assert
    /// the accumulated affected-spends list.
    conflict_parents: Mutex<HashMap<Hash, Vec<Spend>>>,
    /// Records `set_conflicting(hashes, value)` calls in order so tests can
    /// assert the exact BFS batching and per-level direction (true/false).
    conflicting_calls: Mutex<Vec<(Vec<Hash>, bool)>>,
    /// Records `spend` calls so tests can assert the spend path runs.
    spend_calls: Mutex<Vec<(Vec<Spend>, u32, IgnoreFlags)>>,
    /// Records `unspend` calls so tests can assert the unspend path runs.
    unspend_calls: Mutex<Vec<(Vec<Spend>, bool)>>,
    /// Stateful per-tx records modelling the real store's bins (db2). Empty for
    /// the db1 spy tests, which never seed it — so the flag-flip / spend-mutate
    /// extensions below are no-ops for them and they stay green.
    records: Mutex<HashMap<Hash, MemTxRecord>>,
    /// When set, the next `spend` call clears the flag and returns an error
    /// (drives the rollback path in db2 tests).
    fail_next_spend: Mutex<bool>,
    /// When set, a `set_locked(.., false)` call returns an error (drives the
    /// step-5 failure path in db2 tests).
    fail_set_locked_false: Mutex<bool>,
    /// Records `create` calls `(txid, block_height, mined, locked)` so tests can
    /// assert the coinbase UTXO write happens on extend / reorg moveForward.
    create_calls: Mutex<Vec<CreateCall>>,
}

impl MemUtxoStore {
    pub fn with_unmined(unmined: Vec<UnminedTx>) -> Self {
        Self {
            unmined,
            ..Self::default()
        }
    }

    /// Register the spending children of `tx` for the BFS conflict-cascade
    /// tests. `set_conflicting(tx, ..)` will report `children` as the next BFS
    /// level (its `spending_child_tx_hashes`).
    pub fn set_conflict_children(&self, tx: Hash, children: Vec<Hash>) {
        self.conflict_children.lock().unwrap().insert(tx, children);
    }

    /// Register the affected parent spends `set_conflicting(tx, ..)` returns
    /// (one per input of `tx`).
    pub fn set_conflict_parents(&self, tx: Hash, parents: Vec<Spend>) {
        self.conflict_parents.lock().unwrap().insert(tx, parents);
    }

    /// All recorded `set_conflicting` invocations (hashes, value), in BFS order.
    pub fn conflicting_calls(&self) -> Vec<(Vec<Hash>, bool)> {
        self.conflicting_calls.lock().unwrap().clone()
    }

    /// All recorded `spend` invocations, in call order.
    pub fn spend_calls(&self) -> Vec<(Vec<Spend>, u32, IgnoreFlags)> {
        self.spend_calls.lock().unwrap().clone()
    }

    /// All recorded `unspend` invocations, in call order.
    pub fn unspend_calls(&self) -> Vec<(Vec<Spend>, bool)> {
        self.unspend_calls.lock().unwrap().clone()
    }

    /// All recorded `set_locked` invocations (hashes, locked-flag), in call order.
    pub fn locked_calls(&self) -> Vec<(Vec<Hash>, bool)> {
        self.locked_calls.lock().unwrap().clone()
    }

    /// All recorded `create` invocations `(txid, block_height, mined, locked)`,
    /// in call order. Used to assert coinbase UTXO creation on extend/reorg.
    pub fn create_calls(&self) -> Vec<CreateCall> {
        self.create_calls.lock().unwrap().clone()
    }

    /// All recorded `mark_on_longest_chain` invocations (hashes, on-flag), in
    /// call order. Used by the reorg reconciliation test.
    pub fn mark_calls(&self) -> Vec<(Vec<Hash>, bool)> {
        self.mark_calls.lock().unwrap().clone()
    }

    /// Seed a tx's metadata record (the `tx`/`conflicting`/`blockIDs`/`createdAt`
    /// bins). Preserves any already-seeded `spending_datas`/`conflicting_children`.
    pub fn seed_tx(
        &self,
        hash: Hash,
        tx_bytes: Vec<u8>,
        conflicting: bool,
        block_ids: Vec<u32>,
        created_at: i64,
    ) {
        let mut recs = self.records.lock().unwrap();
        let entry = recs.entry(hash).or_default();
        entry.tx_bytes = tx_bytes;
        entry.conflicting = conflicting;
        entry.block_ids = block_ids;
        entry.created_at = created_at;
    }

    /// Seed a tx's per-output spender list (the `utxos` bin). Index = vout.
    pub fn seed_spending_datas(&self, hash: Hash, spending_datas: Vec<Option<SpendingData>>) {
        let mut recs = self.records.lock().unwrap();
        recs.entry(hash).or_default().spending_datas = spending_datas;
    }

    /// Seed a tx's `conflictingCs` bin (one level of spending children).
    pub fn seed_conflicting_children(&self, hash: Hash, children: Vec<Hash>) {
        let mut recs = self.records.lock().unwrap();
        recs.entry(hash).or_default().conflicting_children = children;
    }

    /// Arm/disarm a one-shot injected failure on the next `spend` call.
    pub fn set_fail_next_spend(&self, fail: bool) {
        *self.fail_next_spend.lock().unwrap() = fail;
    }

    /// Arm/disarm an injected failure on `set_locked(.., false)`.
    pub fn set_fail_set_locked_false(&self, fail: bool) {
        *self.fail_set_locked_false.lock().unwrap() = fail;
    }
}

#[async_trait]
impl UtxoStore for MemUtxoStore {
    async fn unmined(&self) -> Result<Vec<UnminedTx>, StoreError> {
        Ok(self.unmined.clone())
    }

    async fn set_mined_multi(
        &self,
        hashes: &[Hash],
        info: &MinedBlockInfo,
    ) -> Result<HashMap<Hash, Vec<u32>>, StoreError> {
        let mut guard = self.mined.lock().unwrap();
        let mut out = HashMap::new();

        for &h in hashes {
            let entry = guard.entry(h).or_default();

            if info.unset_mined {
                entry.retain(|&id| id != info.block_id);
            } else if !entry.contains(&info.block_id) {
                entry.push(info.block_id);
            }

            out.insert(h, entry.clone());
        }

        Ok(out)
    }

    async fn mark_on_longest_chain(&self, hashes: &[Hash], on: bool) -> Result<(), StoreError> {
        self.mark_calls.lock().unwrap().push((hashes.to_vec(), on));
        Ok(())
    }

    async fn set_locked(&self, hashes: &[Hash], locked: bool) -> Result<(), StoreError> {
        self.locked_calls
            .lock()
            .unwrap()
            .push((hashes.to_vec(), locked));

        // db2 injected-failure: a step-5 unlock (locked==false) can be forced to
        // fail to exercise the no-rollback path. db1 tests never arm this.
        if !locked && *self.fail_set_locked_false.lock().unwrap() {
            return Err(StoreError::Backend(
                "injected set_locked(false) failure".into(),
            ));
        }

        Ok(())
    }

    async fn create(
        &self,
        tx: &ba_subtree_bench::tx::Tx,
        block_height: u32,
        mined: Option<MinedBlockInfo>,
        locked: bool,
    ) -> Result<(), StoreError> {
        let txid = tx.txid();
        self.create_calls
            .lock()
            .unwrap()
            .push((txid, block_height, mined.clone(), locked));

        // Model the created record so a subsequent get_tx_meta sees it.
        let mut records = self.records.lock().unwrap();
        let rec = records.entry(txid).or_default();
        rec.tx_bytes = tx.standard_bytes();
        rec.conflicting = false;
        rec.block_ids = mined.as_ref().map(|m| vec![m.block_id]).unwrap_or_default();
        if rec.spending_datas.is_empty() {
            rec.spending_datas = vec![None; tx.outputs.len()];
        }

        Ok(())
    }

    async fn spend(
        &self,
        spends: &[Spend],
        block_height: u32,
        ignore: IgnoreFlags,
    ) -> Result<Vec<Spend>, StoreError> {
        self.spend_calls
            .lock()
            .unwrap()
            .push((spends.to_vec(), block_height, ignore));

        // db2 injected-failure: one-shot. db1 tests never arm this.
        {
            let mut fail = self.fail_next_spend.lock().unwrap();
            if *fail {
                *fail = false;
                return Err(StoreError::Backend("injected spend failure".into()));
            }
        }

        // db2 state mutation: record the spender on the parent UTXO. A missing
        // record (db1 spy tests) is tolerated — the mutation is skipped.
        {
            let mut recs = self.records.lock().unwrap();
            for s in spends {
                if let Some(sd) = &s.spending_data {
                    if let Some(rec) = recs.get_mut(&s.tx_id) {
                        let vout = s.vout as usize;
                        if rec.spending_datas.len() <= vout {
                            rec.spending_datas.resize(vout + 1, None);
                        }
                        rec.spending_datas[vout] = Some(sd.clone());
                    }
                }
            }
        }

        Ok(spends.to_vec())
    }

    async fn unspend(&self, spends: &[Spend], flag_as_locked: bool) -> Result<(), StoreError> {
        self.unspend_calls
            .lock()
            .unwrap()
            .push((spends.to_vec(), flag_as_locked));

        // db2 state mutation: clear the spender on each parent UTXO. A missing
        // record (db1 spy tests) is tolerated — the mutation is skipped.
        {
            let mut recs = self.records.lock().unwrap();
            for s in spends {
                if let Some(rec) = recs.get_mut(&s.tx_id) {
                    let vout = s.vout as usize;
                    if vout < rec.spending_datas.len() {
                        rec.spending_datas[vout] = None;
                    }
                }
            }
        }

        Ok(())
    }

    async fn set_conflicting(
        &self,
        tx_hashes: &[Hash],
        value: bool,
    ) -> Result<(Vec<Spend>, Vec<Hash>), StoreError> {
        self.conflicting_calls
            .lock()
            .unwrap()
            .push((tx_hashes.to_vec(), value));

        // db2 state mutation: flip the real `conflicting` flag when a record
        // exists. db1 spy tests never seed `records`, so this is a no-op for
        // them and they keep asserting on the recorded calls / graph children.
        {
            let mut recs = self.records.lock().unwrap();
            for h in tx_hashes {
                if let Some(rec) = recs.get_mut(h) {
                    rec.conflicting = value;
                }
            }
        }

        let parents_map = self.conflict_parents.lock().unwrap();
        let children_map = self.conflict_children.lock().unwrap();

        let mut affected: Vec<Spend> = Vec::new();
        let mut children: Vec<Hash> = Vec::new();

        for h in tx_hashes {
            if let Some(p) = parents_map.get(h) {
                affected.extend(p.iter().cloned());
            }
            if let Some(c) = children_map.get(h) {
                children.extend(c.iter().copied());
            }
        }

        Ok((affected, children))
    }

    async fn get_tx_meta(&self, hash: &Hash) -> Result<Option<TxMeta>, StoreError> {
        let recs = self.records.lock().unwrap();
        Ok(recs.get(hash).map(|r| TxMeta {
            tx_bytes: r.tx_bytes.clone(),
            conflicting: r.conflicting,
            block_ids: r.block_ids.clone(),
            created_at: r.created_at,
        }))
    }

    async fn get_spending_datas(
        &self,
        hash: &Hash,
    ) -> Result<Vec<Option<SpendingData>>, StoreError> {
        let recs = self.records.lock().unwrap();
        Ok(recs
            .get(hash)
            .map(|r| r.spending_datas.clone())
            .unwrap_or_default())
    }

    async fn get_conflicting_children_bin(&self, hash: &Hash) -> Result<Vec<Hash>, StoreError> {
        let recs = self.records.lock().unwrap();
        Ok(recs
            .get(hash)
            .map(|r| r.conflicting_children.clone())
            .unwrap_or_default())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn returns_seeded_unmined_and_records_mined() {
        let tx = UnminedTx {
            txid: [1u8; 32],
            fee: 5,
            size: 100,
            block_ids: vec![],
            locked: false,
            created_at: 0,
        };
        let store = MemUtxoStore::with_unmined(vec![tx.clone()]);

        let got = store.unmined().await.unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].fee, 5);

        let info = MinedBlockInfo {
            block_id: 9,
            block_height: 1,
            subtree_idx: 0,
            on_longest_chain: true,
            unset_mined: false,
        };
        let res = store.set_mined_multi(&[[1u8; 32]], &info).await.unwrap();
        assert_eq!(res.get(&[1u8; 32]).unwrap(), &vec![9]);
    }

    #[tokio::test]
    async fn set_mined_is_idempotent() {
        let store = MemUtxoStore::default();
        let info = MinedBlockInfo {
            block_id: 3,
            block_height: 1,
            subtree_idx: 0,
            on_longest_chain: true,
            unset_mined: false,
        };
        let h = [2u8; 32];

        store.set_mined_multi(&[h], &info).await.unwrap();
        let res = store.set_mined_multi(&[h], &info).await.unwrap();
        assert_eq!(res[&h], vec![3], "block_id must appear exactly once");
    }

    #[tokio::test]
    async fn unset_mined_removes_block_id() {
        let store = MemUtxoStore::default();
        let h = [3u8; 32];

        let set_info = MinedBlockInfo {
            block_id: 7,
            block_height: 1,
            subtree_idx: 0,
            on_longest_chain: true,
            unset_mined: false,
        };
        store.set_mined_multi(&[h], &set_info).await.unwrap();

        let unset_info = MinedBlockInfo {
            unset_mined: true,
            ..set_info
        };
        let res = store.set_mined_multi(&[h], &unset_info).await.unwrap();
        assert!(res[&h].is_empty(), "block_id must be removed");
    }

    #[tokio::test]
    async fn mark_on_longest_chain_is_noop() {
        let store = MemUtxoStore::default();
        let result = store.mark_on_longest_chain(&[[4u8; 32]], true).await;
        assert!(result.is_ok());
    }

    #[tokio::test]
    async fn set_locked_is_recorded() {
        let store = MemUtxoStore::default();
        store
            .set_locked(&[[5u8; 32], [6u8; 32]], false)
            .await
            .unwrap();
        let calls = store.locked_calls();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].0, vec![[5u8; 32], [6u8; 32]]);
        assert!(!calls[0].1, "boot load unlocks (locked=false)");
    }

    fn h(n: u8) -> Hash {
        [n; 32]
    }

    fn parent_spend(tx: Hash, vin: u32) -> Spend {
        Spend {
            tx_id: [0xEE; 32],
            vout: vin,
            utxo_hash: [0xAB; 32],
            spending_data: Some(super::super::SpendingData { tx_id: tx, vin }),
            conflicting_tx_id: None,
            block_ids: vec![],
        }
    }

    #[tokio::test]
    async fn mark_recursively_single_level_no_children() {
        let store = MemUtxoStore::default();
        // tx 1 has no spending children.
        let (affected, order) = store.mark_conflicting_recursively(&[h(1)]).await.unwrap();

        assert_eq!(order, vec![h(1)], "only the seed is marked");
        assert!(affected.is_empty());
        // One set_conflicting call (the seed level), then the next batch is empty.
        let calls = store.conflicting_calls();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0], (vec![h(1)], true));
    }

    #[tokio::test]
    async fn mark_recursively_multi_level_bfs_order() {
        let store = MemUtxoStore::default();
        // Cascade: 1 -> {2,3}; 2 -> {4}; 3 -> {5}; 4,5 -> leaf.
        store.set_conflict_children(h(1), vec![h(2), h(3)]);
        store.set_conflict_children(h(2), vec![h(4)]);
        store.set_conflict_children(h(3), vec![h(5)]);

        let (_affected, order) = store.mark_conflicting_recursively(&[h(1)]).await.unwrap();

        // BFS order: seed level, then level 1 (2,3), then level 2 (4,5).
        assert_eq!(order, vec![h(1), h(2), h(3), h(4), h(5)]);

        // set_conflicting is called once per non-empty BFS level: [1], [2,3], [4,5].
        let calls = store.conflicting_calls();
        assert_eq!(calls.len(), 3);
        assert_eq!(calls[0], (vec![h(1)], true));
        assert_eq!(calls[1], (vec![h(2), h(3)], true));
        assert_eq!(calls[2], (vec![h(4), h(5)], true));
    }

    #[tokio::test]
    async fn mark_recursively_diamond_dedups_visited() {
        let store = MemUtxoStore::default();
        // Diamond: 1 -> {2,3}; both 2 and 3 -> {4}. 4 must be visited once.
        store.set_conflict_children(h(1), vec![h(2), h(3)]);
        store.set_conflict_children(h(2), vec![h(4)]);
        store.set_conflict_children(h(3), vec![h(4)]);

        let (_affected, order) = store.mark_conflicting_recursively(&[h(1)]).await.unwrap();

        assert_eq!(
            order,
            vec![h(1), h(2), h(3), h(4)],
            "4 appears exactly once despite two parents"
        );

        // Level [2,3] yields children [4,4]; only the first unseen 4 enters the
        // next batch, so the third call processes [4], not [4,4].
        let calls = store.conflicting_calls();
        assert_eq!(calls.len(), 3);
        assert_eq!(calls[2], (vec![h(4)], true));
    }

    #[tokio::test]
    async fn mark_recursively_seed_dedup() {
        let store = MemUtxoStore::default();
        store.set_conflict_children(h(1), vec![h(2)]);
        // Duplicate seed must collapse to one entry.
        let (_affected, order) = store
            .mark_conflicting_recursively(&[h(1), h(1)])
            .await
            .unwrap();
        assert_eq!(order, vec![h(1), h(2)]);
    }

    #[tokio::test]
    async fn mark_recursively_cycle_terminates() {
        let store = MemUtxoStore::default();
        // Cycle: 1 -> 2 -> 1. visited dedup must break it.
        store.set_conflict_children(h(1), vec![h(2)]);
        store.set_conflict_children(h(2), vec![h(1)]);

        let (_affected, order) = store.mark_conflicting_recursively(&[h(1)]).await.unwrap();
        assert_eq!(order, vec![h(1), h(2)]);
    }

    #[tokio::test]
    async fn mark_recursively_accumulates_affected_spends() {
        let store = MemUtxoStore::default();
        store.set_conflict_children(h(1), vec![h(2)]);
        store.set_conflict_parents(h(1), vec![parent_spend(h(1), 0)]);
        store.set_conflict_parents(h(2), vec![parent_spend(h(2), 0), parent_spend(h(2), 1)]);

        let (affected, order) = store.mark_conflicting_recursively(&[h(1)]).await.unwrap();

        assert_eq!(order, vec![h(1), h(2)]);
        assert_eq!(affected.len(), 3, "1 parent for tx1 + 2 parents for tx2");
        // Affected spends accumulate in BFS-level order.
        assert_eq!(affected[0].spending_data.as_ref().unwrap().tx_id, h(1));
        assert_eq!(affected[1].spending_data.as_ref().unwrap().tx_id, h(2));
        assert_eq!(affected[2].spending_data.as_ref().unwrap().tx_id, h(2));
    }

    #[tokio::test]
    async fn unmark_recursively_inverse_clears_with_false() {
        let store = MemUtxoStore::default();
        store.set_conflict_children(h(1), vec![h(2), h(3)]);
        store.set_conflict_children(h(2), vec![h(4)]);

        let cleared = store.unmark_conflicting_recursively(&[h(1)]).await.unwrap();

        assert_eq!(cleared, vec![h(1), h(2), h(3), h(4)], "same BFS order");

        // Every set_conflicting call in the unmark path uses value=false.
        let calls = store.conflicting_calls();
        assert!(!calls.is_empty());
        for c in &calls {
            assert!(!c.1, "unmark must call set_conflicting(.., false)");
        }
    }

    #[tokio::test]
    async fn spend_and_unspend_are_recorded() {
        let store = MemUtxoStore::default();
        let spend = parent_spend(h(9), 0);

        let flags = IgnoreFlags {
            ignore_conflicting: true,
            ignore_locked: false,
        };
        let returned = store
            .spend(std::slice::from_ref(&spend), 200, flags)
            .await
            .unwrap();
        assert_eq!(returned, vec![spend.clone()]);

        let sc = store.spend_calls();
        assert_eq!(sc.len(), 1);
        assert_eq!(sc[0].1, 200);
        assert_eq!(sc[0].2, flags);

        store
            .unspend(std::slice::from_ref(&spend), true)
            .await
            .unwrap();
        let uc = store.unspend_calls();
        assert_eq!(uc.len(), 1);
        assert!(uc[0].1, "flag_as_locked recorded");
    }

    fn ext_tx_one_input(prev: Hash, vout: u32) -> Vec<u8> {
        // minimal EXTENDED tx: 1 input (prev, vout), empty scripts, 1 dummy output.
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
        b.push(0x6a); // locking script
        b.extend_from_slice(&0u32.to_le_bytes()); // locktime
        b
    }

    #[tokio::test]
    async fn mem_seed_tx_and_get_tx_meta() {
        let store = MemUtxoStore::default();
        let bytes = ext_tx_one_input([7u8; 32], 0);
        store.seed_tx([9u8; 32], bytes.clone(), true, vec![5], 42);
        let m = store.get_tx_meta(&[9u8; 32]).await.unwrap().unwrap();
        assert_eq!(m.tx_bytes, bytes);
        assert!(m.conflicting);
        assert_eq!(m.block_ids, vec![5]);
        assert_eq!(m.created_at, 42, "created_at round-trips through seed_tx");
        assert!(store.get_tx_meta(&[0u8; 32]).await.unwrap().is_none());
    }

    #[tokio::test]
    async fn mem_set_conflicting_flips_real_flag() {
        let store = MemUtxoStore::default();
        store.seed_tx([9u8; 32], ext_tx_one_input([7u8; 32], 0), false, vec![], 0);
        store.set_conflicting(&[[9u8; 32]], true).await.unwrap();
        assert!(
            store
                .get_tx_meta(&[9u8; 32])
                .await
                .unwrap()
                .unwrap()
                .conflicting
        );
        store.set_conflicting(&[[9u8; 32]], false).await.unwrap();
        assert!(
            !store
                .get_tx_meta(&[9u8; 32])
                .await
                .unwrap()
                .unwrap()
                .conflicting
        );
    }

    #[tokio::test]
    async fn mem_spend_then_unspend_mutates_spending_datas() {
        let store = MemUtxoStore::default();
        let parent = [0x11; 32];
        let spender = [0x5A; 32];

        // parent P has 1 output, initially unspent.
        store.seed_tx(parent, ext_tx_one_input([7u8; 32], 0), false, vec![], 0);
        store.seed_spending_datas(parent, vec![None]);

        let spend = Spend {
            tx_id: parent,
            vout: 0,
            utxo_hash: [0xAB; 32],
            spending_data: Some(super::super::SpendingData {
                tx_id: spender,
                vin: 0,
            }),
            conflicting_tx_id: None,
            block_ids: vec![],
        };

        let flags = IgnoreFlags {
            ignore_conflicting: true,
            ignore_locked: true,
        };
        store
            .spend(std::slice::from_ref(&spend), 100, flags)
            .await
            .unwrap();

        let after_spend = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(
            after_spend[0],
            Some(super::super::SpendingData {
                tx_id: spender,
                vin: 0
            }),
            "spend records the spender on parent[0]"
        );

        store
            .unspend(std::slice::from_ref(&spend), false)
            .await
            .unwrap();

        let after_unspend = store.get_spending_datas(&parent).await.unwrap();
        assert_eq!(after_unspend[0], None, "unspend clears parent[0]");
    }
}

#[cfg(test)]
mod conflict_helpers {
    use std::collections::HashSet;

    use ba_subtree_bench::hash::Hash;

    use super::super::{SpendingData, UtxoStore};
    use super::MemUtxoStore;

    fn h(n: u8) -> Hash {
        [n; 32]
    }

    fn sd(tx: Hash, vin: u32) -> SpendingData {
        SpendingData { tx_id: tx, vin }
    }

    /// Minimal EXTENDED tx with a single input `(prev, vout)`, copied from the
    /// `tests` module so this module is self-contained.
    fn ext_tx_one_input(prev: Hash, vout: u32) -> Vec<u8> {
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
        b.push(0x6a); // locking script
        b.extend_from_slice(&0u32.to_le_bytes()); // locktime
        b
    }

    /// `get_conflicting_children` (Go `GetConflictingChildren`) must union the
    /// `ConflictingChildren` bin with the current spenders read from the
    /// `SpendingDatas` bin, walk recursively, and exclude the root.
    #[tokio::test]
    async fn get_conflicting_children_walks_bin_and_spenders() {
        let store = MemUtxoStore::default();
        let root = h(1);
        let a = h(0xA);
        let b = h(0xB);
        let c = h(0xC);

        // root: ConflictingChildren=[A], SpendingDatas=[Some(B,0)].
        store.seed_conflicting_children(root, vec![a]);
        store.seed_spending_datas(root, vec![Some(sd(b, 0))]);
        // A: ConflictingChildren=[C].
        store.seed_conflicting_children(a, vec![c]);

        let got = store.get_conflicting_children(root).await.unwrap();
        let got: HashSet<Hash> = got.into_iter().collect();
        let want: HashSet<Hash> = [a, b, c].into_iter().collect();

        assert_eq!(
            got, want,
            "union of bin + spenders, recursive, root excluded"
        );
        assert!(
            !got.contains(&root),
            "root must be excluded from the result"
        );
    }

    /// `get_counter_conflicting` (Go `GetCounterConflictingTxHashes`) must:
    /// include the tx itself (Go line 999); read the current spender of each
    /// parent UTXO this tx's inputs reference; and union that spender's
    /// conflicting children.
    #[tokio::test]
    async fn get_counter_conflicting_picks_current_spender_and_children() {
        let store = MemUtxoStore::default();
        let parent = h(0x50);
        let winner = h(0x57); // W: current spender of (P,0)
        let demoted = h(0x0D); // D: the tx we query (it also spends (P,0))
        let wchild = h(0xC1); // W's conflicting child

        // Parent P: output 0 currently spent by winner W.
        store.seed_spending_datas(parent, vec![Some(sd(winner, 0))]);
        // W has a conflicting child.
        store.seed_conflicting_children(winner, vec![wchild]);
        // D's body: a single input spending (P, 0).
        store.seed_tx(demoted, ext_tx_one_input(parent, 0), true, vec![], 0);

        let got = store.get_counter_conflicting(demoted).await.unwrap();
        let got: HashSet<Hash> = got.into_iter().collect();
        let want: HashSet<Hash> = [demoted, winner, wchild].into_iter().collect();

        assert_eq!(
            got, want,
            "tx itself + current spender W + W's conflicting children"
        );
        assert!(got.contains(&demoted), "must include the queried tx itself");
        assert!(got.contains(&winner), "must include the current spender");
        assert!(got.contains(&wchild), "must include the spender's children");
    }

    /// vout out of range against the parent's `SpendingDatas` length → error
    /// (Go lines 1034-1036).
    #[tokio::test]
    async fn get_counter_conflicting_vout_out_of_range_errors() {
        let store = MemUtxoStore::default();
        let parent = h(0x50);
        let demoted = h(0x0D);

        // Parent P has only 1 output (len 1), but D spends (P, 5).
        store.seed_spending_datas(parent, vec![Some(sd(h(0x57), 0))]);
        store.seed_tx(demoted, ext_tx_one_input(parent, 5), true, vec![], 0);

        let err = store.get_counter_conflicting(demoted).await;
        assert!(err.is_err(), "vout 5 against a 1-output parent must error");
    }

    /// A frozen (coinbase-placeholder `[0xFF; 32]`) conflicting child of the
    /// current spender → error (Go lines 1049-1051).
    #[tokio::test]
    async fn get_counter_conflicting_frozen_child_errors() {
        let store = MemUtxoStore::default();
        let parent = h(0x50);
        let winner = h(0x57); // current spender of (P,0)
        let demoted = h(0x0D); // the tx we query (also spends (P,0))
        let frozen: Hash = [0xFF; 32];

        // Parent P: output 0 currently spent by winner W.
        store.seed_spending_datas(parent, vec![Some(sd(winner, 0))]);
        // W's record exists and its conflicting children include the frozen hash.
        store.seed_tx(winner, ext_tx_one_input(parent, 0), true, vec![], 0);
        store.seed_conflicting_children(winner, vec![frozen]);
        // D's body: a single input spending (P, 0).
        store.seed_tx(demoted, ext_tx_one_input(parent, 0), true, vec![], 0);

        let err = store.get_counter_conflicting(demoted).await;
        assert!(
            err.is_err(),
            "a frozen ([0xFF;32]) conflicting child of the spender must error"
        );
    }
}

//! Async subtree writer task. Receives completed subtrees drained from the
//! assembly (one per `take_newly_chained` hit on the ingest path) and persists
//! each to the blob store in Go's exact on-disk format — the 8-byte
//! `SUBTREE_MAGIC` header followed by `Subtree::serialize()`, keyed by the
//! subtree root hash (the store reverses it for the `.subtree` filename).
//!
//! Mirrors Go `runNewSubtreeListener` / `storeSubtreeData` +
//! `sendSubtreeNotification` (`services/blockassembly/Server.go`): serialize,
//! `Set`, best-effort `SetDAH`, then — FSM-gated and best-effort — send the
//! subtree notification. The notification only fires when the blockchain FSM
//! reports `RUNNING`, and a failure is logged, never propagated (matching Go).

use std::sync::Arc;

use ba_subtree_bench::subtree::Subtree;
use tokio::sync::mpsc::UnboundedReceiver;

use crate::store::{BlobStore, BlockchainClient};
use crate::subtree_store::SUBTREE_MAGIC;

/// Best-effort delete-at-height passed to `set_dah`. Go computes
/// `currentBlockHeight + GlobalBlockHeightRetention`; `set_dah` is currently a
/// logged no-op (real pruning is Capability H), so a fixed `0` is used — the
/// value is not yet consumed.
const PLACEHOLDER_DAH: u32 = 0;

/// Persist one completed subtree: serialize with the magic header and `set` it
/// under its root hash, then `set_dah` best-effort. After a successful store,
/// send the FSM-gated subtree notification (Go `sendSubtreeNotification`): only
/// when the FSM is `RUNNING`, and log-not-fail on error.
pub async fn persist_subtree(
    blob: &Arc<dyn BlobStore>,
    chain: &Arc<dyn BlockchainClient>,
    mut subtree: Subtree,
) {
    let root = match subtree.root_hash() {
        Some(r) => r,
        None => {
            eprintln!("subtree_writer: skipping empty subtree (no root)");
            return;
        }
    };

    let body = subtree.serialize();
    let mut bytes = Vec::with_capacity(SUBTREE_MAGIC.len() + body.len());
    bytes.extend_from_slice(&SUBTREE_MAGIC);
    bytes.extend_from_slice(&body);

    if let Err(e) = blob.set(&root, &bytes).await {
        eprintln!(
            "subtree_writer: failed to persist subtree {}: {e}",
            hex::encode(root)
        );
        return;
    }

    // Best-effort delete-at-height (no-op + log today; pruning is Capability H).
    if let Err(e) = blob.set_dah(&root, PLACEHOLDER_DAH).await {
        eprintln!(
            "subtree_writer: set_dah failed for subtree {} (best-effort): {e}",
            hex::encode(root)
        );
    }

    // FSM-gated, best-effort subtree notification (Go sendSubtreeNotification):
    // skip entirely unless the FSM is RUNNING; never fail the writer on error.
    if chain.is_fsm_current_state("RUNNING").await.unwrap_or(false) {
        if let Err(e) = chain.send_notification_subtree(&root).await {
            eprintln!(
                "subtree_writer: subtree notification failed for {} (best-effort): {e}",
                hex::encode(root)
            );
        }
    }
}

/// Drain the channel of completed subtrees and persist each one. Runs until all
/// senders are dropped (service shutdown). Spawned from `main.rs`; the ingest
/// handlers feed it via the `UnboundedSender` held by `BaService`. `chain` is the
/// blockchain client used for the FSM-gated subtree notification.
pub async fn run_subtree_writer(
    mut rx: UnboundedReceiver<Subtree>,
    blob: Arc<dyn BlobStore>,
    chain: Arc<dyn BlockchainClient>,
) {
    while let Some(subtree) = rx.recv().await {
        persist_subtree(&blob, &chain, subtree).await;
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::sync::Mutex;

    use ba_subtree_bench::hash::{sha256d, Hash};
    use tonic::async_trait;

    use super::*;
    use crate::store::chain_mem::MemBlockchainClient;
    use crate::store::{ChainTip, StoreError};

    fn mem_chain(running: bool) -> Arc<MemBlockchainClient> {
        let tip = ChainTip {
            hash: [0u8; 32],
            height: 0,
            n_bits: 0,
            version: 1,
            median_time: 0,
        };
        Arc::new(MemBlockchainClient::new(tip).with_fsm_running(running))
    }

    fn subtree_of_4() -> (Subtree, Vec<Hash>, Hash) {
        let mut st = Subtree::new();
        let txs: Vec<Hash> = (0u32..4).map(|i| sha256d(&i.to_le_bytes())).collect();
        for (i, h) in txs.iter().enumerate() {
            st.add_node(*h, i as u64, 10);
        }
        let root = st.root_hash().unwrap();
        (st, txs, root)
    }

    /// Recording in-memory BlobStore: captures every `set` (key + bytes) so the
    /// writer test can assert the persisted blob without touching the filesystem
    /// or racing a spawned task.
    #[derive(Default)]
    struct RecordingBlobStore {
        writes: Mutex<HashMap<Hash, Vec<u8>>>,
        dah_calls: Mutex<Vec<(Hash, u32)>>,
    }

    #[async_trait]
    impl BlobStore for RecordingBlobStore {
        async fn tx_hashes(&self, _subtree_hash: &Hash) -> Result<Vec<Hash>, StoreError> {
            Err(StoreError::NotFound("recording store has no reader".into()))
        }

        async fn subtree(
            &self,
            _root: &Hash,
        ) -> Result<ba_subtree_bench::subtree::Subtree, StoreError> {
            Err(StoreError::NotFound("recording store has no reader".into()))
        }

        async fn set(&self, key: &Hash, bytes: &[u8]) -> Result<(), StoreError> {
            self.writes.lock().unwrap().insert(*key, bytes.to_vec());
            Ok(())
        }

        async fn set_dah(&self, key: &Hash, dah: u32) -> Result<(), StoreError> {
            self.dah_calls.lock().unwrap().push((*key, dah));
            Ok(())
        }
    }

    #[tokio::test]
    async fn persist_writes_blob_keyed_by_root_with_magic_header() {
        // Hold a concrete handle to inspect, and a trait-object handle for the API.
        let rec = Arc::new(RecordingBlobStore::default());
        let store: Arc<dyn BlobStore> = rec.clone();
        let mem = mem_chain(true);
        let chain: Arc<dyn BlockchainClient> = mem.clone();

        let (st, txs, root) = subtree_of_4();

        persist_subtree(&store, &chain, st).await;

        let writes = rec.writes.lock().unwrap();
        assert_eq!(writes.len(), 1, "exactly one blob written");

        let bytes = writes.get(&root).expect("blob keyed by the subtree root");
        assert_eq!(
            &bytes[..SUBTREE_MAGIC.len()],
            &SUBTREE_MAGIC,
            "blob must start with the magic header"
        );

        // Deserialize the body back and confirm the txs round-trip.
        let back = Subtree::deserialize(&bytes[SUBTREE_MAGIC.len()..]).expect("deserialize");
        assert_eq!(back.tx_hashes(), txs, "persisted blob holds the right txs");

        // DAH recorded once for the same root (best-effort).
        let dah = rec.dah_calls.lock().unwrap();
        assert_eq!(dah.len(), 1);
        assert_eq!(dah[0].0, root);
    }

    #[tokio::test]
    async fn notification_sent_for_root_when_fsm_running() {
        let rec = Arc::new(RecordingBlobStore::default());
        let store: Arc<dyn BlobStore> = rec.clone();
        let mem = mem_chain(true);
        let chain: Arc<dyn BlockchainClient> = mem.clone();

        let (st, _txs, root) = subtree_of_4();

        persist_subtree(&store, &chain, st).await;

        // Persisted AND notified for the subtree root.
        assert_eq!(rec.writes.lock().unwrap().len(), 1, "blob persisted");
        assert_eq!(
            mem.subtree_notifications(),
            vec![root],
            "RUNNING FSM -> notification recorded for the subtree root"
        );
    }

    #[tokio::test]
    async fn notification_skipped_when_fsm_not_running() {
        let rec = Arc::new(RecordingBlobStore::default());
        let store: Arc<dyn BlobStore> = rec.clone();
        let mem = mem_chain(false);
        let chain: Arc<dyn BlockchainClient> = mem.clone();

        let (st, _txs, _root) = subtree_of_4();

        persist_subtree(&store, &chain, st).await;

        // Persist still happens; notification is gated off.
        assert_eq!(rec.writes.lock().unwrap().len(), 1, "blob still persisted");
        assert!(
            mem.subtree_notifications().is_empty(),
            "not-RUNNING FSM -> NO notification"
        );
    }
}

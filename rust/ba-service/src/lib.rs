//! Library surface for ba-service. ALL modules (including the generated proto
//! types) live here so the binary (`main.rs`) and integration tests share a
//! single crate root — and therefore a single proto type tree. The binary is a
//! thin shell that `use`s these.

pub mod blockassembly_api {
    tonic::include_proto!("blockassembly_api");
}
pub mod model {
    tonic::include_proto!("model");
}
pub mod blockchain_api {
    tonic::include_proto!("blockchain_api");
}

pub mod assembly;
pub mod block;
pub mod chain;
pub mod coinbase;
pub mod config;
pub mod jobstore;
pub mod load;
pub mod process_conflicting;
pub mod reorg;
pub mod server;
pub mod store;
pub mod subtree_store;
pub mod subtree_writer;

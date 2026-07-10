//! Gate 1 — native-Rust port of the Block Assembly subtree engine, validated
//! byte-identical to go-subtree and benchmarked against the tuned Go path.
//! The Go implementation is read-only; everything here lives under `rust/`.

pub mod block_merkle;
pub mod bump;
pub mod hash;
pub mod inpoints;
pub mod merkle;
pub mod power_of_two;
pub mod processor;
pub mod reorg;
pub mod subtree;
pub mod tx;

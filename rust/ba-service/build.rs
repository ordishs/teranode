//! Generates Rust gRPC types from the CANONICAL Teranode protos, read in place.
//! Nothing is copied or written into the Go tree; these are the same .proto
//! files the Go service uses.

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // rust/ba-service -> repo root
    let repo_root = "../..";
    let api_proto =
        format!("{repo_root}/services/blockassembly/blockassembly_api/blockassembly_api.proto");
    // Subscribe to blockchain notifications exactly like the Go BA does.
    let blockchain_proto =
        format!("{repo_root}/services/blockchain/blockchain_api/blockchain_api.proto");

    // Rerun if the canonical protos change.
    println!("cargo:rerun-if-changed={api_proto}");
    println!("cargo:rerun-if-changed={blockchain_proto}");
    println!("cargo:rerun-if-changed={repo_root}/model/model.proto");

    // Emit a file descriptor set so the server can expose gRPC reflection
    // (lets `grpcurl list` / describe work without proto files).
    let out_dir = std::path::PathBuf::from(std::env::var("OUT_DIR")?);

    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .file_descriptor_set_path(out_dir.join("ba_descriptor.bin"))
        .compile_protos(
            &[api_proto.as_str(), blockchain_proto.as_str()],
            &[repo_root],
        )?;

    Ok(())
}

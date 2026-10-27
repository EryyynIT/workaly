fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-changed=migrations");
    println!("cargo:rerun-if-changed=../contracts/user.proto");
    tonic_build::configure().compile_protos(&["../contracts/user.proto"], &["../contracts"])?;
    Ok(())
}

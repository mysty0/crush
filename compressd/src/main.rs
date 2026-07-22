//! CLI entry point: parses arguments, sets up logging, then hands off to
//! [`headroomd::run`].

use clap::Parser;
use headroomd::config::Args;

/// Loads the ONNX Runtime shared library at `path` before any session is
/// created. `load-dynamic` is always compiled in (see Cargo.toml), so
/// this always works; it does not depend on any daemon-side Cargo
/// feature.
fn init_ort_dylib(path: &std::path::Path) -> anyhow::Result<()> {
    let builder = ort::init_from(path)
        .map_err(|e| anyhow::anyhow!("failed to load ONNX Runtime library from {}: {e}", path.display()))?;
    if !builder.commit() {
        anyhow::bail!("failed to commit ONNX Runtime environment loaded from {}", path.display());
    }
    Ok(())
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();

    tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    init_ort_dylib(&args.ort_dylib_path)?;

    headroomd::run(args).await
}

//! CLI argument parsing and default socket path resolution.

use std::path::PathBuf;

use clap::Parser;

/// Local IPC daemon that scores extractive-compression keep/drop decisions
/// for text using an ONNX token-classification model.
#[derive(Debug, Parser)]
#[command(name = "headroomd", version, about)]
pub struct Args {
    /// Path to the Unix domain socket to listen on. Defaults to
    /// `$XDG_RUNTIME_DIR/crush/headroomd.sock` if `XDG_RUNTIME_DIR` is set,
    /// otherwise `/tmp/crush-headroomd-<uid>.sock`.
    #[arg(long)]
    pub socket: Option<PathBuf>,

    /// Path to the ONNX token-classification model file.
    #[arg(long)]
    pub model: PathBuf,

    /// Path to the HuggingFace `tokenizer.json` file.
    #[arg(long)]
    pub tokenizer: PathBuf,

    /// Seconds of inactivity (no requests) after which the daemon exits
    /// cleanly so a supervisor can respawn it on demand.
    #[arg(long, default_value_t = 600)]
    pub idle_timeout_secs: u64,

    /// Maximum sequence length (in tokens) fed to the model at once. Inputs
    /// longer than this are split into chunks; see `model::chunk_token_ids`.
    #[arg(long, default_value_t = 8192)]
    pub max_length: usize,

    /// Run inference on an NVIDIA GPU via the ONNX Runtime CUDA execution
    /// provider, instead of CPU. Only meaningful in a build compiled with
    /// `--features gpu`; on a CPU-only build this flag is refused at
    /// startup rather than silently ignored, since silently falling back
    /// to CPU would look like GPU inference was working when it wasn't.
    #[arg(long, default_value_t = false)]
    pub gpu: bool,

    /// CUDA device id to use when `--gpu` is set. Ignored otherwise.
    #[arg(long, default_value_t = 0)]
    pub gpu_device_id: i32,

    /// Path to the `libonnxruntime.so` (or `.dylib`/`.dll`) to load at
    /// runtime. Required: this daemon always loads ONNX Runtime
    /// dynamically (see compressd/Cargo.toml's `ort` dependency) rather
    /// than linking a copy fetched at build time, so builds work inside
    /// sandboxed/offline build environments (Nix, most CI) and one
    /// binary can be pointed at any ONNX Runtime build -- the stock CPU
    /// release, or a custom one (e.g. compiled with
    /// CMAKE_CUDA_ARCHITECTURES=120 for GPU generations, such as
    /// Blackwell/RTX 50-series, not yet covered by any official
    /// prebuilt release -- see compressd/README.md for the build
    /// recipe).
    #[arg(long)]
    pub ort_dylib_path: PathBuf,
}

/// Resolve the default socket path when `--socket` was not given.
///
/// Mirrors the spec: prefer `$XDG_RUNTIME_DIR/crush/headroomd.sock`, falling
/// back to a uid-scoped path under `/tmp`.
pub fn default_socket_path(xdg_runtime_dir: Option<&str>, uid: u32) -> PathBuf {
    match xdg_runtime_dir {
        Some(dir) if !dir.is_empty() => PathBuf::from(dir).join("crush").join("headroomd.sock"),
        _ => PathBuf::from(format!("/tmp/crush-headroomd-{uid}.sock")),
    }
}

/// Resolve the socket path to use: the explicit `--socket` flag if given,
/// otherwise the computed default.
pub fn resolve_socket_path(
    explicit: Option<PathBuf>,
    xdg_runtime_dir: Option<&str>,
    uid: u32,
) -> PathBuf {
    explicit.unwrap_or_else(|| default_socket_path(xdg_runtime_dir, uid))
}

/// Return the current process's real user id.
///
/// Safety: `getuid(2)` never fails and takes no arguments; this is a thin,
/// side-effect-free wrapper.
pub fn current_uid() -> u32 {
    // SAFETY: getuid(2) is always safe to call and cannot fail.
    unsafe { libc::getuid() }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn uses_xdg_runtime_dir_when_set() {
        let path = default_socket_path(Some("/run/user/1000"), 1000);
        assert_eq!(path, PathBuf::from("/run/user/1000/crush/headroomd.sock"));
    }

    #[test]
    fn falls_back_to_tmp_when_xdg_runtime_dir_unset() {
        let path = default_socket_path(None, 1000);
        assert_eq!(path, PathBuf::from("/tmp/crush-headroomd-1000.sock"));
    }

    #[test]
    fn falls_back_to_tmp_when_xdg_runtime_dir_empty() {
        let path = default_socket_path(Some(""), 42);
        assert_eq!(path, PathBuf::from("/tmp/crush-headroomd-42.sock"));
    }

    #[test]
    fn explicit_socket_overrides_default() {
        let explicit = PathBuf::from("/custom/path.sock");
        let path = resolve_socket_path(Some(explicit.clone()), Some("/run/user/1000"), 1000);
        assert_eq!(path, explicit);
    }

    #[test]
    fn falls_back_to_default_when_no_explicit_socket() {
        let path = resolve_socket_path(None, Some("/run/user/1000"), 1000);
        assert_eq!(path, PathBuf::from("/run/user/1000/crush/headroomd.sock"));
    }

    #[test]
    fn current_uid_matches_libc_getuid() {
        let expected = unsafe { libc::getuid() };
        assert_eq!(current_uid(), expected);
    }
}

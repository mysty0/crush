//! Platform-specific socket transport.
//!
//! The initial implementation only supports Unix domain sockets (Linux and
//! macOS). Windows named-pipe support is a documented TODO: the daemon
//! lifecycle and IPC protocol are transport-agnostic (see `protocol.rs`), so
//! adding a named-pipe listener that produces the same
//! `AsyncRead + AsyncWrite` interface used by `server::handle_connection`
//! should be a self-contained addition, gated behind `cfg(windows)`.

use std::path::Path;

use anyhow::{Context, Result};

#[cfg(unix)]
pub use unix::{bind_unix_listener, remove_socket_file, UnixListener};

#[cfg(unix)]
mod unix {
    use super::*;

    pub type UnixListener = tokio::net::UnixListener;

    /// Bind a Unix domain socket listener at `path`, creating the parent
    /// directory if needed and removing a stale socket file left behind by
    /// a previous, uncleanly-terminated instance.
    pub fn bind_unix_listener(path: &Path) -> Result<UnixListener> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).with_context(|| {
                format!("failed to create socket directory {}", parent.display())
            })?;
        }

        // Remove a stale socket file, if any. We don't attempt to detect
        // whether another instance is actively listening on it; the Go
        // supervisor is responsible for ensuring only one daemon per socket
        // path is started.
        if path.exists() {
            std::fs::remove_file(path).with_context(|| {
                format!("failed to remove stale socket file {}", path.display())
            })?;
        }

        tokio::net::UnixListener::bind(path)
            .with_context(|| format!("failed to bind Unix socket at {}", path.display()))
    }

    /// Remove the socket file at `path`, ignoring a "not found" error (it
    /// may have already been cleaned up, or never created).
    pub fn remove_socket_file(path: &Path) {
        match std::fs::remove_file(path) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => {
                tracing::warn!(error = %e, path = %path.display(), "Failed to remove socket file on shutdown");
            }
        }
    }
}

#[cfg(windows)]
pub fn bind_unix_listener(_path: &Path) -> Result<()> {
    anyhow::bail!(
        "Windows named-pipe transport is not implemented yet; headroomd currently only supports Unix domain \
         sockets. TODO: add a named-pipe listener in transport.rs that satisfies the same connection interface \
         as the Unix implementation."
    )
}

#[cfg(windows)]
pub fn remove_socket_file(_path: &Path) {}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(unix)]
    #[tokio::test]
    async fn bind_unix_listener_creates_parent_dir_and_removes_stale_socket() {
        let tmp = tempfile::tempdir().unwrap();
        let socket_path = tmp.path().join("nested").join("dir").join("test.sock");

        // Bind once.
        let listener = bind_unix_listener(&socket_path).unwrap();
        assert!(socket_path.exists());
        drop(listener);

        // Simulate a stale socket file left behind (the file still exists
        // on disk after the listener above is dropped, since we didn't
        // clean it up).
        assert!(socket_path.exists());

        // Binding again should remove the stale file and succeed rather
        // than erroring with "address already in use".
        let listener2 = bind_unix_listener(&socket_path).unwrap();
        drop(listener2);
    }

    #[cfg(unix)]
    #[test]
    fn remove_socket_file_ignores_missing_file() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("does-not-exist.sock");
        // Should not panic.
        remove_socket_file(&path);
    }

    #[cfg(unix)]
    #[test]
    fn remove_socket_file_removes_existing_file() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("present.sock");
        std::fs::write(&path, b"").unwrap();
        assert!(path.exists());
        remove_socket_file(&path);
        assert!(!path.exists());
    }
}

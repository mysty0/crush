//! `headroomd`: a local IPC daemon that scores which subword tokens in a
//! text are safe to drop (extractive compression), backed by an ONNX
//! token-classification model.
//!
//! See `README.md` for the full IPC protocol specification. This crate is
//! organized as:
//!
//! - [`protocol`]: wire format (request/response JSON shapes + framing).
//! - [`compress`]: pure keep/drop-scores -> compressed-text/dropped-spans
//!   logic, independent of any model or I/O.
//! - [`model`]: ONNX Runtime + tokenizer loading and inference, including
//!   the long-input chunking strategy.
//! - [`transport`]: platform socket binding (Unix domain sockets today).
//! - [`server`]: per-connection request/response loop.
//! - [`config`]: CLI args and default socket path resolution.
//! - [`run`]: top-level daemon lifecycle (bind, accept loop, idle timeout,
//!   signal handling, cleanup).

pub mod compress;
pub mod config;
pub mod model;
pub mod protocol;
pub mod server;
pub mod transport;

use std::sync::{Arc, Mutex as StdMutex};
use std::time::{Duration, Instant};

use anyhow::Result;
use config::Args;
use model::Model;

/// Run the daemon to completion: load the model, bind the socket, accept
/// connections until idle timeout or a shutdown signal, then clean up.
pub async fn run(args: Args) -> Result<()> {
    let xdg_runtime_dir = std::env::var("XDG_RUNTIME_DIR").ok();
    let uid = config::current_uid();
    let socket_path =
        config::resolve_socket_path(args.socket.clone(), xdg_runtime_dir.as_deref(), uid);

    tracing::info!(model = %args.model.display(), tokenizer = %args.tokenizer.display(), gpu = args.gpu, "Loading model");
    let model = Arc::new(Model::load(
        &args.model,
        &args.tokenizer,
        args.max_length,
        model::Accelerator::from_args(args.gpu, args.gpu_device_id),
    )?);
    tracing::info!("Model loaded");

    let listener = transport::bind_unix_listener(&socket_path)?;
    tracing::info!(socket = %socket_path.display(), "Listening");

    let start_time = Instant::now();
    let last_activity = Arc::new(StdMutex::new(Instant::now()));
    let idle_timeout = Duration::from_secs(args.idle_timeout_secs);

    let tick_period = if idle_timeout.is_zero() {
        Duration::from_millis(50)
    } else {
        idle_timeout.min(Duration::from_secs(1))
    };
    let mut idle_check = tokio::time::interval(tick_period);

    let result = loop {
        tokio::select! {
            accepted = listener.accept() => {
                match accepted {
                    Ok((stream, _addr)) => {
                        *last_activity.lock().unwrap() = Instant::now();
                        let model = Arc::clone(&model);
                        let last_activity = Arc::clone(&last_activity);
                        tokio::spawn(async move {
                            if let Err(e) = server::handle_connection(stream, model, start_time, last_activity).await {
                                tracing::warn!(error = %e, "Connection handler failed");
                            }
                        });
                    }
                    Err(e) => {
                        tracing::warn!(error = %e, "Failed to accept connection");
                    }
                }
            }
            _ = idle_check.tick() => {
                let elapsed = last_activity.lock().unwrap().elapsed();
                if elapsed >= idle_timeout {
                    tracing::info!(idle_secs = elapsed.as_secs(), "Idle timeout reached, shutting down");
                    break Ok(());
                }
            }
            _ = shutdown_signal() => {
                tracing::info!("Received shutdown signal, shutting down");
                break Ok(());
            }
        }
    };

    transport::remove_socket_file(&socket_path);
    result
}

/// Resolves when the process receives SIGINT or (on Unix) SIGTERM.
#[cfg(unix)]
async fn shutdown_signal() {
    use tokio::signal::unix::{signal, SignalKind};

    let mut sigterm = signal(SignalKind::terminate()).expect("failed to install SIGTERM handler");
    let mut sigint = signal(SignalKind::interrupt()).expect("failed to install SIGINT handler");

    tokio::select! {
        _ = sigterm.recv() => {}
        _ = sigint.recv() => {}
    }
}

#[cfg(windows)]
async fn shutdown_signal() {
    let _ = tokio::signal::ctrl_c().await;
}

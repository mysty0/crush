//! Connection handling: reads framed requests off a Unix socket, dispatches
//! them, and writes back framed JSON responses.

use std::sync::{Arc, Mutex as StdMutex};
use std::time::Instant;

use anyhow::Result;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

use crate::compress::build_compression;
use crate::model::Model;
use crate::protocol::{self, Request, Response};

/// Handle a single client connection: read requests, dispatch, respond,
/// repeat until the client disconnects. Each successfully-read request
/// updates `last_activity` so the daemon's idle timeout resets.
pub async fn handle_connection(
    mut stream: UnixStream,
    model: Arc<Model>,
    start_time: Instant,
    last_activity: Arc<StdMutex<Instant>>,
) -> Result<()> {
    loop {
        let payload = match read_frame(&mut stream).await {
            Ok(Some(payload)) => payload,
            Ok(None) => return Ok(()),
            Err(e) => {
                let response = Response::error(format!("framing error: {e}"));
                write_response(&mut stream, &response).await.ok();
                return Ok(());
            }
        };

        *last_activity.lock().unwrap() = Instant::now();

        let response = match protocol::parse_request(&payload) {
            Ok(request) => dispatch(request, &model, start_time).await,
            Err(e) => Response::error(format!("invalid request: {e}")),
        };

        write_response(&mut stream, &response).await?;
    }
}

async fn dispatch(request: Request, model: &Model, start_time: Instant) -> Response {
    match request {
        Request::Ping => Response::pong(model.is_loaded(), start_time.elapsed().as_secs()),
        Request::Compress { text, threshold } => match model.score(&text).await {
            Ok(scores) => {
                let result = build_compression(&text, &scores, threshold);
                Response::compress(
                    result.compressed,
                    result.keep_rate,
                    result.dropped_spans,
                    model.is_loaded(),
                )
            }
            Err(e) => Response::error(format!("inference failed: {e}")),
        },
    }
}

async fn write_response(stream: &mut UnixStream, response: &Response) -> Result<()> {
    let framed = protocol::encode_message(response)?;
    stream.write_all(&framed).await?;
    Ok(())
}

/// Read one length-prefixed frame, returning `Ok(None)` on a clean EOF
/// (client closed the connection between messages).
async fn read_frame(stream: &mut UnixStream) -> Result<Option<Vec<u8>>> {
    let mut len_buf = [0u8; 4];
    match stream.read_exact(&mut len_buf).await {
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return Ok(None),
        Err(e) => return Err(e.into()),
    }

    let len = protocol::decode_len_prefix(len_buf)?;
    let mut payload = vec![0u8; len as usize];
    stream.read_exact(&mut payload).await?;
    Ok(Some(payload))
}

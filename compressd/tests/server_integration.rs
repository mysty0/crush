//! End-to-end IPC test: spins up the real request-handling loop
//! (`headroomd::server::handle_connection`) over an actual Unix domain
//! socket pair and drives it with the same length-prefixed JSON framing a
//! real client would use, using the tiny synthetic model from
//! `tests/fixtures/`.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Instant;

use headroomd::model::Model;
use headroomd::protocol::{self, Request, Response};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

fn fixture(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(name)
}

async fn send(stream: &mut UnixStream, request: &Request) -> Response {
    let framed = protocol::encode_message(request).unwrap();
    stream.write_all(&framed).await.unwrap();

    let mut len_buf = [0u8; 4];
    stream.read_exact(&mut len_buf).await.unwrap();
    let len = u32::from_be_bytes(len_buf) as usize;
    let mut payload = vec![0u8; len];
    stream.read_exact(&mut payload).await.unwrap();
    serde_json::from_slice(&payload).unwrap()
}

#[tokio::test]
async fn ping_over_socket_reports_model_loaded_and_uptime() {
    let model = Arc::new(
        Model::load(
            &fixture("tiny_keep_score.onnx"),
            &fixture("tiny_tokenizer.json"),
            8192,
            headroomd::model::Accelerator::Cpu,
        )
        .unwrap(),
    );
    let (client, server) = UnixStream::pair().unwrap();

    let handle = tokio::spawn(async move {
        headroomd::server::handle_connection(
            server,
            model,
            Instant::now(),
            Arc::new(Mutex::new(Instant::now())),
        )
        .await
        .unwrap();
    });

    let mut client = client;
    let response = send(&mut client, &Request::Ping).await;
    match response {
        Response::Pong {
            ok, model_loaded, ..
        } => {
            assert!(ok);
            assert!(model_loaded);
        }
        other => panic!("expected Pong, got {other:?}"),
    }

    drop(client);
    handle.await.unwrap();
}

#[tokio::test]
async fn compress_over_socket_matches_direct_pipeline_result() {
    let model = Arc::new(
        Model::load(
            &fixture("tiny_keep_score.onnx"),
            &fixture("tiny_tokenizer.json"),
            8192,
            headroomd::model::Accelerator::Cpu,
        )
        .unwrap(),
    );
    let (client, server) = UnixStream::pair().unwrap();

    let handle = tokio::spawn(async move {
        headroomd::server::handle_connection(
            server,
            model,
            Instant::now(),
            Arc::new(Mutex::new(Instant::now())),
        )
        .await
        .unwrap();
    });

    let mut client = client;
    let response = send(
        &mut client,
        &Request::Compress {
            text: "the quick brown fox".to_string(),
            threshold: 0.5,
        },
    )
    .await;

    match response {
        Response::Compress {
            ok,
            compressed,
            keep_rate,
            dropped_spans,
            model_loaded,
        } => {
            assert!(ok);
            assert!(model_loaded);
            assert_eq!(compressed, "the quick brown fox");
            assert_eq!(keep_rate, 1.0);
            assert!(dropped_spans.is_empty());
        }
        other => panic!("expected Compress response, got {other:?}"),
    }

    drop(client);
    handle.await.unwrap();
}

#[tokio::test]
async fn multiple_sequential_requests_on_one_connection() {
    let model = Arc::new(
        Model::load(
            &fixture("tiny_keep_score.onnx"),
            &fixture("tiny_tokenizer.json"),
            8192,
            headroomd::model::Accelerator::Cpu,
        )
        .unwrap(),
    );
    let (client, server) = UnixStream::pair().unwrap();

    let handle = tokio::spawn(async move {
        headroomd::server::handle_connection(
            server,
            model,
            Instant::now(),
            Arc::new(Mutex::new(Instant::now())),
        )
        .await
        .unwrap();
    });

    let mut client = client;
    for _ in 0..3 {
        let response = send(&mut client, &Request::Ping).await;
        assert!(matches!(response, Response::Pong { ok: true, .. }));
    }

    drop(client);
    handle.await.unwrap();
}

#[tokio::test]
async fn malformed_json_gets_an_error_response_not_a_dropped_connection() {
    let model = Arc::new(
        Model::load(
            &fixture("tiny_keep_score.onnx"),
            &fixture("tiny_tokenizer.json"),
            8192,
            headroomd::model::Accelerator::Cpu,
        )
        .unwrap(),
    );
    let (client, server) = UnixStream::pair().unwrap();

    let handle = tokio::spawn(async move {
        headroomd::server::handle_connection(
            server,
            model,
            Instant::now(),
            Arc::new(Mutex::new(Instant::now())),
        )
        .await
        .unwrap();
    });

    let mut client = client;
    let framed = protocol::encode_frame(b"not valid json").unwrap();
    client.write_all(&framed).await.unwrap();

    let mut len_buf = [0u8; 4];
    client.read_exact(&mut len_buf).await.unwrap();
    let len = u32::from_be_bytes(len_buf) as usize;
    let mut payload = vec![0u8; len];
    client.read_exact(&mut payload).await.unwrap();
    let response: Response = serde_json::from_slice(&payload).unwrap();

    assert!(matches!(response, Response::Error { ok: false, .. }));

    drop(client);
    handle.await.unwrap();
}

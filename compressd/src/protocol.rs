//! IPC wire protocol: framing and JSON message shapes.
//!
//! Transport is a length-prefixed JSON stream: every message (request or
//! response, in either direction) is a 4-byte big-endian `u32` giving the
//! byte length of the UTF-8 JSON payload that immediately follows.
//!
//! This module intentionally keeps the framing logic free of any I/O so it
//! can be unit tested without a socket. The async read/write helpers built
//! on top of it live in `server.rs`.

use serde::{Deserialize, Serialize};

/// Maximum accepted frame payload size (16 MiB). This bounds how much memory
/// a single (possibly hostile or buggy) client can force us to allocate
/// before we've even looked at the JSON.
pub const MAX_FRAME_LEN: u32 = 16 * 1024 * 1024;

/// Default keep/drop probability threshold used when a `compress` request
/// omits the `threshold` field.
pub const DEFAULT_THRESHOLD: f32 = 0.5;

fn default_threshold() -> f32 {
    DEFAULT_THRESHOLD
}

/// A request read off the socket.
#[derive(Debug, Clone, PartialEq, Deserialize, Serialize)]
#[serde(tag = "method", rename_all = "lowercase")]
pub enum Request {
    Compress {
        text: String,
        #[serde(default = "default_threshold")]
        threshold: f32,
    },
    Ping,
}

/// A `[start, end)` byte range into the original input text.
pub type ByteSpan = [usize; 2];

/// A response written to the socket.
///
/// Serialization is untagged: each variant serializes to the flat JSON shape
/// described in the protocol spec, discriminated only by which fields are
/// present (`ok` plus either `error`, or `compressed`/`keep_rate`/
/// `dropped_spans`, or `uptime_secs`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum Response {
    Compress {
        ok: bool,
        compressed: String,
        keep_rate: f32,
        dropped_spans: Vec<ByteSpan>,
        model_loaded: bool,
    },
    Pong {
        ok: bool,
        model_loaded: bool,
        uptime_secs: u64,
    },
    Error {
        ok: bool,
        error: String,
    },
}

impl Response {
    pub fn compress(
        compressed: String,
        keep_rate: f32,
        dropped_spans: Vec<ByteSpan>,
        model_loaded: bool,
    ) -> Self {
        Response::Compress {
            ok: true,
            compressed,
            keep_rate,
            dropped_spans,
            model_loaded,
        }
    }

    pub fn pong(model_loaded: bool, uptime_secs: u64) -> Self {
        Response::Pong {
            ok: true,
            model_loaded,
            uptime_secs,
        }
    }

    pub fn error(message: impl Into<String>) -> Self {
        Response::Error {
            ok: false,
            error: message.into(),
        }
    }
}

/// Errors that can occur while framing a message.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum FrameError {
    #[error("frame length {0} exceeds max allowed frame length {MAX_FRAME_LEN}")]
    TooLarge(u32),
    #[error("payload is not valid UTF-8: {0}")]
    InvalidUtf8(String),
}

/// Encode a JSON payload into a length-prefixed frame ready to write to the
/// socket.
pub fn encode_frame(payload: &[u8]) -> Result<Vec<u8>, FrameError> {
    let len: u32 = payload
        .len()
        .try_into()
        .map_err(|_| FrameError::TooLarge(u32::MAX))?;
    if len > MAX_FRAME_LEN {
        return Err(FrameError::TooLarge(len));
    }
    let mut buf = Vec::with_capacity(4 + payload.len());
    buf.extend_from_slice(&len.to_be_bytes());
    buf.extend_from_slice(payload);
    Ok(buf)
}

/// Serialize `value` to JSON and encode it into a length-prefixed frame.
pub fn encode_message<T: Serialize>(value: &T) -> Result<Vec<u8>, anyhow::Error> {
    let payload = serde_json::to_vec(value)?;
    Ok(encode_frame(&payload)?)
}

/// Parse a 4-byte big-endian length prefix, validating it against
/// [`MAX_FRAME_LEN`].
pub fn decode_len_prefix(bytes: [u8; 4]) -> Result<u32, FrameError> {
    let len = u32::from_be_bytes(bytes);
    if len > MAX_FRAME_LEN {
        return Err(FrameError::TooLarge(len));
    }
    Ok(len)
}

/// Deserialize a raw JSON payload (with the length prefix already stripped)
/// into a `Request`.
pub fn parse_request(payload: &[u8]) -> Result<Request, anyhow::Error> {
    Ok(serde_json::from_slice(payload)?)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compress_request_round_trips() {
        let req = Request::Compress {
            text: "hello world".to_string(),
            threshold: 0.7,
        };
        let json = serde_json::to_string(&req).unwrap();
        let back: Request = serde_json::from_str(&json).unwrap();
        assert_eq!(req, back);
    }

    #[test]
    fn compress_request_parses_spec_shape() {
        let json = r#"{"method": "compress", "text": "string to compress", "threshold": 0.5}"#;
        let req: Request = serde_json::from_str(json).unwrap();
        assert_eq!(
            req,
            Request::Compress {
                text: "string to compress".to_string(),
                threshold: 0.5,
            }
        );
    }

    #[test]
    fn compress_request_threshold_defaults() {
        let json = r#"{"method": "compress", "text": "hi"}"#;
        let req: Request = serde_json::from_str(json).unwrap();
        assert_eq!(
            req,
            Request::Compress {
                text: "hi".to_string(),
                threshold: DEFAULT_THRESHOLD,
            }
        );
    }

    #[test]
    fn ping_request_parses_spec_shape() {
        let json = r#"{"method": "ping"}"#;
        let req: Request = serde_json::from_str(json).unwrap();
        assert_eq!(req, Request::Ping);
    }

    #[test]
    fn unknown_method_fails_to_parse() {
        let json = r#"{"method": "explode"}"#;
        let result: Result<Request, _> = serde_json::from_str(json);
        assert!(result.is_err());
    }

    #[test]
    fn compress_response_matches_spec_shape() {
        let resp = Response::compress("string".to_string(), 0.81, vec![[12, 45], [80, 102]], true);
        let json = serde_json::to_value(&resp).unwrap();
        assert_eq!(
            json,
            serde_json::json!({
                "ok": true,
                "compressed": "string",
                "keep_rate": 0.81_f32 as f64,
                "dropped_spans": [[12, 45], [80, 102]],
                "model_loaded": true,
            })
        );
    }

    #[test]
    fn pong_response_matches_spec_shape() {
        let resp = Response::pong(true, 123);
        let json = serde_json::to_value(&resp).unwrap();
        assert_eq!(
            json,
            serde_json::json!({
                "ok": true,
                "model_loaded": true,
                "uptime_secs": 123,
            })
        );
    }

    #[test]
    fn error_response_matches_spec_shape() {
        let resp = Response::error("boom");
        let json = serde_json::to_value(&resp).unwrap();
        assert_eq!(json, serde_json::json!({"ok": false, "error": "boom"}));
    }

    #[test]
    fn encode_frame_prefixes_length() {
        let payload = br#"{"ok":true}"#;
        let framed = encode_frame(payload).unwrap();
        assert_eq!(&framed[0..4], &(payload.len() as u32).to_be_bytes());
        assert_eq!(&framed[4..], payload);
    }

    #[test]
    fn encode_frame_rejects_oversized_payload() {
        let payload = vec![0u8; (MAX_FRAME_LEN + 1) as usize];
        let err = encode_frame(&payload).unwrap_err();
        assert_eq!(err, FrameError::TooLarge(MAX_FRAME_LEN + 1));
    }

    #[test]
    fn decode_len_prefix_rejects_oversized_len() {
        let bytes = (MAX_FRAME_LEN + 1).to_be_bytes();
        let err = decode_len_prefix(bytes).unwrap_err();
        assert_eq!(err, FrameError::TooLarge(MAX_FRAME_LEN + 1));
    }

    #[test]
    fn decode_len_prefix_accepts_valid_len() {
        let bytes = 42u32.to_be_bytes();
        assert_eq!(decode_len_prefix(bytes).unwrap(), 42);
    }

    #[test]
    fn parse_request_rejects_invalid_json() {
        let err = parse_request(b"not json").unwrap_err();
        assert!(!err.to_string().is_empty());
    }

    #[test]
    fn full_round_trip_encode_then_parse() {
        let req = Request::Compress {
            text: "round trip me".to_string(),
            threshold: 0.42,
        };
        let payload = serde_json::to_vec(&req).unwrap();
        let framed = encode_frame(&payload).unwrap();
        let len = decode_len_prefix([framed[0], framed[1], framed[2], framed[3]]).unwrap();
        assert_eq!(len as usize, payload.len());
        let parsed = parse_request(&framed[4..]).unwrap();
        assert_eq!(parsed, req);
    }
}

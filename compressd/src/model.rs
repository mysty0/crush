//! ONNX Runtime + tokenizer inference pipeline.
//!
//! Loads a token-classification ONNX model (expected output shape
//! `[batch, seq_len]`, one P(keep) score per subword token) and a matching
//! HuggingFace `tokenizer.json`, then scores arbitrary input text.
//!
//! # Long-input chunking strategy
//!
//! Inputs longer than `max_length` tokens (default 8192, matching
//! ModernBERT's context window) are **not** truncated. Instead the token
//! stream is split into consecutive chunks that each fit within
//! `max_length` once the tokenizer's special tokens (e.g. `[CLS]`/`[SEP]`)
//! are added back, each chunk is run through the model independently, and
//! the resulting per-token keep scores are concatenated back together in
//! original order. This means every token in the input gets a real model
//! score (no blind truncation drops the tail of long documents silently),
//! at the cost of losing cross-chunk attention context at chunk boundaries.
//! This tradeoff is a deliberate choice for a compression pre-pass: missing
//! a small amount of cross-chunk context is preferable to hard-truncating
//! (and therefore always fully keeping, uncompressed) the end of long
//! inputs.

use std::path::Path;

use anyhow::{Context, Result};
use ort::session::Session;
use ort::value::Tensor;
use tokenizers::Tokenizer;
use tokio::sync::Mutex;

use crate::compress::TokenScore;

/// Which compute backend a [`Model`] should run inference on.
///
/// `Cpu` always works. `Gpu` requests the ONNX Runtime CUDA execution
/// provider on the given device id; whether it's actually available
/// depends on this binary having been built with the `gpu` Cargo feature
/// (which pulls in `ort`'s `cuda` feature) and a working CUDA/cuDNN
/// installation on the machine at run time. See [`Model::load`] for how a
/// `Gpu` request that can't be satisfied is handled.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Accelerator {
    Cpu,
    Gpu { device_id: i32 },
}

impl Accelerator {
    /// Build an `Accelerator` from the daemon's `--gpu`/`--gpu-device-id`
    /// CLI flags.
    pub fn from_args(gpu: bool, device_id: i32) -> Self {
        if gpu {
            Accelerator::Gpu { device_id }
        } else {
            Accelerator::Cpu
        }
    }
}

/// A loaded model + tokenizer pair, ready to score text.
pub struct Model {
    session: Mutex<Session>,
    tokenizer: Tokenizer,
    prefix_ids: Vec<u32>,
    suffix_ids: Vec<u32>,
    max_length: usize,
}

impl Model {
    /// Load the ONNX model and tokenizer from disk. This performs the
    /// (potentially slow) session creation and should be called once at
    /// daemon startup, before accepting connections.
    ///
    /// `accelerator` selects CPU (always available) or GPU inference.
    /// Requesting `Accelerator::Gpu` on a binary that was not built with
    /// the `gpu` Cargo feature is a hard error at startup, not a silent
    /// CPU fallback: a caller who asked for GPU and got CPU without being
    /// told would draw the wrong conclusion about their setup (e.g. "the
    /// GPU build works but this machine's CUDA install must be broken"
    /// when actually the binary never had CUDA support compiled in at
    /// all). Within a `gpu`-featured build, a genuine registration
    /// failure (missing CUDA/cuDNN libraries, unsupported device, etc.)
    /// is also surfaced as an error for the same reason -- see
    /// [`build_session`].
    pub fn load(
        model_path: &Path,
        tokenizer_path: &Path,
        max_length: usize,
        accelerator: Accelerator,
    ) -> Result<Self> {
        let tokenizer = Tokenizer::from_file(tokenizer_path).map_err(|e| {
            anyhow::anyhow!(
                "failed to load tokenizer from {}: {e}",
                tokenizer_path.display()
            )
        })?;
        let (prefix_ids, suffix_ids) = discover_special_wrapper(&tokenizer)?;
        let min_max_length = prefix_ids.len() + suffix_ids.len() + 1;

        let session = build_session(accelerator)
            .context("failed to create ONNX Runtime session builder")?
            .commit_from_file(model_path)
            .with_context(|| format!("failed to load ONNX model from {}", model_path.display()))?;

        Ok(Self {
            session: Mutex::new(session),
            tokenizer,
            prefix_ids,
            suffix_ids,
            max_length: max_length.max(min_max_length),
        })
    }

    /// Score every subword token in `text`, returning them in original-text
    /// order with byte offsets into `text`.
    pub async fn score(&self, text: &str) -> Result<Vec<TokenScore>> {
        if text.is_empty() {
            return Ok(Vec::new());
        }

        let encoding = self
            .tokenizer
            .encode(text, false)
            .map_err(|e| anyhow::anyhow!("tokenization failed: {e}"))?;
        let ids = encoding.get_ids();
        let offsets = encoding.get_offsets();

        let content_budget = self.max_length - self.prefix_ids.len() - self.suffix_ids.len();
        let mut scores =
            Vec::with_capacity(ids.len() + 2 * self.prefix_ids.len().max(self.suffix_ids.len()));

        for chunk in chunk_token_ids(ids.len(), content_budget) {
            let chunk_ids = &ids[chunk.clone()];
            let chunk_offsets = &offsets[chunk.clone()];
            let input_ids = wrap_with_specials(&self.prefix_ids, chunk_ids, &self.suffix_ids);
            let keep_scores = self.run_session(&input_ids).await?;

            debug_assert_eq!(keep_scores.len(), input_ids.len());

            for _ in &self.prefix_ids {
                scores.push(TokenScore::special(0, 0));
            }
            for (i, &(start, end)) in chunk_offsets.iter().enumerate() {
                let score = keep_scores[self.prefix_ids.len() + i];
                scores.push(TokenScore::new(start, end, score));
            }
            for _ in &self.suffix_ids {
                scores.push(TokenScore::special(0, 0));
            }
        }

        Ok(scores)
    }

    async fn run_session(&self, input_ids: &[u32]) -> Result<Vec<f32>> {
        let seq_len = input_ids.len();
        let ids_i64: Vec<i64> = input_ids.iter().map(|&id| id as i64).collect();
        let attention_mask: Vec<i64> = vec![1; seq_len];
        let shape = vec![1i64, seq_len as i64];

        let mut session = self.session.lock().await;

        let needs_token_type = session.inputs().iter().any(|i| i.name() == "token_type_ids");

        let input_ids_tensor = Tensor::from_array((shape.clone(), ids_i64))
            .context("failed to build input_ids tensor")?;
        let attention_mask_tensor = Tensor::from_array((shape.clone(), attention_mask))
            .context("failed to build attention_mask tensor")?;

        let mut inputs: Vec<(std::borrow::Cow<str>, ort::session::SessionInputValue)> = vec![
            ("input_ids".into(), input_ids_tensor.into()),
            ("attention_mask".into(), attention_mask_tensor.into()),
        ];
        if needs_token_type {
            let token_type_ids: Vec<i64> = vec![0; seq_len];
            let token_type_tensor = Tensor::from_array((shape, token_type_ids))
                .context("failed to build token_type_ids tensor")?;
            inputs.push(("token_type_ids".into(), token_type_tensor.into()));
        }

        let outputs = session
            .run(inputs)
            .context("ONNX Runtime inference failed")?;
        let (_shape, data) = outputs[0]
            .try_extract_tensor::<f32>()
            .context("failed to extract keep-score tensor from model output")?;

        Ok(data.to_vec())
    }

    /// Whether the model was loaded successfully. Always `true` for a live
    /// `Model` instance -- this exists so `ping` and error paths can report
    /// `model_loaded: false` uniformly without an `Option<Model>` at every
    /// call site.
    pub fn is_loaded(&self) -> bool {
        true
    }
}

/// Build a `SessionBuilder` configured for the requested accelerator.
///
/// `Accelerator::Gpu` on a build without the `gpu` Cargo feature is a
/// hard error (see [`Model::load`]'s doc comment for why this must not
/// silently fall back to CPU). Within a `gpu`-featured build, the CUDA
/// execution provider is registered with `error_on_failure()` for the
/// same reason: a registration failure (missing CUDA/cuDNN runtime
/// libraries, an unsupported device id, a CUDA/ORT version mismatch)
/// must surface as a startup error, not a quiet, unnoticed fallback to a
/// working-but-much-slower CPU session.
#[cfg(feature = "gpu")]
fn build_session(accelerator: Accelerator) -> Result<ort::session::builder::SessionBuilder> {
    let builder = Session::builder().context("failed to create ONNX Runtime session builder")?;
    match accelerator {
        Accelerator::Cpu => Ok(builder),
        Accelerator::Gpu { device_id } => {
            // Cap the CUDA memory arena at 2GB. ORT's CUDA arena only
            // grows, never shrinks, for the life of the process, and
            // our workload sees a new tool-output length on nearly every
            // call -- each distinct shape can need a differently-sized
            // scratch buffer the arena hasn't seen before, so without a
            // cap it converges toward the union of every distinct buffer
            // size ever requested over the daemon's lifetime rather than
            // any single call's actual cost. Confirmed via controlled
            // A/B testing (varying conv-algorithm search, memory
            // pattern, and arena extend strategy each independently made
            // no difference to this growth) that a memory limit is the
            // one setting that directly bounds it, rather than changing
            // how the arena grows.
            let cuda = ort::ep::CUDA::default()
                .with_device_id(device_id)
                .with_memory_limit(2 * 1024 * 1024 * 1024)
                .build()
                .error_on_failure();
            builder.with_execution_providers([cuda]).map_err(|e| {
                anyhow::anyhow!(
                    "failed to register CUDA execution provider on device {device_id}: {}",
                    e.message()
                )
            })
        }
    }
}

#[cfg(not(feature = "gpu"))]
fn build_session(accelerator: Accelerator) -> Result<ort::session::builder::SessionBuilder> {
    match accelerator {
        Accelerator::Cpu => {
            Session::builder().context("failed to create ONNX Runtime session builder")
        }
        Accelerator::Gpu { .. } => Err(anyhow::anyhow!(
            "--gpu was requested but this headroomd binary was built without the \
             `gpu` Cargo feature (no CUDA execution provider support compiled in); \
             rebuild with `cargo build --features gpu` or drop --gpu to run on CPU"
        )),
    }
}

/// Split `total_tokens` into consecutive `[start, end)` ranges of at most
/// `chunk_size` tokens each. `chunk_size` of `0` is treated as `1` to
/// guarantee forward progress.
pub fn chunk_token_ids(total_tokens: usize, chunk_size: usize) -> Vec<std::ops::Range<usize>> {
    let chunk_size = chunk_size.max(1);
    if total_tokens == 0 {
        return Vec::new();
    }
    let mut chunks = Vec::with_capacity(total_tokens.div_ceil(chunk_size));
    let mut start = 0;
    while start < total_tokens {
        let end = (start + chunk_size).min(total_tokens);
        chunks.push(start..end);
        start = end;
    }
    chunks
}

/// Prepend `prefix` and append `suffix` special-token ids around a content
/// chunk, producing the full `input_ids` sequence to feed the model.
pub fn wrap_with_specials(prefix: &[u32], content: &[u32], suffix: &[u32]) -> Vec<u32> {
    let mut out = Vec::with_capacity(prefix.len() + content.len() + suffix.len());
    out.extend_from_slice(prefix);
    out.extend_from_slice(content);
    out.extend_from_slice(suffix);
    out
}

/// Discover which token ids the tokenizer wraps content with when
/// `add_special_tokens` is enabled (e.g. `[CLS] ... [SEP]` for BERT-style
/// tokenizers, `<s> ... </s>` for RoBERTa-style ones), by encoding a short
/// placeholder string and inspecting the special-tokens mask.
///
/// This avoids hardcoding any particular tokenizer's special token names or
/// count, so the same code works whether the eventual production tokenizer
/// wraps content with one token on each side, several, or none.
fn discover_special_wrapper(tokenizer: &Tokenizer) -> Result<(Vec<u32>, Vec<u32>)> {
    let encoding = tokenizer
        .encode("x", true)
        .map_err(|e| anyhow::anyhow!("failed to probe tokenizer special tokens: {e}"))?;
    let ids = encoding.get_ids();
    let mask = encoding.get_special_tokens_mask();
    debug_assert_eq!(ids.len(), mask.len());

    let mut prefix_end = 0;
    while prefix_end < mask.len() && mask[prefix_end] == 1 {
        prefix_end += 1;
    }
    let mut suffix_start = mask.len();
    while suffix_start > prefix_end && mask[suffix_start - 1] == 1 {
        suffix_start -= 1;
    }

    Ok((ids[..prefix_end].to_vec(), ids[suffix_start..].to_vec()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn chunk_token_ids_splits_evenly() {
        let chunks = chunk_token_ids(10, 4);
        assert_eq!(chunks, vec![0..4, 4..8, 8..10]);
    }

    #[test]
    fn chunk_token_ids_single_chunk_when_under_budget() {
        let chunks = chunk_token_ids(3, 8);
        assert_eq!(chunks, vec![0..3]);
    }

    #[test]
    fn chunk_token_ids_empty_input_yields_no_chunks() {
        let chunks = chunk_token_ids(0, 8);
        assert_eq!(chunks, Vec::<std::ops::Range<usize>>::new());
    }

    #[test]
    fn chunk_token_ids_exact_multiple() {
        let chunks = chunk_token_ids(8, 4);
        assert_eq!(chunks, vec![0..4, 4..8]);
    }

    #[test]
    fn chunk_token_ids_zero_chunk_size_still_progresses() {
        let chunks = chunk_token_ids(3, 0);
        assert_eq!(chunks, vec![0..1, 1..2, 2..3]);
    }

    #[test]
    fn wrap_with_specials_places_prefix_and_suffix() {
        let wrapped = wrap_with_specials(&[101], &[10, 11, 12], &[102]);
        assert_eq!(wrapped, vec![101, 10, 11, 12, 102]);
    }

    #[test]
    fn wrap_with_specials_handles_empty_wrapper() {
        let wrapped = wrap_with_specials(&[], &[10, 11], &[]);
        assert_eq!(wrapped, vec![10, 11]);
    }
}

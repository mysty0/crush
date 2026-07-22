//! Pure, model-free logic for turning per-token keep scores into a
//! compressed string plus the byte spans that were dropped from the
//! original text.
//!
//! This is deliberately independent of `ort`/`tokenizers` so it can be
//! exhaustively unit tested with synthetic token spans, without needing a
//! real ONNX model on disk.

use crate::protocol::ByteSpan;

/// A single subword token's position in the original text plus the model's
/// keep-probability score for it.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct TokenScore {
    /// Byte offset of the token's first byte in the original text.
    pub start: usize,
    /// Byte offset one past the token's last byte in the original text.
    pub end: usize,
    /// P(keep) in `[0, 1]` as produced by the model.
    pub keep_score: f32,
    /// Whether this is a tokenizer-added special token (e.g. CLS/SEP) that
    /// has no corresponding span in the original text and should always be
    /// kept without affecting `keep_rate` or contributing text.
    pub is_special: bool,
}

impl TokenScore {
    pub fn new(start: usize, end: usize, keep_score: f32) -> Self {
        Self {
            start,
            end,
            keep_score,
            is_special: false,
        }
    }

    pub fn special(start: usize, end: usize) -> Self {
        Self {
            start,
            end,
            keep_score: 1.0,
            is_special: true,
        }
    }

    /// A token has no real span in the original text (typically a special
    /// token emitted by the tokenizer with a zero-width offset).
    fn is_empty_span(&self) -> bool {
        self.end <= self.start
    }
}

/// The result of applying a keep/drop threshold to a sequence of
/// [`TokenScore`]s over some original text.
#[derive(Debug, Clone, PartialEq)]
pub struct CompressionResult {
    pub compressed: String,
    pub keep_rate: f32,
    pub dropped_spans: Vec<ByteSpan>,
}

/// Build a [`CompressionResult`] from `original` text and its per-token
/// scores.
///
/// Tokens are expected to be in original-text order with non-decreasing
/// `start` offsets (as produced by a HuggingFace tokenizer's offset
/// mapping); this is not re-sorted here.
///
/// Algorithm:
/// 1. A token is "kept" if it is special, or its `keep_score >= threshold`.
/// 2. Consecutive dropped tokens whose spans touch or overlap are merged
///    into a single `[start, end)` entry in `dropped_spans`, so the result
///    reflects contiguous *runs* of dropped text rather than one entry per
///    token.
/// 3. The compressed string is defined as the original text with every
///    `dropped_spans` range removed -- i.e. `compressed` and
///    `dropped_spans` are two views of the same decision, and either can
///    be derived from the other. Concretely this means any original bytes
///    that aren't covered by *any* token (e.g. the whitespace between
///    tokens for a tokenizer whose offsets don't include leading/trailing
///    space) are preserved rather than silently dropped, since they were
///    never part of a dropped token's span.
/// 4. `keep_rate` is `kept / total`, counting only tokens with a non-empty
///    span (specials with empty spans don't count either way). If there are
///    no such tokens, `keep_rate` is `1.0` and nothing is dropped.
pub fn build_compression(
    original: &str,
    tokens: &[TokenScore],
    threshold: f32,
) -> CompressionResult {
    let mut dropped_spans: Vec<ByteSpan> = Vec::new();
    let mut kept_real = 0usize;
    let mut total_real = 0usize;

    for token in tokens {
        if token.is_empty_span() {
            continue;
        }
        total_real += 1;

        let keep = token.is_special || token.keep_score >= threshold;
        if keep {
            kept_real += 1;
            continue;
        }

        // Dropped, non-empty span: extend the last dropped run if it's
        // contiguous (or overlapping), otherwise start a new one.
        match dropped_spans.last_mut() {
            Some(last) if token.start <= last[1] => {
                last[1] = last[1].max(token.end);
            }
            _ => dropped_spans.push([token.start, token.end]),
        }
    }

    let keep_rate = if total_real == 0 {
        1.0
    } else {
        kept_real as f32 / total_real as f32
    };

    let compressed = remove_spans(original, &dropped_spans);

    CompressionResult {
        compressed,
        keep_rate,
        dropped_spans,
    }
}

/// Return `original` with every `[start, end)` range in `spans` removed.
/// `spans` must be sorted and non-overlapping (as produced by
/// [`build_compression`]'s merge step).
fn remove_spans(original: &str, spans: &[ByteSpan]) -> String {
    let mut compressed = String::with_capacity(original.len());
    let mut cursor = 0usize;
    for span in spans {
        if span[0] > cursor {
            compressed.push_str(&original[cursor..span[0]]);
        }
        cursor = cursor.max(span[1]);
    }
    if cursor < original.len() {
        compressed.push_str(&original[cursor..]);
    }
    compressed
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keeps_everything_above_threshold() {
        let text = "the quick fox";
        let tokens = vec![
            TokenScore::new(0, 3, 0.9),  // "the"
            TokenScore::new(3, 9, 0.8),  // " quick"
            TokenScore::new(9, 13, 0.7), // " fox"
        ];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "the quick fox");
        assert_eq!(result.dropped_spans, Vec::<ByteSpan>::new());
        assert_eq!(result.keep_rate, 1.0);
    }

    #[test]
    fn drops_single_low_score_token() {
        let text = "the quick fox";
        let tokens = vec![
            TokenScore::new(0, 3, 0.9),  // "the" kept
            TokenScore::new(3, 9, 0.1),  // " quick" dropped
            TokenScore::new(9, 13, 0.7), // " fox" kept
        ];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "the fox");
        assert_eq!(result.dropped_spans, vec![[3, 9]]);
        assert!((result.keep_rate - 2.0 / 3.0).abs() < 1e-6);
    }

    #[test]
    fn merges_adjacent_dropped_spans() {
        let text = "abcdefghij";
        let tokens = vec![
            TokenScore::new(0, 2, 0.9),  // "ab" kept
            TokenScore::new(2, 4, 0.1),  // "cd" dropped
            TokenScore::new(4, 6, 0.2),  // "ef" dropped, contiguous with previous
            TokenScore::new(6, 8, 0.3),  // "gh" dropped, contiguous with previous
            TokenScore::new(8, 10, 0.9), // "ij" kept
        ];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "abij");
        assert_eq!(result.dropped_spans, vec![[2, 8]]);
        assert!((result.keep_rate - 2.0 / 5.0).abs() < 1e-6);
    }

    #[test]
    fn does_not_merge_nonadjacent_dropped_spans() {
        let text = "abXcdYef";
        let tokens = vec![
            TokenScore::new(0, 2, 0.1), // "ab" dropped
            TokenScore::new(2, 3, 0.9), // "X" kept
            TokenScore::new(3, 5, 0.1), // "cd" dropped
            TokenScore::new(5, 6, 0.9), // "Y" kept
            TokenScore::new(6, 8, 0.1), // "ef" dropped
        ];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "XY");
        assert_eq!(result.dropped_spans, vec![[0, 2], [3, 5], [6, 8]]);
    }

    #[test]
    fn threshold_is_inclusive_at_boundary() {
        let text = "ab";
        let tokens = vec![TokenScore::new(0, 2, 0.5)];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "ab");
        assert_eq!(result.keep_rate, 1.0);
    }

    #[test]
    fn overridable_threshold_changes_decision() {
        let text = "ab";
        let tokens = vec![TokenScore::new(0, 2, 0.6)];
        let low_threshold = build_compression(text, &tokens, 0.5);
        let high_threshold = build_compression(text, &tokens, 0.9);
        assert_eq!(low_threshold.compressed, "ab");
        assert_eq!(high_threshold.compressed, "");
        assert_eq!(high_threshold.dropped_spans, vec![[0, 2]]);
    }

    #[test]
    fn special_tokens_are_always_kept_and_dont_affect_keep_rate() {
        let text = "hi";
        let tokens = vec![
            TokenScore::special(0, 0),  // CLS-like, empty span
            TokenScore::new(0, 2, 0.1), // "hi" dropped
            TokenScore::special(0, 0),  // SEP-like, empty span
        ];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "");
        assert_eq!(result.dropped_spans, vec![[0, 2]]);
        assert_eq!(result.keep_rate, 0.0);
    }

    #[test]
    fn all_tokens_special_yields_full_keep_rate_and_no_drops() {
        let text = "";
        let tokens = vec![TokenScore::special(0, 0), TokenScore::special(0, 0)];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "");
        assert_eq!(result.dropped_spans, Vec::<ByteSpan>::new());
        assert_eq!(result.keep_rate, 1.0);
    }

    #[test]
    fn empty_token_list_preserves_original_text_untouched() {
        let result = build_compression("some text", &[], 0.5);
        assert_eq!(result.compressed, "some text");
        assert_eq!(result.dropped_spans, Vec::<ByteSpan>::new());
        assert_eq!(result.keep_rate, 1.0);
    }

    #[test]
    fn dropped_spans_reference_original_not_compressed_offsets() {
        // Regression test for the spec requirement that dropped_spans index
        // into the ORIGINAL text, not the compressed output.
        let text = "0123456789";
        let tokens = vec![
            TokenScore::new(0, 3, 0.9),  // "012" kept
            TokenScore::new(3, 6, 0.1),  // "345" dropped -- original offset 3..6
            TokenScore::new(6, 10, 0.9), // "6789" kept
        ];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "0126789");
        // Byte range [3,6) is only valid against the ORIGINAL text.
        assert_eq!(
            &text[result.dropped_spans[0][0]..result.dropped_spans[0][1]],
            "345"
        );
    }

    #[test]
    fn preserves_whitespace_between_tokens_whose_offsets_exclude_it() {
        // Word-level tokenizers (unlike BPE-style ones) often report token
        // offsets that don't include surrounding whitespace, e.g. "quick"
        // in "the quick fox" might be offset (4, 9), leaving byte 3..4
        // (the space) uncovered by any token. That gap must survive into
        // the compressed output when neither neighboring token is dropped.
        let text = "the quick fox";
        let tokens = vec![
            TokenScore::new(0, 3, 0.9),   // "the", kept
            TokenScore::new(4, 9, 0.9),   // "quick", kept (note: gap at byte 3)
            TokenScore::new(10, 13, 0.9), // "fox", kept (note: gap at byte 9)
        ];
        let result = build_compression(text, &tokens, 0.5);
        assert_eq!(result.compressed, "the quick fox");
        assert!(result.dropped_spans.is_empty());
    }

    #[test]
    fn dropping_a_word_level_token_leaves_its_own_span_gap_but_keeps_others() {
        let text = "the quick fox";
        let tokens = vec![
            TokenScore::new(0, 3, 0.9),   // "the", kept
            TokenScore::new(4, 9, 0.1),   // "quick", dropped
            TokenScore::new(10, 13, 0.9), // "fox", kept
        ];
        let result = build_compression(text, &tokens, 0.5);
        // Only the token's own span (4..9) is removed; the space before it
        // (byte 3) and after it (byte 9) are untouched since they were
        // never part of any token.
        assert_eq!(result.compressed, "the  fox");
        assert_eq!(result.dropped_spans, vec![[4, 9]]);
    }
}

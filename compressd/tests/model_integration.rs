//! Integration test exercising the full ONNX Runtime + tokenizer pipeline
//! against a tiny, hand-built synthetic model (see
//! `tests/fixtures/gen_fixtures.py` for how the fixtures were produced).
//!
//! The fixture model is **not** a real compression model -- it has no
//! learned weights. It computes `sigmoid(input_ids * attention_mask)`
//! elementwise, giving a deterministic, easy-to-predict `[batch, seq_len]`
//! float output that matches the shape headroomd expects from a real
//! token-classification model. This lets us test the full path -- session
//! creation, tokenization, tensor construction, `session.run`, and output
//! extraction -- without needing network access to download real model
//! weights.
//!
//! What this test does NOT cover: numerical correctness of an actual
//! compression model's predictions (there is no real model available in
//! this environment), and the "drop" branch of the keep/drop decision
//! against a real model's output (the fixture model can only ever produce
//! `sigmoid(id) >= 0.5` for non-negative `id`, so nothing it produces is
//! ever "dropped" at the default threshold). That decision logic is
//! covered exhaustively and independently in `src/compress.rs`'s unit
//! tests using synthetic scores.

use std::path::PathBuf;

use headroomd::compress::build_compression;
use headroomd::model::Model;

fn fixture(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(name)
}

fn load_model(max_length: usize) -> Model {
    Model::load(
        &fixture("tiny_keep_score.onnx"),
        &fixture("tiny_tokenizer.json"),
        max_length,
        headroomd::model::Accelerator::Cpu,
    )
    .expect("failed to load fixture model")
}

#[tokio::test]
async fn scores_tokens_with_expected_offsets_and_specials() {
    let model = load_model(8192);
    let text = "the quick brown fox";

    let scores = model.score(text).await.expect("scoring failed");

    // [CLS] the quick brown fox [SEP]
    assert_eq!(scores.len(), 6);

    assert!(scores[0].is_special);
    assert_eq!((scores[0].start, scores[0].end), (0, 0));

    assert!(scores[5].is_special);
    assert_eq!((scores[5].start, scores[5].end), (0, 0));

    let expected = [("the", 4u32), ("quick", 5), ("brown", 6), ("fox", 7)];
    for (i, (word, vocab_id)) in expected.iter().enumerate() {
        let tok = &scores[i + 1];
        assert!(!tok.is_special);
        assert_eq!(&text[tok.start..tok.end], *word);
        let expected_score = 1.0 / (1.0 + (-(*vocab_id as f32)).exp());
        assert!(
            (tok.keep_score - expected_score).abs() < 1e-5,
            "token {word}: expected keep_score {expected_score}, got {}",
            tok.keep_score
        );
    }
}

#[tokio::test]
async fn full_pipeline_round_trip_keeps_everything_above_threshold() {
    let model = load_model(8192);
    let text = "the quick brown fox";

    let scores = model.score(text).await.expect("scoring failed");
    let result = build_compression(text, &scores, 0.5);

    // Every real token's sigmoid(id) score is well above 0.5 for id >= 4,
    // so nothing should be dropped and the compressed text should equal
    // the original.
    assert_eq!(result.compressed, text);
    assert!(result.dropped_spans.is_empty());
    assert_eq!(result.keep_rate, 1.0);
}

#[tokio::test]
async fn chunks_long_input_and_preserves_token_order() {
    // Force chunking: content budget per chunk is max_length - 2 (for the
    // [CLS]/[SEP] wrapper), so max_length=5 gives a content budget of 3
    // tokens per chunk against our 10-word input.
    let model = load_model(5);
    let text = "the quick brown fox jumps over the lazy dog hello";

    let scores = model.score(text).await.expect("scoring failed");

    // 10 content tokens, chunked into ceil(10/3) = 4 chunks, each wrapped
    // with its own [CLS]/[SEP] pair: 10 content + 4*2 specials = 18.
    assert_eq!(scores.len(), 18);

    // Extract just the non-special tokens and check they reconstruct the
    // original text in order with correct offsets, proving the chunk
    // boundaries didn't drop, duplicate, or reorder anything.
    let real_tokens: Vec<_> = scores.iter().filter(|t| !t.is_special).collect();
    assert_eq!(real_tokens.len(), 10);

    let reconstructed: Vec<&str> = real_tokens.iter().map(|t| &text[t.start..t.end]).collect();
    assert_eq!(
        reconstructed,
        vec!["the", "quick", "brown", "fox", "jumps", "over", "the", "lazy", "dog", "hello"]
    );

    // Every real token should have a plausible (non-NaN, in-range) keep
    // score coming back from the model.
    for tok in &real_tokens {
        assert!(tok.keep_score.is_finite());
        assert!((0.0..=1.0).contains(&tok.keep_score));
    }
}

#[tokio::test]
async fn empty_text_short_circuits_without_calling_the_model() {
    let model = load_model(8192);
    let scores = model.score("").await.expect("scoring empty text failed");
    assert!(scores.is_empty());
}

#[tokio::test]
async fn ping_style_is_loaded_reports_true_once_model_is_constructed() {
    let model = load_model(8192);
    assert!(model.is_loaded());
}

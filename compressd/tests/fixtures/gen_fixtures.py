#!/usr/bin/env python3
"""Generate test fixtures for headroomd's integration test.

This script is NOT part of the crate build -- it's a one-time (or
re-run-when-needed) developer tool that produces two small, checked-in
binary fixtures under `tests/fixtures/`:

  - `tiny_tokenizer.json`: a minimal whitespace/word-level HuggingFace
    tokenizer with BERT-style [CLS]/[SEP]/[UNK]/[PAD] special tokens.
  - `tiny_keep_score.onnx`: a hand-built ONNX graph (no learned weights)
    that takes `input_ids` and `attention_mask` (int64, [batch, seq_len])
    and produces a `keep_scores` tensor (float32, [batch, seq_len]) by
    casting input_ids to float, masking with attention_mask, and squashing
    through a Sigmoid. This is NOT a real compression model -- it exists
    purely to exercise headroomd's ONNX Runtime session creation, input
    tensor construction, inference call, and output tensor extraction
    end-to-end with a real (if trivial) .onnx file, matching the
    [batch, seq_len] float output shape the real production model will
    have.

Regenerate with:

    nix-shell -p python3 python3Packages.onnx python3Packages.tokenizers \
        --run 'python3 tests/fixtures/gen_fixtures.py'
"""

import json
import os

import onnx
from onnx import TensorProto, helper


def gen_tokenizer(path: str) -> None:
    vocab = {
        "[UNK]": 0,
        "[CLS]": 1,
        "[SEP]": 2,
        "[PAD]": 3,
        "the": 4,
        "quick": 5,
        "brown": 6,
        "fox": 7,
        "jumps": 8,
        "over": 9,
        "lazy": 10,
        "dog": 11,
        "hello": 12,
        "world": 13,
    }

    tokenizer = {
        "version": "1.0",
        "truncation": None,
        "padding": None,
        "added_tokens": [
            {
                "id": tid,
                "content": tok,
                "single_word": False,
                "lstrip": False,
                "rstrip": False,
                "normalized": False,
                "special": True,
            }
            for tok, tid in [("[UNK]", 0), ("[CLS]", 1), ("[SEP]", 2), ("[PAD]", 3)]
        ],
        "normalizer": {"type": "BertNormalizer", "clean_text": True, "handle_chinese_chars": True, "strip_accents": None, "lowercase": True},
        "pre_tokenizer": {"type": "Whitespace"},
        "post_processor": {
            "type": "TemplateProcessing",
            "single": [
                {"SpecialToken": {"id": "[CLS]", "type_id": 0}},
                {"Sequence": {"id": "A", "type_id": 0}},
                {"SpecialToken": {"id": "[SEP]", "type_id": 0}},
            ],
            "pair": [
                {"SpecialToken": {"id": "[CLS]", "type_id": 0}},
                {"Sequence": {"id": "A", "type_id": 0}},
                {"SpecialToken": {"id": "[SEP]", "type_id": 0}},
                {"Sequence": {"id": "B", "type_id": 1}},
                {"SpecialToken": {"id": "[SEP]", "type_id": 1}},
            ],
            "special_tokens": {
                "[CLS]": {"id": "[CLS]", "ids": [1], "tokens": ["[CLS]"]},
                "[SEP]": {"id": "[SEP]", "ids": [2], "tokens": ["[SEP]"]},
            },
        },
        "decoder": None,
        "model": {
            "type": "WordLevel",
            "vocab": vocab,
            "unk_token": "[UNK]",
        },
    }

    with open(path, "w") as f:
        json.dump(tokenizer, f, indent=2)


def gen_onnx_model(path: str) -> None:
    batch = "batch"
    seq = "seq_len"

    input_ids = helper.make_tensor_value_info("input_ids", TensorProto.INT64, [batch, seq])
    attention_mask = helper.make_tensor_value_info("attention_mask", TensorProto.INT64, [batch, seq])
    keep_scores = helper.make_tensor_value_info("keep_scores", TensorProto.FLOAT, [batch, seq])

    cast_ids = helper.make_node("Cast", ["input_ids"], ["ids_float"], to=TensorProto.FLOAT)
    cast_mask = helper.make_node("Cast", ["attention_mask"], ["mask_float"], to=TensorProto.FLOAT)
    masked = helper.make_node("Mul", ["ids_float", "mask_float"], ["masked"])
    sigmoid = helper.make_node("Sigmoid", ["masked"], ["keep_scores"])

    graph = helper.make_graph(
        [cast_ids, cast_mask, masked, sigmoid],
        "tiny_keep_score",
        [input_ids, attention_mask],
        [keep_scores],
    )

    model = helper.make_model(graph, producer_name="headroomd-test-fixture-gen", opset_imports=[helper.make_opsetid("", 17)])
    model.ir_version = 9
    onnx.checker.check_model(model)
    onnx.save(model, path)


if __name__ == "__main__":
    here = os.path.dirname(os.path.abspath(__file__))
    gen_tokenizer(os.path.join(here, "tiny_tokenizer.json"))
    gen_onnx_model(os.path.join(here, "tiny_keep_score.onnx"))
    print("Wrote tiny_tokenizer.json and tiny_keep_score.onnx")

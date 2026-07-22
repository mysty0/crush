# headroomd

`headroomd` is a small local IPC daemon, written in Rust, that scores which
subword tokens in a piece of text are safe to drop for **extractive
compression**. It runs an ONNX token-classification model that assigns a
P(keep) score to each token, then a client-supplied threshold decides which
tokens survive. It is designed to be spawned/supervised by a Go process (in
this case, [Crush](https://github.com/charmbracelet/crush)) that talks to it
over a Unix domain socket.

This crate does **not** download or train any model. It expects a local
ONNX model file and a matching HuggingFace `tokenizer.json` to already be
present on disk (fetching/caching those is the supervisor's job).

## Building

```sh
cd compressd
cargo build --release
```

The release binary is written to `target/release/headroomd`.

This crate always loads ONNX Runtime **dynamically at run time**
(`ort`'s `load-dynamic` feature) rather than linking a copy `ort` fetches
at build time (`download-binaries`) -- that fetch needs network access
during the Cargo build, which is incompatible with sandboxed/offline
build environments (Nix, most CI). This means:

- The build itself never needs network access and never depends on any
  particular ONNX Runtime version being available at compile time.
- At **run time**, `--ort-dylib-path` (required) must point at a
  `libonnxruntime.so`/`.dylib`/`.dll` you already have -- e.g. an
  official release from
  [microsoft/onnxruntime](https://github.com/microsoft/onnxruntime/releases),
  or a custom build (see [GPU support](#gpu-support-cuda) below).

### Nix / NixOS

See `flake.nix` at the repo root for a proper Nix packaging of
`headroomd`: `nix build .#headroomd` (CPU) or `nix build
.#headroomd-cuda` (GPU) produce a wrapped binary with its ONNX Runtime
dylib and CUDA/cuDNN runtime libraries already resolved via RPATH/
`autoPatchelfHook` -- no manual `LD_LIBRARY_PATH` or `--ort-dylib-path`
needed at the call site.

## Running

```sh
headroomd --model /path/to/model.onnx --tokenizer /path/to/tokenizer.json \
  --ort-dylib-path /path/to/libonnxruntime.so
```

### CLI flags

| Flag | Default | Description |
| --- | --- | --- |
| `--model <path>` | *(required)* | Path to the ONNX token-classification model. |
| `--tokenizer <path>` | *(required)* | Path to a HuggingFace `tokenizer.json`. |
| `--ort-dylib-path <path>` | *(required)* | Path to `libonnxruntime.so`/`.dylib`/`.dll` to load at runtime. |
| `--socket <path>` | see below | Unix domain socket path to listen on. |
| `--idle-timeout-secs <secs>` | `600` | Exit cleanly after this many seconds with no requests. |
| `--max-length <tokens>` | `8192` | Max tokens fed to the model per inference call; longer inputs are chunked (see below). |
| `--gpu` | `false` | Run inference on an NVIDIA GPU via CUDA. Requires a build with `--features gpu` and an ONNX Runtime build (see `--ort-dylib-path`) with CUDA support compiled in. |
| `--gpu-device-id <id>` | `0` | CUDA device id to use when `--gpu` is set. |

### Default socket path

If `--socket` is not given:

- `$XDG_RUNTIME_DIR/crush/headroomd.sock` if `XDG_RUNTIME_DIR` is set, else
- `/tmp/crush-headroomd-<uid>.sock`

### Lifecycle

- The model is loaded fully before the socket is bound and any connections
  are accepted.
- Idle timeout: if no request is received for `--idle-timeout-secs` seconds,
  the daemon logs a message and exits with status `0`. This is intentional
  and lets a supervisor treat "process exited" as "idle, will respawn on
  next use" rather than as a crash.
- `SIGINT`/`SIGTERM` trigger the same graceful shutdown path: the socket
  file is removed before the process exits.
- A stale socket file left behind by an unclean previous exit is removed
  automatically before binding.
- Logs go to stderr via `tracing`; set `RUST_LOG` (e.g. `RUST_LOG=debug`) to
  adjust verbosity.

### Platform support

Only Unix domain sockets are implemented (Linux, macOS). Windows named-pipe
support is an intentional TODO -- see the module docs in `src/transport.rs`
for what a Windows implementation would need to provide.

## GPU support (CUDA)

Build with `cargo build --release --features gpu` to compile in the ONNX
Runtime CUDA execution provider's registration code, then pass `--gpu`
(and optionally `--gpu-device-id`) at runtime. GPU support additionally
requires an `--ort-dylib-path` build of ONNX Runtime that itself has
CUDA support compiled in -- the `gpu` Cargo feature alone does not
provide one.

**Official prebuilt ONNX Runtime releases may not support your GPU.**
As of ONNX Runtime 1.27.1 (the latest release at time of writing), no
official prebuilt CUDA package includes native kernels for Blackwell/
RTX 50-series GPUs (compute capability `sm_120`) -- using one produces
`cudaErrorNoKernelImageForDevice` at inference time, not at load time.
The fix (native `sm_120` kernels) is merged upstream but not yet
released; if you hit this, build ONNX Runtime from source instead:

```sh
git clone --branch v1.24.2 --depth 1 https://github.com/microsoft/onnxruntime
cd onnxruntime
git submodule update --init --recursive --depth 1
./build.sh --config Release --update --build --parallel "$(nproc)" --nvcc_threads 1 \
  --build_shared_lib --use_cuda \
  --cuda_home /path/to/cuda --cudnn_home /path/to/cudnn \
  --skip_tests --cmake_generator Ninja \
  --cmake_extra_defines CMAKE_CUDA_ARCHITECTURES=120 \
  --cmake_extra_defines onnxruntime_BUILD_UNIT_TESTS=OFF \
  --cmake_extra_defines FETCHCONTENT_TRY_FIND_PACKAGE_MODE=NEVER
```

This produces `build/Linux/Release/libonnxruntime.so`, which you pass to
`headroomd` via `--ort-dylib-path`. Expect this to take 15-30+ minutes
and need real disk space (10-20GB) for build artifacts. On NixOS, use
`nix build .#headroomd-cuda` instead, which handles this end-to-end
(see `flake.nix`).

## IPC protocol

This section is the source of truth for both this Rust daemon and any
client implementation (e.g. a Go client in the main Crush codebase).

**Transport:** a Unix domain socket at the configured path. One connection
may be reused for multiple sequential request/response pairs; only one
request may be in flight per connection at a time (no pipelining in v1).

**Framing:** every message (request or response, either direction) is a
4-byte big-endian `u32` giving the byte length of the UTF-8 JSON payload
that immediately follows:

```
+----------------+------------------------------+
| length (u32be) | JSON payload (length bytes)   |
+----------------+------------------------------+
```

### Requests

Compress some text:

```json
{"method": "compress", "text": "string to compress", "threshold": 0.5}
```

`threshold` is optional and defaults to `0.5` if omitted.

Health check:

```json
{"method": "ping"}
```

### Responses

Successful `compress`:

```json
{
  "ok": true,
  "compressed": "string",
  "keep_rate": 0.81,
  "dropped_spans": [[12, 45], [80, 102]],
  "model_loaded": true
}
```

- `compressed`: the input with dropped spans removed.
- `keep_rate`: fraction of real (non-special) tokens kept, in `[0, 1]`.
- `dropped_spans`: `[start_byte, end_byte)` ranges into the **original**
  input text (not the compressed output) that were dropped. Contiguous
  runs of dropped tokens are merged into a single span. A client can
  reconstruct the compressed text itself by removing these ranges from the
  original, or use the `compressed` field directly.

Successful `ping`:

```json
{"ok": true, "model_loaded": true, "uptime_secs": 123}
```

Error (either method):

```json
{"ok": false, "error": "human readable message"}
```

## Model inference pipeline

- [`ort`](https://docs.rs/ort) (ONNX Runtime bindings) loads the model
  once at startup.
- [`tokenizers`](https://docs.rs/tokenizers) loads the tokenizer and
  produces subword token ids + byte offsets into the original text.
- Inputs to the model are `input_ids` and `attention_mask` (both
  `int64[batch=1, seq_len]`); a `token_type_ids` input of zeros is added
  automatically if the model declares one. The expected output is a single
  tensor of shape `[batch, seq_len]`, one `float32` P(keep) score per
  token.
- A token is kept if it's a tokenizer-added special token (e.g.
  `[CLS]`/`[SEP]`), or if its score is `>= threshold`. The daemon discovers
  what "special token wrapping" a given tokenizer uses (how many tokens it
  adds at the front/back, and their ids) by encoding a short placeholder
  string and inspecting the tokenizer's special-tokens mask, rather than
  hardcoding e.g. BERT's `[CLS]`/`[SEP]` -- this keeps it correct for
  RoBERTa/ModernBERT-style `<s>`/`</s>` wrapping too.

### Long-input chunking

Inputs whose token count exceeds `--max-length` (default 8192, matching
ModernBERT's context window) are **not** truncated. Instead the content
token stream is split into consecutive chunks that each fit within
`max-length` once the tokenizer's special-token wrapper is re-added, each
chunk is run through the model independently, and the resulting per-token
keep scores are concatenated back together in original order. Every token
in the input gets a real model score this way; the tradeoff is that tokens
near a chunk boundary lose cross-chunk attention context, which is judged
preferable to silently and fully keeping (because it was truncated away)
the tail of very long documents.

## Testing

```sh
cargo test
```

- `src/compress.rs` unit-tests the pure keep/drop-scores -> compressed-text
  and dropped-spans logic exhaustively, using synthetic token scores. This
  is fully independent of ONNX Runtime and needs no model file.
- `src/protocol.rs` unit-tests request/response (de)serialization and frame
  encoding/decoding against the exact JSON shapes in this document.
- `src/model.rs` unit-tests the pure chunking/wrapping helpers
  (`chunk_token_ids`, `wrap_with_specials`).
- `src/transport.rs` unit-tests socket binding/cleanup behavior against a
  real (temp-dir) Unix socket.
- `tests/model_integration.rs` and `tests/server_integration.rs` are true
  integration tests that load a real (if tiny and weight-free) `.onnx`
  file through `ort` and run real inference and real socket I/O end to end.
  These use the fixtures in `tests/fixtures/` (see
  `tests/fixtures/gen_fixtures.py` for how they were generated: a
  hand-built ONNX graph computing `sigmoid(input_ids * attention_mask)`
  with a matching tiny word-level tokenizer -- no learned weights are
  needed to exercise session creation, tensor construction, inference, and
  output extraction).

**What is not tested:** numerical correctness of an actual trained
compression model's predictions. No real model weights are available in
this environment (this daemon is deliberately built to not auto-download
weights), so the integration tests can only validate that the *pipeline*
(tokenize -> tensor -> `session.run` -> extract -> chunk-merge ->
keep/drop) is wired correctly, using a deterministic weight-free stand-in
model with the same input/output tensor shapes a real
ModernBERT-based token classifier would have.

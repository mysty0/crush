{
  description = "Crush development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = {
    self,
    nixpkgs,
    flake-utils,
  }:
    flake-utils.lib.eachDefaultSystem (
      system: let
        # CUDA/cuDNN are unfree-licensed; allowUnfree must be set here
        # (rather than relying on the caller's ~/.config/nixpkgs/config.nix
        # or a NIXPKGS_ALLOW_UNFREE env var) so `nix build .#headroomd-cuda`
        # works out of the box for anyone with this flake, matching how
        # the CPU-only packages need no special config at all.
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfree = true;
          config.cudaSupport = true;
        };

        # --- headroomd (compressd/) -------------------------------------
        #
        # headroomd is a dynamically-linked Rust + CUDA daemon (unlike the
        # main `crush` Go binary, which is CGO_ENABLED=0/fully static and
        # so needs no linking help at all). It always loads ONNX Runtime
        # at *run time* via --ort-dylib-path (see compressd/Cargo.toml's
        # `ort` dependency: `load-dynamic`, no `download-binaries`, so the
        # Cargo build itself never needs network access and works fine
        # inside Nix's sandbox). These packages wire a concrete ONNX
        # Runtime build to headroomd via `autoPatchelfHook` (fixes the
        # ELF RPATH so the binary finds its .so dependencies through the
        # Nix store) and `makeWrapper` (bakes in --ort-dylib-path and any
        # extra runtime library paths CUDA needs), so the built package
        # runs correctly with no manual LD_LIBRARY_PATH/env exports at
        # the call site -- the same "just run it" experience as `crush`
        # itself, just achieved differently because the underlying binary
        # actually has shared-library dependencies to resolve.

        headroomdSrc = ./compressd;

        # Official prebuilt CPU-only ONNX Runtime release. Fully
        # reproducible: fetchurl is a fixed-output derivation (the output
        # hash is pinned, so Nix allows the network fetch and verifies
        # the result against it), so this needs no local build step and
        # works identically on any machine/CI.
        onnxruntime-cpu = pkgs.stdenv.mkDerivation {
          pname = "onnxruntime-cpu";
          version = "1.24.2";
          src = pkgs.fetchurl {
            url = "https://github.com/microsoft/onnxruntime/releases/download/v1.24.2/onnxruntime-linux-x64-1.24.2.tgz";
            sha256 = "1ri5mpz7i8idqx575ggg0lh0x1ckcsa1fiv82wp68qsnp9s58wj3";
          };
          nativeBuildInputs = [pkgs.autoPatchelfHook];
          buildInputs = [pkgs.stdenv.cc.cc.lib];
          installPhase = ''
            mkdir -p $out/lib $out/include
            cp -P lib/*.so* $out/lib/
            cp -r include/* $out/include/
          '';
        };

        # GPU / Blackwell (sm_120, e.g. RTX 50-series) support. As of ONNX
        # Runtime 1.27.1 (the latest official release), no prebuilt CUDA
        # package includes native sm_120 kernels -- the fix (PR #29711)
        # is merged upstream but not yet released (targeted for 1.28.0).
        # A prebuilt CUDA package on an sm_120 GPU fails at *inference*
        # time with cudaErrorNoKernelImageForDevice, not at load time.
        #
        # Building ONNX Runtime from source with
        # CMAKE_CUDA_ARCHITECTURES=120 fixes this (see
        # compressd/README.md's "GPU support (CUDA)" section for the
        # exact recipe), but that build fetches several dependencies
        # (abseil, protobuf, onnx, cutlass, cudnn_frontend, ...) over the
        # network via CMake's FetchContent during configure, which a
        # plain (non-fixed-output) Nix derivation cannot do inside the
        # sandbox. Properly hermetic-izing that (vendoring every
        # FetchContent dependency as its own pinned Nix fetch, or wrapping
        # the whole build as a fixed-output derivation with a pinned
        # final-output hash) is real follow-up work.
        #
        # For now, this package imports an already-built copy of that
        # custom ONNX Runtime from a local path (default: the location
        # used when this was built by hand -- see compressd/README.md).
        # This makes the *packaging* (RPATH patching, wrapper, no manual
        # env vars) fully "proper" and Nix-native; only the *source* of
        # the .so files is machine-local/impure until the from-source
        # recipe above is hermeticized. Override via:
        #   HEADROOMD_CUDA_ORT_DIR=/path/to/build/Linux/Release nix build .#headroomd-cuda
        onnxruntimeCudaSrcDir = let
          envOverride = builtins.getEnv "HEADROOMD_CUDA_ORT_DIR";
        in
          if envOverride != ""
          then envOverride
          else "/home/mysty/build/onnxruntime-src/build/Linux/Release";

        # `onnxruntimeCudaSrcDir` is a plain string (Nix's sandboxed
        # builds can't see arbitrary filesystem paths -- only genuine
        # Nix `Path` values get imported into the store and made visible
        # to the sandbox). `/. + <string>` converts it into a real Path,
        # which Nix then imports at evaluation time. A `filter` limits
        # what gets copied to just the .so files we actually need: the
        # source directory is a full ORT build tree (object files,
        # _deps/ with vendored source for abseil/protobuf/onnx/cutlass/
        # etc, ninja metadata) -- likely tens of GB -- and copying all of
        # that into the store just to extract two files would be both
        # extremely slow and pointless.
        onnxruntimeCudaLibs = builtins.path {
          path = /. + onnxruntimeCudaSrcDir;
          name = "onnxruntime-cuda-sm120-libs";
          filter = path: type:
            (type == "regular" || type == "symlink") && pkgs.lib.hasPrefix "libonnxruntime" (baseNameOf path);
        };

        onnxruntime-cuda-sm120 = pkgs.stdenv.mkDerivation {
          pname = "onnxruntime-cuda-sm120";
          version = "1.24.2";
          src = onnxruntimeCudaLibs;
          dontUnpack = true;
          nativeBuildInputs = [pkgs.autoPatchelfHook];
          buildInputs = [
            pkgs.stdenv.cc.cc.lib
            pkgs.cudaPackages.cudatoolkit
            pkgs.cudaPackages.cudnn
          ];
          # autoPatchelf resolves libonnxruntime.so's own direct ELF
          # dependencies against buildInputs above, but
          # libonnxruntime_providers_cuda.so is loaded via dlopen at
          # run time (not a direct dependency ELF can see), so its own
          # transitive CUDA deps (cublasLt, cufft, curand, nvrtc, ...)
          # still need to be resolvable -- autoPatchelf patches that
          # library's RPATH too since it's included in this derivation's
          # output, which is sufficient for the dynamic loader to find
          # them without any LD_LIBRARY_PATH at call time.
          installPhase = ''
            mkdir -p $out/lib
            cp -P $src/* $out/lib/
          '';
        };

        mkHeadroomd = {
          pname,
          cargoFeatures ? [],
          onnxruntime,
          extraLdLibraryPath ? [],
        }:
          pkgs.rustPlatform.buildRustPackage {
            inherit pname;
            version = "0.1.0";
            src = headroomdSrc;
            cargoLock.lockFile = headroomdSrc + "/Cargo.lock";
            buildNoDefaultFeatures = true;
            buildFeatures = cargoFeatures;
            nativeBuildInputs = [pkgs.autoPatchelfHook pkgs.makeWrapper];
            buildInputs = [pkgs.stdenv.cc.cc.lib];
            # headroomd itself doesn't link ONNX Runtime at build time
            # (see the module doc comment above), so there's nothing for
            # autoPatchelf to resolve against onnxruntime's .so files
            # here -- the wrapper below is what actually wires them in,
            # via --ort-dylib-path at invocation.
            postFixup = ''
              wrapProgram $out/bin/headroomd \
                --add-flags "--ort-dylib-path ${onnxruntime}/lib/libonnxruntime.so" \
                --prefix LD_LIBRARY_PATH : "${onnxruntime}/lib${pkgs.lib.optionalString (extraLdLibraryPath != []) (":" + pkgs.lib.makeLibraryPath extraLdLibraryPath)}"
            '';
            doCheck = false;
          };
      in {
        packages = {
          headroomd = mkHeadroomd {
            pname = "headroomd";
            onnxruntime = onnxruntime-cpu;
          };
          headroomd-cuda = mkHeadroomd {
            pname = "headroomd-cuda";
            cargoFeatures = ["gpu"];
            onnxruntime = onnxruntime-cuda-sm120;
            extraLdLibraryPath = [pkgs.cudaPackages.cudatoolkit pkgs.cudaPackages.cudnn];
          };
          inherit onnxruntime-cpu onnxruntime-cuda-sm120;
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            # Go toolchain
            go_1_26

            # Development tools
            gopls # Go language server
            golangci-lint # Linter
            gofumpt # Formatter (stricter than gofmt)
            go-task # Task runner
            delve # Go debugger

            # Additional tools
            git # Version control
            gh # GitHub CLI
            svu # Semantic version utility
            sqlc # SQL code generator
          ];

          shellHook = ''
            # Set Go environment variables
            export CGO_ENABLED=0
          '';
        };
      }
    );
}

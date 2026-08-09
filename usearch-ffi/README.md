# Bluge vector engine native adapter

This crate exposes the USearch vector engine through a stable C ABI. USearch
owns HNSW construction, search, updates, compaction, distance calculation, and
serialization. Bluge owns document mapping, filtering, hybrid search, and the
embedded zapx vector section.

The Go module does not invoke Cargo, a C++ compiler, or cgo during `go build`.
Native libraries are built independently by GitHub Actions and loaded at
runtime. The Windows integration uses `purego` and remains compatible with
`CGO_ENABLED=0`.

## Implementation boundary

USearch is a C++ engine. Its official Rust crate packages the C++ sources,
Rust API, and Cargo build integration; it is not a Rust reimplementation of
the search algorithm. This adapter depends on that crate and adds the stable
`bluge_usearch_*` C ABI consumed by Bluge.

The Rust layer handles ownership and error conversion outside the search hot
loop. Vector data and result buffers cross the boundary without per-element
translation.

## GitHub Actions artifacts

The `Vector Engine Native Libraries` workflow builds the following 64-bit
artifacts:

| Artifact | Runner architecture | Native library |
| --- | --- | --- |
| `vector-engine-windows-amd64` | Windows x86-64 | `vector_engine.dll` |
| `vector-engine-windows-arm64` | Windows ARM64 | `vector_engine.dll` |
| `vector-engine-linux-amd64` | Linux x86-64 | `libvector_engine.so` |
| `vector-engine-linux-arm64` | Linux ARM64 | `libvector_engine.so` |
| `vector-engine-darwin-amd64` | macOS x86-64 | `libvector_engine.dylib` |
| `vector-engine-darwin-arm64` | macOS ARM64 | `libvector_engine.dylib` |

Each artifact also contains `manifest.json` and a SHA256 file. The manifest
records the ABI version, resolved USearch version, Rust target, runtime model,
and compiled/runtime-available SIMD families.

32-bit Windows, 32-bit Linux, and 32-bit ARM are intentionally unsupported.

There is exactly one native library for each OS/architecture pair. Windows is
built only with the matching official LLVM 20.1.8 Windows package for the MSVC
ABI, Linux only with GCC 13, and macOS only with the Apple Clang toolchain in
Xcode 16.4. All targets use Rust 1.90.0. The workflow does not produce
alternative GCC, Clang, or Zig builds for the same target.

## Runtime requirements

Windows artifacts use the MSVC ABI and dynamically link the Microsoft Visual
C++ Redistributable 2015-2022 (14.x). Linux artifacts target glibc and use the
system C++ runtime. macOS artifacts use the system C++ runtime supplied by
macOS.

NumKong is enabled on every supported target. Its scalar fallback is always
present, while architecture-specific kernels are selected at runtime inside
that one library. Baseline, SSE, AVX, AVX2, and other ISA families are not
packaged as separate downloads.

## Versioning

`Cargo.lock` pins the exact USearch dependency compiled into an artifact. The
C ABI has its own version returned by `bluge_usearch_abi_version`; Bluge rejects
libraries with an incompatible ABI before opening an index.

The workflow currently uploads Actions artifacts. Attaching the same validated
artifacts to GitHub Releases is intentionally a separate publishing step.

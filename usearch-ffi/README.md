# Bluge vector engine native adapter

This crate exposes the USearch vector engine through a stable C ABI. USearch
owns HNSW construction, search, updates, compaction, distance calculation, and
serialization. Bluge owns document mapping, filtering, hybrid search, and the
embedded zapx vector section.

The Go module does not invoke Cargo, a C++ compiler, or cgo during `go build`.
Native libraries are built independently by GitHub Actions and loaded at
runtime. All supported Go integrations use `purego` and remain compatible with
`CGO_ENABLED=0`.

## Implementation boundary

USearch is a C++ engine. Its official Rust crate packages the C++ sources,
Rust API, and Cargo build integration; it is not a Rust reimplementation of
the search algorithm. This adapter depends on that crate and adds the stable
`bluge_usearch_*` C ABI consumed by Bluge.

The Rust layer handles ownership and error conversion outside the search hot
loop. Vector data and result buffers cross the boundary without per-element
translation. For filtered queries, Go passes candidate keys as one array and
Rust builds the `HashSet` consumed by USearch's predicate; native code never
calls back into Go.

ABI 3 also accepts row-major vector additions and key removals in batches. The
Go adapter bounds each addition call to roughly 4 MiB of `float32` values and
at most 4096 rows; USearch still owns each HNSW insertion, but the process no
longer pays one Go-to-native transition per vector.

## Loading from Go

The shared Go binding uses `purego.RegisterLibFunc` for typed C calls on every
supported platform. Only opening and closing the native library differs:
Windows delegates to `LoadLibrary`, while Linux and macOS delegate to
`purego.Dlopen`. This is the same thin platform boundary used by zvec's pure-Go
binding; Bluge does not implement a PE, ELF, or Mach-O loader.

`WithLibraryPath` is authoritative when set. Otherwise Bluge checks
`BLUGE_USEARCH_LIBRARY_PATH`, the Go executable's directory, and finally the
operating system's native library search path. Deployment should normally put
the matching library beside the main executable. The process working directory
is not the placement contract.

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
C ABI has its own version returned by `bluge_usearch_abi_version`; the current
filtered-search and bulk-mutation contract is ABI 3, and Bluge rejects
incompatible libraries before opening an index.

The workflow currently uploads Actions artifacts. Attaching the same validated
artifacts to GitHub Releases is intentionally a separate publishing step.

#!/usr/bin/env bash
set -euo pipefail

# Run from Git Bash on Windows or bash on Linux/macOS. Output is never replaced.
cd "$(dirname "$0")/.."
output="${1:?usage: bash docs/run-collection-benchmark.sh NEW_OUTPUT_DIRECTORY [test flags]}"
shift
: "${BLUGE_USEARCH_LIBRARY_PATH:?set the native library path}"
export GOMAXPROCS="${GOMAXPROCS:-4}"
if [[ -e "$output" || -e "$output.log" || -e "$output.environment.txt" ]]; then
    printf '%s\n' "output already exists: $output" >&2
    exit 1
fi
mkdir -p "$(dirname "$output")"
{
    date -u
    go version
    go env GOOS GOARCH CGO_ENABLED
    uname -a
    git log -1 --format='%H %s'
    git status --short
    printf 'GOMAXPROCS=%s\nBLUGE_USEARCH_LIBRARY_PATH=%s\n' "$GOMAXPROCS" "$BLUGE_USEARCH_LIBRARY_PATH"
    sha256sum "$BLUGE_USEARCH_LIBRARY_PATH" collection*.go usearch-ffi/Cargo.lock usearch-ffi/src/lib.rs
} > "$output.environment.txt"
go test -c -o "$output.test.exe" .
"$output.test.exe" '-test.run=^TestCollectionStorageBenchmark$' '-test.v' '-test.timeout=2h' \
    "-collection-bench-out=$output" "$@" | tee "$output.log"

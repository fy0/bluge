#!/usr/bin/env bash
# Runs the embedded USearch search benchmarks one benchmark case per process so
# that a corpus build and its heap growth cannot leak into another case's
# measurement. Running several cases in one process inflated the same
# measurement by up to 4x. Each case is repeated in separate processes and the
# report uses the median.
#
# Usage: BLUGE_USEARCH_LIBRARY_PATH=... bash docs/run-embedded-bench.sh <label> [repeats]
#   writes docs/embedded-bench-<label>.txt   (end-to-end cases)
#   then   docs/embedded-bench-phases.txt    (current revision only, see below)
#
# Note: `go test -bench` splits its pattern on "/" and matches each element
# separately and unanchored, so every element needs its own "$" or the ties
# corpora silently match the plain corpora as well.
#
# Measuring a baseline revision:
#   The end-to-end benchmarks in vector_usearch_embedded_bench_test.go compile
#   against any revision, but vector_usearch_embedded_phase_bench_test.go calls
#   rankCandidates/collectCandidates and vector_prepared_filter_test.go uses
#   the prepared-filter API, so both must be moved aside first:
#
#     git stash push vector_usearch_embedded.go
#     mv vector_prepared_filter_test.go vector_usearch_embedded_phase_bench_test.go /tmp/
#     bash docs/run-embedded-bench.sh before
#     mv /tmp/vector_prepared_filter_test.go /tmp/vector_usearch_embedded_phase_bench_test.go .
#     git stash pop
set -u
label="${1:-run}"
repeats="${2:-3}"
export BLUGE_USEARCH_LIBRARY_PATH="${BLUGE_USEARCH_LIBRARY_PATH:-/tmp/ve/vector_engine.dll}"
out="docs/embedded-bench-${label}.txt"
phases="docs/embedded-bench-phases.txt"

# run_bench <file> <section> <anchored benchmark pattern> [sub-benchmark path]
run_bench() {
  local file="$1" section="$2" pattern="$3" path="${4:-}" i=0
  echo "### ${section}" >>"$file"
  while [ "$i" -lt "$repeats" ]; do
    go test -tags blugevectorstats -run '^$' -bench "$pattern" \
      -benchtime 100x -count 1 . 2>&1 | grep -E "^Benchmark${path}" >>"$file"
    i=$((i + 1))
  done
}

: >"$out"
for name in \
  '27k/1seg/k=192/nofilter' \
  '27k/1seg/k=512/nofilter' \
  '27k/8seg/k=192/nofilter' \
  '27k/8seg/k=512/nofilter' \
  '27k/26seg/k=192/nofilter' \
  '27k/26seg/k=512/nofilter' \
  '41k/26seg/k=192/nofilter' \
  '41k/26seg/k=512/nofilter' \
  '27k/26seg/k=192/docnum-filter' \
  '27k/26seg/k=512/docnum-filter' \
  '41k/26seg/k=512/docnum-filter' \
  '27k/26seg/k=512/id-filter' \
  '27k/26seg-ties/k=192/nofilter' \
  '27k/26seg-ties/k=512/nofilter' ; do
  anchored="$(echo "BenchmarkEmbeddedVectorSearch/${name}" | sed 's|/|$/|g')$"
  run_bench "$out" "$name" "$anchored" "EmbeddedVectorSearch/${name}"
done

# The phase benchmarks only exist in the current revision, so a baseline run
# leaves this file untouched instead of truncating it.
if [ -f vector_usearch_embedded_phase_bench_test.go ]; then
  : >"$phases"
  for corpus in '27k/1seg' '27k/26seg'; do
    for k in 192 512; do
      for row in native-ann native-ann-filtered stored-id-reads \
        stored-id-reads-sorted rank-candidates rank-previous ; do
        name="${corpus}/${row}/k=${k}"
        anchored="$(echo "BenchmarkEmbeddedVectorPhases/${name}" | sed 's|/|$/|g')$"
        run_bench "$phases" "$name" "$anchored" "EmbeddedVectorPhases/${name}"
      done
    done
  done
else
  echo "skipping phase benchmarks: vector_usearch_embedded_phase_bench_test.go is absent"
fi

echo "wrote ${out}${phases:+, and ${phases}}"

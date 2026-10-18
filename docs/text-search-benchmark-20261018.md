# Text Search Benchmark (2026-10-18)

This report records the text measurements used for `Current v2[2026.10]` in
the README. The measured source is a clean `git archive` of
`1df08adac9af796f321e78a7801636f7e762955f`. The benchmark runner was recovered
from the repository's local examples archive; its only change is displaying
query phases with 0.001 ms precision instead of 0.1 ms.

The [raw reports and runner](benchmarks/text-search-20261018/) and
[machine-readable summary](benchmarks/text-search-20261018/summary.json)
include all three builds, one unmeasured query warm-up process, ten measured
query processes, commands, source and executable identities, and file hashes.

## Workload And Environment

| Property | Value |
|---|---|
| Measurement date | 2026-10-18 |
| OS/architecture | Windows/amd64 |
| Go | 1.24.6 |
| CPU | Intel Core i5-13600KF, 14 cores / 20 logical CPUs |
| RAM | 64 GiB installed |
| GOMAXPROCS | Default, 20 logical CPUs |
| Documents / archive files | 1,547,580 / 24 |
| Indexed body text | 176.24 MiB |
| Batch size | 1,000 documents |
| Writer | Ordinary safe `Writer`, default merge configuration |
| Query | Canonical BM25, `LOCATION` in `body`, score-descending Top 5 |
| Build flags | `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, `-ldflags="-s -w"` |

The source archive is 58,432,902 bytes with SHA-256
`42b6b69724d9be6de33e73c4b58bd6a93c46b8b5a26e78d233eb0e463b03718b`.
It is the same archive, line selection, IDs, stored fields, analyzer, and batch
contract as the existing [cross-engine workload](data-simplification-benchmark.md#workload-contract).
The document fields are the stored `_id`, text `file`, keyword `side`, and
text `body` fields.

Three builds ran sequentially, with no concurrent test suite or second
benchmark. Each started with a separate, absent index directory. Build timing
includes ingestion, final `Writer.Close()`, and its merge/persistence drain.
Directory sizes count logical bytes after close. Memory peaks are the highest
`runtime.MemStats.Alloc` and `Sys` samples collected after batches; they can
miss a transient peak during final close and do not measure process RSS.

## Build Results

| Run | Build | Throughput | Segments | Directory bytes | Directory | Peak Alloc | Peak Sys |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 115.700 s | 13,376 docs/s | 20 | 318,001,292 | 303.27 MiB | 77.80 MiB | 113.71 MiB |
| 2 | 114.371 s | 13,531 docs/s | 21 | 318,277,595 | 303.53 MiB | 76.21 MiB | 109.02 MiB |
| 3 | 118.004 s | 13,115 docs/s | 21 | 318,277,797 | 303.53 MiB | 73.14 MiB | 109.33 MiB |

README build time, throughput, and directory size are arithmetic means:
**116.025 s**, **13,341 docs/s**, and **303.45 MiB**. Throughput averages the
per-run rates printed by the runner. Its memory rows use the highest sampled
peak across these runs: **77.80 MiB Alloc** and **113.71 MiB Sys**.

The stripped executable is **4,819,456 bytes (4.60 MiB)**. This is the size
of the benchmark runner built with the flags above.

## Query Results

Queries use the first build's 20-segment index. One unmeasured process first
warms filesystem caches. Ten additional fresh processes each open a reader,
perform a first query, perform 20 further queries on that reader, and close it.
Every query includes request construction, result iteration, and stored-field
visitation. Repeated queries are checked for identical IDs, order, and exact
scores inside each process; printed Top-5 results also match across processes
and all three build reports.

| Metric | Samples | Result |
|---|---:|---:|
| Fresh lifecycle, median of open + first query + close per process | 10 | **6.403 ms** |
| Resident query median | 200 | **1.666 ms** |
| Resident query p95, nearest rank | 200 | **2.247 ms** |

The fresh lifecycle excludes the 20 additional resident queries. No query-result
cache is used. Filesystem caches are not flushed, so the fresh measurement
describes a process/reader lifecycle with warm filesystem caches. Timings use
the runner's Go monotonic clock. Displayed medians are rounded half up to
0.001 ms; comparison ratios use unrounded aggregates in the JSON summary.

## Cross-Engine Comparison

The README retains official Bluge v0.2.2, Bleve v2.5.7, LanceDB 0.31.0, and
zvec v0.5.1 measurements from their July 2026 runs on this machine. This pass
remeasured this branch. It does not claim those other versions were rerun
in October. Their original values and methodology are in the
[cross-engine report](data-simplification-v2-benchmark.md#cross-engine-comparison)
and [official Bluge/Bleve report](data-simplification-benchmark.md#results-summary).

Normalized to official Bluge = 100%, the current results are 16.42% for the
fresh query lifecycle, 63.83% for index size, 100.00% for executable size,
72.63% for build time, 137.70% for throughput, 108.49% for peak Alloc, and
112.43% for peak Sys.

LanceDB Match-only omits positions and phrase-query support. The zvec adapter
duplicates pre-tokenized fields and supplies a compatibility vector. The
cross-engine tables describe these workload integrations, and their memory
figures use different runtime accounting. Collection and embedded ANN numbers
in the README have separate synthetic workloads and measurement dates.

## Reproduction

From this repository, create a clean source checkout and copy the archived
runner into it. Use a new output directory and provide the verified source
archive path:

```bash
unset GOMAXPROCS
git worktree add --detach tmp/text-benchmark-source 1df08ad
mkdir -p tmp/text-benchmark-source/examples/data_simplification tmp/text-benchmark-output
cp docs/benchmarks/text-search-20261018/runner.go.txt tmp/text-benchmark-source/examples/data_simplification/main.go
(
  cd tmp/text-benchmark-source
  CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o ../text-benchmark-output/current.exe ./examples/data_simplification
)
for n in 1 2 3; do
  tmp/text-benchmark-output/current.exe -archive /absolute/path/to/data-simplification.tar.bz2 \
    -index "tmp/text-benchmark-output/index-$n" -batch 1000 -keep-index
done
tmp/text-benchmark-output/current.exe -archive /absolute/path/to/data-simplification.tar.bz2 \
  -index tmp/text-benchmark-output/index-1 -query-only -profile-query -warm-queries 20
for n in 1 2 3 4 5 6 7 8 9 10; do
  tmp/text-benchmark-output/current.exe -archive /absolute/path/to/data-simplification.tar.bz2 \
    -index tmp/text-benchmark-output/index-1 -query-only -profile-query -warm-queries 20
done
```

The runner's existing option validation, reader profiling, and result-stability
tests passed before measurement. Full-corpus validation checked the archive
hash, document/file/text totals, directory byte totals, and all query profiles.

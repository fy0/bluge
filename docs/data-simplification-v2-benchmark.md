# zapx-bluge v2 Data Simplification Benchmark

This report records the final zapx-bluge v2 ordinary Writer convergence and
exact Block-Max BM25 WAND results. It supersedes the v1 performance summary in
the repository README. The complete v1, OfflineWriter, official Bluge, and
Bleve measurements remain in the
[historical v1 report](data-simplification-benchmark.md).

## Workload And Environment

| Property | Value |
|---|---|
| Measurement dates | 2026-07-27 through 2026-07-28 |
| OS/architecture | Windows/amd64 |
| Go | 1.24.6 |
| CPU | Intel Core i5-13600KF, 14 cores / 20 logical CPUs |
| RAM | 64 GiB |
| Documents | 1,547,580 |
| Accepted archive files | 24 |
| Indexed body text | 176.24 MiB |
| Batch size | 1,000 documents |
| Writer | ordinary safe `Writer` |
| Query | canonical BM25, score-descending Top 5 |

The source archive has SHA-256
`42b6b69724d9be6de33e73c4b58bd6a93c46b8b5a26e78d233eb0e463b03718b`.
Build timing starts before ingestion and ends only after `Writer.Close()` has
persisted the final snapshot and waited for merger convergence. No benchmark
uses a query-result cache.

## Root Cause And Repair

Three effects combined to create the unstable merger backlog:

1. Dense postings merge iteration reloaded and replayed a frequency/norm chunk
   for each posting. High-frequency terms therefore repeated decoding work
   inside a chunk instead of advancing one sequential decoder cursor.
2. `Writer.Close()` closed the shared stop channel before the persister and
   merger had converged. Depending on scheduling, Close could leave hundreds
   of segments in the last persisted snapshot.
3. The merge planner emitted clean singleton `1 -> 1` rewrites. Instrumented
   traces showed the same 447,000-document segment rewritten twice, with each
   rewrite consuming approximately 23-24 seconds while adding no deletions or
   topology progress.

Impact encoding was measurable but was not the primary bottleneck. Its original
implementation still allocated one sort closure and work slice for every
64-posting block.

The production repair:

* preserves the dense postings decoder cursor across sequential `Next()` calls
  while retaining exact rank-based positioning for non-sequential `Advance()`;
* adds explicit merger drain and persister barriers, with Close performing
  `merge -> persist -> merge -> persist` while a batch lifecycle lock prevents
  new mutation;
* skips clean singleton merge tasks while retaining singleton rewrites that
  actually apply deletions;
* uses a fixed 64-entry impact frontier, allocation-free insertion sort,
  reusable buffers, and direct streaming into the encoder; and
* fixes `Writer.Stats()` to return the populated statistics copy.

The vector backend, vector section/address/cache extension points, and default
unsupported-vector behavior are unchanged. V2 remains intentionally unable to
open v1 indexes.

In the final low-cost convergence trace, Close began with
`root/persisted/merged = 3273/3272/3273` and 24 segments. The first barrier
advanced the state to `3273/3273/3273` without changing the topology. This
directly demonstrates that Close now waits for the outstanding persister epoch
instead of depending on a sleep or aborting the background workers.

## V1 And V2 Ordinary Writer Results

| Sample | Build | Throughput | Segments | Directory |
|---|---:|---:|---:|---:|
| v2 run 1 | 122.511s | 12,632 docs/s | 20 | 303.27 MiB |
| v2 run 2 | 118.812s | 13,025 docs/s | 22 | 303.85 MiB |
| v2 run 3 | 118.981s | 13,007 docs/s | 21 | 303.53 MiB |
| v2 validation run | 128.637s | 12,031 docs/s | 20 | 303.27 MiB |
| **v2 average** | **122.235s** | **12,674 docs/s** | **20.8** | **303.48 MiB** |
| **v2 median** | **120.746s** | **12,817 docs/s** | **20-22** | **303.40 MiB** |
| v1 contemporaneous run | 123.513s | 12,530 docs/s | 28 | 300.93 MiB |

The v2 build average is 1.03% below the contemporaneous v1 build and its average
directory is 0.85% larger. The build median is 2.24% below v1. Every v2 sample
converged to a v1-comparable 20-22-segment topology. The fourth run also
recorded 21 total directory files,
318,000,492 bytes, peak Go `Alloc` of 78.39 MiB, and peak Go `Sys` of
118.05 MiB. These Go heap values use the historical runner's after-batch
sampling method; they are not continuous process-RSS measurements and can miss
a transient peak during final Close convergence.

The Windows/amd64 benchmark executable built with `CGO_ENABLED=0`, `-trimpath`,
`-buildvcs=false`, and `-ldflags="-s -w"` is 4,749,824 bytes (4.53 MiB).

## Cross-Engine Comparison

| Engine and profile | Build | Throughput | Directory | Fresh `LOCATION` lifecycle |
|---|---:|---:|---:|---:|
| **zapx-bluge v2 ordinary Writer** | **122.235s average** | **12,674 docs/s** | **303.48 MiB** | **8.203 ms median** |
| zapx-bluge v1 ordinary Writer | 123.513s | 12,530 docs/s | 300.93 MiB | 18.019 ms median |
| Bleve v2.5.7 scorch | 145.974s | 10,602 docs/s | 536.47 MiB | 35 ms median |
| LanceDB 0.31, positions enabled | 157.440s | 9,830 docs/s | 669.00 MiB | 34 ms median |
| LanceDB 0.31, positions disabled | **92.239s** | **16,778 docs/s** | **242.95 MiB** | 32.5 ms median |
| zvec v0.5.1 experimental adapter | 786.275s | 1,968 docs/s | 1,549.43 MiB | 227 ms median |

Against the feature-preserving configurations, v2 built 16.26% faster than
Bleve, 22.36% faster than LanceDB with positions, and 84.45% faster than the
measured zvec adapter. Its directory was respectively 43.43%, 54.64%, and
80.41% smaller.

LanceDB Match-only built 24.54% faster and was 19.95% smaller than v2, but it
disables token positions and cannot provide equivalent phrase and location
behavior. The zvec adapter duplicates pre-tokenized string fields and includes
a required one-dimensional compatibility vector. The external builds are
historical single runs on the same machine and corpus. zvec and LanceDB native
RSS must not be compared directly with Bluge or Bleve Go heap samples.

## Query Performance

Ten fresh processes each opened a final 20-segment v2 index and executed 20
same-reader searches, producing 200 resident samples per term. A sample includes
request construction, the full search, result iteration, and stored-field
visitation.

| Query | Total candidates | Scored | Reduction | Median | p95 | Worst process p95 |
|---|---:|---:|---:|---:|---:|---:|
| `LOCATION` | 287,959 | 1,979 | 99.31% | 1.630 ms | 2.249 ms | 2.667 ms |
| `THE` | 1,042,353 | 3,136 | 99.70% | 1.684 ms | 2.254 ms | 2.668 ms |

The complete fresh `LOCATION` lifecycle, measured in ten processes, had an
8.203 ms median. Median v2 phases were 2.345 ms open, 1.986 ms first query, and
3.794 ms close. The same phase-sum procedure produced an 18.019 ms v1 median.
The filesystem cache was not flushed between processes, matching the historical
fresh-handle methodology; these are not cold-disk measurements.

The two-term `LOCATION OR THE` Top-5 request scored 146,084 of 1,330,312 term
candidates, an 89.02% reduction. Low-cardinality terms intentionally retain the
ordinary collection path. Five 10,001-query `INVERNESS` runs measured:

| Engine | Mean batch | Median batch | Mean per query |
|---|---:|---:|---:|
| v1 | 367.226 ms | 368.366 ms | 36.72 microseconds |
| v2 | 330.667 ms | 329.156 ms | 33.06 microseconds |

V2 was 9.96% faster on the rare-term mean and therefore introduced no rare-term
latency regression.

## Exactness Evidence

WAND is not the same traversal algorithm as exhaustive collection: it skips
blocks whose exact upper bound cannot enter the heap. It deliberately preserves
the scoring and ordering contract. Each 64-posting block stores the
non-dominated `(frequency, raw norm)` frontier, and BM25 evaluates every point
in that frontier to obtain an exact upper bound. Blocks are processed in
document order, so an equal-score block after the current heap contents cannot
win the document-number tie break.

Two full-corpus comparisons were run:

* On one v2 index, WAND and a similarity wrapper that disables the impact path
  matched for `LOCATION`, `THE`, `INVERNESS`, and `LOCATION OR THE` at
  Top-1/5/20/100 where applicable.
* Separate v1 and v2 binaries opened their native reference indexes and matched
  the same four queries at Top-1/5/20/100.

Every comparison matched the document ID, hit order, and
`math.Float64bits(score)`, not merely a rounded display score. Unit coverage
also exercises three BM25 modes, multiple segments, logical deletions, updates,
low-cardinality segments, and tied scores.

## Validation

All commands below passed on the final source state on 2026-07-28:

| Command | Result |
|---|---|
| `go test ./... -count=1 -timeout=10m` | passed |
| `go test -race ./... -count=1 -timeout=15m` | passed |
| `$env:GOARCH='386'; go test ./... -count=1 -timeout=15m` | passed |
| `go test . -run '^(TestBM25RawTopNMatchesBaselineAndSkipsCandidates\|TestBM25RawTopNExactAcrossSegmentsAndDeletions)$' -count=20 -timeout=10m` | passed |
| `go test ./internal/zapxtext -run '^(TestV2ImpactPostingsSurviveMergeAndDrops\|TestV2DensePostingsSequentialCursorAfterAdvance\|TestV2RejectsV1Segment)$' -count=20 -timeout=10m` | passed |
| `go test ./index -run '^(TestWriterCloseWaitsForMergeConvergence\|TestWriterClosePersistsUnsafeBatchesBeforeConvergence)$' -count=50 -timeout=10m` | passed |
| `go test ./index/mergeplan -run '^(TestPlanSkipsCleanSingletonTask\|TestRosterWithDeletionsMakesProgress)$' -count=50 -timeout=10m` | passed |
| `go test -race ./index -run '^(TestWriterCloseWaitsForMergeConvergence\|TestWriterClosePersistsUnsafeBatchesBeforeConvergence)$' -count=10 -timeout=10m` | passed |

A temporary full-corpus harness additionally compared the forced ordinary and
WAND paths, then compared separate v1 and v2 binaries against their native
indexes. It covered four queries and Top-1/5/20/100, checked score bit patterns,
and was removed after the successful run.

## Remaining Risks

Writer convergence still depends on storage latency and background CPU load;
the four builds ranged from 118.812 to 128.637 seconds. The fixed Close barriers
make the resulting topology deterministic within the merge policy, but they do
not remove ordinary wall-clock noise. The measured 20-22 segment range is the
expected consequence of asynchronous batch and merge boundaries, not an
unbounded backlog.

Candidate counts can move slightly when segment and 64-posting block boundaries
change, while exact scores and ordering remain stable. External-engine ratios
should be read as workload integration results rather than intrinsic engine
rankings because storage models, position support, vector requirements, native
memory accounting, and lifecycle work differ.

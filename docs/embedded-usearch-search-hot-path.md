# Embedded USearch search hot path: deferred candidate identifiers

This document records the verification, the change, and the before/after
measurements for the search hot path of `EmbeddedUSearchVectorBackend`
(`vector_usearch_embedded.go`). Everything here was measured on a real
on-disk index with deterministic synthetic vectors; no embedding service was
called and no production corpus was required.

## Measured environment

| Property | Value |
|---|---|
| Measurement date | 2026-09-14 to 2026-09-15 |
| OS/architecture | Windows 11 / amd64 |
| CPU | Intel Core i5-13600KF |
| Go | go1.24.6 windows/amd64 |
| Native library | `vector_engine.dll` from the Bluge v0.6.0 release, SHA-256 `2c639ed0f84af47b924216731790d6682f6a1da0a31ad75d9bc6980cc3a79803` |
| Native engine | usearch 2.26.0, adapter 0.1.0, ABI 3 |
| Benchmark commands | `docs/run-embedded-bench.sh`, summarized by `docs/summarize-embedded-bench.py` |
| Raw results | `docs/embedded-bench-before.txt`, `docs/embedded-bench-after.txt`, `docs/embedded-bench-phases.txt` |

## What the code actually did

The three observations in the request were checked against the code before
anything was changed.

1. **Per-segment top-k, then a global sort and truncation.** Confirmed.
   `searchCandidates` asked every segment for `k` candidates, resolved the
   stored identifier of *every* candidate that was not deleted through
   `embeddedDocumentID` -> `Snapshot.VisitStoredFields` (which decodes the
   whole 32-document stored block containing the document), then ran one
   `sort.SliceStable` over the concatenation and truncated to `k`. Candidates
   that the truncation discarded therefore paid for a stored-field read and
   for the sort.

2. **`Reader.VectorSearch` / `vectorFilterDocumentNumbers`.** Confirmed. With a
   non-nil filter, `Reader` first evaluates the filter, materializes and sorts
   the matching global document numbers, and then prefers the internal
   document-number path (`vectorDocumentCandidateSearcher`) over the exported
   `VectorCandidateSearcher`. `embeddedUSearchSegment.allowedKeys` converts
   those document numbers to native keys with a binary search over the sorted
   `payload.DocIDs` mapping and does **not** read stored fields.

3. **The two candidate-searcher interfaces are not equivalent.** Confirmed.
   `SearchCandidates` (`allowedIDs`) requests `count = len(segment.payload.DocIDs)`
   per segment, that is every vector in the segment, and only then filters by
   stored identifier. Measured on 27k documents in a single segment at k=192:
   36.9 ms through `SearchCandidates` against 0.41 ms unfiltered. `Reader`
   never selects this path for the embedded backend, and this change does not
   make it a substitute for the document-number path. It is left functionally
   unchanged; it only inherits the faster sort described below.

A fourth cost was not in the original report: the global ordering used
`sort.SliceStable` over `[]VectorHit`, which sorts through
`reflectlite.Swapper` and `typedmemmove`. A CPU profile of the k=512,
26-segment case attributed **13.6% of total CPU** to `sort.stable_func`.

## The change

`vector_usearch_embedded.go`:

* `searchCandidates` now delegates to `collectCandidates` and `rankCandidates`.
* `collectCandidates` performs the per-segment native search and records
  `(globalDoc, score)` for every surviving candidate **without reading any
  stored identifier**. Identifiers are resolved here only when the filter
  itself is expressed in terms of identifiers (`allowedIDs`), because that
  filter cannot be evaluated without them.
* `rankCandidates` orders by score first, which needs no identifier, and splits
  the candidates by the k-th score:
  * **selected** - score strictly above the cutoff: returned regardless of
    identifier, so the identifier is only read for output.
  * **tied** - score exactly the cutoff: they compete for the remaining slots,
    so their identifiers are read and compared.
  * **dropped** - score strictly below the cutoff: they can never reach the
    result, so their identifiers are **never read**.
  When there are no more candidates than `k` nothing can be dropped, so the
  split is skipped and every candidate is ordered once by (score, identifier).
* All three sorts use `slices.SortFunc` with `cmp.Compare` instead of
  `sort.SliceStable`, removing the reflection-based swapper.
* The per-candidate `VectorSimilarity(...)` conversion was hoisted out of the
  candidate loop.
* The result slice is now sized to the result instead of `len(segments)*k`
  (at 26 segments and k=512 that was a 13312-entry, 426 KiB allocation retained
  by the returned slice).

The per-segment request count is still exactly `k`. It is **not** divided by
the segment count, so a globally good result concentrated in one segment is
still found; `TestEmbeddedSearchBestResultsInOneSegment` fails if the number of
results coming from the best segment drops to what an even per-segment split
would produce.

## Why the results are unchanged

* **Ordering.** The documented order is score descending, then identifier
  ascending. `rankCandidates` returns every candidate with score above the
  cutoff (in that order) followed by the smallest identifiers of the tie group.
  A candidate with a score below the cutoff can never enter the result, so
  dropping it is equivalent to sorting it and truncating.
* **Stability.** The new sorts are not stable, which is safe: two candidates
  compare equal only when *both* the score and the identifier are equal, and
  such candidates produce byte-identical `VectorHit` entries, so the returned
  slice is unchanged. Candidates that share a score but differ in identifier
  are ordered by identifier exactly as before, and the score split does not
  depend on the order within a score group.
* **Unchanged paths.** Deleted-document exclusion, the allowed-set constraint,
  the empty-allowed-set contract, `k` larger than the number of candidates,
  missing fields, invalid dimensions, NaN/infinity rejection, invalid-k
  rejection, native status handling, and closed-index handling are all checked
  in the same order, for the same candidates, as before.

### Error-exposure trade-off (explicit)

Before this change, a candidate whose stored `_id` could not be read (for
example a corrupt stored block) aborted the whole search even when the
candidate would have been discarded by the global truncation. Now identifiers
are read only for the candidates that reach the result or the boundary tie
group, so a corrupt stored identifier on a *dropped* candidate is no longer
reported by that search.

What is deliberately preserved: every document that appears in the result still
has its identifier read, so a result can never contain a fabricated, empty, or
mismatched identifier; when the error does surface it is the same error value
and message as before; and none of the key-validation, result-count, or
deleted-document checks were deferred - those all run during collection.

This was accepted because the alternative is reading every candidate's
identifier, which is exactly the cost being removed. The detection property
degrades from "any search over the field fails" to "searches fail when the
corrupt document is ranked into the result". There is no separate integrity
check API; a full-scan verification would need to be written separately if that
guarantee is required.

## Second change: reader-bound prepared filter

`reader.go` adds a minimal way to evaluate a filter once and reuse it:

```go
prepared, err := reader.PrepareVectorFilter(ctx, filter)   // evaluated once
defer prepared.Close()
hits, err := reader.VectorSearchPrepared(ctx, field, query, k, prepared)
```

* The filter is resolved at preparation time into whatever the reader's backend
  consumes, and the representation is frozen there:
  * a backend that filters by document number (the embedded backend) stores the
    sorted allowed document numbers;
  * a backend that filters by stored identifier (the flat in-memory backend,
    the global sidecar USearch backend) stores the allowed identifier set;
  * a backend that supports neither returns `ErrVectorFilterUnsupported` from
    `PrepareVectorFilter`, rather than failing later at search time.
  The `Query` object is not retained, so mutating it after preparation cannot
  change the prepared filter's results.
* The third case is a boundary worth naming: a third-party `VectorIndex` that
  applies `Query` filters only inside `Search` cannot offer a frozen filter
  outcome, so it cannot be prepared. It keeps working through
  `Reader.VectorSearch` with the `Query`; only the prepared-filter entry point
  is unavailable to it. Every backend in this repository falls into one of the
  first two cases.
* Both representations are held privately. The raw document-number slice is
  never exposed, so callers cannot hold document numbers without the lifetime
  check.
* Ownership is verified on every use: a filter is rejected with
  `ErrVectorPreparedFilter` when it is nil, closed, was prepared by a different
  `Reader`, or when its `Reader` is closed. `Reader` gained an atomic `closed`
  flag set by `Close` for that last check, and `PrepareVectorFilter` checks it
  **before** doing any work, so a closed reader is never searched again and no
  unusable object is handed out.
* The unfiltered case (nil `Query`) is represented distinctly from a filter
  matching no documents: a nil filter searches without a filter, an empty match
  set returns an empty result. This holds for both representations, because an
  empty match set is stored as a non-nil empty slice or map.
* The embedded backend therefore always uses the document-number path and never
  the `allowedIDs` path.
* The prepared filter is read-only after creation and may be shared by
  concurrent searches on its own reader (`Close` only sets a flag; it does not
  clear the stored set, so it cannot race with a search that already passed the
  lifetime check).
* No global cache, LRU, or application-level checkpoint concept was
  introduced. Reuse scope is explicit and caller-owned.

`ErrVectorPreparedFilter` is the only new public error value;
`PreparedVectorFilter`, `Reader.PrepareVectorFilter`, `Reader.VectorSearchPrepared`
are the only new public API. `VectorSearch`, `SearchVector`,
`SearchVectorRequest`, and `HybridSearch` are unchanged.

Hybrid wiring: `HybridSearchRequest.Vector` still carries a `Query` filter, so
hybrid search does not use the prepared filter in this round. Wiring it would
mean adding a `*PreparedVectorFilter` field to `VectorSearchRequest` and
validating it in `SearchVectorRequest` and `HybridSearch`; that was left out to
keep the public surface minimal. Until then, a caller that repeats a vector
query with the same filter should call `VectorSearchPrepared` directly.

## Benchmarks

### Method

* Deterministic synthetic corpora: `doc%06d` identifiers, a keyword `group`
  field, a short text field, and a 64-dimension cosine vector generated by an
  explicit 64-bit LCG (stable across Go releases).
* Segment count is exact: the merge budget is raised out of reach and one
  writer batch is one segment; the harness fails if the observed segment count
  differs from the requested one.
* Ties are produced on purpose by drawing vectors from a small alphabet, so
  many documents share bit-identical scores and the global K boundary is a tie.
* Each benchmark case - end-to-end and phase alike - runs in its **own
  process** with 5 untimed warmup iterations, 100 timed iterations, and 3
  repetitions; the tables report the median. Per-case isolation matters:
  running several cases in one process inflated the same measurement by up to
  4x through accumulated heap growth.
* Only the search is timed. Index construction, the filter query evaluation for
  the identifier-filter case, and warmup are outside the timer. The
  document-number filter case *does* include its filter evaluation, because
  `Reader.VectorSearch` performs it per call - that is the cost the prepared
  filter removes.
* Raw output: `docs/embedded-bench-before.txt`, `docs/embedded-bench-after.txt`
  (end-to-end) and `docs/embedded-bench-phases.txt` (phases, current revision
  only). `docs/run-embedded-bench.sh` reproduces all three and documents how to
  measure a stashed baseline.

### What the synthetic corpus does and does not represent

The corpora are 64-dimension vectors with a three-field stored document
(identifier, keyword group, short text). The production workload uses
1024-dimension vectors and stores source text, so the stored blocks are much
larger and the native distance computations are much wider. Both of those costs
are present in the "after" column too; the change removes per-candidate stored
identifier reads and per-candidate ranking work, which scale with
`segments * k` rather than with dimension or document size. The reported
percentages are therefore specific to this corpus and must not be read as a
prediction for the application; see the bce-go-rag section below.

The documents are also far smaller than real ones: 25 bytes of stored text per
document (`doc-000123` + `g1` + `chunk body g1`) against source chunks of
kilobytes. The index is therefore almost entirely vector payload, as the next
section shows.

### Corpus scale and build time

Measured on the same machine with `BLUGE_USEARCH_SCALE_TEST=1 go test -run
'^TestEmbeddedCorpusScale$' -v .`:

| Corpus | Build | Index size | Per document | Per vector |
|---|---|---|---|---|
| 27k / 1 seg / 64 dim | 6.5 s | 12.0 MB | 453 B | 404 B |
| 27k / 26 seg / 64 dim | 3.5 s | 12.2 MB | 461 B | 407 B |
| 41k / 26 seg / 64 dim | 9.0 s | 18.6 MB | 460 B | 406 B |
| 5k / 1 seg / 1024 dim | 22.3 s | 21.0 MB | 4298 B | 4245 B |
| 27k / 1 seg / 1024 dim | 3 m 44 s | 113.2 MB | 4293 B | 4244 B |
| 27k / 26 seg / 1024 dim | 30.4 s | 113.4 MB | 4301 B | 4247 B |

Three things worth noting, none of which this change altered:

* Per vector, 1024 dimensions cost 4245 B against 404 B at 64 dimensions. The
  raw float32 payload is `4 * dimensions` (4096 B and 256 B), so the USearch
  graph adds roughly 150 B per vector in both cases - the graph overhead is
  dimension-independent, the vector body is not.
* Index size does not depend on the segment count (113.2 MB against 113.4 MB
  for the same vectors), only on the number of vectors and their width.
* **Build time depends heavily on the segment count at 1024 dimensions**: the
  same 27000 vectors take 3 m 44 s as one segment and 30.4 s as 26 segments.
  Building many small native indexes is much cheaper than one large one.

These numbers describe this synthetic corpus, not a source corpus: real stored
chunks would add their text on top of the vector payload, and at 27k chunks of
1024 dimensions the vector payload alone is already 112 MB.

### The 1024-dimension shape, before and after

The 64-dimension table above is the wrong denominator for judging whether the
gain transfers, so the app-like shape was measured directly instead of
extrapolated. 27k documents, 26 segments, 1024 dimensions, k=512, no filter,
three processes of 50 iterations:

| | Runs | Median | Allocations |
|---|---|---|---|
| Before | 96.5 / 106.8 / 80.9 ms | 96.5 ms | 66846 |
| After | 62.5 / 71.5 / 69.3 ms | 69.3 ms | 2781 |

The gain holds at the application's dimension but is smaller: **-28% against
-43%** on the comparable 64-dimension shape. That is the expected direction -
the native ANN search over 1024-dimension vectors costs more, so the removed
work is a smaller share of the total - and it is why the 64-dimension
percentages are not quoted as application predictions. The allocation **count**
drops by the same 24x as at 64 dimensions (66846 -> 2781), because a count does
not depend on the dimension; the allocated **bytes** fall much less
(1581511 -> 789802 B/op, about 2x).

### End-to-end results (median of 3 processes, 100 iterations each)

`k` is the requested result count; "docnum-filter" is a keyword filter matching
one quarter of the corpus; "id-filter" is the exported
`SearchCandidates`/`allowedIDs` path.

| Case | Before | After | Delta | Allocs before | Allocs after |
|---|---|---|---|---|---|
| 27k / 1 seg / k=192, no filter | 0.31 ms | 0.42 ms | +35.8% | 972 | 970 |
| 27k / 1 seg / k=512, no filter | 1.09 ms | 0.87 ms | -20.2% | 2572 | 2570 |
| 27k / 8 seg / k=192, no filter | 3.55 ms | 3.66 ms | +2.9% | 7748 | 1026 |
| 27k / 8 seg / k=512, no filter | 7.13 ms | 5.97 ms | -16.3% | 20549 | 2626 |
| 27k / 26 seg / k=192, no filter | 8.67 ms | 5.50 ms | -36.6% | 25173 | 1170 |
| **27k / 26 seg / k=512, no filter** | **16.56 ms** | **9.38 ms** | **-43.3%** | **66776** | **2771** |
| 41k / 26 seg / k=192, no filter | 9.68 ms | 6.10 ms | -37.0% | 25173 | 1170 |
| **41k / 26 seg / k=512, no filter** | **20.56 ms** | **12.03 ms** | **-41.5%** | **66776** | **2772** |
| 27k / 26 seg / k=192, docnum-filter | 11.42 ms | 9.26 ms | -19.0% | 32543 | 8539 |
| 27k / 26 seg / k=512, docnum-filter | 14.19 ms | 11.29 ms | -20.4% | 41335 | 10141 |
| 41k / 26 seg / k=512, docnum-filter | 24.47 ms | 20.54 ms | -16.1% | 62909 | 13739 |
| 27k / 26 seg / k=512, ties | 5.06 ms | 3.64 ms | -28.0% | 30474 | 17087 |
| 27k / 26 seg / k=192, ties | 3.29 ms | 2.90 ms | -11.8% | 19273 | 17086 |
| 27k / 26 seg / k=512, id-filter | 24.14 ms | 24.77 ms | +2.6% | 135220 | 135218 |

Reading of these numbers:

* The gain grows with the number of segments, which is the intended shape: the
  savings are candidates that would have been read and then discarded, and that
  number is `segments * k - k`.
* **Two rows show no gain, and both are sub-millisecond or native-dominated.**
  The 1 seg / k=192 row is a 0.3-0.4 ms measurement, i.e. at the noise floor
  (its repeats span 0.31-0.42 ms on both sides). The 8 seg / k=192 row is
  reproducible - before 3.32/3.58/3.55 ms against after 3.76/3.66/3.42 ms - and
  flat within 3% while allocations drop 7.5x. The most likely reason is that
  the native ANN search already accounts for nearly all of that time: the phase
  data below covers 1 and 26 segments, not 8, so this case's split was not
  measured directly and is not claimed.
* The heavy-tie cases gain less (-12% to -28%) than the plain corpora at the
  same shape, because with mass ties many candidates sit exactly on the cutoff
  and their identifiers must still be read to break the tie. That is the
  documented trade-off: lower gain, identical results.
* The `id-filter` path is unchanged within noise, because that path must
  resolve identifiers to evaluate its filter, so it has nothing to defer. It
  remains roughly 2.5x slower than the document-number path and is still not
  used by `Reader` for the embedded backend.
* 41k documents in 26 segments is the closest synthetic stand-in for the
  reported production shape (about 1600 vectors per segment).

### Stored identifier reads

Measured with `-tags blugevectorstats` on the same corpora (no ties, so the
tie group does not add reads):

| Case | Candidate slots (`segments*k`) | Stored id reads before | Stored id reads after |
|---|---|---|---|
| 27k / 1 seg / k=512 | 512 | 512 | 512 |
| 27k / 8 seg / k=512 | 4096 | 4096 | 512 |
| 27k / 26 seg / k=192 | 4992 | 4992 | 192 |
| 27k / 26 seg / k=512 | 13312 | 13312 | 512 |
| 41k / 26 seg / k=512 | 13312 | 13312 | 512 |

After the change a search reads exactly one identifier per returned result, and
no more than one per candidate in the boundary tie group. `TestEmbeddedSearchDefersIdentifierReads`
asserts this bound (and that the previous implementation reads more) so the
property cannot silently regress.

### Phase noise floor

`BenchmarkEmbeddedVectorPhases` measures the phases separately, and each phase
case also runs in its own process for the reason above. The pure-native and
pure-identifier-read phases contain **no changed code**, yet repeated runs moved
them by up to +/-25% (for example `native-ann-filtered` at 27k/1seg/k=512:
20.3 ms and 11.1 ms in two runs of identical code). These phase numbers are
therefore only usable at the millisecond scale:

| Phase (27k / 26 seg) | k=192 | k=512 |
|---|---|---|
| native ANN | 4.38 ms | 6.94 ms |
| native ANN with a key allow-list | 8.62 ms | 8.77 ms |
| stored-id reads, 4992 / 13312 candidates, native result order | 1.88 ms | 5.28 ms |
| stored-id reads, same candidates, document order | 1.27 ms | 3.45 ms |
| rank candidates, current implementation | 0.50 ms | 1.39 ms |
| rank candidates, previous implementation | 1.20 ms | 2.78 ms |

### Ranking cost, measured rather than asserted

The previous version of this benchmark shrank its work slice to `k` after the
first iteration, so from the second iteration on it sorted only `k` elements of
an already-sorted list; its numbers were meaningless and have been replaced.
The corrected benchmark hands `rankCandidates` a full candidate list on every
iteration, built the way a search builds it: one descending run per segment,
the runs overlapping, with a tie group on the global K boundary. It measures the
real production function with identifiers already resolved, which isolates the
ranking from the identifier reads that the separate phase measures.

`rank-previous` is a replica of the previous implementation running in the same
binary on the same score multiset (one `sort.SliceStable` over
`[]VectorHit` plus a truncation), so the two rows are directly comparable:

| Candidates | Previous (`[]VectorHit`, 32 B) | Current (`[]embeddedVectorCandidate`, 40 B) | Ratio |
|---|---|---|---|
| 4992 (26 seg, k=192) | 1.20 ms | 0.50 ms | 2.4x |
| 13312 (26 seg, k=512) | 2.78 ms | 1.39 ms | 2.0x |

The current row is not favoured by element size: its candidates are larger, so
the 2x is a floor rather than a ceiling.

The native search is unchanged, as expected, and dominates the remaining time
at 26 segments and k=512. Reading identifiers in document order rather than
native-result order was measured at ~1.5x faster (3.45 ms against 5.28 ms) -
the stored block cache holds one 32-document block, so scattered reads decode
far more blocks. That reordering was deliberately **not** implemented: after
the change only `k + ties` identifiers are read, so the absolute saving is in
the tens of microseconds, and reordering would break the natural stability of
the candidate list for no measurable benefit.

The baseline file `docs/embedded-bench-before.txt` contains no phase rows,
because the corrected phase benchmark calls `rankCandidates`, which does not
exist in a stashed baseline; the baseline run therefore skips the phase file
(the script says so when it does). The ranking comparison above is a
same-binary comparison instead.

Because of that noise floor, no conclusion in this document is drawn from a
phase number or from adding phases together; the end-to-end table above is the
basis for every claim.

## What was not verified

* No production corpus, no real embeddings, and no 20k-blob workload was used.
  41k synthetic vectors in 26 segments is the largest case measured.
* The single-segment cases are **within the noise floor**, and the table above
  shows it in both directions: k=192 reads +35.8% and k=512 reads -20.2%, while
  five focused repetitions of k=512 with 300 iterations per process gave a
  median of 854 us before and 888 us after (+4%) with an unchanged minimum
  (840 us against 842 us). Treat single-segment searches as unchanged: there is
  nothing to defer when one segment supplies every candidate.
* The 8 seg / k=192 case is reproducible but unexplained by direct measurement
  (see the end-to-end section): it is flat while allocations drop 7.5x, and its
  phase split was not measured.
* No claim is made that the change is linear in the segment count or in `k`.
  The measured relationship is only "more segments and larger k means more
  discarded candidates, hence more saved identifier reads".
* Linux/macOS and arm64 were not benchmarked; only Windows/amd64.
* No recall or relevance change was measured, because none is expected: the
  candidate set and the ordering rule are unchanged.
* The `id-filter`/`allowedIDs` path was not redesigned or benchmarked for
  improvement; only its inherited sort cost changed.
* No segment-level parallelism, automatic merge tuning, unified global vector
  graph, or global cache was added.

## Public API, index compatibility, error behaviour

* **Index compatibility: unchanged.** No chunk identifier, stored format,
  payload format, or native ABI changed. No reindex is required; the same
  serialized USearch bytes and `DocIDs` mapping are read as before.
* **Changed public API:** `ErrVectorPreparedFilter` (new error value),
  `PreparedVectorFilter` (new type), `Reader.PrepareVectorFilter` (new method),
  `Reader.VectorSearchPrepared` (new method). Everything else is unchanged, and
  no existing signature or behaviour was altered.
* **Error behaviour:** the error values and messages produced for every
  validation failure are unchanged, and the same checks run for the same
  candidates. The single behavioural change is the deferred-identifier error
  exposure described above.
* `Reader` gained an unexported `atomic.Bool` field. `Reader.Close` now also
  marks the reader closed; it remains the case that closing a reader while a
  search is in flight is not synchronized, exactly as before.

## Using this from bce-go-rag

Nothing in bce-go-rag was modified in this round. The relevant points for the
next round:

* No caller change is needed to get the main improvement: the winning path is
  `Reader.VectorSearch` / `SearchVectorRequest` with a document-number filter,
  which is already what the embedded backend is routed to.
* **The measured gain does not transfer to the application as a number.** The
  benchmarks use 64-dimension vectors and a three-field stored document;
  the production index uses 1024-dimension vectors and stores source text. The
  part that was removed scales with the number of candidates, not with the
  dimension, so the *direction* should carry over, but two things shrink the
  relative gain: native ANN search over 1024-dimension vectors and stored-block
  decoding of much larger documents both cost more than they do here, and the
  candidate count that was being over-read is the same `segments * k`. Treat
  the table as evidence that the wasted work is gone, not as a predicted
  speedup. Re-measure on the production index before promising a number.
* Allocation **counts** drop sharply, but only in the shapes the change
  targets, and the factor varies more than the timing does:
  * 26 segments, no filter: 66776 -> 2771 per search at k=512 and
    25173 -> 1170 at k=192, i.e. 22-24x - and the same 24x at 1024 dimensions,
    because a count does not depend on the dimension. This is the row to quote.
  * 8 segments, no filter: about 7.6x.
  * document-number filter at 26 segments: 3.8-4.6x, because the filter itself
    still materializes candidates.
  * heavy-tie corpora: 1.1-1.8x, and the identifier-filter path: 1.0x, since
    that path reads every candidate's identifier by definition.
* The counts above are **allocations, not bytes**. Allocated bytes fall far
  less - 1557287 -> 758586 B/op (about 2x) in the 26 seg / k=512 case - because
  a surviving `embeddedVectorCandidate` is larger than the `VectorHit` it
  replaces. No memory-footprint claim is made beyond that.
* The application-layer hydration cost (~23 ms for 512 results in the report)
  was not measured here at all. Whether the search or the hydration now
  dominates the request depends on the production index, so no ordering
  between them is claimed.
* If a request issues several vector queries with the *same* filter, replace
  the repeated `VectorSearch` calls with one `PrepareVectorFilter` plus
  `VectorSearchPrepared` per query. The filter query is then evaluated once
  instead of once per query. The prepared filter is owned by the reader; call
  `Close` when the request finishes, and do not cache it across readers.
* Do not switch the embedded backend to `SearchCandidates` because it looks
  simpler: it retrieves every vector of every segment per query (measured 36.9 ms
  against 0.41 ms at 27k documents in one segment, k=192) and its results are
  equivalent only by coincidence of the filter.
* `HybridSearch` does not yet accept a prepared filter; if the hybrid path
  needs it, that is a follow-up API addition.

## Follow-ups not done

1. Wire `*PreparedVectorFilter` into `VectorSearchRequest`/`HybridSearchRequest`.
2. Reorder identifier reads by document number if the tie group ever becomes
   large enough for the ~1.7x locality difference to matter.
3. Clamp the per-segment request to `min(k, len(segment.payload.DocIDs))`. It is
   semantically neutral for the vector count but not provably neutral for which
   candidates the native search returns when a segment is full of identical
   vectors (it returned 153 results for a 200-vector segment at k=192), so it
   was left alone.
4. Replace the full score sort with a partial selection. The sort is no longer
   dominant after the `slices.SortFunc` change, so this is not currently worth
   the complexity.

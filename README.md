# ![Bluge](docs/bluge.png) Bluge

[![PkgGoDev](https://pkg.go.dev/badge/github.com/fy0/bluge)](https://pkg.go.dev/github.com/fy0/bluge)
[![Tests](https://github.com/fy0/bluge/workflows/Tests/badge.svg?branch=master&event=push)](https://github.com/fy0/bluge/actions?query=workflow%3ATests+event%3Apush+branch%3Amaster)
[![Lint](https://github.com/fy0/bluge/workflows/Lint/badge.svg?branch=master&event=push)](https://github.com/fy0/bluge/actions?query=workflow%3ALint+event%3Apush+branch%3Amaster)

modern text indexing in go - [blugelabs.com](https://www.blugelabs.com/)

## Installation

The module is published directly from this repository and does not require a
`replace` directive:

```console
go get github.com/fy0/bluge@master
```

```go
import "github.com/fy0/bluge"
```

## About This Branch

This branch is a performance- and correctness-focused fork of the official
Bluge v0.2.2 baseline (`5741419`).  It keeps the public document, writer, and
query APIs familiar while replacing the segment implementation and optimizing
the indexing and search hot paths.

### Search And Storage Scenarios

| Scenario | API or backend | Storage and lifecycle |
|---|---|---|
| Text search | `Writer` / `Reader` | Pure-Go BM25 indexing and search; no native library required. |
| Segment-integrated chunk search | `EmbeddedUSearchVectorBackend` | Text and vectors share segment snapshots; merges rebuild each segment's ANN payload. |
| Global sidecar ANN | `USearchVectorBackend` | One global vector index per field, persisted alongside the text index. |
| Independent embedding models | `Collection` (experimental) | One shared BM25 corpus with named vector spaces, selected-model readers, and independent vector publication and reclamation. |
| Exact vector baseline | `FlatVectorBackend` or Collection with `VectorStorage: "flat"` | Exact search with a cgo-free backend. |

### Performance Comparison

The following tables summarize the latest recorded results for
`Current v2[2026.10]`. Each workload lists its measurement conditions and date.

#### Text Indexing And BM25

The cross-engine comparison uses 1,547,580 documents, 176.24 MiB of body text,
ordinary safe writers with batches of 1,000, and score-descending Top-5
`LOCATION` searches. All measurements use Windows/amd64 on the same machine.
The current branch was remeasured on 2026-10-18 at `1df08ad`: build time,
throughput, and index size are three-run means; memory values are the highest
after-batch Go heap samples across those runs. Query values are medians from
ten fresh processes with 20 resident searches each, after one unmeasured
warm-up process. Other engines retain their July 2026 baselines.

| Metric | Current v2[2026.10] | Bluge Official | Bleve v2.5.7 |
|---|---:|---:|---:|
| Writer configuration | ordinary, batch 1,000 | ordinary, batch 1,000 | scorch, batch 1,000 |
| Indexing time | **1m56.025s average** | 2m39.744s | 2m25.974s |
| Throughput | **13,341 docs/s** | 9,688 docs/s | 10,602 docs/s |
| Fresh `LOCATION` lifecycle | **6.403 ms** | 39 ms | 35 ms |
| Resident `LOCATION` median | **1.666 ms** | 30.1 ms | 16.6 ms |
| Peak Go `Alloc` | 77.80 MiB | **71.71 MiB** | 78.53 MiB |
| Peak Go `Sys` | 113.71 MiB | **101.14 MiB** | 108.46 MiB |
| Final index size | **303.45 MiB average** | 475.40 MiB | 536.47 MiB |
| Release `.exe` size | **4.60 MiB / 4,819,456 bytes** | **4.60 MiB / 4,819,456 bytes** | 10.67 MiB / 11,192,320 bytes |

#### Improvement Ratios

Official Bluge is normalized to 100%. Query time uses the fresh lifecycle.
Lower percentages are better for time, size, and memory; higher percentages
are better for throughput.

| Engine | Query time | Index size | Binary size | Indexing time | Throughput | Peak `Alloc` | Peak `Sys` |
|---|---:|---:|---:|---:|---:|---:|---:|
| Bluge Official | 100% | 100% | 100% | 100% | 100% | 100% | 100% |
| **Current v2[2026.10]** | **16.42%** | **63.83%** | **100.00%** | **72.63%** | **137.70%** | 108.49% | 112.43% |
| Bleve v2.5.7 | 89.74% | 112.85% | 232.23% | 91.38% | 109.43% | 109.51% | 107.24% |

Relative to official Bluge, the current measurements have an 83.58% lower fresh
query lifecycle, a 36.17% smaller index, and 27.37% less indexing time, with
37.70% higher throughput.

#### LanceDB And zvec

| Metric | Current v2[2026.10] | LanceDB positions | LanceDB Match-only | zvec v0.5.1 adapter |
|---|---:|---:|---:|---:|
| Build time | **1m56.025s average** | 2m37.440s | **1m32.239s** | 13m6.275s |
| Throughput | **13,341 docs/s** | 9,830 docs/s | **16,778 docs/s** | 1,968 docs/s |
| Fresh `LOCATION` lifecycle | **6.403 ms** | 34 ms | 32.5 ms | 227 ms |
| Final directory | **303.45 MiB average** | 669.00 MiB | **242.95 MiB** | 1,549.43 MiB |

LanceDB Match-only omits token positions and phrase-query support. The zvec
adapter uses duplicate pre-tokenized fields and a compatibility vector.
External-engine figures retain the original single-run baselines; native RSS
and Go heap samples use different memory accounting. These tables compare
text indexing and search, with vector workloads reported separately below.

See the [October text benchmark report](docs/text-search-benchmark-20261018.md)
for current per-run data, raw logs, methodology, and reproduction commands, and
the [cross-engine source report](docs/data-simplification-v2-benchmark.md) plus
[official Bluge/Bleve baselines](docs/data-simplification-benchmark.md) for
the comparison measurements.

#### Multi-Model Collection

One shared text corpus with either one or two independent vector spaces:

| Metric | Current v2[2026.10], one model | Current v2[2026.10], two models |
|---|---:|---:|
| Final total file size | **8.37 MiB** | **15.52 MiB** |
| Query p50, per model | **0.0634 ms** | **0.0613 / 0.0633 ms** |
| Query p95, per model | **0.1027 ms** | **0.0952 / 0.1123 ms** |
| Total time for two update rounds | **253.2 ms** | **503.1 ms** |
| Reclamation after releasing the pinned reader | **5.4 ms** | **18.4 ms** |
| Recall@10, per model | **100%** | **100% / 99.6875%** |

Recorded for the 2026-10-17 report on Windows/amd64, Go 1.24.6,
`GOMAXPROCS=4`, using the same native DLL. Each model has 10,000 synthetic
128-dimension vectors, initially written in one batch, followed by two rounds
of 1% updates. Background merging is disabled; 32 queries per model are each
timed three times. Each configuration was measured once. The measurements
precede the final format cleanup and update/key-mapping follow-up changes
described in the report.

Collection loads selected model spaces, avoids constructing a full filter set
when all vectors match the text snapshot, and retains the current generation
plus generations pinned by readers. Vector publication writes a complete
generation; first load includes integrity checks and document-version reads.
These small synthetic workloads describe Collection storage and query behavior;
production scale and cross-engine ANN comparisons require separate measurements.

See the [Collection report](docs/collection-simplification-20261017.md) and
[machine-readable summary](docs/benchmarks/collection-simplification-20261017/summary.json)
for configurations, recall validation, and reproduction commands.

#### Embedded Vector Search

Embedded search reads stored identifiers for candidates that reach the global
Top-K or its cutoff tie group. Reader-bound prepared filters let repeated
queries reuse the same filter evaluation.

| Workload (26 segments, k=512) | Current v2[2026.10], median query time | Allocations/search |
|---|---:|---:|
| 27,000 docs, 64 dimensions, unfiltered | **9.38 ms** | 2,771 |
| 41,000 docs, 64 dimensions, unfiltered | **12.03 ms** | 2,772 |
| 27,000 docs, 1024 dimensions, unfiltered | **69.3 ms** | 2,781 |
| 27,000 docs, 64 dimensions, document-number filter | **11.29 ms** | 10,141 |

These synthetic workloads were measured on Windows/amd64 on 2026-09-14
through 2026-09-15. Times are medians of three process-level measurements:
100 iterations per process at 64 dimensions and 50 at 1024 dimensions.
The document-number filter matches one quarter of the corpus and includes
per-query filter evaluation. Timing depends on segment count, K, dimensions,
and filtering.

See the [embedded search report](docs/embedded-usearch-search-hot-path.md)
for all workloads, raw results, correctness checks, and reproduction commands.

### Branch Highlights

* **zapx-bluge v2 segment format** - the default backend is a pure-Go,
  text-focused adaptation of zapx.  It does not require FAISS or cgo.
* **Exact Block-Max BM25 WAND** - high-cardinality terms persist 64-posting
  blocks with non-dominated frequency/raw-norm impacts.  Score-descending
  Top-N term and OR queries use exact block upper bounds and a raw collector;
  unsupported similarities, sorts, aggregations, explanations, locations, and
  low-cardinality-only terms retain the ordinary collection path.
* **Compact text storage** - stored fields are packed into Snappy-compressed
  blocks, integer posting chunks select raw or compressed encoding by size,
  and native numeric and document-value metadata use compact canonical forms.
* **Exact BM25 inputs** - the standard BM25 IDF formula is corrected and
  guarded against invalid statistics.  Segments persist exact per-field
  document counts and total term frequencies, including multi-valued fields.
* **Configured norms are preserved** - custom `DefaultSimilarity` and
  `PerFieldSimilarity` norm calculations survive segment creation, persistence,
  and merges.
* **Native Bluge document build path** - analyzed fields and token frequencies
  are exported directly into the segment builder.  Custom
  `segment.Document` implementations continue to use the compatibility path.
* **Faster ordinary Writer persistence** - a bounded set of newly persisted,
  read-only segments is reused instead of being immediately reopened and
  mmap'ed.  Reuse is capped at four segments and 8 MiB; persistence, fsync,
  snapshot atomicity, and crash recovery are unchanged.
* **Parallel OfflineWriter** - segment construction uses bounded parallelism,
  followed by bounded, multi-round merges that honor the configured merge
  fan-in.
* **Lower query overhead** - collection statistics are read directly from the
  segment, BM25 can score raw stored norms, single-term queries avoid redundant
  work, Block-Max skips non-competitive postings without scoring them, and the
  common score-descending Top-N path avoids generic sort-value construction.
* **Stable `_id` behavior** - `_id` remains stored, indexed, and available as a
  doc value by default.
* **32-bit coverage** - atomic counters used by the segment code are aligned
  and the core packages are tested with `GOARCH=386`.

### Background Write Failures

Background segment merges and snapshot persistence retry with exponential
backoff: 1, 2, 4, 8, 16, and 32 seconds, then once per minute until success or
writer close. They retry even without new writes, allowing recovery after disk
space is freed. Only success resets the delay; new snapshots do not bypass it.
Use `config.WithBackgroundRetryBackoff(initialDelay, maxDelay)` to customize
the delays, for example:

```go
config = config.WithBackgroundRetryBackoff(time.Second, 5*time.Minute)
```

Zero uses the default for that delay; negative delays or a maximum below the
initial delay are rejected by `OpenWriter`.
Errors reported through `index.Config.AsyncError` include the retry status and
preserve the underlying cause for `errors.Is`. `Writer.Close()` returns an
outstanding background error promptly instead of waiting through backoff.

For a full storage volume, free space and background work will resume on a
subsequent attempt, without restarting the writer. Check write and close errors:
data whose persistence failed is not guaranteed durable. Backoff does not free
disk space; applications should also limit log and backup retention.

### Index Compatibility

The on-disk format is **zapx-bluge v2**.  It is intentionally incompatible with
zapx-bluge v1, the official ice-backed release, and the earlier experimental
zap v17 format; those indexes must be rebuilt or migrated offline.  The vector
API and zapx vector section extension points remain available.  The default
build does not require FAISS or cgo and returns `ErrVectorUnsupported` unless an
application supplies a vector backend.

The repository also includes opt-in vector backends. `FlatVectorBackend` is a
cgo-free exact baseline with sidecar persistence, updates, deletes, and Bluge
query filters. `USearchVectorBackend` is the first ANN integration. On 64-bit
Windows, Linux, and macOS it loads a separately-built USearch 2.26 native
library through `purego`, so the Go package remains buildable and runnable with
`CGO_ENABLED=0`. See the [vector
search selection and implementation notes](docs/vector-search-selection.md) and
the [USearch native adapter instructions](usearch-ffi/README.md) before
deploying the ANN backend.

`EmbeddedUSearchVectorBackend` is the format-integrated variant for the
read/write knowledge-base path. It stores one serialized USearch payload and
its local-doc mapping in the zapx vector section of each segment; it does not
create a per-field `.usearch` sidecar. Segment merges rebuild that payload
after applying deletes and doc-number remapping. If the native library is
missing while opening an existing index, text search remains available and
vector search returns `ErrVectorUnsupported`; writing new vector documents
still requires the native artifact.

For RAG ingestion, one Bluge `Document` should represent one chunk, matching
the row model used by LanceDB and zvec. Give the chunk its own `_id`, index the
chunk text for BM25, and store `source_id`, chunk position, and other metadata
on that same document. With the embedded or sidecar backends, attach its
embedding as a vector field. With `Collection`, write text once and publish
embeddings separately to named vector spaces using the matching document
revision. Text and ANN results then fuse on the same chunk ID; the full source
normally remains in an external source store instead of being indexed a second
time. Bluge does not split source material or run an embedding model.

`Writer.InsertMany` and `Writer.UpdateMany` submit a slice of documents as one
text-index batch and one vector mutation batch. `OfflineWriter.InsertMany`
groups a large initial corpus by its configured batch size and is preferred for
bulk construction. Native USearch additions cross the FFI boundary in bounded
row-major batches rather than one call per vector. The default text-only path
for `Insert` and `Batch` does not allocate vector changes or inspect document
IDs, while still rejecting an accidental vector field before the text batch is
accepted.

Vector requests can be composed without changing the text search API:

```go
vector := bluge.NewVectorSearchRequest("embedding", query).SetK(10)
hits, err := reader.SearchVectorRequest(ctx, vector)

hybrid := bluge.NewHybridSearchRequest(
    bluge.NewMatchQuery("wireless headphones").SetField("title"),
    vector,
).SetK(10).SetFusion(bluge.HybridFusionRRF)
hybridHits, err := reader.HybridSearch(ctx, hybrid)
```

`HybridSearch` supports a Bluge filter, weighted score fusion, and reciprocal
rank fusion. Bluge evaluates filters against the text snapshot and pushes the
resulting native key set into USearch's filtered HNSW traversal without native
callbacks into Go. The USearch native library is built and published
independently by the `usearch-ffi` GitHub Actions workflow; it is not compiled
by `go build`.

### Independent Vector Spaces

The experimental `Collection` API shares one BM25 corpus across independently
published embedding models. `collection.Reader("model-a")` and
`OpenCollectionReader(config, "model-a")` select and pin only the named spaces.
`UpsertDocuments(documents, versions...)` accepts explicit embedding-input
versions: reuse a version for metadata-only changes, and change it when the
embedding inputs change. `PendingVectors` lists missing or stale embeddings.
Model execution, migration, traffic switching, and rollback stay with the
application.

Vector generations are published atomically. Automatic maintenance reclaims
obsolete generations while preserving those held by readers, including across
processes. `Maintain` performs explicit cleanup and `VerifyCollection` checks
payload checksums and metadata. Current Collection files require checksummed
catalogs; earlier experimental formats must be rebuilt. USearch update workloads
require the native adapter built from this branch, which includes the
compaction/key-mapping fix. See the
[Collection usage and storage guide](docs/separate-vector-storage-experiment.md).

### BM25 Scoring Modes

`NewBM25Similarity()` is the default and implements the canonical BM25 term
score, including the `(k1 + 1)` numerator factor. It uses the number of
documents containing a field when calculating that field's average length.

Two explicit compatibility modes are also available:

```go
import "github.com/fy0/bluge/search/similarity"

config := bluge.DefaultConfig(path)

// Preserve scores produced by earlier versions of this Bluge fork.
config.DefaultSimilarity = similarity.NewLegacyBM25Similarity()

// Approximate Bleve v2.5.7 BM25 ranking for migration or comparison.
config.DefaultSimilarity = similarity.NewBleveBM25Similarity()
```

The Bleve mode reproduces its square-root term frequency, query
normalization, global document-count statistics, and per-segment dictionary
cardinality used as the average-length numerator. Its compatibility contract
is result ordering for Term, Match AND/OR, Phrase, and Boolean text queries;
raw scores are not portable between engines. All three built-in modes share
the same on-disk norm encoding, so switching among them does not require
rebuilding a `zapx-bluge v2` index.

## Features

* Supported field types:
    * Text, Numeric, Date, Geo Point
* Supported query types:
    * Term, Phrase, Match, Match Phrase, Prefix
    * Conjunction, Disjunction, Boolean
    * Numeric Range, Date Range
* BM25 Similarity/Scoring with pluggable interfaces
* Search result match highlighting
* Extendable Aggregations:
    * Bucketing
        * Terms
        * Numeric Range
        * Date Range
    * Metrics
        * Min/Max/Count/Sum
        * Avg/Weighted Avg
        * Cardinality Estimation ([HyperLogLog++](https://github.com/axiomhq/hyperloglog))
        * Quantile Approximation ([T-Digest](https://github.com/caio/go-tdigest)) 

## Indexing

```go
    config := bluge.DefaultConfig(path)
    writer, err := bluge.OpenWriter(config)
    if err != nil {
        log.Fatalf("error opening writer: %v", err)
    }
    defer writer.Close()

    doc := bluge.NewDocument("example").
        AddField(bluge.NewTextField("name", "bluge"))

    err = writer.Update(doc.ID(), doc)
    if err != nil {
        log.Fatalf("error updating document: %v", err)
    }
```

## Querying

```go
    reader, err := writer.Reader()
    if err != nil {
        log.Fatalf("error getting index reader: %v", err)
    }
    defer reader.Close()

    query := bluge.NewMatchQuery("bluge").SetField("name")
    request := bluge.NewTopNSearch(10, query).
        WithStandardAggregations()
    documentMatchIterator, err := reader.Search(context.Background(), request)
    if err != nil {
        log.Fatalf("error executing search: %v", err)
    }
    match, err := documentMatchIterator.Next()
    for err == nil && match != nil {
        err = match.VisitStoredFields(func(field string, value []byte) bool {
            if field == "_id" {
                fmt.Printf("match: %s\n", string(value))
            }
            return true
        })
        if err != nil {
            log.Fatalf("error loading stored fields: %v", err)
        }
        match, err = documentMatchIterator.Next()
    }
    if err != nil {
        log.Fatalf("error iterator document matches: %v", err)
    }
```

## Repobeats

![Alt](https://repobeats.axiom.co/api/embed/0d7f8bc7927e15b07f1ae592eeff01811c5a2f80.svg "Repobeats analytics image")

## License

Apache License Version 2.0

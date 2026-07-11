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

### Performance Comparison

| Metric | Current | Current (OfflineWriter) | Bluge Official | Bleve |
|---|---:|---:|---:|---:|
| Writer configuration | ordinary, batch 1,000 | batch 1,000, merge 10, concurrency 2 | ordinary, batch 1,000 | batch 1,000 |
| Build time | **2m4.044s** | **1m50.945s** | 2m39.744s | 2m25.974s |
| Throughput | **12,476 docs/s** | **13,949 docs/s** | 9,688 docs/s | 10,602 docs/s |
| Query-only, `LOCATION`, Top 5 | **20 ms** | **20 ms** | 39 ms | 35 ms |
| Peak Go `Alloc` | **72.77 MiB** | 81.02 MiB | 71.71 MiB | 78.53 MiB |
| Peak Go `Sys` | **109.58 MiB** | 134.30 MiB | 101.14 MiB | 108.46 MiB |
| Final index size | **300.93 MiB** | **285.49 MiB** | 475.40 MiB | 536.47 MiB |
| Release `.exe` size | **4.62 MiB / 4,845,568 bytes** | 4.62 MiB / 4,845,568 bytes | **4.60 MiB / 4,819,456 bytes** | 10.67 MiB / 11,192,320 bytes |

#### Improvement Ratios

| Comparison | Query time | Index size | Binary size | Build time | Throughput | Peak `Alloc` | Peak `Sys` |
|---|---:|---:|---:|---:|---:|---:|---:|
| Current OfflineWriter vs Current | same | **1.05x smaller** | same | **1.12x faster** | **1.12x higher** | 1.11x more | 1.23x more |
| Current vs Bluge Official | **1.95x faster** | **1.58x smaller** | 1.01x more | **1.29x faster** | **1.29x higher** | 1.01x more | 1.08x more |
| Current vs Bleve | **1.75x faster** | **1.78x smaller** | **2.31x smaller** | **1.18x faster** | **1.18x higher** | **1.08x less** | 1.01x more |

The following fresh results, measured on 2026-07-11, compare this branch at
`67210a9`, the official Bluge v0.2.2 baseline at `5741419`, and Bleve v2.5.7
using its `scorch` index and BM25 scoring. All variants used the same workload:

* Windows/amd64, Go 1.24.6
* Intel Core i5-13600KF (14 cores / 20 logical CPUs), 64 GiB RAM
* `data-simplification.tar.bz2`: 1,547,580 documents and 176.24 MiB of indexed
  body text
* ordinary safe `Writer`, batch size 1,000
* the additional Current OfflineWriter run used the default merge fan-in of 10
  and default build/merge concurrency of 2
* Bleve text fields used `unicode` tokenization plus lowercase filtering to
  match Bluge rather than Bleve's stop-word-removing default standard analyzer
* build figures are one low-load run per engine; query-only figures are medians
  of ten fresh process runs
* release binary sizes use Windows/amd64, `CGO_ENABLED=0`, `-trimpath`,
  `-buildvcs=false`, and `-ldflags="-s -w"`

Current ordinary and OfflineWriter modes share one executable; selecting the
writer does not require a separate build. The release-size comparison uses the
same benchmark functionality and build flags for all three executables. The
OfflineWriter query cell uses the Current query-only median because both modes
produce the same final format and identical Top-5 results; a separate ten-run
OfflineWriter query distribution was not collected. Improvement ratios state
the comparison direction explicitly; resource increases are shown as `more`
rather than presented as improvements.

Sorted query-only distributions were
`38/38/38/39/39/39/39/40/41/42 ms` for the official baseline,
`19/19/20/20/20/20/21/21/21/21 ms` for this branch, and
`33/33/34/34/35/35/36/36/37/45 ms` for Bleve.

Against the previous pre-compaction measurement at `5fbb646`, the current
branch reduced Writer build time from 2m10.766s to 2m4.044s (**-5.14%**), index
size from 552.03 MiB to 300.93 MiB (**-45.49%**), and query-only median from
22 ms to 20 ms (**-9.09%**).

For the single-field `LOCATION` query, canonical BM25 and the official baseline
shared four of their Top 5 document IDs. Bleve's Top 5 did not overlap because
Bleve v2.5.7 uses different term-frequency and average-length behavior. Setting
`NewBleveBM25Similarity()` on the same `zapx-bluge v1` index reproduced Bleve's
five IDs, ordering, and scores without rebuilding the index.

Reproduce the workload with:

```console
go run ./examples/data_simplification -archive <path-to-data-simplification.tar.bz2> -index <index-directory> -query LOCATION -batch 1000 -keep-index
```

See [the full benchmark reproduction guide](docs/data-simplification-benchmark.md)
for the data checksum, exact workload contract, official Bluge and Bleve setup,
query-only procedure, and captured outputs.

Wall-clock results are sensitive to background load, storage, Go version, and
segment merge timing.  Compare multiple-run medians rather than a single run.

### Branch Highlights

* **zapx-bluge v1 segment format** - the default backend is a pure-Go,
  text-focused adaptation of zapx.  It does not require FAISS or cgo.
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
  work, and the common score-descending Top-N path avoids generic sort-value
  construction.
* **Stable `_id` behavior** - `_id` remains stored, indexed, and available as a
  doc value by default.
* **32-bit coverage** - atomic counters used by the segment code are aligned
  and the core packages are tested with `GOARCH=386`.

### Index Compatibility

The on-disk format is **zapx-bluge v1**.  This is a breaking change from the
official ice-backed release and from the earlier experimental zap v17 format.
Existing ice or experimental zap indexes must be rebuilt or migrated offline;
the core library does not open them.  The vector API is an extension boundary
only and returns `ErrVectorUnsupported` unless an application supplies a vector
backend.

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
rebuilding a `zapx-bluge v1` index.

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

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

### Branch Highlights

* **zapx-bluge v1 segment format** - the default backend is a pure-Go,
  text-focused adaptation of zapx.  It does not require FAISS or cgo.
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

### Performance Against Official Bluge

The following results, measured on 2026-07-10, compare this branch at
`eb9cdb7` with the official Bluge v0.2.2 baseline at `5741419`.  Both revisions
used the same runner and workload:

* Windows/amd64, Go 1.24.6
* Intel Core i5-13600KF (14 cores / 20 logical CPUs), 64 GiB RAM
* `data-simplification.tar.bz2`: 1,547,580 documents and 176.24 MiB of indexed
  body text
* ordinary safe `Writer`, batch size 1,000
* build figures are medians of three runs; query-only figures are medians of ten
  fresh process runs

| Metric | Official `5741419` | This branch | Change |
|---|---:|---:|---:|
| Writer build time | 2m59.686s | 1m54.431s | **-36.32% (1.57x throughput)** |
| Writer throughput | 8,613 docs/s | 13,524 docs/s | **+57.02%** |
| Query-only, `LOCATION`, Top 5 | 40 ms | 22 ms | **-45.00% (1.82x)** |
| Peak Go `Alloc` | 76.25 MiB | 74.02 MiB | **-2.92%** |
| Peak Go `Sys` | 109.74 MiB | 121.02 MiB | +10.28% |
| Final index size | 475.40 MiB | 552.03 MiB | +16.12% |

Build run distributions were `3m18.817s / 2m59.686s / 2m47.807s` for the
official baseline and `1m54.431s / 1m59.383s / 1m52.245s` for this branch.
Sorted query-only distributions were `38/39/39/40/40/40/40/41/41/42 ms` and
`21/21/21/22/22/22/24/24/24/24 ms`, respectively.

The latest bounded persisted-segment reuse is also isolated by a direct A/B
against its immediate parent: ordinary Writer build median improved from
1m58.347s to 1m54.431s (**-3.31%**) while Peak `Alloc` and `Sys` remained within
6.73% of that parent.  The larger index is an explicit tradeoff of the new
format and its exact field statistics.  BM25 scores are not expected to match
the official baseline because this branch fixes the baseline IDF formula and
persists exact statistics; results are stable across rebuilds of this format.

Reproduce the workload with:

```console
go run ./examples/data_simplification -archive <path-to-data-simplification.tar.bz2> -index <index-directory> -query LOCATION -batch 1000 -keep-index
```

Wall-clock results are sensitive to background load, storage, Go version, and
segment merge timing.  Compare multiple-run medians rather than a single run.

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

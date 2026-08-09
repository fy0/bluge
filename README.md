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

| Metric | Current v2 | Bluge Official | Bleve v2.5.7 |
|---|---:|---:|---:|
| Writer configuration | ordinary, batch 1,000 | ordinary, batch 1,000 | scorch, batch 1,000 |
| Indexing time | **2m2.235s average** | 2m39.744s | 2m25.974s |
| Throughput | **12,674 docs/s** | 9,688 docs/s | 10,602 docs/s |
| Fresh `LOCATION` lifecycle | **8.203 ms** | 39 ms | 35 ms |
| Resident `LOCATION` median | **1.630 ms** | 30.1 ms | 16.6 ms |
| Peak Go `Alloc` | 78.39 MiB | **71.71 MiB** | 78.53 MiB |
| Peak Go `Sys` | 118.05 MiB | **101.14 MiB** | 108.46 MiB |
| Final index size | **303.48 MiB average** | 475.40 MiB | 536.47 MiB |
| Release `.exe` size | **4.53 MiB / 4,749,824 bytes** | 4.60 MiB / 4,819,456 bytes | 10.67 MiB / 11,192,320 bytes |

#### Improvement Ratios

Official Bluge is normalized to 100%. Values below 100% are better for query
time, index size, binary size, indexing time, and memory; values above 100% are
better only for throughput.

| Engine | Query time | Index size | Binary size | Indexing time | Throughput | Peak `Alloc` | Peak `Sys` |
|---|---:|---:|---:|---:|---:|---:|---:|
| Bluge Official | 100% | 100% | 100% | 100% | 100% | 100% | 100% |
| **Current v2** | **21.03%** | **63.84%** | **98.56%** | **76.52%** | **130.82%** | 109.32% | 116.72% |
| Bleve v2.5.7 | 89.74% | 112.85% | 232.23% | 91.38% | 109.43% | 109.51% | 107.24% |

Current v2 lowers the fresh query lifecycle by 78.97%, index size by 36.16%,
and indexing time by 23.48% relative to official Bluge, while increasing
throughput by 30.82%.

#### LanceDB And zvec

| Metric | Current v2 | LanceDB positions | LanceDB Match-only | zvec v0.5.1 adapter |
|---|---:|---:|---:|---:|
| Build time | **2m2.235s average** | 2m37.440s | **1m32.239s** | 13m6.275s |
| Throughput | **12,674 docs/s** | 9,830 docs/s | **16,778 docs/s** | 1,968 docs/s |
| Fresh `LOCATION` lifecycle | **8.203 ms** | 34 ms | 32.5 ms | 227 ms |
| Final directory | **303.48 MiB average** | 669.00 MiB | **242.95 MiB** | 1,549.43 MiB |

LanceDB Match-only omits token positions and phrase-query support, so it is not
feature-equivalent to the position-preserving configurations. The zvec adapter
uses duplicate pre-tokenized fields and a compatibility vector. External engine
builds are historical single runs on the same machine and workload; native RSS
and Go heap samples are not directly comparable.

#### Exact Block-Max BM25 WAND

| Query | Total candidates | Scored candidates | Reduction | Resident median | p95 |
|---|---:|---:|---:|---:|---:|
| `LOCATION` | 287,959 | 1,979 | 99.31% | 1.630 ms | 2.249 ms |
| `THE` | 1,042,353 | 3,136 | 99.70% | 1.684 ms | 2.254 ms |
| `LOCATION OR THE` | 1,330,312 | 146,084 | 89.02% | not profiled | not profiled |

The build and directory figures are four-run averages. Query values are medians
from ten fresh processes, with 20 resident searches per process and no result
cache. The current v2 memory row is from the validation build; the other memory
rows retain their original single-run measurements. WAND changes candidate
traversal but preserves scoring semantics: full indexes matched the ordinary
collector for Top-1/5/20/100 by document ID, tie
order, and every bit of the `float64` score.

See the [detailed v1/v2 benchmark report](docs/data-simplification-v2-benchmark.md)
for per-run data, root-cause evidence, methodology, validation commands, and
remaining scheduling risks. The older standalone measurements remain in the
[historical benchmark report](docs/data-simplification-benchmark.md).

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

### Index Compatibility

The on-disk format is **zapx-bluge v2**.  It is intentionally incompatible with
zapx-bluge v1, the official ice-backed release, and the earlier experimental
zap v17 format; those indexes must be rebuilt or migrated offline.  The vector
API and zapx vector section extension points remain available.  The default
build does not require FAISS or cgo and returns `ErrVectorUnsupported` unless an
application supplies a vector backend.

The repository also includes opt-in vector backends. `FlatVectorBackend` is a
cgo-free exact baseline with sidecar persistence, updates, deletes, and Bluge
query filters. `USearchVectorBackend` is the first ANN integration: on Windows,
it loads a separately-built USearch 2.26 native DLL through `purego`, so the Go
package remains buildable and runnable with `CGO_ENABLED=0`. See the [vector
search selection and implementation notes](docs/vector-search-selection.md) and
the [USearch native adapter instructions](usearch-ffi/README.md) before
deploying the ANN backend.

`EmbeddedUSearchVectorBackend` is the format-integrated variant for the
read/write knowledge-base path. It stores one serialized USearch payload and
its local-doc mapping in the zapx vector section of each segment; it does not
create a per-field `.usearch` sidecar. Segment merges rebuild that payload
after applying deletes and doc-number remapping. If the DLL is missing while
opening an existing index, text search remains available and vector search
returns `ErrVectorUnsupported`; writing new vector documents still requires
the native artifact.

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
rank fusion. The USearch DLL is built and published independently by the
`usearch-ffi` GitHub Actions workflow; it is not compiled by `go build`.

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

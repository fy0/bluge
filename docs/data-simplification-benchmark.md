# Data Simplification Benchmark Reproduction

This document reproduces the ordinary Writer benchmark used to compare:

1. this fork at `055c561` using `zapx-bluge v1` and canonical BM25;
2. official Bluge v0.2.2 at `57414197005148539c5dc5db8ab581594969df79`
   using its default ice v1 segment format; and
3. Bleve v2.5.7 using `scorch` and BM25.

The measurements below were collected on 2026-07-11. Build figures are one
low-load run per engine. Query-only figures are medians of ten fresh process
runs, so the build numbers should not be treated as stable multi-run medians.

## Data Set

Download the archive directly from the source repository:

```text
https://raw.githubusercontent.com/louismartin/dress-data/master/data-simplification.tar.bz2
```

PowerShell:

```powershell
$archive = "$HOME\Downloads\data-simplification.tar.bz2"
Invoke-WebRequest `
  -Uri 'https://raw.githubusercontent.com/louismartin/dress-data/master/data-simplification.tar.bz2' `
  -OutFile $archive
Get-FileHash -Algorithm SHA256 $archive
```

curl:

```console
curl -L -o data-simplification.tar.bz2 \
  https://raw.githubusercontent.com/louismartin/dress-data/master/data-simplification.tar.bz2
sha256sum data-simplification.tar.bz2
```

Expected archive metadata:

| Property | Value |
|---|---|
| Size | 58,432,902 bytes (55.73 MiB) |
| SHA-256 | `42b6b69724d9be6de33e73c4b58bd6a93c46b8b5a26e78d233eb0e463b03718b` |

## Measured Environment

| Property | Value |
|---|---|
| OS/architecture | Windows/amd64 |
| Go | 1.24.6 |
| CPU | Intel Core i5-13600KF, 14 cores / 20 logical CPUs |
| RAM | 64 GiB |
| Documents | 1,547,580 |
| Accepted archive files | 24 |
| Indexed body text | 176.24 MiB |
| Batch size | 1,000 documents |
| Query | Match `LOCATION` in `body`, Top 5 |

Record the Go version and CPU load before each build. The reported runs began
with total CPU samples below approximately 6 percent. Antivirus scanning,
filesystem cache state, storage load, and merge timing can materially affect a
single build.

## Workload Contract

All runners must implement the same archive and field behavior. A run is not
comparable unless it reports exactly 1,547,580 documents and 176.24 MiB of body
text.

Archive selection:

* accept regular tar entries ending exactly in `.src` or `.dst`;
* reject entries ending in `.vocab.tmp.t7` or `.map.t7`;
* scan each accepted file line by line;
* trim surrounding whitespace and skip empty lines;
* preserve the original one-based source line number;
* construct the document ID as `<tar-entry-name>:<line-number>`.

Fields:

| Field | Analyzer | Indexed | Stored | Doc values |
|---|---|---:|---:|---:|
| `_id` | keyword / engine ID | yes | Bluge field / Bleve hit ID | Bluge default / Bleve internal |
| `file` | Unicode + lowercase | yes | yes | no |
| `side` | keyword | yes | yes | no |
| `body` | Unicode + lowercase | yes | yes | no |

The Bluge benchmark uses an ordinary safe `Writer`, not `OfflineWriter`.
Official Bluge already has a real atomic batch API: `bluge.NewBatch()`,
`batch.Insert(doc)`, and `writer.Batch(batch)`. The runner accumulates 1,000
documents and calls `Writer.Batch` once, then resets and reuses the batch.

The essential Bluge loop is:

```go
writer, err := bluge.OpenWriter(bluge.DefaultConfig(indexPath))
if err != nil {
    return err
}
defer writer.Close()

batch := bluge.NewBatch()
batchDocs := 0

// For every accepted, non-empty archive line:
doc := bluge.NewDocument(id).
    AddField(bluge.NewTextField("file", fileName).StoreValue()).
    AddField(bluge.NewKeywordField("side", side).StoreValue()).
    AddField(bluge.NewTextField("body", body).StoreValue())
batch.Insert(doc)
batchDocs++

if batchDocs >= 1000 {
    if err := writer.Batch(batch); err != nil {
        return err
    }
    batch.Reset()
    batchDocs = 0
}
```

Close the Writer before measuring directory size or opening a query Reader.
Sample `runtime.MemStats.Alloc` and `runtime.MemStats.Sys` after every batch and
retain their maxima.

## Current Fork

Check out the measured revision and run the repository benchmark harness:

```powershell
git checkout 055c561

go run ./examples/data_simplification `
  -archive $archive `
  -index "$env:TEMP\bluge-fork-retest" `
  -query LOCATION `
  -batch 1000 `
  -keep-index
```

The default configuration uses canonical BM25. Do not enable legacy or Bleve
compatibility scoring for this primary measurement.

Observed build output:

```text
index:   ...\bluge-fork-retest (300.93 MiB, kept=true)
files:   24 text files
docs:    1547580
text:    176.24 MiB uncompressed indexed text
batch:   1000 docs
build:   2m4.044s, 12476 docs/sec, 1.42 MiB/sec
query:   "LOCATION" in 107ms, 5 hits shown
memory:  peak Alloc=72.77 MiB peak Sys=109.58 MiB final Alloc=15.48 MiB final Sys=109.58 MiB
```

Canonical BM25 Top 5:

```text
1  3.3314  data-simplification/wikilarge/wiki.full.aner.train.dst:155625
2  3.3242  data-simplification/wikilarge/wiki.full.aner.train.dst:285127
3  3.3242  data-simplification/wikismall/PWKP_108016.tag.80.aner.train.dst:85444
4  3.3156  data-simplification/wikilarge/wiki.full.aner.train.dst:44789
5  3.3122  data-simplification/wikilarge/wiki.full.aner.train.src:284192
```

### Additional OfflineWriter Builds

Both OfflineWriter builds used 1,000 documents per initial segment. The first
used the default merge fan-in of 10 and the default concurrency of 2, without
calling `WithOfflineWriterConcurrency`:

```go
config := bluge.DefaultConfig(indexPath)
writer, err := bluge.OpenOfflineWriter(config, 1000, 10)
```

Default configuration output:

```text
index:   ...\bluge-offline-default-c2-m10 (285.49 MiB, kept=true)
files:   24 text files
docs:    1547580
text:    176.24 MiB uncompressed indexed text
batch:   1000 docs
writer:  offline, merge-max=10 concurrency=2 (defaults)
build:   1m50.945s, 13949 docs/sec, 1.59 MiB/sec
query:   "LOCATION" in 49ms, 5 hits shown
memory:  peak Alloc=81.02 MiB peak Sys=134.30 MiB final Alloc=36.05 MiB final Sys=134.30 MiB
```

The tuned build used a merge fan-in of 32 segments and six concurrent segment
build or merge tasks:

```go
config := bluge.DefaultConfig(indexPath).
    WithOfflineWriterConcurrency(6)

writer, err := bluge.OpenOfflineWriter(config, 1000, 32)
if err != nil {
    return err
}

// Insert every document from the same workload contract above.
if err := writer.Insert(doc); err != nil {
    return err
}

// Close waits for asynchronous builds and performs all merge rounds.
if err := writer.Close(); err != nil {
    return err
}
```

The third argument is `maxSegmentsToMerge`: each merge task consumes at most
that many segments. It does not control the final segment count. `Close()`
completed both hierarchical merge plans into one `.seg` file and one snapshot
file.

Memory was sampled every 25 ms throughout archive ingestion, asynchronous
segment construction, and the final `Close()` merge. Observed output:

```text
index:   ...\bluge-offline-s32-c6 (285.49 MiB, kept=true)
files:   24 text files
docs:    1547580
text:    176.24 MiB uncompressed indexed text
batch:   1000 docs
writer:  offline, merge-max=32 concurrency=6
build:   1m11.561s, 21626 docs/sec, 2.46 MiB/sec
query:   "LOCATION" in 46ms, 5 hits shown
memory:  peak Alloc=155.23 MiB peak Sys=187.85 MiB final Alloc=52.82 MiB final Sys=187.85 MiB
```

The post-build queries were correctness checks. Both OfflineWriter indexes had
the same Top 5 IDs, ordering, and canonical BM25 scores as the ordinary Writer
index. No 10-run query-only distribution was collected for these additional
builds.

| Metric | Ordinary Writer | OfflineWriter 10 / 2 | OfflineWriter 32 / 6 |
|---|---:|---:|---:|
| Build time | 2m4.044s | 1m50.945s | **1m11.561s** |
| Throughput | 12,476 docs/s | 13,949 docs/s | **21,626 docs/s** |
| Index size | 300.93 MiB | **285.49 MiB** | **285.49 MiB** |
| Peak Go `Alloc` | 72.77 MiB | 81.02 MiB | 155.23 MiB |
| Peak Go `Sys` | 109.58 MiB | 134.30 MiB | 187.85 MiB |

The default OfflineWriter was 10.56 percent faster than the ordinary Writer
and used 11.34 percent more peak `Alloc`. The 32 / 6 configuration was another
35.50 percent faster than the default OfflineWriter, but used 91.59 percent
more peak `Alloc` than the default.

Both OfflineWriter configurations produced exactly 299,356,126 bytes: one
299,356,102-byte segment and one 24-byte snapshot. The smaller index relative
to the ordinary Writer therefore comes from OfflineWriter forcing all data
into one final segment, not from increasing the merge fan-in from 10 to 32.

Very large fan-in values are not automatically better. With 1,548 initial
segments, fan-in 10 takes approximately four merge rounds, fan-in 32 takes
three, and fan-in 300 takes two. However, one fan-in-300 task may open and merge
up to 300 segment readers at once. Multiple concurrent tasks multiply the
file-handle, mmap, heap, and I/O pressure. Use a larger fan-in only after
measuring repeated build medians and peak memory on the deployment platform;
32 is a more conservative high-throughput setting for this workload than 300.

## Official Bluge v0.2.2

Create a detached worktree at the exact official baseline:

```powershell
$repo = (Get-Location).Path
git worktree add --detach .worktrees/master-original `
  57414197005148539c5dc5db8ab581594969df79

$officialRunner = Join-Path $repo '.worktrees\master-original\examples\data_simplification'
New-Item -ItemType Directory -Force $officialRunner | Out-Null
Copy-Item `
  (Join-Path $repo 'examples\data_simplification\main.go') `
  (Join-Path $officialRunner 'main.go')

$runnerPath = Join-Path $officialRunner 'main.go'
$runnerSource = Get-Content -Raw -LiteralPath $runnerPath
$runnerSource.Replace('github.com/fy0/bluge', 'github.com/blugelabs/bluge') |
  Set-Content -NoNewline -LiteralPath $runnerPath
```

This copies the same benchmark harness and changes only the Bluge import. Run
it from the official worktree so its `go.mod` selects the original ice
dependencies:

```powershell
Set-Location .worktrees/master-original

go run ./examples/data_simplification `
  -archive $archive `
  -index "$env:TEMP\bluge-official-retest" `
  -query LOCATION `
  -batch 1000 `
  -keep-index
```

Observed build output:

```text
index:   ...\bluge-official-retest (475.40 MiB, kept=true)
files:   24 text files
docs:    1547580
text:    176.24 MiB uncompressed indexed text
batch:   1000 docs
build:   2m39.744s, 9688 docs/sec, 1.10 MiB/sec
query:   "LOCATION" in 86ms, 5 hits shown
memory:  peak Alloc=71.71 MiB peak Sys=101.14 MiB final Alloc=1.80 MiB final Sys=101.14 MiB
```

Official Top 5:

```text
1  13.5807  data-simplification/wikilarge/wiki.full.aner.train.src:284192
2  13.5807  data-simplification/wikismall/PWKP_108016.tag.80.aner.train.src:83171
3  13.5558  data-simplification/wikilarge/wiki.full.aner.train.dst:155625
4  13.5477  data-simplification/wikilarge/wiki.full.aner.train.dst:44789
5  13.5319  data-simplification/wikilarge/wiki.full.aner.train.dst:285127
```

## Bleve v2.5.7

Use Bleve v2.5.7. Bleve v2.6.0 requires a newer Go toolchain than the measured
Go 1.24.6 environment.

```console
mkdir bleve-reference-runner
cd bleve-reference-runner
go mod init example.com/bleve-reference-runner
go get github.com/blevesearch/bleve/v2@v2.5.7
```

Use `bleve.NewIndexMapping()`, set `ScoringModel = "bm25"`, and use the default
`scorch` index type. Do not use Bleve's built-in standard analyzer because it
also removes English stop words. Define the analyzer that matches Bluge:

```go
mapping := bleve.NewIndexMapping()
mapping.ScoringModel = "bm25"
mapping.DefaultMapping = bleve.NewDocumentStaticMapping()

err := mapping.AddCustomAnalyzer("bluge_standard", map[string]interface{}{
    "type":          "custom",
    "tokenizer":     "unicode",
    "token_filters": []interface{}{"to_lower"},
})
```

The runner must also register Bleve's custom analyzer implementation:

```go
import _ "github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
```

For `file` and `body`, use text field mappings with analyzer
`bluge_standard`. For `side`, use `bleve.NewKeywordFieldMapping()`. Set all
three mappings to indexed and stored, with term vectors, `_all`, and doc values
disabled. Use `index.NewBatch()`, call `batch.Index(id, document)` for 1,000
documents, and submit with `index.Batch(batch)`. Close the index before sizing
or reopening it for search.

The query setup is:

```go
query := bleve.NewMatchQuery("LOCATION")
query.SetField("body")
request := bleve.NewSearchRequestOptions(query, 5, 0, false)
request.Fields = []string{"file", "side", "body"}
result, err := index.Search(request)
```

Equivalent command for the reference runner:

```powershell
go run . `
  -archive $archive `
  -index "$env:TEMP\bleve-retest" `
  -query LOCATION `
  -batch 1000
```

Observed build output:

```text
engine:  Bleve v2.5.7 (scorch, BM25, unicode+lowercase)
index:   ...\bleve-retest (536.47 MiB)
files:   24 text files
docs:    1547580
text:    176.24 MiB uncompressed indexed text
batch:   1000 docs
build:   2m25.974s, 10602 docs/sec, 1.21 MiB/sec
query:   "LOCATION" in 129ms, 5 hits shown
memory:  peak Alloc=78.53 MiB peak Sys=108.46 MiB final Alloc=3.09 MiB final Sys=108.46 MiB
```

Bleve Top 5:

```text
1  0.9173  data-simplification/wikilarge/wiki.full.aner.ori.train.dst:147890
2  0.9173  data-simplification/wikilarge/wiki.full.aner.ori.train.dst:152048
3  0.9173  data-simplification/wikilarge/wiki.full.aner.ori.train.dst:156892
4  0.9173  data-simplification/wikilarge/wiki.full.aner.ori.train.dst:157766
5  0.9173  data-simplification/wikilarge/wiki.full.aner.ori.train.dst:158871
```

## Query-Only Runs

Each query-only timing must come from a fresh process that opens the retained
index, executes one Top-5 query, closes the reader/index, and exits. Do not
include `go run` compilation time in the reported query duration; time only the
open-and-query operation inside the program.

PowerShell example for a runner with `-query-only` support:

```powershell
1..10 | ForEach-Object {
  go run ./examples/data_simplification `
    -archive $archive `
    -index "$env:TEMP\bluge-fork-retest" `
    -query LOCATION `
    -batch 1000 `
    -query-only
}
```

Sorted distributions:

| Engine | Ten fresh-process query times | Median |
|---|---|---:|
| Official Bluge | `38/38/38/39/39/39/39/40/41/42 ms` | 39 ms |
| This fork | `19/19/20/20/20/20/21/21/21/21 ms` | 20 ms |
| Bleve | `33/33/34/34/35/35/36/36/37/45 ms` | 35 ms |

## Results Summary

| Metric | Official Bluge | This fork | Bleve v2.5.7 |
|---|---:|---:|---:|
| Writer build time | 2m39.744s | **2m4.044s** | 2m25.974s |
| Writer throughput | 9,688 docs/s | **12,476 docs/s** | 10,602 docs/s |
| Query-only median | 39 ms | **20 ms** | 35 ms |
| Peak Go `Alloc` | 71.71 MiB | 72.77 MiB | 78.53 MiB |
| Peak Go `Sys` | 101.14 MiB | 109.58 MiB | 108.46 MiB |
| Final index size | 475.40 MiB | **300.93 MiB** | 536.47 MiB |

Relative to official Bluge, this fork's measured build was 22.35 percent
faster, throughput was 28.78 percent higher, query median was 48.72 percent
lower, and the index was 36.70 percent smaller. Relative to Bleve, the build
was 15.02 percent faster, throughput was 17.68 percent higher, query median was
42.86 percent lower, and the index was 43.91 percent smaller.

Canonical BM25 in this fork and official Bluge shared four of their five IDs
for this query. Bleve's Top 5 did not overlap because Bleve v2.5.7 uses
different term-frequency and average-length behavior. Setting
`similarity.NewBleveBM25Similarity()` on the same `zapx-bluge v1` index
reproduced all five Bleve IDs, their order, and their scores without rebuilding
the index; that compatibility query completed in 21 ms in the measured run.

## Cleanup

Close every Writer, Reader, and Bleve Index before cleanup. On Windows this is
required to release mmap-backed files and directory locks.

```powershell
Remove-Item -Recurse -Force -LiteralPath "$env:TEMP\bluge-fork-retest"
Remove-Item -Recurse -Force -LiteralPath "$env:TEMP\bluge-official-retest"
Remove-Item -Recurse -Force -LiteralPath "$env:TEMP\bleve-retest"
```

Before publishing results, verify that no temporary index, pprof file, or test
binary remains and report whether build values are single runs or multi-run
medians.

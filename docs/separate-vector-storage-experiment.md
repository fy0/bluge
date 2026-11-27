# Collection Vector Storage

`Collection` provides one BM25 corpus with independent vector spaces. A vector
space has a stable ID such as `e5-base-v1` and immutable dimension, metric, and
field metadata. Model, model revision, and preprocessing descriptions are optional.
The application selects models and decides when to switch traffic between spaces.

```go
config := bluge.DefaultCollectionConfig("./kb")
collection, _ := bluge.OpenCollection(config)
defer collection.Close()

collection.CreateVectorSpace(bluge.VectorSpaceSpec{
    ID: "model-a-v1", Dims: 768, // Defaults: field "embedding", cosine similarity.
})
revisions, _ := collection.UpsertDocuments(documents)
collection.UpsertVectors("model-a-v1", []bluge.VectorRecord{{
    ID: revisions[0].ID, Revision: revisions[0].Revision, Vector: embedding,
}})
reader, _ := collection.Reader("model-a-v1")
defer reader.Close()
hits, _ := reader.SearchVectorRequest(ctx, "model-a-v1",
    bluge.NewVectorSearchRequest("embedding", query).SetK(10))
```

The text index is stored under `text/`. The collection-level `collection.json`
stores only the format version, corpus identity, and storage backend.
Each vector space has a `spec.json` under
`vectors/space-<id>/`, containing its specification and an ordered generation list.

Each generation consists of two files. Generation numbers use 20 decimal digits:

* `index-<generation>` contains the native HNSW or exact vector data.
* `metadata-<generation>.json` contains the space specification, vector revisions,
  index file size, and native key mapping when applicable.

The index and metadata are written and synced before an atomic replacement of
`spec.json`, the sole vector commit point. Readers select the newest generation
listed in that file. JSON publication uses a temporary file, `Sync`, and atomic
replacement. Opening a space checks the format version, corpus identity, strictly
increasing generation list, and referenced file presence.

Each document stores its revision in the internal `_collection_revision` field.
The revision and text commit atomically in one text batch; callers cannot supply
that reserved field. Document writes and deletes do not acquire `generations.lock`.
Readers capture a text snapshot and pinned vector generations. Vector searches,
`PendingVectors`, and `VectorStatus` load and cache revisions from that reader's
text snapshot on first use. Opening a reader and running text searches do not scan
document revisions.

By default, text updates create a fresh revision, so an older embedding is
excluded until the caller writes a vector with the new revision. Alternatively,
pass one nonempty embedding input version per document:
`collection.UpsertDocuments(documents, versions...)`. Keeping that version
preserves vectors when only unrelated metadata changes; change it when embedding
inputs change. `UpsertVectors` rejects stale revisions before publishing a
generation. `PendingVectors` reports missing or stale embeddings; the application
runs models and manages retry queues.

`collection.Reader("model-a-v1")` and
`bluge.OpenCollectionReader(config, "model-a-v1")` load catalogs and pin generations
only for the requested names. Omit names to select all registered spaces.
When every stored vector matches the text snapshot, searches use the native
unfiltered path unless a business filter is supplied. Revision exclusions still
intersect with business filters, including prepared filters.

Set `CollectionConfig.VectorStorage` to `"flat"` for a cgo-free exact-search
backend. The default `"usearch"` mode loads the native library through
`LibraryPath` or `BLUGE_USEARCH_LIBRARY_PATH` and requires `ABI_VERSION == 4`.
The current Collection disk format version is 3.

## Generation Maintenance

Publication attempts maintenance of that space; opening a writer maintains all
spaces. Only the current generation and older generations held by readers are
retained. Both `Collection.Reader` and `OpenCollectionReader` acquire shared
kernel file locks for **all** selected spaces before returning, even when vectors
will only be loaded much later. `Close` releases them after closing native handles;
process termination also releases them. No PID timeout or in-process reference
count determines whether a generation is safe to delete.

`generations.lock` is a permanent coordinator inode. Discovery, reader lease
acquisition, spec publication, and retirement use this lock. Maintenance also
requires the collection writer lock, so it cannot delete active staging data.
Readers must be able to create/open lease files for writing. Use local
filesystems with working kernel locks and atomic rename.

```go
result, err := collection.Maintain(ctx)
// Restrict cleanup to a named model:
result, err = collection.Maintain(ctx, "model-a-v1")
// When no writer is open:
result, err = bluge.MaintainCollection(ctx, config)
// UpsertVectors returning nil means committed. Cleanup errors are separate:
maintenanceErr := collection.LastMaintenanceError()
// Metadata and file-size verification (all spaces by default):
err = bluge.VerifyCollection(ctx, config, "model-a-v1")
```

Maintenance validates the selected specifications, generation ordering, and
referenced file presence before reclamation. A missing `spec.json` in a directory
containing generation files is corruption; maintenance preserves those files.
Obsolete unpinned generations are
removed from `spec.json` atomically, then their files are deleted. Abandoned
`.pending-*` directories and uncommitted index/metadata files are reclaimed on
reopen or the next maintenance pass. Maintenance is idempotent and preserves
unknown files.

`VerifyCollection` checks retained generation metadata, corpus and space identity,
native key mappings, and recorded index sizes. It reads JSON metadata and file
stats; native index dimensions and row counts are checked when opening the index.
`VectorStatus` and `PendingVectors` use document and generation revisions without loading
vector payloads. Verification holds the coordinator while reading selected
metadata, blocking publication and new reader creation; existing readers can
keep querying.

An automatic maintenance error leaves an already published vector generation
committed. `LastMaintenanceError` exposes it; an explicit `Maintain` returns the
error with partial progress.

Windows atomic renames retry transient sharing/access conflicts for at most
127 ms before returning the underlying error with both paths. This accommodates
briefly open handles and does not delete the target before replacement.

## Reproducible Benchmark

The simplification and 10,000-row before/after results are recorded in
[the 2026-10-17 report](collection-simplification-20261017.md). It compares the
previous two-generation retention policy with current-only retention plus reader pins.

```bash
BLUGE_USEARCH_LIBRARY_PATH=/absolute/path/to/vector_engine.dll \
  bash docs/run-collection-benchmark.sh tmp/storage-experiment
```

The opt-in `TestCollectionStorageBenchmark` defaults to 100,000 synthetic documents,
768 float32 dimensions, cosine, HNSW 16/128/64, batches of 10,000, 20 rounds updating
1% of documents chosen randomly with a fixed seed (unique within each round),
and 64 independent queries per model. It runs Embedded USearch,
sidecar USearch, and Collection with one and two models. Each case has one shared
text index. Background merging is explicitly disabled in all cases; actual segment
and deletion counts are recorded before and after updates. This isolates storage
and update behavior from nondeterministic merge scheduling, and does not represent
a fully merged production index.

Every case produces `report.json`, query worker configuration/results, and index
files. Query workers are fresh processes; OS page caches are not evicted. Open,
first query per model, 192 resident timings per model, p50/p95, and per-query recall
against the existing Flat exact backend are separate. `process_ms` is the entire
worker lifetime including Flat validation, **not** a cold-start latency.
Windows query timings use `QueryPerformanceCounter`, because the Go clock can
round short queries to zero on this platform. Other platforms use Go's monotonic
clock. Each query result records its clock explicitly.
After timing, every current global-index key and float32 vector is also checked
against the generated corpus. This prevents incorrect key mappings from being
misinterpreted as ordinary ANN recall loss.

Sizes are logical bytes. In initial/final summaries, embedded native payload and encoded mapping/envelope
bytes are subtracted from the shared segment container; its residual includes
text, shared segment headers, and snapshots. Collection current/history native
payloads, current/history JSON mappings, control files, and uncommitted files are
separate. Disk peaks are sampled every 5 ms, include temporary files, and can miss
short-lived peaks. Resident vector memory and filesystem allocation granularity
are outside these disk measurements. An additional Collection pin probe performs
three vector-only publications while an independent lazy reader holds the earlier
generation, then measures reclamation after release.

Per-batch Embedded disk samples use `unsplit_embedded_container_bytes`; decoding
all segment payloads during every update would disturb subsequent measurements.
The initial/final summaries split this container using the actual vector payloads.
With merging disabled, embedded native payloads include deleted rows in still-live
segments; these are not counted as separate generations. The query report records
their deleted-row count so this update overhead is not mistaken for format overhead.

Use `-collection-bench-docs`, `-collection-bench-dims`, `-collection-bench-batch`,
`-collection-bench-rounds`, `-collection-bench-queries`, and
`-collection-bench-cases=collection` after the output path for smaller smoke runs.
The dataset is synthetic, not model-generated or real business data. It fixes
input vectors, queries and write order; native HNSW graph internals and randomized
Collection revision tokens are not promised bit-identical across runs.

Collection temporary measurements also include final generation filenames that
are not yet catalog-committed. On Windows the sampler opens catalogs with delete
sharing so observation does not prevent the atomic replacement it is measuring.
Directory sampling also uses delete sharing. Sampling still has observation
overhead, and Windows name replacement may require the bounded sharing retry.

Archive and validate a completed six-case run without copying its large indices:

```bash
node docs/summarize-collection-benchmark.mjs tmp/storage-experiment \
  docs/benchmarks/storage-experiment
```

The archive includes raw reports, individual query timings and recalls, execution
logs, derived totals, and final generation lists. The derived retain-all total
sums the measured current payload and mapping sizes of every build/update
generation; it is not a separate no-GC timing experiment.
Additional completed run roots can be supplied after the archive path; later
roots replace earlier cases by name, and all source logs and provenance are kept.
The validator refuses partial cases or inconsistent workload/library settings.

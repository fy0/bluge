# Separate Vector Storage Experiment

This branch adds an experimental `Collection` API for one BM25 corpus with
independent vector spaces. A vector space is identified by a stable ID such as
`e5-base-v1` and has immutable dimension, metric, and field metadata. Model,
model revision, and preprocessing descriptions are optional. Model migration,
traffic switching, readiness decisions, and rollback belong to the application.

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

The text index is stored under `text/`. Each vector space is stored below
`vectors/space-<id>/`. A committed generation contains:

* `index-<generation>.usearch` or `index-<generation>.flat`, containing the
  native HNSW or exact vector data;
* `metadata-<generation>.json`, containing the space specification and the
  `chunk_id -> document revision` map;
* `commit-<generation>.json`, containing SHA-256 checksums for the two files.

Files are written to a temporary directory and published by rename. The native
file, JSON mapping and commit certificate are written first; atomically replacing
the checksummed generation catalog in `spec.json` is the publication point.
Readers select the newest generation in that catalog. By default, text updates
create a fresh revision, so an older embedding is excluded until the caller
writes a vector with the new revision. Alternatively, pass one nonempty embedding
input version per document: `collection.UpsertDocuments(documents, versions...)`.
Keeping that version preserves vectors when only unrelated metadata changes;
the caller must change it when embedding inputs change. The library does not
infer embedding inputs from document fields. `PendingVectors` reports missing or
stale embeddings; model invocation and retry queues remain outside Bluge.

`collection.Reader("model-a-v1")` and
`bluge.OpenCollectionReader(config, "model-a-v1")` load catalogs and pin generations
only for the requested names. Omit names to select all registered spaces.
When every stored vector matches the text snapshot, searches use the native
unfiltered path unless a business filter is supplied. Revision exclusions still
intersect with business filters, including prepared filters.

Set `CollectionConfig.VectorStorage` to `"flat"` for a cgo-free exact-search
baseline. The default `"usearch"` mode requires the existing USearch native
library through `LibraryPath` or `BLUGE_USEARCH_LIBRARY_PATH`.

This branch is unreleased. Collection accepts only its current format; earlier
experimental formats must be rebuilt. The existing `Writer`, `Reader`, sidecar,
and embedded-vector APIs remain available.

Rebuild the native adapter from this branch before running update workloads.
USearch 2.26.0 compaction permutes graph slots but leaves stale key lookups and
free-slot positions in the live handle. The adapter now serializes and reloads
after compaction before subsequent mutations. Without this fix, the Linux
1,000-row smoke workload reopened with 990 native rows after ten replacements,
and key-to-vector identity can be wrong even when the count looks correct.
The ABI and float32 format are unchanged. The round trip consumes a temporary
in-memory native serialization and is included in update timing. Existing corrupt
generations are reported, not repaired or silently rolled back.

## Generation Maintenance

Publication attempts maintenance of that space; opening a writer maintains all
spaces. Only the current generation and older generations held by readers are
retained. Both `Collection.Reader` and `OpenCollectionReader` acquire shared
kernel file locks for **all** selected spaces before returning, even when vectors
will only be loaded much later. `Close` releases them after closing native handles;
process termination also releases them. No PID timeout or in-process reference
count determines whether a generation is safe to delete.

`generations.lock` is a permanent coordinator inode. Discovery, reader lease
acquisition, commit publication, and retirement use this lock. Maintenance also
requires the existing collection writer lock, so it cannot delete an active
writer's staging data. Readers must be able to create/open lease files for writing.
All participating processes must use this protocol; old experimental binaries
that do not acquire leases must be stopped before enabling maintenance. Use local
filesystems with working kernel locks and atomic rename (tested on NTFS and ext4).

```go
result, err := collection.Maintain(ctx)
// Restrict cleanup to a named model:
result, err = collection.Maintain(ctx, "model-a-v1")
// When no writer is open:
result, err = bluge.MaintainCollection(ctx, config)
// UpsertVectors returning nil means committed. Cleanup errors are separate:
maintenanceErr := collection.LastMaintenanceError()
// Explicit payload checksum and metadata verification (all spaces by default):
err = bluge.VerifyCollection(ctx, config, "model-a-v1")
```

Maintenance first validates selected catalogs, commit identities, and payload
file presence. A malformed catalog or commit, or a missing referenced file,
causes an error before reclamation starts. It does not select an older commit
to conceal corruption. Full payload hashes and vector metadata are checked by
`VerifyCollection` and when loading a generation for reading or writing; native
structural checks happen when opening the vector index. Maintenance alone is
not an integrity audit and can reclaim obsolete payloads without hashing them.
Once safe, obsolete generations are durably removed from the catalog, then their
commit certificates are renamed to `.retired-commit-...json` before deleting their
payloads. Interrupted retirement, abandoned
`.pending-*` directories, and uncommitted index/metadata files are reclaimed on
reopen or the next maintenance pass. Maintenance is idempotent. Unknown files are
preserved. The writer lock excludes active staging writes during this sweep.

An automatic maintenance error never changes an already successful vector commit
into a failed batch. `LastMaintenanceError` exposes it; an explicit `Maintain`
returns it together with partial progress. Maintenance reads small catalogs and
commit records rather than all vector payloads. Explicit `VerifyCollection`
still costs O(total selected retained bytes) and holds the coordinator during
verification, blocking publication and new reader creation for that pass.
Existing readers can keep querying.

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

The manifest, space specification, commit certificates, and vector metadata use
one current format version. A checksummed catalog is required; there is no
filename-based fallback or automatic format migration. Catalog checksums detect
accidental changes to model specs or generation lists. A catalog-referenced
commit file that disappears is corruption, not an invitation to choose an older
generation. A certificate not yet listed in the catalog is uncommitted and can
be reclaimed, even if all three of its files were completely written.

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
logs, source/library hashes, derived totals, and verified final generation lists.
The derived retain-all total sums the measured current payload and mapping sizes
of every build/update generation; it is not a separate no-GC timing experiment.
Additional completed run roots can be supplied after the archive path; later
roots replace earlier cases by name, and all source logs and provenance are kept.
The validator refuses partial cases or inconsistent workload/library settings.

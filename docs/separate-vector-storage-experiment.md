# Separate Vector Storage Experiment

This branch adds an experimental `Collection` API for one BM25 corpus with
independent vector spaces. A vector space is identified by a stable ID such as
`e5-base-v1` and has immutable model, dimension, metric, and field metadata.

```go
config := bluge.DefaultCollectionConfig("./kb")
collection, _ := bluge.OpenCollection(config)
defer collection.Close()

collection.CreateVectorSpace(bluge.VectorSpaceSpec{
    ID: "model-a-v1", Field: "embedding", Model: "model-a",
    ModelRevision: "1", Dims: 768, Similarity: bluge.VectorCosine,
})
revisions, _ := collection.UpsertDocuments(documents)
collection.UpsertVectors("model-a-v1", []bluge.VectorRecord{{
    ID: revisions[0].ID, Revision: revisions[0].Revision, Vector: embedding,
}})
reader, _ := collection.Reader()
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

Files are written to a temporary directory and published by rename. Readers
only load the generation named by a commit file. Text updates create a fresh
revision, so an older embedding is excluded until the caller writes a vector
with the new revision. `PendingVectors` reports missing or stale embeddings;
the model invocation and retry queue remain outside Bluge.

Set `CollectionConfig.VectorStorage` to `"flat"` for a cgo-free exact-search
baseline. The default `"usearch"` mode requires the existing USearch native
library through `LibraryPath` or `BLUGE_USEARCH_LIBRARY_PATH`.

This is an experiment rather than a compatibility migration. Existing
`Writer`, `Reader`, sidecar, and embedded-vector APIs remain unchanged.

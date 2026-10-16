// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"context"
	"errors"
	"os"
	"testing"
)

func collectionTestDoc(id, text string) *Document {
	return NewDocument(id).AddField(NewTextField("body", text))
}

func TestCollectionIndependentVectorSpacesAndRevisions(t *testing.T) {
	root := t.TempDir()
	config := DefaultCollectionConfig(root)
	config.VectorStorage = "flat"
	collection, err := OpenCollection(config)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()

	a := VectorSpaceSpec{ID: "model-a-v1", Field: "embedding", Model: "model-a", ModelRevision: "1", Dims: 2, Similarity: VectorCosine}
	b := VectorSpaceSpec{ID: "model-b-v1", Field: "embedding", Model: "model-b", ModelRevision: "1", Dims: 2, Similarity: VectorCosine}
	if err := collection.CreateVectorSpace(a); err != nil {
		t.Fatal(err)
	}
	if err := collection.CreateVectorSpace(b); err != nil {
		t.Fatal(err)
	}
	revisions, err := collection.UpsertDocuments([]*Document{
		collectionTestDoc("red", "red apple"), collectionTestDoc("blue", "blue ocean"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 {
		t.Fatalf("expected two revisions, got %d", len(revisions))
	}

	pending, err := collection.Reader()
	if err != nil {
		t.Fatal(err)
	}
	page, err := pending.PendingVectors(context.Background(), a.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("expected two pending vectors, got %d", len(page))
	}
	_ = pending.Close()

	if err := collection.UpsertVectors(a.ID, []VectorRecord{
		{ID: revisions[0].ID, Revision: revisions[0].Revision, Vector: []float32{1, 0}},
		{ID: revisions[1].ID, Revision: revisions[1].Revision, Vector: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := collection.UpsertVectors(b.ID, []VectorRecord{
		{ID: revisions[0].ID, Revision: revisions[0].Revision, Vector: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}

	reader, err := collection.Reader()
	if err != nil {
		t.Fatal(err)
	}
	statusA, err := reader.VectorStatus(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statusA.ValidVectors != 2 || statusA.PendingVectors != 0 {
		t.Fatalf("unexpected A status: %+v", statusA)
	}
	statusB, err := reader.VectorStatus(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statusB.ValidVectors != 1 || statusB.PendingVectors != 1 {
		t.Fatalf("unexpected B status: %+v", statusB)
	}

	hits, err := reader.SearchVectorRequest(context.Background(), a.ID, NewVectorSearchRequest(a.Field, []float32{1, 0}).SetK(2))
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "red", "blue")
	bHits, err := reader.SearchVectorRequest(context.Background(), b.ID, NewVectorSearchRequest(b.Field, []float32{0, 1}).SetK(2))
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, bHits, "red")

	updated, err := collection.UpsertDocuments([]*Document{collectionTestDoc("red", "green forest")})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.UpsertVectors(a.ID, []VectorRecord{{ID: "red", Revision: revisions[0].Revision, Vector: []float32{1, 0}}}); !errors.Is(err, ErrVectorRevisionConflict) {
		t.Fatalf("expected stale revision conflict, got %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err = collection.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	hits, err = reader.SearchVectorRequest(context.Background(), a.ID, NewVectorSearchRequest(a.Field, []float32{1, 0}).SetK(2))
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "blue")
	statusA, err = reader.VectorStatus(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statusA.PendingVectors != 1 || statusA.StaleVectors != 1 {
		t.Fatalf("unexpected stale status: %+v", statusA)
	}
	if err := collection.UpsertVectors(a.ID, []VectorRecord{{ID: updated[0].ID, Revision: updated[0].Revision, Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}

	if err := collection.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCollection(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedReader, err := reopened.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedReader.Close()
	hits, err = reopenedReader.SearchVectorRequest(context.Background(), a.ID, NewVectorSearchRequest(a.Field, []float32{1, 0}).SetK(2))
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "red", "blue")
}

func TestCollectionHybridSearchUsesSelectedSpace(t *testing.T) {
	root := t.TempDir()
	config := DefaultCollectionConfig(root)
	config.VectorStorage = "flat"
	collection, err := OpenCollection(config)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	spec := VectorSpaceSpec{ID: "semantic-v1", Field: "embedding", Model: "semantic", ModelRevision: "1", Dims: 2, Similarity: VectorCosine}
	if err := collection.CreateVectorSpace(spec); err != nil {
		t.Fatal(err)
	}
	revs, err := collection.UpsertDocuments([]*Document{collectionTestDoc("red", "apple fruit"), collectionTestDoc("blue", "ocean water")})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.UpsertVectors(spec.ID, []VectorRecord{{ID: revs[0].ID, Revision: revs[0].Revision, Vector: []float32{1, 0}}, {ID: revs[1].ID, Revision: revs[1].Revision, Vector: []float32{0, 1}}}); err != nil {
		t.Fatal(err)
	}
	r, err := collection.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	textQuery := NewMatchQuery("apple").SetField("body")
	hits, err := r.HybridSearch(context.Background(), spec.ID, NewHybridSearchRequest(textQuery, NewVectorSearchRequest(spec.Field, []float32{0, 1})).SetK(2).SetFusion(HybridFusionRRF))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != "red" || hits[0].VectorRank == 0 || hits[1].VectorRank == 0 {
		t.Fatalf("unexpected hybrid results: %+v", hits)
	}
}

func TestCollectionUSearchStorage(t *testing.T) {
	library := os.Getenv("BLUGE_USEARCH_LIBRARY_PATH")
	if library == "" {
		t.Skip("set BLUGE_USEARCH_LIBRARY_PATH to run collection USearch test")
	}
	root := t.TempDir()
	config := DefaultCollectionConfig(root)
	config.LibraryPath = library
	collection, err := OpenCollection(config)
	if err != nil {
		t.Fatal(err)
	}
	spec := VectorSpaceSpec{ID: "native-v1", Field: "embedding", Model: "native", ModelRevision: "1", Dims: 2, Similarity: VectorCosine}
	if err := collection.CreateVectorSpace(spec); err != nil {
		t.Fatal(err)
	}
	revs, err := collection.UpsertDocuments([]*Document{collectionTestDoc("red", "apple"), collectionTestDoc("blue", "water")})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.UpsertVectors(spec.ID, []VectorRecord{{ID: revs[0].ID, Revision: revs[0].Revision, Vector: []float32{1, 0}}, {ID: revs[1].ID, Revision: revs[1].Revision, Vector: []float32{0, 1}}}); err != nil {
		t.Fatal(err)
	}
	r, err := collection.Reader()
	if err != nil {
		t.Fatal(err)
	}
	hits, err := r.SearchVectorRequest(context.Background(), spec.ID, NewVectorSearchRequest(spec.Field, []float32{1, 0}).SetK(2))
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "red", "blue")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := collection.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCollection(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r, err = reopened.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	hits, err = r.SearchVectorRequest(context.Background(), spec.ID, NewVectorSearchRequest(spec.Field, []float32{0, 1}).SetK(1))
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "blue")
}

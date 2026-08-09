// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestDefaultVectorBackendUnsupported(t *testing.T) {
	config := InMemoryOnlyConfig()
	if config.VectorBackend == nil {
		t.Fatal("expected default vector backend")
	}
	if config.VectorBackend.Name() != "unsupported" {
		t.Fatalf("expected unsupported backend, got %s", config.VectorBackend.Name())
	}
	_, err := config.VectorBackend.Open(config)
	if !errors.Is(err, ErrVectorUnsupported) {
		t.Fatalf("expected ErrVectorUnsupported, got %v", err)
	}
}

func TestVectorFieldRequiresConfiguredBackend(t *testing.T) {
	writer, err := OpenWriter(InMemoryOnlyConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	err = writer.Insert(NewDocument("doc").AddField(
		NewVectorField("embedding", []float32{1, 0})))
	if !errors.Is(err, ErrVectorUnsupported) {
		t.Fatalf("expected ErrVectorUnsupported, got %v", err)
	}
}

func TestFlatVectorValidationHappensBeforeTextBatch(t *testing.T) {
	config := InMemoryOnlyConfig().WithVectorBackend(NewFlatVectorBackend(""))
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Insert(NewDocument("valid").AddField(
		NewVectorField("embedding", []float32{1, 0}))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(NewDocument("invalid").AddField(
		NewVectorField("embedding", []float32{1, 0, 0}))); !errors.Is(err, ErrVectorInvalidDimension) {
		t.Fatalf("expected ErrVectorInvalidDimension, got %v", err)
	}

	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	count, err := reader.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("invalid vector batch changed text count to %d", count)
	}
}

func TestFlatVectorSearchPersistsUpdatesAndFilters(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index")
	vectorPath := filepath.Join(t.TempDir(), "vectors.sidecar")
	config := DefaultConfig(indexPath).WithVectorBackend(NewFlatVectorBackend(vectorPath))

	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(NewDocument("red").
		AddField(NewTextField("color", "red")).
		AddField(NewVectorField("embedding", []float32{1, 0}))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(NewDocument("blue").
		AddField(NewTextField("color", "blue")).
		AddField(NewVectorField("embedding", []float32{0, 1}))); err != nil {
		t.Fatal(err)
	}

	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	hits, err := reader.VectorSearch(context.Background(), "embedding", []float32{0.9, 0.1}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "red", "blue")

	filtered, err := reader.VectorSearch(context.Background(), "embedding", []float32{0.9, 0.1}, 2,
		NewTermQuery("blue").SetField("color"))
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, filtered, "blue")
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	if err := writer.Update(Identifier("red"), NewDocument("red").
		AddField(NewTextField("color", "red")).
		AddField(NewVectorField("embedding", []float32{0, 1}))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Delete(Identifier("blue")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err = OpenReader(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	hits, err = reader.VectorSearch(context.Background(), "embedding", []float32{0, 1}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "red")
}

func TestVectorFieldDoesNotEnterTextSegment(t *testing.T) {
	config := InMemoryOnlyConfig().WithVectorBackend(NewFlatVectorBackend(""))
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Insert(NewDocument("doc").
		AddField(NewVectorField("embedding", []float32{1, 2}))); err != nil {
		t.Fatal(err)
	}
	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	fields, err := reader.Fields()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if field == "embedding" {
			t.Fatalf("vector field leaked into text segment fields: %v", fields)
		}
	}
}

func TestOfflineVectorChangesCommitAfterSegmentBuild(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index")
	vectorPath := filepath.Join(t.TempDir(), "vectors.sidecar")
	config := DefaultConfig(indexPath).WithVectorBackend(NewFlatVectorBackend(vectorPath))
	offline, err := OpenOfflineWriter(config, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.Insert(NewDocument("offline").AddField(
		NewVectorField("embedding", []float32{0, 1}))); err != nil {
		t.Fatal(err)
	}
	if err := offline.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReader(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	hits, err := reader.VectorSearch(context.Background(), "embedding", []float32{0, 1}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "offline")
}

func assertVectorIDs(t *testing.T, hits []VectorHit, ids ...Identifier) {
	t.Helper()
	if len(hits) != len(ids) {
		t.Fatalf("got %d vector hits, expected %d: %v", len(hits), len(ids), hits)
	}
	for i, id := range ids {
		if hits[i].ID != id {
			t.Fatalf("hit %d has ID %q, expected %q", i, hits[i].ID, id)
		}
	}
}

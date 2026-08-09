//go:build windows

package bluge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUSearchVectorBackendWindows(t *testing.T) {
	libraryPath := os.Getenv(usearchLibraryPathEnv)
	if libraryPath == "" {
		t.Skip("set BLUGE_USEARCH_LIBRARY_PATH to run the native USearch integration test")
	}
	if _, err := os.Stat(libraryPath); err != nil {
		t.Fatalf("USearch native library is unavailable: %v", err)
	}

	root := t.TempDir()
	backend := NewUSearchVectorBackendWithLibrary(filepath.Join(root, "vectors"), libraryPath)
	config := DefaultConfig(filepath.Join(root, "index")).WithVectorBackend(backend)
	hardware, err := backend.HardwareAcceleration()
	if err != nil {
		t.Fatal(err)
	}
	if hardware.Compiled == "" || hardware.Available == "" {
		t.Fatalf("native hardware probe returned empty values: %+v", hardware)
	}

	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(NewDocument("red").
		AddField(NewTextField("color", "red")).
		AddField(NewVectorField("embedding", []float32{1, 0})).
		AddField(NewVectorFieldWithSimilarity("dot", []float32{1, 0}, VectorDot)).
		AddField(NewVectorFieldWithSimilarity("l2", []float32{1, 0}, VectorL2))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(NewDocument("blue").
		AddField(NewTextField("color", "blue")).
		AddField(NewVectorField("embedding", []float32{0, 1})).
		AddField(NewVectorFieldWithSimilarity("dot", []float32{0, 1}, VectorDot)).
		AddField(NewVectorFieldWithSimilarity("l2", []float32{0, 1}, VectorL2))); err != nil {
		t.Fatal(err)
	}

	reader, err := writer.Reader()
	if err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	hits, err := reader.VectorSearch(context.Background(), "embedding", []float32{0.9, 0.1}, 2, nil)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "red", "blue")
	dotHits, err := reader.VectorSearch(context.Background(), "dot", []float32{1, 0}, 2, nil)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	assertVectorIDs(t, dotHits, "red", "blue")
	if dotHits[0].Score < 0.99 {
		t.Fatalf("dot score was not converted from USearch distance: %v", dotHits)
	}
	l2Hits, err := reader.VectorSearch(context.Background(), "l2", []float32{1, 0}, 2, nil)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	assertVectorIDs(t, l2Hits, "red", "blue")
	if l2Hits[0].Score > 0.001 {
		t.Fatalf("L2 score was not converted from USearch distance: %v", l2Hits)
	}
	filtered, err := reader.VectorSearch(context.Background(), "embedding", []float32{0.9, 0.1}, 2,
		NewTermQuery("blue").SetField("color"))
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	assertVectorIDs(t, filtered, "blue")
	if err := reader.Close(); err != nil {
		_ = writer.Close()
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

func TestEmbeddedUSearchVectorBackendWindows(t *testing.T) {
	libraryPath := os.Getenv(usearchLibraryPathEnv)
	if libraryPath == "" {
		t.Skip("set BLUGE_USEARCH_LIBRARY_PATH to run the native USearch integration test")
	}
	if _, err := os.Stat(libraryPath); err != nil {
		t.Fatalf("USearch native library is unavailable: %v", err)
	}

	root := t.TempDir()
	indexPath := filepath.Join(root, "index")
	backend := NewEmbeddedUSearchVectorBackend().WithLibraryPath(libraryPath)
	config := DefaultConfig(indexPath).WithVectorBackend(backend)

	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(NewDocument("red").
		AddField(NewTextField("color", "red apple")).
		AddField(NewKeywordField("kind", "fruit")).
		AddField(NewVectorField("embedding", []float32{1, 0}))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(NewDocument("blue").
		AddField(NewTextField("color", "blue car")).
		AddField(NewKeywordField("kind", "vehicle")).
		AddField(NewVectorField("embedding", []float32{0, 1}))); err != nil {
		t.Fatal(err)
	}

	reader, err := writer.Reader()
	if err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	hits, err := reader.VectorSearch(context.Background(), "embedding", []float32{0.9, 0.1}, 2, nil)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "red", "blue")
	filtered, err := reader.VectorSearch(context.Background(), "embedding", []float32{0.9, 0.1}, 2,
		NewTermQuery("vehicle").SetField("kind"))
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	assertVectorIDs(t, filtered, "blue")
	hybrid, err := reader.HybridSearch(context.Background(), NewHybridSearchRequest(
		NewMatchQuery("apple").SetField("color"),
		NewVectorSearchRequest("embedding", []float32{0, 1}),
	).SetK(2).SetFusion(HybridFusionRRF))
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatal(err)
	}
	if len(hybrid) != 2 || hybrid[0].ID != "red" {
		t.Fatalf("unexpected embedded hybrid results: %v", hybrid)
	}
	if err := reader.Close(); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}

	if err := writer.Update(Identifier("red"), NewDocument("red").
		AddField(NewTextField("color", "red apple updated")).
		AddField(NewKeywordField("kind", "fruit")).
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
	hits, err = reader.VectorSearch(context.Background(), "embedding", []float32{0, 1}, 2, nil)
	if err != nil {
		_ = reader.Close()
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "red")
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	var sidecars []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".usearch") {
			sidecars = append(sidecars, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sidecars) != 0 {
		t.Fatalf("embedded backend created sidecar files: %v", sidecars)
	}

	// Text segments remain readable when the optional native artifact is absent.
	t.Setenv(usearchLibraryPathEnv, "")
	degradedConfig := DefaultConfig(indexPath).WithVectorBackend(
		backend.WithLibraryPath(filepath.Join(root, "missing-usearch.dll")))
	degradedReader, err := OpenReader(degradedConfig)
	if err != nil {
		t.Fatalf("text reader did not degrade without native DLL: %v", err)
	}
	if _, err := degradedReader.Search(context.Background(),
		NewTopNSearch(1, NewMatchQuery("updated").SetField("color"))); err != nil {
		_ = degradedReader.Close()
		t.Fatalf("text search failed in degraded mode: %v", err)
	}
	if _, err := degradedReader.VectorSearch(context.Background(), "embedding", []float32{0, 1}, 1, nil); !errors.Is(err, ErrVectorUnsupported) {
		_ = degradedReader.Close()
		t.Fatalf("expected vector search to be unavailable without DLL, got %v", err)
	}
	if err := degradedReader.Close(); err != nil {
		t.Fatal(err)
	}

	offlineRoot := filepath.Join(root, "offline")
	offlineConfig := DefaultConfig(offlineRoot).WithVectorBackend(
		NewEmbeddedUSearchVectorBackend().WithLibraryPath(libraryPath))
	offline, err := OpenOfflineWriter(offlineConfig, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []*Document{
		NewDocument("offline-red").AddField(NewVectorField("embedding", []float32{1, 0})),
		NewDocument("offline-blue").AddField(NewVectorField("embedding", []float32{0, 1})),
		NewDocument("offline-green").AddField(NewVectorField("embedding", []float32{0.8, 0.2})),
	} {
		if err := offline.Insert(doc); err != nil {
			_ = offline.Close()
			t.Fatal(err)
		}
	}
	if err := offline.Close(); err != nil {
		t.Fatal(err)
	}
	offlineReader, err := OpenReader(offlineConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer offlineReader.Close()
	hits, err = offlineReader.VectorSearch(context.Background(), "embedding", []float32{1, 0}, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, hits, "offline-red", "offline-green", "offline-blue")
}

package bluge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCollectionNativeMetadataTracksLiveVectors(t *testing.T) {
	if os.Getenv("BLUGE_USEARCH_LIBRARY_PATH") == "" {
		t.Skip("native library not configured")
	}
	c, specs, _ := gcCollection(t, "usearch")
	spec := specs[0]
	old, err := c.Reader(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	previous := Identifier("a")
	// Churn document IDs, then reinsert the first ID after its key was reclaimed.
	for _, id := range []Identifier{"new-1", "new-2", "new-3", "a"} {
		if err := c.DeleteDocuments(previous); err != nil {
			t.Fatal(err)
		}
		revisions, err := c.UpsertDocuments([]*Document{collectionTestDoc(string(id), "apple")})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.UpsertVectors(spec.ID, []VectorRecord{{ID: id, Revision: revisions[0].Revision, Vector: []float32{1, 0}}}); err != nil {
			t.Fatal(err)
		}
		metadata, err := loadCollectionVectorMetadata(c.config, c.spaces[spec.ID])
		if err != nil {
			t.Fatal(err)
		}
		if len(metadata.Native.IDs) != len(metadata.Revisions) {
			t.Fatalf("native mapping retains deleted IDs: %d keys for %d vectors", len(metadata.Native.IDs), len(metadata.Revisions))
		}
		if metadata.Native.IDs[string(previous)] != 0 {
			t.Fatalf("deleted ID %s still has a native key", previous)
		}
		reader, err := OpenCollectionReader(c.config, spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		hits, searchErr := reader.SearchVectorRequest(context.Background(), spec.ID,
			NewVectorSearchRequest(spec.Field, []float32{1, 0}).SetK(2))
		closeErr := reader.Close()
		if searchErr != nil || closeErr != nil {
			t.Fatalf("search: %v; close: %v", searchErr, closeErr)
		}
		assertVectorIDs(t, hits, id, "b")
		previous = id
	}
	gcQuery(t, old, specs[:1]) // Lazy loading still uses the old generation's keys.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCollection(c.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reader, err := reopened.Reader(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	gcQuery(t, reader, specs[:1])
}

func TestCollectionNativeUpdateKeyIdentity(t *testing.T) {
	if os.Getenv("BLUGE_USEARCH_LIBRARY_PATH") == "" {
		t.Skip("native library not configured")
	}
	cfg := DefaultCollectionConfig(filepath.Join(t.TempDir(), "native updates"))
	c, err := OpenCollection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	spec := VectorSpaceSpec{ID: "model", Field: "embedding", Model: "synthetic", ModelRevision: "1", Dims: 32, Similarity: VectorCosine}
	if err := c.CreateVectorSpace(spec); err != nil {
		t.Fatal(err)
	}
	expected := make(map[Identifier][]float32)
	for round := 0; round <= 4; round++ {
		count := 10
		if round == 0 {
			count = 1000
		}
		docs := make([]*Document, count)
		for i := range docs {
			key := i
			if round != 0 {
				key = (i*97 + round*13) % 1000
			}
			docs[i] = collectionTestDoc(fmt.Sprintf("doc-%04d", key), "text")
		}
		revisions, err := c.UpsertDocuments(docs)
		if err != nil {
			t.Fatal(err)
		}
		records := make([]VectorRecord, count)
		for i, revision := range revisions {
			vector := make([]float32, spec.Dims)
			for d := range vector {
				vector[d] = float32((i*1543 + d*97 + round*13) % 997)
			}
			expected[revision.ID] = vector
			records[i] = VectorRecord{ID: revision.ID, Revision: revision.Revision, Vector: vector}
		}
		// Exercise both replacement-only batches and real removals that compact
		// the graph before replacing other keys in the same batch.
		if round > 0 && round%2 == 0 {
			deleted := Identifier(fmt.Sprintf("doc-%04d", 999-round))
			if err := c.DeleteDocuments(deleted); err != nil {
				t.Fatal(err)
			}
			delete(expected, deleted)
		}
		if err := c.UpsertVectors(spec.ID, records); err != nil {
			t.Fatal(err)
		}
		space := c.spaces[spec.ID]
		metadata, err := loadCollectionVectorMetadata(c.config, space)
		if err != nil {
			t.Fatal(err)
		}
		vectors, err := openCollectionVectors(c.config, space, metadata, "")
		if err != nil {
			t.Fatal(err)
		}
		u := vectors.(*usearchVectorIndex)
		actual := make([]float32, spec.Dims)
		for id, want := range expected {
			key := u.manifest.IDs[string(id)]
			if u.api.get(u.indices[spec.Field], key, actual) != 0 || !reflect.DeepEqual(actual, want) {
				_ = vectors.Close()
				t.Fatalf("round %d: vector key identity mismatch for %s", round, id)
			}
		}
		if err := vectors.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

package bluge

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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

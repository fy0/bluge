package bluge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectionSelectedSpaces(t *testing.T) {
	for _, independent := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer", true: "independent"}[independent], func(t *testing.T) {
			c, specs, revs := gcCollection(t, "flat")
			open := c.Reader
			if independent {
				open = func(ids ...string) (*CollectionReader, error) { return OpenCollectionReader(c.config, ids...) }
			}
			for _, id := range []string{"missing", "../model-a"} {
				if r, err := open(id); !errors.Is(err, ErrVectorSpaceNotFound) {
					if r != nil {
						_ = r.Close()
					}
					t.Fatalf("missing space %q: %v", id, err)
				}
			}
			r, err := open(specs[0].ID, specs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			spaces, err := r.VectorSpaces()
			if err != nil || len(spaces) != 1 || spaces[0] != specs[0] {
				t.Fatalf("selected spaces: %v, %v", spaces, err)
			}
			for i := 0; i < 3; i++ {
				gcPublishValues(t, c, specs, revs, []float32{0, 1}, []float32{1, 0})
			}
			gcGenerations(t, c, specs[:1], 1, 4)
			gcGenerations(t, c, specs[1:], 4)
			gcQuery(t, r, specs[:1]) // The lazy reader still sees generation 1.
			if _, err := r.VectorStatus(context.Background(), specs[1].ID); !errors.Is(err, ErrVectorSpaceNotFound) {
				t.Fatalf("unselected space accessible: %v", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Maintain(context.Background(), specs[0].ID); err != nil {
				t.Fatal(err)
			}
			gcGenerations(t, c, specs, 4)
		})
	}
}

func TestCollectionTargetedOperationsIgnoreOtherSpace(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	bDir := collectionSpacePath(c.config.Path, specs[1].ID)
	if err := os.WriteFile(filepath.Join(bDir, "spec.json"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(bDir, ".pending-other-space")
	if err := os.WriteFile(abandoned, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Updating A must neither load B's catalog nor sweep B's abandoned files.
	gcPublish(t, c, specs[:1], revs)
	if err := c.LastMaintenanceError(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Maintain(context.Background(), specs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); err != nil {
		t.Fatal("targeted maintenance touched another space", err)
	}
	if err := VerifyCollection(context.Background(), c.config, specs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCollection(context.Background(), c.config); !errors.Is(err, ErrCollectionCorrupt) {
		t.Fatalf("full verification missed B: %v", err)
	}
	r, err := OpenCollectionReader(c.config, specs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	gcQuery(t, r, specs[:1])
	if all, err := OpenCollectionReader(c.config); !errors.Is(err, ErrCollectionCorrupt) {
		if all != nil {
			_ = all.Close()
		}
		t.Fatalf("all-space reader missed B: %v", err)
	}
}

func TestCollectionExplicitInputRevisions(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	ctx := context.Background()
	updated, err := c.UpsertDocuments([]*Document{
		collectionTestDoc("a", "apple").AddField(NewStoredOnlyField("label", []byte("new label"))),
	}, revs[0].Revision)
	if err != nil || len(updated) != 1 || updated[0] != revs[0] {
		t.Fatalf("metadata update: %v, %v", updated, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenCollection(c.config)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := c.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	gcQuery(t, r, specs)
	for _, spec := range specs {
		pending, err := r.PendingVectors(ctx, spec.ID, "", 10)
		if err != nil || len(pending) != 0 {
			t.Fatalf("metadata update invalidated %s: %v, %v", spec.ID, pending, err)
		}
	}
	docs := []*Document{collectionTestDoc("a", "changed"), collectionTestDoc("b", "changed")}
	for _, revisions := range [][]string{{"one"}, {"one", ""}, {"one", "two", "three"}} {
		if _, err := c.UpsertDocuments(docs, revisions...); err == nil {
			t.Fatal("accepted invalid input revisions", revisions)
		}
	}
	if c.revisions["a"] != revs[0].Revision || c.revisions["b"] != revs[1].Revision {
		t.Fatal("invalid batch changed revisions")
	}
	if _, err := c.UpsertDocuments(docs[:1], "embedding-input-v2"); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertVectors(specs[0].ID, []VectorRecord{{ID: "a", Revision: revs[0].Revision, Vector: []float32{1, 0}}}); !errors.Is(err, ErrVectorRevisionConflict) {
		t.Fatalf("stale write: %v", err)
	}
	current, err := c.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	for _, spec := range specs {
		hits, err := current.SearchVectorRequest(ctx, spec.ID, NewVectorSearchRequest("embedding", []float32{1, 0}).SetK(2))
		if err != nil {
			t.Fatal(err)
		}
		assertVectorIDs(t, hits, "b")
		pending, err := current.PendingVectors(ctx, spec.ID, "", 10)
		if err != nil || len(pending) != 1 || pending[0] != (DocumentRevision{ID: "a", Revision: "embedding-input-v2"}) {
			t.Fatalf("changed embedding input: %v, %v", pending, err)
		}
	}
	gcQuery(t, r, specs) // Explicit versions still preserve snapshot isolation.
}

func TestCollectionMinimalSpaceSpec(t *testing.T) {
	c, _, _ := gcCollection(t, "flat")
	spec := VectorSpaceSpec{ID: "minimal", Dims: 3}
	for i := 0; i < 2; i++ {
		if err := c.CreateVectorSpace(spec); err != nil {
			t.Fatal(err)
		}
	}
	r, err := OpenCollectionReader(c.config, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	spaces, err := r.VectorSpaces()
	spec.Field, spec.Similarity = "embedding", VectorCosine
	if err != nil || len(spaces) != 1 || spaces[0] != spec {
		t.Fatalf("normalized spec: %v, %v", spaces, err)
	}
	hits, err := r.SearchVectorRequest(context.Background(), spec.ID, NewVectorSearchRequest("embedding", []float32{1, 0, 0}))
	if err != nil || len(hits) != 0 {
		t.Fatalf("empty space: %v, %v", hits, err)
	}
}

func TestCollectionFiltersAndViewOwnership(t *testing.T) {
	for _, storage := range []string{"flat", "usearch"} {
		t.Run(storage, func(t *testing.T) {
			if storage == "usearch" && os.Getenv("BLUGE_USEARCH_LIBRARY_PATH") == "" {
				t.Skip("native library not configured")
			}
			c, specs, _ := gcCollection(t, storage)
			ctx := context.Background()
			for _, stale := range []bool{false, true} {
				if stale {
					if err := c.DeleteDocuments("a"); err != nil {
						t.Fatal(err)
					}
				}
				r, err := c.Reader(specs[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				for _, tc := range []struct {
					filter Query
					want   []Identifier
				}{
					{nil, []Identifier{"a", "b"}},
					{NewTermQuery("apple").SetField("body"), []Identifier{"a"}},
					{NewTermQuery("water").SetField("body"), []Identifier{"b"}},
					{NewTermQuery("absent").SetField("body"), nil},
				} {
					want := tc.want
					if stale && len(want) != 0 && want[0] == "a" {
						want = want[1:]
					}
					hits, err := r.SearchVectorRequest(ctx, specs[0].ID, NewVectorSearchRequest("embedding", []float32{1, 0}).SetK(2).SetFilter(tc.filter))
					if err != nil {
						t.Fatal(err)
					}
					assertVectorIDs(t, hits, want...)
					prepared, err := r.PrepareVectorFilter(ctx, specs[0].ID, tc.filter)
					if err != nil {
						t.Fatal(err)
					}
					hits, err = r.VectorSearchPrepared(ctx, specs[0].ID, "embedding", []float32{1, 0}, 2, prepared)
					if err != nil {
						t.Fatal(err)
					}
					assertVectorIDs(t, hits, want...)
					_ = prepared.Close()
				}
				view := r.views[specs[0].ID]
				if (view.vector.(*collectionVectorView).allowed != nil) != stale {
					t.Fatal("revision filter should only exist for stale vectors")
				}
				prepared, err := r.PrepareVectorFilter(ctx, specs[0].ID, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer prepared.Close()
				if !stale {
					if err := view.Close(); err != nil {
						t.Fatal(err)
					}
				}
				wantCount := uint64(2)
				if stale {
					wantCount = 1
				}
				if count, err := r.Count(); err != nil || count != wantCount {
					t.Fatalf("closing view closed text: %d, %v", count, err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := view.VectorSearchPrepared(ctx, "embedding", []float32{1, 0}, 2, prepared); !errors.Is(err, ErrVectorPreparedFilter) {
					t.Fatalf("prepared filter outlived reader: %v", err)
				}
			}
		})
	}
}

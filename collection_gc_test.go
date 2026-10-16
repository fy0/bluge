package bluge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fy0/bluge/index/lock"
)

func gcCollection(t *testing.T, storage string) (*Collection, []VectorSpaceSpec, []DocumentRevision) {
	t.Helper()
	cfg := DefaultCollectionConfig(filepath.Join(t.TempDir(), "collection with spaces"))
	cfg.VectorStorage = storage
	cfg.LibraryPath = os.Getenv("BLUGE_USEARCH_LIBRARY_PATH")
	c, err := OpenCollection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	var specs []VectorSpaceSpec
	for _, id := range []string{"model-a", "model-b"} {
		s := VectorSpaceSpec{ID: id, Field: "embedding", Model: id, ModelRevision: "1", Dims: 2, Similarity: VectorCosine}
		if err := c.CreateVectorSpace(s); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, s)
	}
	revs, err := c.UpsertDocuments([]*Document{collectionTestDoc("a", "apple"), collectionTestDoc("b", "water")})
	if err != nil {
		t.Fatal(err)
	}
	gcPublish(t, c, specs, revs)
	return c, specs, revs
}

func gcPublish(t *testing.T, c *Collection, specs []VectorSpaceSpec, revs []DocumentRevision) {
	t.Helper()
	gcPublishValues(t, c, specs, revs, []float32{1, 0}, []float32{0, 1})
}

func gcPublishValues(t *testing.T, c *Collection, specs []VectorSpaceSpec, revs []DocumentRevision, a, b []float32) {
	t.Helper()
	for _, s := range specs {
		if err := c.UpsertVectors(s.ID, []VectorRecord{{ID: revs[0].ID, Revision: revs[0].Revision, Vector: a}, {ID: revs[1].ID, Revision: revs[1].Revision, Vector: b}}); err != nil {
			t.Fatal(err)
		}
	}
}

func gcGenerations(t *testing.T, c *Collection, specs []VectorSpaceSpec, want ...uint64) {
	t.Helper()
	for _, s := range specs {
		dir := collectionSpacePath(c.config.Path, s.ID)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got []uint64
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "commit-") {
				n, _, err := collectionFileGeneration(e.Name())
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, n)
				space := c.spaces[s.ID]
				space.Generation = n
				if _, err := loadCollectionVectorMetadata(c.config, space); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s generations = %v, want %v", s.ID, got, want)
		}
	}
}

func gcQuery(t *testing.T, r *CollectionReader, specs []VectorSpaceSpec) {
	t.Helper()
	for _, s := range specs {
		hits, err := r.SearchVectorRequest(context.Background(), s.ID, NewVectorSearchRequest(s.Field, []float32{1, 0}).SetK(2))
		if err != nil {
			t.Fatal(err)
		}
		assertVectorIDs(t, hits, "a", "b")
	}
}

func TestCollectionGenerationPins(t *testing.T) {
	for _, storage := range []string{"flat", "usearch"} {
		t.Run(storage, func(t *testing.T) {
			if storage == "usearch" && os.Getenv("BLUGE_USEARCH_LIBRARY_PATH") == "" {
				t.Skip("native library not configured")
			}
			c, specs, revs := gcCollection(t, storage)
			local, err := c.Reader()
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			independent, err := OpenCollectionReader(c.config)
			if err != nil {
				t.Fatal(err)
			}
			defer independent.Close()
			gcQuery(t, local, specs[:1]) // The other model and independent reader stay lazy.
			for i := 0; i < 4; i++ {
				gcPublishValues(t, c, specs, revs, []float32{0, 1}, []float32{1, 0})
			}
			gcGenerations(t, c, specs, 1, 5)
			gcQuery(t, independent, specs)
			gcQuery(t, local, specs)
			latest, err := c.Reader()
			if err != nil {
				t.Fatal(err)
			}
			defer latest.Close()
			for _, spec := range specs {
				hits, err := latest.SearchVectorRequest(context.Background(), spec.ID, NewVectorSearchRequest(spec.Field, []float32{1, 0}).SetK(2))
				if err != nil {
					t.Fatal(err)
				}
				assertVectorIDs(t, hits, "b", "a")
			}
			if err := local.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Maintain(context.Background()); err != nil {
				t.Fatal(err)
			}
			gcGenerations(t, c, specs, 1, 5)
			if err := independent.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Maintain(context.Background()); err != nil {
				t.Fatal(err)
			}
			gcGenerations(t, c, specs, 5)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := MaintainCollection(context.Background(), c.config); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A subprocess is used so kernel lock release is tested after both Close and kill.
func TestCollectionLeaseProcess(t *testing.T) {
	root := os.Getenv("BLUGE_GC_CHILD_ROOT")
	if root == "" {
		return
	}
	cfg := DefaultCollectionConfig(root)
	cfg.VectorStorage = os.Getenv("BLUGE_GC_CHILD_STORAGE")
	r, err := OpenCollectionReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "query":
			specs, err := r.VectorSpaces()
			if err != nil {
				t.Fatal(err)
			}
			gcQuery(t, r, specs)
			fmt.Println("QUERIED")
		case "close":
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

func TestCollectionCrossProcessPins(t *testing.T) {
	for _, storage := range []string{"flat", "usearch"} {
		if storage == "usearch" && os.Getenv("BLUGE_USEARCH_LIBRARY_PATH") == "" {
			continue
		}
		for _, kill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/kill=%v", storage, kill), func(t *testing.T) {
				c, specs, revs := gcCollection(t, storage)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCollectionLeaseProcess$")
				cmd.Env = append(os.Environ(), "BLUGE_GC_CHILD_ROOT="+c.config.Path, "BLUGE_GC_CHILD_STORAGE="+storage)
				cmd.Stderr = os.Stderr
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				in, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer cmd.Process.Kill()
				scanner := bufio.NewScanner(out)
				if !scanner.Scan() || scanner.Text() != "READY" {
					t.Fatal("child did not pin generations")
				}
				for i := 0; i < 3; i++ {
					gcPublishValues(t, c, specs, revs, []float32{0, 1}, []float32{1, 0})
				}
				gcGenerations(t, c, specs, 1, 4)
				if _, err := fmt.Fprintln(in, "query"); err != nil {
					t.Fatal(err)
				}
				if !scanner.Scan() || scanner.Text() != "QUERIED" {
					t.Fatal("child lazy query failed")
				}
				if kill {
					if err := cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := fmt.Fprintln(in, "close"); err != nil {
						t.Fatal(err)
					}
				}
				if err := cmd.Wait(); err != nil && !kill {
					t.Fatal(err)
				}
				if _, err := c.Maintain(context.Background()); err != nil {
					t.Fatal(err)
				}
				gcGenerations(t, c, specs, 4)
			})
		}
	}
}

func TestCollectionMaintenanceRecovery(t *testing.T) {
	for _, phase := range []string{"staging", "index", "metadata", "commit-unpublished", "catalog-retired", "retired", "partly-deleted"} {
		t.Run(phase, func(t *testing.T) {
			c, specs, revs := gcCollection(t, "flat")
			r, err := c.Reader()
			if err != nil {
				t.Fatal(err)
			}
			gcPublish(t, c, specs, revs)
			gcPublish(t, c, specs, revs)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			dir := collectionSpacePath(c.config.Path, specs[0].ID)
			oldIndex, oldMeta, oldCommit := collectionGenerationFiles(dir, "flat", 1)
			newIndex, newMeta, newCommit := collectionGenerationFiles(dir, "flat", 4)
			copyFile := func(from, to string) {
				b, err := os.ReadFile(from)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(to, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			pending := filepath.Join(dir, ".pending-interrupted")
			if err := os.Mkdir(pending, 0o700); err != nil {
				t.Fatal(err)
			}
			copyFile(oldIndex, filepath.Join(pending, "index.flat"))
			switch phase {
			case "index":
				copyFile(oldIndex, newIndex)
			case "metadata":
				copyFile(oldIndex, newIndex)
				copyFile(oldMeta, newMeta)
			case "commit-unpublished":
				copyFile(oldIndex, newIndex)
				var metadata collectionVectorMetadata
				if err := readCollectionJSON(oldMeta, &metadata); err != nil {
					t.Fatal(err)
				}
				metadata.Generation = 4
				if err := writeCollectionJSON(newMeta, metadata); err != nil {
					t.Fatal(err)
				}
				indexHash, err := collectionFileHash(newIndex)
				if err != nil {
					t.Fatal(err)
				}
				metaHash, err := collectionFileHash(newMeta)
				if err != nil {
					t.Fatal(err)
				}
				commit := collectionCommit{Version: collectionFormatVersion, CorpusID: c.corpus, SpaceID: specs[0].ID, Generation: 4, IndexSHA256: indexHash, MetadataSHA256: metaHash}
				if err := writeCollectionJSON(newCommit, commit); err != nil {
					t.Fatal(err)
				}
				reader, err := OpenCollectionReader(c.config)
				if err != nil {
					t.Fatal(err)
				}
				if reader.spaces[specs[0].ID].Generation != 3 {
					t.Fatal("reader selected an unpublished commit")
				}
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
			case "catalog-retired":
				gcRetireCatalog(t, c.config, specs[0].ID, 1)
			case "retired", "partly-deleted":
				gcRetireCatalog(t, c.config, specs[0].ID, 1)
				if err := publishCollectionFile(oldCommit, filepath.Join(dir, ".retired-"+filepath.Base(oldCommit))); err != nil {
					t.Fatal(err)
				}
				if phase == "partly-deleted" {
					if err := os.Remove(oldIndex); err != nil {
						t.Fatal(err)
					}
				}
			}
			reopened, err := OpenCollection(c.config)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.LastMaintenanceError() != nil {
				t.Fatal(reopened.LastMaintenanceError())
			}
			for i := 0; i < 2; i++ {
				if _, err := reopened.Maintain(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			gcGenerations(t, reopened, specs, 3)
			for _, path := range []string{pending, newIndex, newMeta, newCommit, oldIndex, oldMeta, oldCommit} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("residual %s: %v", path, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".pending-") || strings.HasPrefix(entry.Name(), ".retired-") {
					t.Fatal("temporary file survived recovery", entry.Name())
				}
			}
		})
	}
}

func TestCollectionVerificationAndStructuralCorruption(t *testing.T) {
	for _, file := range []string{"index", "metadata", "commit", "spec", "missing-index", "missing-commit", "catalog", "missing-catalog"} {
		t.Run(file, func(t *testing.T) {
			c, specs, revs := gcCollection(t, "flat")
			r, err := c.Reader()
			if err != nil {
				t.Fatal(err)
			}
			gcPublish(t, c, specs, revs)
			gcPublish(t, c, specs, revs)
			_ = r.Close()
			dir := collectionSpacePath(c.config.Path, specs[0].ID)
			idx, meta, commit := collectionGenerationFiles(dir, "flat", 3)
			path := map[string]string{"index": idx, "metadata": meta, "commit": commit, "spec": filepath.Join(dir, "spec.json"), "missing-index": idx, "missing-commit": commit, "catalog": filepath.Join(dir, "spec.json"), "missing-catalog": filepath.Join(dir, "spec.json")}[file]
			if file == "missing-index" || file == "missing-commit" {
				err = os.Remove(path)
			} else if file == "catalog" || file == "missing-catalog" {
				var space collectionSpace
				if err := readCollectionJSON(path, &space); err != nil {
					t.Fatal(err)
				}
				if file == "catalog" {
					space.Generations = space.Generations[:1]
				} else {
					space.Generations = nil
				}
				err = replaceCollectionJSON(path, space) // Deliberately preserve the old checksum.
			} else {
				err = os.WriteFile(path, []byte("corrupt"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyCollection(context.Background(), c.config); !errors.Is(err, ErrCollectionCorrupt) {
				t.Fatalf("expected corruption: %v", err)
			}
			_, _, oldest := collectionGenerationFiles(dir, "flat", 1)
			if _, err := os.Stat(oldest); err != nil {
				t.Fatal("verification modified data", err)
			}
			if file == "index" || file == "metadata" {
				// Maintenance does not hash payloads; loading a vector generation does.
				if _, err := c.Maintain(context.Background()); err != nil {
					t.Fatal(err)
				}
				reader, err := OpenCollectionReader(c.config, specs[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				if _, err := reader.SearchVectorRequest(context.Background(), specs[0].ID,
					NewVectorSearchRequest("embedding", []float32{1, 0})); !errors.Is(err, ErrCollectionCorrupt) {
					t.Fatalf("lazy load accepted corruption: %v", err)
				}
				if err := VerifyCollection(context.Background(), c.config); !errors.Is(err, ErrCollectionCorrupt) {
					t.Fatalf("cleanup concealed current corruption: %v", err)
				}
				return
			}
			if _, err := c.Maintain(context.Background()); !errors.Is(err, ErrCollectionCorrupt) {
				t.Fatalf("maintenance accepted structural corruption: %v", err)
			}
			if _, err := os.Stat(oldest); err != nil {
				t.Fatal("cleanup deleted old commit on structural corruption", err)
			}
			_ = c.Close()
			if opened, err := OpenCollection(c.config); !errors.Is(err, ErrCollectionCorrupt) {
				if opened != nil {
					_ = opened.Close()
				}
				t.Fatalf("reopen: %v", err)
			}
		})
	}
}

func TestCollectionCommitSuccessDespiteMaintenanceFailure(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	dir := collectionSpacePath(c.config.Path, specs[0].ID)
	_, _, commit := collectionGenerationFiles(dir, "flat", 1)
	blocker := filepath.Join(dir, ".retired-"+filepath.Base(commit))
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertVectors(specs[0].ID, []VectorRecord{{ID: revs[0].ID, Revision: revs[0].Revision, Vector: []float32{1, 0}}}); err != nil {
		t.Fatalf("committed batch returned failure: %v", err)
	}
	if c.LastMaintenanceError() == nil {
		t.Fatal("maintenance error not observable")
	}
	gcGenerations(t, c, specs[:1], 1, 2)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	gcGenerations(t, c, specs[:1], 2)
}

func TestCollectionLaterSpaceStructuralCorruptionPreventsAllRetirement(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	reader, err := c.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for i := 0; i < 3; i++ {
		gcPublish(t, c, specs, revs)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, commit := collectionGenerationFiles(collectionSpacePath(c.config.Path, specs[1].ID), "flat", 4)
	if err := os.WriteFile(commit, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.Maintain(context.Background()); !errors.Is(err, ErrCollectionCorrupt) {
			t.Fatalf("expected second-space corruption: %v", err)
		}
		gcGenerations(t, c, specs[:1], 1, 4)
	}
}

func TestCollectionConcurrentDiscoveryAndCleanup(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 12; i++ {
				r, err := OpenCollectionReader(c.config)
				if err != nil {
					t.Error(err)
					return
				}
				gcQuery(t, r, specs)
				if err := r.Close(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := 0; i < 12; i++ {
		gcPublish(t, c, specs, revs)
	}
	wg.Wait()
	if _, err := c.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	gcGenerations(t, c, specs, 13)
}

func TestCollectionDeleteReinsertAndFilters(t *testing.T) {
	for _, storage := range []string{"flat", "usearch"} {
		t.Run(storage, func(t *testing.T) {
			if storage == "usearch" && os.Getenv("BLUGE_USEARCH_LIBRARY_PATH") == "" {
				t.Skip("native library not configured")
			}
			c, specs, revs := gcCollection(t, storage)
			old, err := c.Reader()
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()
			if err := c.DeleteDocuments("a"); err != nil {
				t.Fatal(err)
			}
			newRevs, err := c.UpsertDocuments([]*Document{collectionTestDoc("a", "green")})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.UpsertVectors(specs[0].ID, []VectorRecord{{ID: "a", Revision: revs[0].Revision, Vector: []float32{1, 0}}}); !errors.Is(err, ErrVectorRevisionConflict) {
				t.Fatal(err)
			}
			pending, err := c.Reader()
			if err != nil {
				t.Fatal(err)
			}
			hits, err := pending.SearchVectorRequest(context.Background(), specs[0].ID, NewVectorSearchRequest("embedding", []float32{1, 0}).SetK(2))
			if err != nil {
				t.Fatal(err)
			}
			assertVectorIDs(t, hits, "b")
			_ = pending.Close()
			revs[0] = newRevs[0]
			for i := 0; i < 3; i++ {
				gcPublish(t, c, specs, revs)
			}
			gcQuery(t, old, specs)
			r, err := c.Reader()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			for _, s := range specs {
				filter := NewTermQuery("green").SetField("body")
				hits, err := r.SearchVectorRequest(context.Background(), s.ID, NewVectorSearchRequest("embedding", []float32{1, 0}).SetK(2).SetFilter(filter))
				if err != nil {
					t.Fatal(err)
				}
				assertVectorIDs(t, hits, "a")
				prepared, err := r.PrepareVectorFilter(context.Background(), s.ID, filter)
				if err != nil {
					t.Fatal(err)
				}
				hits, err = r.VectorSearchPrepared(context.Background(), s.ID, "embedding", []float32{1, 0}, 2, prepared)
				if err != nil {
					t.Fatal(err)
				}
				assertVectorIDs(t, hits, "a")
			}
		})
	}
}

func TestCollectionMaintenanceCancellation(t *testing.T) {
	c, specs, _ := gcCollection(t, "flat")
	gate, err := lockCollectionCoordinator(context.Background(), c.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Maintain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	gcGenerations(t, c, specs, 1)
}

func TestCollectionDistinctReaderGenerations(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	first, err := c.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	gcPublish(t, c, specs[:1], revs)
	second, err := OpenCollectionReader(c.config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	gcPublish(t, c, specs, revs)
	gcPublish(t, c, specs, revs)
	gcGenerations(t, c, specs[:1], 1, 2, 4)
	gcGenerations(t, c, specs[1:], 1, 3)
	gcQuery(t, first, specs)
	gcQuery(t, second, specs)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	gcGenerations(t, c, specs[:1], 2, 4)
	gcGenerations(t, c, specs[1:], 1, 3)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	gcGenerations(t, c, specs[:1], 4)
	gcGenerations(t, c, specs[1:], 3)
}

func TestCollectionInterruptedProcess(t *testing.T) {
	root, phase := os.Getenv("BLUGE_GC_CRASH_ROOT"), os.Getenv("BLUGE_GC_CRASH_PHASE")
	if root == "" {
		return
	}
	cfg := DefaultCollectionConfig(root)
	cfg.VectorStorage = "flat"
	if phase == "publish" {
		c, err := OpenCollection(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Println("READY")
		if !bufio.NewScanner(os.Stdin).Scan() {
			t.Fatal("missing publish command")
		}
		if err := c.UpsertVectors("model-a", []VectorRecord{{ID: "a", Revision: c.revisions["a"], Vector: []float32{0, 1}}}); err != nil {
			t.Fatal(err)
		}
		t.Fatal("publication should be blocked by the coordinator")
	}
	l, err := lock.OpenExclusive(filepath.Join(root, "collection.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	gate, err := lockCollectionCoordinator(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	dir := collectionSpacePath(root, "model-a")
	idx, _, commit := collectionGenerationFiles(dir, "flat", 1)
	gcRetireCatalog(t, cfg, "model-a", 1)
	if err := publishCollectionFile(commit, filepath.Join(dir, ".retired-"+filepath.Base(commit))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(idx); err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	bufio.NewScanner(os.Stdin).Scan() // Killed while holding writer and coordinator locks.
}

func gcRetireCatalog(t *testing.T, cfg CollectionConfig, id string, retired uint64) {
	t.Helper()
	dir := collectionSpacePath(cfg.Path, id)
	var space collectionSpace
	if err := readCollectionJSON(filepath.Join(dir, "spec.json"), &space); err != nil {
		t.Fatal(err)
	}
	var kept []uint64
	for _, n := range space.Generations {
		if n != retired {
			kept = append(kept, n)
		}
	}
	space.Generations = kept
	if err := writeCollectionSpace(dir, &space); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionRequiresCurrentFormat(t *testing.T) {
	for _, target := range []string{"manifest", "space"} {
		t.Run(target, func(t *testing.T) {
			c, specs, _ := gcCollection(t, "flat")
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(c.config.Path, "collection.json")
			if target == "space" {
				path = filepath.Join(collectionSpacePath(c.config.Path, specs[0].ID), "spec.json")
			}
			var data map[string]interface{}
			if err := readCollectionJSON(path, &data); err != nil {
				t.Fatal(err)
			}
			data["version"] = collectionFormatVersion + 1
			if err := replaceCollectionJSON(path, data); err != nil {
				t.Fatal(err)
			}
			if r, err := OpenCollectionReader(c.config); !errors.Is(err, ErrCollectionCorrupt) {
				if r != nil {
					_ = r.Close()
				}
				t.Fatalf("reader accepted unsupported format: %v", err)
			}
			if w, err := OpenCollection(c.config); !errors.Is(err, ErrCollectionCorrupt) {
				if w != nil {
					_ = w.Close()
				}
				t.Fatalf("writer accepted unsupported format: %v", err)
			}
		})
	}
}

func TestCollectionOrphanLeaseIsNeverDeleted(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	r, err := c.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	gcPublish(t, c, specs, revs)
	gcPublish(t, c, specs, revs)
	gcRetireCatalog(t, c.config, specs[0].ID, 1)
	if _, err := c.Maintain(context.Background()); !errors.Is(err, ErrCollectionCorrupt) {
		t.Fatalf("unreferenced reader lease: %v", err)
	}
	gcQuery(t, r, specs)
}

func TestCollectionKilledPublicationAndCleanup(t *testing.T) {
	for _, phase := range []string{"publish", "cleanup"} {
		t.Run(phase, func(t *testing.T) {
			c, specs, revs := gcCollection(t, "flat")
			if phase == "cleanup" {
				r, err := c.Reader()
				if err != nil {
					t.Fatal(err)
				}
				gcPublish(t, c, specs, revs)
				gcPublish(t, c, specs, revs)
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCollectionInterruptedProcess$")
			cmd.Env = append(os.Environ(), "BLUGE_GC_CRASH_ROOT="+c.config.Path, "BLUGE_GC_CRASH_PHASE="+phase)
			cmd.Stderr = os.Stderr
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			scanner := bufio.NewScanner(out)
			if !scanner.Scan() || scanner.Text() != "READY" {
				t.Fatal("child not ready")
			}
			var gate lock.LockedFile
			if phase == "publish" {
				gate, err = lockCollectionCoordinator(ctx, c.config.Path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fmt.Fprintln(in, "publish"); err != nil {
					t.Fatal(err)
				}
				_, metadata, _ := collectionGenerationFiles(collectionSpacePath(c.config.Path, specs[0].ID), "flat", 2)
				for {
					if _, err := os.Stat(metadata); err == nil {
						break
					}
					if err := ctx.Err(); err != nil {
						t.Fatal("child never reached publication", err)
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
			// Active publication and cleanup must both exclude another maintainer.
			if _, err := MaintainCollection(ctx, c.config); !collectionLockBusy(err) {
				t.Fatalf("writer protection: %v", err)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if gate != nil {
				if err := gate.Close(); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := OpenCollection(c.config)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if phase == "publish" {
				gcGenerations(t, reopened, specs, 1)
			} else {
				gcGenerations(t, reopened, specs, 3)
			}
			if _, err := reopened.Maintain(ctx); err != nil {
				t.Fatal(err)
			}
			r, err := reopened.Reader()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			gcQuery(t, r, specs)
		})
	}
}

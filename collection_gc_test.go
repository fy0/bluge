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
	"github.com/fy0/bluge/internal/fsatomic"
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
			if strings.HasPrefix(e.Name(), "metadata-") {
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
				// #nosec G204 G702 -- Relaunch this test binary with a fixed helper test.
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
				defer func() { _ = cmd.Process.Kill() }() // The process may already have exited.
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
	c, specs, revs := gcCollection(t, "flat")
	old, err := c.Reader()
	if err != nil {
		t.Fatal(err)
	}
	gcPublish(t, c, specs, revs)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	dir := collectionSpacePath(c.config.Path, specs[0].ID)
	pending := filepath.Join(dir, ".pending-interrupted")
	if err := os.Mkdir(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath, metadataPath := collectionGenerationFiles(dir, 3)
	for _, path := range []string{indexPath, metadataPath, filepath.Join(pending, "payload")} {
		if err := os.WriteFile(path, []byte("unpublished"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := OpenCollectionReader(c.config)
	if err != nil {
		t.Fatal(err)
	}
	gcQuery(t, reader, specs)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCollection(c.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	gcGenerations(t, reopened, specs, 2)
	for _, path := range []string{pending, indexPath, metadataPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unreferenced payload remains: %s: %v", path, err)
		}
	}
	if _, err := reopened.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionMissingSpecPreservesGenerationFiles(t *testing.T) {
	c, specs, _ := gcCollection(t, "flat")
	dir := collectionSpacePath(c.config.Path, specs[0].ID)
	if err := os.Remove(filepath.Join(dir, "spec.json")); err != nil {
		t.Fatal(err)
	}
	indexPath, metadataPath := collectionGenerationFiles(dir, 1)
	paths := []string{indexPath, metadataPath, filepath.Join(dir, ".pending-preserve")}
	if err := os.WriteFile(paths[2], []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Maintain(context.Background()); !errors.Is(err, ErrCollectionCorrupt) {
		t.Fatalf("maintenance accepted missing spec: %v", err)
	}
	if err := VerifyCollection(context.Background(), c.config); !errors.Is(err, ErrCollectionCorrupt) {
		t.Fatalf("verification accepted missing spec: %v", err)
	}
	if reader, err := OpenCollectionReader(c.config); !errors.Is(err, ErrCollectionCorrupt) {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("reader accepted missing spec: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenCollection(c.config); !errors.Is(err, ErrCollectionCorrupt) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("writer accepted missing spec: %v", err)
	}
	if _, err := MaintainCollection(context.Background(), c.config); !errors.Is(err, ErrCollectionCorrupt) {
		t.Fatalf("offline maintenance accepted missing spec: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("corrupt space lost file: %s: %v", path, err)
		}
	}
}

func TestCollectionMaintenanceFailureDoesNotFailCommit(t *testing.T) {
	c, specs, revs := gcCollection(t, "flat")
	spaceID := specs[0].ID
	leasePath := collectionLeasePath(collectionSpacePath(c.config.Path, spaceID), 1)
	if err := os.Mkdir(leasePath, 0o700); err != nil {
		t.Fatal(err)
	}
	gcPublish(t, c, specs[:1], revs)
	if c.LastMaintenanceError() == nil {
		t.Fatal("maintenance failure was not recorded")
	}
	r, err := OpenCollectionReader(c.config, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	gcQuery(t, r, specs[:1])
	status, err := r.VectorStatus(context.Background(), spaceID)
	if err != nil || status.Generation != 2 || status.ValidVectors != 2 {
		t.Fatalf("committed generation: %+v, %v", status, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Maintain(context.Background(), spaceID); err != nil {
		t.Fatal(err)
	}
	if c.LastMaintenanceError() != nil {
		t.Fatal(c.LastMaintenanceError())
	}
	gcGenerations(t, c, specs[:1], 2)
}

func TestCollectionVerificationAndStructuralCorruption(t *testing.T) {
	for _, target := range []string{"index", "metadata", "missing-index", "unordered-generations"} {
		t.Run(target, func(t *testing.T) {
			c, specs, _ := gcCollection(t, "flat")
			dir := collectionSpacePath(c.config.Path, specs[0].ID)
			indexPath, metadataPath := collectionGenerationFiles(dir, 1)
			var err error
			switch target {
			case "index":
				err = os.WriteFile(indexPath, []byte("truncated"), 0o600)
			case "metadata":
				err = os.WriteFile(metadataPath, []byte("invalid JSON"), 0o600)
			case "missing-index":
				err = os.Remove(indexPath)
			case "unordered-generations":
				space := c.spaces[specs[0].ID]
				space.Generations = []uint64{1, 1}
				err = writeCollectionSpace(dir, &space)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyCollection(context.Background(), c.config); !errors.Is(err, ErrCollectionCorrupt) {
				t.Fatalf("verification accepted corruption: %v", err)
			}
		})
	}
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
	indexPath, _ := collectionGenerationFiles(collectionSpacePath(c.config.Path, specs[1].ID), 4)
	if err := os.Remove(indexPath); err != nil {
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
		if err := c.loadRevisions(); err != nil {
			t.Fatal(err)
		}
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
	idx, _ := collectionGenerationFiles(dir, 1)
	gcRetireFirstGeneration(t, cfg, "model-a")
	if err := os.Remove(idx); err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	bufio.NewScanner(os.Stdin).Scan() // Killed while holding writer and coordinator locks.
}

func gcRetireFirstGeneration(t *testing.T, cfg CollectionConfig, id string) {
	t.Helper()
	dir := collectionSpacePath(cfg.Path, id)
	var space collectionSpace
	if err := readCollectionJSON(filepath.Join(dir, "spec.json"), &space); err != nil {
		t.Fatal(err)
	}
	var kept []uint64
	for _, n := range space.Generations {
		if n != 1 {
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
	gcRetireFirstGeneration(t, c.config, specs[0].ID)
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
			// #nosec G204 G702 -- Relaunch this test binary with a fixed helper test.
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
			defer func() { _ = cmd.Process.Kill() }() // The process may already have exited.
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
				_, metadata := collectionGenerationFiles(collectionSpacePath(c.config.Path, specs[0].ID), 2)
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
			if _, err := MaintainCollection(ctx, c.config); !fsatomic.LockBusy(err) {
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

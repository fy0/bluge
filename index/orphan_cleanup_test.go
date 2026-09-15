//  Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package index

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fy0/bluge/index/mergeplan"
)

// segmentFileIDs lists the .seg ids present in a filesystem directory.
func segmentFileIDs(t *testing.T, cfg Config) []uint64 {
	t.Helper()
	dir := cfg.DirectoryFunc()
	if err := dir.Setup(true); err != nil {
		t.Fatalf("error setting up directory for listing: %v", err)
	}
	ids, err := dir.List(ItemKindSegment)
	if err != nil {
		t.Fatalf("error listing segments: %v", err)
	}
	return ids
}

// TestOrphanedSegmentFilesRemovedOnOpen verifies that segment files present
// in the directory but not referenced by any loadable snapshot are removed
// when the writer opens, reclaiming space lost to interrupted runs.
func TestOrphanedSegmentFilesRemovedOnOpen(t *testing.T) {
	cfg, cleanup := CreateConfig("TestOrphanedSegmentFilesRemovedOnOpen")
	defer func() {
		if err := cleanup(); err != nil {
			t.Log(err)
		}
	}()

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	batch := NewBatch()
	doc := &FakeDocument{
		NewFakeField("_id", "1", true, false, false),
		NewFakeField("name", "hello", true, false, true),
	}
	doc.FakeComposite("_all", nil)
	batch.Update(testIdentifier("1"), doc)
	if err = idx.Batch(batch); err != nil {
		t.Fatal(err)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}

	liveSegs := segmentFileIDs(t, cfg)
	if len(liveSegs) != 1 {
		t.Fatalf("expected 1 live segment, got %v", liveSegs)
	}

	// drop two fake orphan segment files: one with a low id, one high
	fsDir, ok := cfg.DirectoryFunc().(*FileSystemDirectory)
	if !ok {
		t.Skip("test requires the filesystem directory")
	}
	for _, id := range []uint64{0xdead, 0xbeef0000} {
		path := filepath.Join(fsDir.path, fsDir.fileName(ItemKindSegment, id))
		if err := ioutil.WriteFile(path, []byte("orphan"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	idx, err = OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// orphan files must be gone already at open
	got := segmentFileIDs(t, cfg)
	if len(got) != 1 || got[0] != liveSegs[0] {
		t.Fatalf("expected only live segment %v, got %v", liveSegs, got)
	}

	r, err := idx.Reader()
	if err != nil {
		t.Fatal(err)
	}
	count, err := r.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected doc count 1, got %d", count)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSkippedMergeSegmentFileRemoved verifies that a merge output segment
// file whose introduction is skipped (because all its docs became obsolete)
// is removed instead of remaining on disk forever.
func TestSkippedMergeSegmentFileRemoved(t *testing.T) {
	cfg, cleanup := CreateConfig("TestSkippedMergeSegmentFileRemoved")
	var introComplete, mergeIntroStart, mergeIntroComplete sync.WaitGroup
	introComplete.Add(1)
	mergeIntroStart.Add(1)
	mergeIntroComplete.Add(1)
	var segIntroCompleted int
	cfg.EventCallback = func(e Event) {
		if e.Kind == EventKindBatchIntroduction {
			segIntroCompleted++
			if segIntroCompleted == 3 {
				introComplete.Done()
			}
		} else if e.Kind == EventKindMergeTaskIntroductionStart {
			mergeIntroStart.Done()
			introComplete.Wait()
		} else if e.Kind == EventKindMergeTaskIntroduction {
			mergeIntroComplete.Done()
		}
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Log(err)
		}
	}()

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}

	batch := NewBatch()
	doc := &FakeDocument{
		NewFakeField("_id", "1", true, false, false),
		NewFakeField("name", "test3", true, false, true),
	}
	doc.FakeComposite("_all", nil)
	batch.Update(testIdentifier("1"), doc)
	if err = idx.Batch(batch); err != nil {
		t.Fatal(err)
	}

	batch.Reset()
	doc = &FakeDocument{
		NewFakeField("_id", "2", true, false, false),
		NewFakeField("name", "test2updated", true, false, true),
	}
	doc.FakeComposite("_all", nil)
	batch.Update(testIdentifier("2"), doc)
	if err = idx.Batch(batch); err != nil {
		t.Fatal(err)
	}

	mergeIntroStart.Wait()

	batch.Reset()
	batch.Delete(testIdentifier("1"))
	batch.Delete(testIdentifier("2"))
	doc = &FakeDocument{
		NewFakeField("_id", "3", true, false, false),
		NewFakeField("name", "test3updated", true, false, true),
	}
	doc.FakeComposite("_all", nil)
	batch.Update(testIdentifier("3"), doc)
	if err = idx.Batch(batch); err != nil {
		t.Fatal(err)
	}

	mergeIntroComplete.Wait()

	if skipped := atomic.LoadUint64(&idx.stats.TotFileMergeIntroductionsObsoleted); skipped != 1 {
		t.Fatalf("expected 1 skipped merge introduction, got %d", skipped)
	}

	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}

	// after a clean close with a converged merge, every .seg file on disk
	// must be referenced by the last snapshot
	got := segmentFileIDs(t, cfg)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 segment file after close, got %v", got)
	}
}

// updateOneDoc indexes a single document with the given id.
func updateOneDoc(t *testing.T, idx *Writer, id string) {
	t.Helper()
	batch := NewBatch()
	doc := &FakeDocument{
		NewFakeField("_id", id, true, false, false),
		NewFakeField("name", "name"+id, true, false, true),
	}
	doc.FakeComposite("_all", nil)
	batch.Update(testIdentifier(id), doc)
	if err := idx.Batch(batch); err != nil {
		t.Fatal(err)
	}
}

// noFileMerging keeps the on-disk layout deterministic for these tests:
// one segment and one snapshot per batch, no merge-produced snapshots.
func noFileMerging(cfg *Config) {
	cfg.MergePlanOptions.CalcBudget = func(_ int64, _ int64, _ *mergeplan.Options) int {
		return 1000
	}
}

// dirContents reads every file in a filesystem index directory into a
// name -> contents map.
func dirContents(t *testing.T, path string) map[string][]byte {
	t.Helper()
	entries, err := ioutil.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := ioutil.ReadFile(filepath.Join(path, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents[e.Name()] = data
	}
	return contents
}

func assertSameContents(t *testing.T, want, got map[string][]byte) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("file set changed: want %d files, got %d", len(want), len(got))
	}
	for name, wantData := range want {
		gotData, ok := got[name]
		if !ok {
			t.Fatalf("file %s missing after failed open", name)
		}
		if !bytes.Equal(wantData, gotData) {
			t.Fatalf("file %s contents changed after failed open", name)
		}
	}
}

// TestOpenWriterFailsOnUnreadableSnapshot verifies that when any snapshot
// file cannot be loaded, OpenWriter returns an error and the failed open
// leaves every file untouched: no cleanup sweep, no new snapshot written
// over the unreadable epoch. The failed open must still release the lock
// and the partially loaded root.
func TestOpenWriterFailsOnUnreadableSnapshot(t *testing.T) {
	corruptions := map[string]func(t *testing.T, path string){
		// flips a content byte; load fails on the CRC check
		"crc": func(t *testing.T, path string) {
			raw, err := ioutil.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw[len(raw)/2] ^= 0xff
			if err := ioutil.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		},
		// overwrites the format version byte with an unsupported value
		"version": func(t *testing.T, path string) {
			raw, err := ioutil.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw[0] = 0x7f
			if err := ioutil.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"truncate": func(t *testing.T, path string) {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, fi.Size()/2); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, corrupt := range corruptions {
		corrupt := corrupt
		t.Run(name, func(t *testing.T) {
			cfg, cleanup := CreateConfig("TestOpenWriterFailsOnUnreadableSnapshot" + name)
			defer func() {
				if err := cleanup(); err != nil {
					t.Log(err)
				}
			}()
			cfg.DeletionPolicyFunc = func() DeletionPolicy {
				return NewKeepNLatestDeletionPolicy(3)
			}
			noFileMerging(&cfg)

			idx, err := OpenWriter(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"1", "2", "3"} {
				updateOneDoc(t, idx, id)
			}
			if err = idx.Close(); err != nil {
				t.Fatal(err)
			}

			fsDir, ok := cfg.DirectoryFunc().(*FileSystemDirectory)
			if !ok {
				t.Skip("test requires the filesystem directory")
			}

			segsBefore := segmentFileIDs(t, cfg)
			if len(segsBefore) != 3 {
				t.Fatalf("expected 3 segment files, got %v", segsBefore)
			}

			snapEpochs, err := fsDir.List(ItemKindSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapEpochs) != 3 {
				t.Fatalf("expected 3 snapshots, got %v", snapEpochs)
			}
			// List returns newest first: [batch3-persist, batch3-intro,
			// batch2-persist]. Corrupt the newest and delete the middle
			// one, so the third batch's segment is referenced only by
			// the corrupted snapshot - the data-loss scenario. Also
			// drop an orphan segment file that no snapshot references.
			snpPath := filepath.Join(fsDir.path,
				fsDir.fileName(ItemKindSnapshot, snapEpochs[0]))
			midPath := filepath.Join(fsDir.path,
				fsDir.fileName(ItemKindSnapshot, snapEpochs[1]))
			corrupt(t, snpPath)
			if err := os.Remove(midPath); err != nil {
				t.Fatal(err)
			}
			if err := ioutil.WriteFile(filepath.Join(fsDir.path,
				fsDir.fileName(ItemKindSegment, 0xdead)), []byte("orphan"), 0600); err != nil {
				t.Fatal(err)
			}

			filesBefore := dirContents(t, fsDir.path)

			idx, err = OpenWriter(cfg)
			if err == nil {
				_ = idx.Close()
				t.Fatal("expected OpenWriter to fail on unreadable snapshot")
			}
			if !strings.Contains(err.Error(), "snapshot") {
				t.Fatalf("expected a snapshot load error, got %v", err)
			}

			// the failed open must not have removed or modified anything:
			// the corrupted snapshot, the segment only it references and
			// the orphan all survive byte-for-byte
			assertSameContents(t, filesBefore, dirContents(t, fsDir.path))

			// a second open must hit the same snapshot error - not a
			// stale lock - proving the failed open released its resources
			idx, err = OpenWriter(cfg)
			if err == nil {
				_ = idx.Close()
				t.Fatal("expected second OpenWriter to fail as well")
			}
			if !strings.Contains(err.Error(), "snapshot") ||
				strings.Contains(err.Error(), "exclusive") {
				t.Fatalf("expected snapshot error after failed open, got %v", err)
			}

			// once the unreadable snapshot is removed by the operator,
			// the writer opens on the remaining snapshot and sweeps the
			// now provably unreferenced files
			if err := os.Remove(snpPath); err != nil {
				t.Fatal(err)
			}
			idx, err = OpenWriter(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r, err := idx.Reader()
			if err != nil {
				t.Fatal(err)
			}
			count, err := r.Count()
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Close(); err != nil {
				t.Fatal(err)
			}
			// the newest remaining snapshot is the second batch's
			if count != 2 {
				t.Fatalf("expected doc count 2 from newest loadable snapshot, got %d", count)
			}
			if err = idx.Close(); err != nil {
				t.Fatal(err)
			}

			// segsBefore is newest-first; the newest entry is the batch-3
			// segment only the corrupted snapshot referenced, so the
			// surviving batch-2 snapshot keeps the remaining two
			if got := segmentFileIDs(t, cfg); !reflect.DeepEqual(got, segsBefore[1:]) {
				t.Fatalf("expected segments %v after recovery, got %v", segsBefore[1:], got)
			}
		})
	}
}

// TestOpenWriterFailsOnMissingSegment verifies that a snapshot whose
// referenced segment file is missing also fails the open without touching
// any file, and that the released lock allows reopening once the snapshot
// is removed.
func TestOpenWriterFailsOnMissingSegment(t *testing.T) {
	cfg, cleanup := CreateConfig("TestOpenWriterFailsOnMissingSegment")
	defer func() {
		if err := cleanup(); err != nil {
			t.Log(err)
		}
	}()

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	updateOneDoc(t, idx, "1")
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}

	fsDir, ok := cfg.DirectoryFunc().(*FileSystemDirectory)
	if !ok {
		t.Skip("test requires the filesystem directory")
	}
	segs := segmentFileIDs(t, cfg)
	if len(segs) != 1 {
		t.Fatalf("expected 1 segment file, got %v", segs)
	}
	snaps, err := fsDir.List(ItemKindSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	snpPath := filepath.Join(fsDir.path,
		fsDir.fileName(ItemKindSnapshot, snaps[0]))
	if err := os.Remove(filepath.Join(fsDir.path,
		fsDir.fileName(ItemKindSegment, segs[0]))); err != nil {
		t.Fatal(err)
	}

	filesBefore := dirContents(t, fsDir.path)

	idx, err = OpenWriter(cfg)
	if err == nil {
		_ = idx.Close()
		t.Fatal("expected OpenWriter to fail on missing segment")
	}
	assertSameContents(t, filesBefore, dirContents(t, fsDir.path))

	// the failed open released the lock: removing the offending snapshot
	// leaves an empty but openable index
	if err := os.Remove(snpPath); err != nil {
		t.Fatal(err)
	}
	idx, err = OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSweepRemovesOrphansOnlyWithMultipleSnapshotsRetained verifies that
// when the policy retains more than one snapshot, the open-time sweep still
// removes only files not referenced by any retained snapshot.
func TestSweepRemovesOrphansOnlyWithMultipleSnapshotsRetained(t *testing.T) {
	cfg, cleanup := CreateConfig("TestSweepRemovesOrphansOnlyWithMultipleSnapshotsRetained")
	defer func() {
		if err := cleanup(); err != nil {
			t.Log(err)
		}
	}()
	cfg.DeletionPolicyFunc = func() DeletionPolicy {
		return NewKeepNLatestDeletionPolicy(2)
	}
	noFileMerging(&cfg)

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2", "3"} {
		updateOneDoc(t, idx, id)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}

	fsDir, ok := cfg.DirectoryFunc().(*FileSystemDirectory)
	if !ok {
		t.Skip("test requires the filesystem directory")
	}

	segsBefore := segmentFileIDs(t, cfg)

	// drop two fake orphan segment files
	for _, id := range []uint64{0xdead, 0xbeef0000} {
		path := filepath.Join(fsDir.path, fsDir.fileName(ItemKindSegment, id))
		if err := ioutil.WriteFile(path, []byte("orphan"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	idx, err = OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// the orphans are gone and every segment referenced by a retained
	// snapshot is still present
	if got := segmentFileIDs(t, cfg); !reflect.DeepEqual(got, segsBefore) {
		t.Fatalf("expected segments %v after orphan sweep, got %v", segsBefore, got)
	}
	for _, id := range []uint64{0xdead, 0xbeef0000} {
		path := filepath.Join(fsDir.path, fsDir.fileName(ItemKindSegment, id))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected orphan segment %x removed, stat err: %v", id, err)
		}
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}
}

// flakyRemoveDirectory fails the first Remove call for each segment id in
// failOnce, then delegates to the wrapped directory.
type flakyRemoveDirectory struct {
	Directory
	failOnce map[uint64]bool
}

func (d *flakyRemoveDirectory) Remove(kind string, id uint64) error {
	if kind == ItemKindSegment && d.failOnce[id] {
		delete(d.failOnce, id)
		return fmt.Errorf("injected failure removing segment %x", id)
	}
	return d.Directory.Remove(kind, id)
}

// TestSweepRetriesFailedRemove verifies that a segment file whose removal
// fails during the open-time sweep stays tracked and is retried by the next
// Cleanup.
func TestSweepRetriesFailedRemove(t *testing.T) {
	cfg, cleanup := CreateConfig("TestSweepRetriesFailedRemove")
	defer func() {
		if err := cleanup(); err != nil {
			t.Log(err)
		}
	}()

	fsDir, ok := cfg.DirectoryFunc().(*FileSystemDirectory)
	if !ok {
		t.Skip("test requires the filesystem directory")
	}
	failOnce := map[uint64]bool{}
	cfg.DirectoryFunc = func() Directory {
		return &flakyRemoveDirectory{
			Directory: NewFileSystemDirectory(fsDir.path),
			failOnce:  failOnce,
		}
	}

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	updateOneDoc(t, idx, "1")
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}

	// drop an orphan segment file whose first removal will fail
	orphanPath := filepath.Join(fsDir.path, fsDir.fileName(ItemKindSegment, 0xdead))
	if err := ioutil.WriteFile(orphanPath, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	failOnce[0xdead] = true

	// the open-time cleanup attempts the removal, which fails; the error
	// is per-file and must not fail the open
	idx, err = OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("expected orphan to survive failed remove: %v", err)
	}

	// the failed id stays tracked, so the cleanup at Close retries and
	// removes it
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphan removed after retry, stat err: %v", err)
	}
}

// TestSweepWithOpenReaderHoldingSnapshot verifies the open-time sweep works
// while a reader holds a snapshot open: orphans are removed, the live
// segment stays, and the reader remains usable.
func TestSweepWithOpenReaderHoldingSnapshot(t *testing.T) {
	cfg, cleanup := CreateConfig("TestSweepWithOpenReaderHoldingSnapshot")
	defer func() {
		if err := cleanup(); err != nil {
			t.Log(err)
		}
	}()

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	updateOneDoc(t, idx, "1")
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}

	fsDir, ok := cfg.DirectoryFunc().(*FileSystemDirectory)
	if !ok {
		t.Skip("test requires the filesystem directory")
	}
	liveSegs := segmentFileIDs(t, cfg)

	orphanPath := filepath.Join(fsDir.path, fsDir.fileName(ItemKindSegment, 0xdead))
	if err := ioutil.WriteFile(orphanPath, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReader(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// the writer lock is separate from the reader, so this opens fine
	idx, err = OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if got := segmentFileIDs(t, cfg); !reflect.DeepEqual(got, liveSegs) {
		t.Fatalf("expected segments %v after orphan sweep, got %v", liveSegs, got)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphan segment removed, stat err: %v", err)
	}

	count, err := r.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected reader doc count 1, got %d", count)
	}

	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}
}

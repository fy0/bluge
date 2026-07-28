package index

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/fy0/bluge/index/mergeplan"
	segment "github.com/fy0/bluge/segment"
)

func TestWriterCloseWaitsForMergeConvergence(t *testing.T) {
	cfg, cleanup := CreateConfig("TestWriterCloseWaitsForMergeConvergence")
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
	}()

	// Keep the persister moving while the first file merge is deliberately
	// blocked, and require the planner to converge all segments into one.
	cfg.PersisterNapUnderNumFiles = 1000
	cfg.MergePlanOptions.CalcBudget = func(int64, int64, *mergeplan.Options) int {
		return 1
	}

	mergeBlocked := make(chan struct{})
	releaseMerge := make(chan struct{})
	var blockOnce sync.Once
	cfg.EventCallback = func(event Event) {
		if event.Kind == EventKindMergeTaskIntroductionStart {
			blockOnce.Do(func() {
				close(mergeBlocked)
				<-releaseMerge
			})
		}
	}

	w, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 40; i++ {
		id := strconv.Itoa(i)
		batch := NewBatch()
		batch.Update(testIdentifier(id), &FakeDocument{
			NewFakeField("_id", id, true, false, false),
			NewFakeField("body", "common term", false, false, true),
		})
		if err := w.Batch(batch); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-mergeBlocked:
	case <-time.After(10 * time.Second):
		t.Fatal("merger did not reach the blocked introduction")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- w.Close()
	}()
	close(releaseMerge)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("writer close did not converge")
	}

	if err := w.Batch(NewBatch()); err != segment.ErrClosed {
		t.Fatalf("batch after close: got %v want %v", err, segment.ErrClosed)
	}

	reader, err := OpenReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	if got := len(reader.Segments()); got != 1 {
		t.Fatalf("segments after close: got %d want 1", got)
	}
	count, err := reader.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 40 {
		t.Fatalf("documents after close: got %d want 40", count)
	}
}

func TestWriterClosePersistsUnsafeBatchesBeforeConvergence(t *testing.T) {
	cfg, cleanup := CreateConfig("TestWriterClosePersistsUnsafeBatchesBeforeConvergence")
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
	}()
	cfg = cfg.WithUnsafeBatches()
	cfg.MergePlanOptions.CalcBudget = func(int64, int64, *mergeplan.Options) int {
		return 1
	}

	w, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		id := strconv.Itoa(i)
		batch := NewBatch()
		batch.Update(testIdentifier(id), &FakeDocument{
			NewFakeField("_id", id, true, false, false),
			NewFakeField("body", "common term", false, false, true),
		})
		if err := w.Batch(batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got := len(reader.Segments()); got != 1 {
		t.Fatalf("segments after close: got %d want 1", got)
	}
	count, err := reader.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 8 {
		t.Fatalf("documents after close: got %d want 8", count)
	}
}

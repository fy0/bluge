package index

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/fy0/bluge/segment"
)

func TestByteMergePlanningPreservesUpdatesAndReopen(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.MergePlanningByBytes = true
	cfg.MergePlanOptions.MaxSegmentSize = 64 * 1024
	cfg.MergePlanOptions.FloorSegmentSize = 8 * 1024
	cfg.MergePlanOptions.SegmentsPerMergeTask = 4
	w, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	data := make([]byte, 8*1024)
	_, _ = rand.New(rand.NewSource(1)).Read(data)
	want := make(map[string]string)
	write := func(id, body string) {
		t.Helper()
		b := NewBatch()
		b.Update(testIdentifier(id), &FakeDocument{
			NewFakeField("_id", id, true, false, true), NewFakeField("body", body, true, false, false),
		})
		if err := w.Batch(b); err != nil {
			t.Fatal(err)
		}
		want[id] = body
	}
	for i := 0; i < 30; i++ {
		write(fmt.Sprint(i), string(data))
	}
	pinned, err := w.Reader()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		write(fmt.Sprint(i), "replacement")
	}
	b := NewBatch()
	for i := 5; i < 10; i++ {
		b.Delete(testIdentifier(fmt.Sprint(i)))
		delete(want, fmt.Sprint(i))
	}
	if err := w.Batch(b); err != nil {
		t.Fatal(err)
	}
	if n, err := pinned.Count(); err != nil || n != 30 {
		t.Fatalf("pinned snapshot: %d %v", n, err)
	}
	if err := pinned.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.Stats().TotFileMergeZapEnd == 0 {
		t.Fatal("test did not merge")
	}
	r, err := OpenReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.segment) < 2 {
		t.Fatal("byte limit merged the whole index into one segment")
	}
	for _, ss := range r.segment {
		if ss.segment.encodedBytes == 0 {
			t.Fatal("reopened segment lost byte size")
		}
		// Encoded output can differ from the input estimate; allow footer and
		// block-boundary overhead, while excluding an unrestricted full merge.
		if ss.segment.encodedBytes > 72*1024 {
			t.Fatalf("unexpected merged size: %d", ss.segment.encodedBytes)
		}
		it := ss.DocNumbersLive().Iterator()
		for it.HasNext() {
			var id, body string
			if err := ss.VisitDocument(uint64(it.Next()), func(name string, value []byte) bool {
				if name == "_id" {
					id = string(value)
				}
				if name == "body" {
					body = string(value)
				}
				return true
			}); err != nil {
				t.Fatal(err)
			}
			expected, ok := want[id]
			if !ok || body != expected {
				t.Fatalf("unexpected or stale document %q", id)
			}
			delete(want, id)
		}
	}
	if len(want) > 0 {
		t.Fatalf("lost documents: %d", len(want))
	}
}

type byteMergeTestSegment struct{ segment.Segment }

func (byteMergeTestSegment) Count() uint64 { return 4 }
func (byteMergeTestSegment) Size() int     { return 4000 }

func TestByteMergeSizesAndPersistenceBacklog(t *testing.T) {
	ss := &segmentSnapshot{segment: &segmentWrapper{Segment: byteMergeTestSegment{}, encodedBytes: 4000}}
	sized := byteSizeSegment{ss}
	if sized.FullSize() != 4000 || sized.LiveSize() != 4000 {
		t.Fatal("incorrect full size")
	}
	ss.deleted = roaring.BitmapOf(0)
	if sized.LiveSize() != 3000 {
		t.Fatal("deletions not reflected in merge weight")
	}
	ss.deleted = roaring.BitmapOf(0, 1, 2, 3)
	if sized.LiveSize() != 0 {
		t.Fatal("fully deleted segment cannot be removed")
	}
	ss.deleted = nil
	cfg := InMemoryOnlyConfig()
	cfg.MergePlanningByBytes = true
	cfg.MergePlanOptions.MaxSegmentSize = 6000
	w := &Writer{config: cfg}
	// Two unpersisted segments exceed the merge budget. No merge channels are
	// available: the persister must return to direct persistence without waiting.
	snapshot := &Snapshot{segment: []*segmentSnapshot{ss, ss}}
	merged, err := w.persistSnapshotMaybeMerge(nil, nil, snapshot)
	if err != nil || merged {
		t.Fatalf("large memory backlog: merged=%t error=%v", merged, err)
	}
}

package zap

import (
	"bytes"
	"math/rand"
	"testing"

	bleveindex "github.com/blevesearch/bleve_index_api"

	"github.com/fy0/bluge/internal/blugeidx"
)

func TestSmallBatchesDoNotOverallocateAfterLargeDocuments(t *testing.T) {
	data := make([]byte, 256*1024)
	_, _ = rand.New(rand.NewSource(1)).Read(data)
	doc, err := blugeidx.NewDocument([]*blugeidx.Field{
		blugeidx.NewField("_id", []byte("doc"), bleveindex.StoreField, 0, nil, 3),
		blugeidx.NewField("body", data, bleveindex.StoreField, 0, nil, uint64(len(data))),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the pooled builder after a large stored-only document. The old
	// estimate reserved roughly 25 MiB for each following 256 KiB document.
	for i := 0; i < 8; i++ {
		seg, _, err := New([]*blugeidx.Document{doc})
		if err != nil {
			t.Fatal(err)
		}
		if seg.Size() > 2*1024*1024 {
			t.Fatalf("one 256 KiB document retained %d bytes", seg.Size())
		}
		var stored []byte
		err = seg.(*SegmentBase).VisitStoredFields(0, func(name string, _ byte, value []byte, _ []uint64) bool {
			if name == "body" {
				stored = append([]byte(nil), value...)
			}
			return true
		})
		if err != nil || !bytes.Equal(stored, data) {
			t.Fatalf("stored content: %v", err)
		}
		if err := seg.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

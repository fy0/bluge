package zapxbluge

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	bleveindex "github.com/blevesearch/bleve_index_api"

	"github.com/fy0/bluge/analysis"
	"github.com/fy0/bluge/internal/blugeidx"
	blugeseg "github.com/fy0/bluge/segment"
)

type mergeRecordingWriter struct {
	bytes.Buffer
	writes []int
}

func (w *mergeRecordingWriter) Write(data []byte) (int, error) {
	w.writes = append(w.writes, len(data))
	return w.Buffer.Write(data)
}

func TestMergeUsesConfiguredOutputBuffer(t *testing.T) {
	data := make([]byte, 512)
	_, _ = rand.New(rand.NewSource(1)).Read(data)
	idFrequencies, _ := analysis.TokenFrequency(analysis.TokenStream{
		&analysis.Token{Term: []byte("one"), PositionIncr: 1},
	}, false, 0)
	native, err := blugeidx.NewDocument([]*blugeidx.Field{
		blugeidx.NewField("_id", []byte("one"), bleveindex.IndexField|bleveindex.StoreField,
			1, idFrequencies, 3),
		blugeidx.NewField("body", data, bleveindex.StoreField, 0, nil, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	segment, _, err := New([]blugeseg.Document{&exporterTestDocument{native: native}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	segments := []blugeseg.Segment{segment, segment}
	drops := make([]*roaring.Bitmap, len(segments))
	for _, withOptions := range []bool{false, true} {
		for _, size := range []int{0, 64} {
			merger := Merge(segments, drops, size)
			if withOptions {
				merger = MergeWithOptions(segments, drops, size, nil)
			}
			output := &mergeRecordingWriter{}
			written, err := merger.WriteTo(output, nil)
			if err != nil || written != int64(output.Len()) {
				t.Fatalf("WriteTo: bytes=%d, output=%d, error=%v", written, output.Len(), err)
			}
			if size == 0 {
				if len(output.writes) != 1 {
					t.Fatalf("default buffer: got writes %v, want one write", output.writes)
				}
			} else if !slices.Contains(output.writes, size) {
				t.Fatalf("buffer %d was not used (options=%v): writes %v", size, withOptions, output.writes)
			}
			// Sections may be emitted in different orders; verify decoded contents.
			merged, err := Load(blugeseg.NewDataBytes(output.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if merged.Count() != 2 {
				t.Fatalf("merged %d documents, want 2", merged.Count())
			}
			for docNum := uint64(0); docNum < merged.Count(); docNum++ {
				var body []byte
				err := merged.VisitStoredFields(docNum, func(field string, value []byte) bool {
					if field == "body" {
						body = append([]byte(nil), value...)
					}
					return true
				})
				if err != nil || !bytes.Equal(body, data) {
					t.Fatalf("stored body failed round trip: %v", err)
				}
			}
		}
	}
}

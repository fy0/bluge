package zap

import (
	"bytes"
	"reflect"
	"testing"

	index "github.com/blevesearch/bleve_index_api"

	blugeseg "github.com/fy0/bluge/segment"
)

func TestVectorSectionDoesNotLoadDocValues(t *testing.T) {
	payload := blugeseg.VectorPayload{
		Backend: "test", Dimensions: 2, Similarity: "dot_product",
		DocIDs: []uint32{0}, Data: []byte{4, 5, 6},
	}
	var buffer bytes.Buffer
	buffer.WriteByte(0)
	if err := writeVectorPayload(NewCountHashWriter(&buffer), payload); err != nil {
		t.Fatal(err)
	}
	sb := &SegmentBase{
		mem: buffer.Bytes(), numDocs: 1,
		fieldsMap:         map[string]uint16{"embedding": 1},
		fieldsInv:         []string{"embedding"},
		fieldsOptions:     map[string]index.FieldIndexingOptions{"embedding": index.IndexField},
		fieldsSectionsMap: [][]uint64{{0, 1}},
		fieldDvReaders:    make([][]*docValueReader, NumSections),
	}
	if err := sb.loadDvReaders(); err != nil {
		t.Fatal(err)
	}
	if len(sb.fieldDvNames) != 0 {
		t.Fatalf("unexpected docvalues: %v", sb.fieldDvNames)
	}
	got, err := sb.VectorPayload("embedding")
	if err != nil || !reflect.DeepEqual(got, payload) {
		t.Fatalf("payload: got %+v, error %v", got, err)
	}
	sb.mem = sb.mem[:len(sb.mem)-1]
	if _, err := sb.VectorPayload("embedding"); err == nil {
		t.Fatal("accepted truncated vector payload")
	}
}

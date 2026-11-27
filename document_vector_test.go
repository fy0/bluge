package bluge

import (
	"testing"

	"github.com/fy0/bluge/internal/blugeidx"
	"github.com/fy0/bluge/segment"
)

type customVectorDocument struct {
	segment.Document
}

func TestCustomDocumentPreservesVectorSimilarity(t *testing.T) {
	for _, metric := range []VectorSimilarity{VectorL2, VectorDot} {
		t.Run(string(metric), func(t *testing.T) {
			doc := NewDocument("custom").AddField(
				NewVectorFieldWithSimilarity("embedding", []float32{2, 3}, metric))
			doc.Analyze()
			converted, err := blugeidx.FromSegmentDocument(customVectorDocument{doc})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			converted.VisitVectors(func(vector *blugeidx.Vector) {
				count++
				if vector.Name() != "embedding" || vector.Similarity() != string(metric) {
					t.Fatalf("vector field: %s %s", vector.Name(), vector.Similarity())
				}
				if values := vector.Values(); len(values) != 2 || values[0] != 2 || values[1] != 3 {
					t.Fatalf("vector values: %v", values)
				}
			})
			if count != 1 {
				t.Fatalf("vector count: %d", count)
			}
		})
	}
}

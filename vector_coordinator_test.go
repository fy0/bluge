package bluge

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestOfflineVectorSchemaRejectedBeforeText(t *testing.T) {
	for _, metric := range []VectorSimilarity{VectorCosine, VectorDot} {
		t.Run(string(metric), func(t *testing.T) {
			root := t.TempDir()
			config := DefaultConfig(filepath.Join(root, "text")).
				WithVectorBackend(NewFlatVectorBackend(filepath.Join(root, "vectors")))
			writer, err := OpenOfflineWriter(config, 1, 2)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Insert(NewDocument("valid").AddField(
				NewVectorField("embedding", []float32{1, 0}))); err != nil {
				t.Fatal(err)
			}
			values := []float32{1, 0, 0}
			wantErr := ErrVectorInvalidDimension
			if metric == VectorDot {
				values, wantErr = []float32{1, 0}, ErrVectorInvalidField
			}
			err = writer.Insert(NewDocument("invalid").AddField(
				NewVectorFieldWithSimilarity("embedding", values, metric)))
			if !errors.Is(err, wantErr) {
				t.Fatalf("insert: %v", err)
			}
			if err := writer.Close(); !errors.Is(err, wantErr) {
				t.Fatalf("close: %v", err)
			}
			reader, err := OpenReader(config)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if count, err := reader.Count(); err != nil || count != 1 {
				t.Fatalf("text count: %d, %v", count, err)
			}
		})
	}
}

//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Corpus scale for the embedded backend: how large the index is, how long it
// takes to build, and how the 1024-dimension shape searched. The other
// benchmarks use 64 dimensions and tiny stored documents, which makes them a
// poor guide to size and build time, so these are kept separate and opt-in.

const scaleTestEnv = "BLUGE_USEARCH_SCALE_TEST"

// BenchmarkEmbeddedVectorHighDimension searches a 1024-dimension, 26-segment
// corpus, which is the shape the application uses. It is deliberately not part
// of BenchmarkEmbeddedVectorSearch: building the corpus takes about 30 seconds
// per process, and the single-segment variant takes minutes.
//
//	BLUGE_USEARCH_LIBRARY_PATH=... go test -run '^$' \
//	  -bench '^BenchmarkEmbeddedVectorHighDimension$' -benchtime 50x -count 3 .
func BenchmarkEmbeddedVectorHighDimension(b *testing.B) {
	corpus := buildEmbeddedCorpus(b, embeddedCorpusSpec{
		Documents:   27000,
		SegmentSize: 27000 / 26,
		Dimensions:  1024,
	})
	defer corpus.Close()
	query := benchQuery(1024)
	for i := 0; i < embeddedWarmupRuns; i++ {
		if _, err := corpus.search(512, query, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := corpus.search(512, query, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// TestEmbeddedCorpusScale reports index size and build time per corpus shape.
// It is skipped unless BLUGE_USEARCH_SCALE_TEST is set, because the 1024
// dimension single-segment case alone takes several minutes.
//
//	BLUGE_USEARCH_LIBRARY_PATH=... BLUGE_USEARCH_SCALE_TEST=1 \
//	  go test -run '^TestEmbeddedCorpusScale$' -v .
func TestEmbeddedCorpusScale(t *testing.T) {
	if os.Getenv(scaleTestEnv) == "" {
		t.Skipf("set %s to measure index size and build time", scaleTestEnv)
	}
	cases := []struct {
		name       string
		documents  int
		segments   int
		dimensions int
	}{
		{"27k/1seg/d64", 27000, 1, 64},
		{"27k/26seg/d64", 27000, 26, 64},
		{"41k/26seg/d64", 41378, 26, 64},
		{"5k/1seg/d1024", 5000, 1, 1024},
		{"27k/1seg/d1024", 27000, 1, 1024},
		{"27k/26seg/d1024", 27000, 26, 1024},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			spec := embeddedCorpusSpec{
				Documents:   c.documents,
				SegmentSize: (c.documents + c.segments - 1) / c.segments,
				Dimensions:  c.dimensions,
			}
			start := time.Now()
			corpus := buildEmbeddedCorpus(t, spec)
			build := time.Since(start)
			defer corpus.Close()

			// The corpus builds its index under this test's temp root.
			root := filepath.Dir(t.TempDir())
			var total int64
			err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				total += info.Size()
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			segments := corpus.embeddedIndex(t).fields[embeddedBenchField]
			var vectorBytes int
			for _, segment := range segments {
				vectorBytes += len(segment.payload.Data)
			}
			storedText := len(string(embeddedDocID(0))) + len("g0") + len("chunk body g0")
			t.Logf("%s: build=%v index=%dKB (%.0f B/doc) vector payload=%dKB (%.0f B/vector) stored text=%dB/doc segments=%d",
				c.name, build.Round(time.Millisecond), total/1024,
				float64(total)/float64(c.documents),
				vectorBytes/1024,
				float64(vectorBytes)/float64(c.documents),
				storedText, len(segments))
		})
	}
}

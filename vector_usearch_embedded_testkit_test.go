//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/fy0/bluge/index/mergeplan"
)

// This file builds deterministic synthetic corpora for the embedded USearch
// backend so the search hot path can be measured and verified without network
// embedding calls or a real source corpus.

const embeddedBenchField = "embedding"

// embeddedTestLibrary returns the native library path or skips the test.
func embeddedTestLibrary(tb testing.TB) string {
	tb.Helper()
	libraryPath := os.Getenv(usearchLibraryPathEnv)
	if libraryPath == "" {
		tb.Skip("set BLUGE_USEARCH_LIBRARY_PATH to run embedded USearch tests")
	}
	if _, err := os.Stat(libraryPath); err != nil {
		tb.Fatalf("USearch native library is unavailable: %v", err)
	}
	return libraryPath
}

// embeddedCorpusSpec describes a synthetic index. Segments is enforced by
// disabling the merge budget, so every SegmentSize documents become exactly one
// segment.
type embeddedCorpusSpec struct {
	Documents   int
	SegmentSize int
	Dimensions  int
	// VectorOf returns the vector for document i. Deterministic callers keep
	// benchmarks and assertions reproducible.
	VectorOf func(i int) []float32
	// GroupOf returns the filter group for document i.
	GroupOf func(i int) string
}

func (s embeddedCorpusSpec) segments() int {
	if s.SegmentSize <= 0 {
		return 1
	}
	segments := (s.Documents + s.SegmentSize - 1) / s.SegmentSize
	if segments < 1 {
		return 1
	}
	return segments
}

type embeddedCorpus struct {
	reader   *Reader
	config   Config
	spec     embeddedCorpusSpec
	segments int
}

func (c *embeddedCorpus) Close() {
	if c != nil && c.reader != nil {
		_ = c.reader.Close()
		c.reader = nil
	}
}

func embeddedDocID(i int) Identifier { return Identifier(fmt.Sprintf("doc-%06d", i)) }

// deterministicVector fills out with values in [-1, 1) derived from a 64-bit
// LCG. The generator is written out rather than taken from math/rand so the
// corpus is stable across Go releases.
func deterministicVector(seed uint64, out []float32) []float32 {
	state := seed*6364136223846793005 + 1442695040888963407
	for i := range out {
		state = state*6364136223846793005 + 1442695040888963407
		out[i] = float32(int64(state>>11))/float32(int64(1)<<52) - 1
	}
	return out
}

// quantizedVector returns one of alphabet distinct vectors. Documents sharing a
// vector produce bit-identical distances, which is how the tie corpora are
// built.
func quantizedVector(i, alphabet, dimensions int) []float32 {
	vector := make([]float32, dimensions)
	slot := i % alphabet
	if slot < dimensions {
		vector[slot] = 1
		return vector
	}
	vector[slot%dimensions] = 1
	vector[(slot+1)%dimensions] = 1
	return vector
}

// buildEmbeddedCorpus writes a real on-disk index with the requested segment
// count. Merges are disabled through the merge budget so the segment count is
// exact and reported back to the caller.
func buildEmbeddedCorpus(tb testing.TB, spec embeddedCorpusSpec) *embeddedCorpus {
	tb.Helper()
	libraryPath := embeddedTestLibrary(tb)
	if spec.Documents <= 0 || spec.Dimensions <= 0 {
		tb.Fatalf("invalid corpus spec: %+v", spec)
	}
	if spec.VectorOf == nil {
		dims := spec.Dimensions
		spec.VectorOf = func(i int) []float32 {
			return deterministicVector(uint64(i)+1, make([]float32, dims))
		}
	}
	if spec.GroupOf == nil {
		spec.GroupOf = func(i int) string { return fmt.Sprintf("g%d", i%4) }
	}
	if spec.SegmentSize <= 0 || spec.SegmentSize > spec.Documents {
		spec.SegmentSize = spec.Documents
	}

	root := tb.TempDir()
	config := DefaultConfig(filepath.Join(root, "index")).
		WithVectorBackend(NewEmbeddedUSearchVectorBackend().WithLibraryPath(libraryPath))
	// A merge budget of zero would merge every eligible segment into one, so
	// the budget is raised out of reach instead. This keeps the segment count
	// exactly equal to ceil(Documents / SegmentSize).
	config.indexConfig.MergePlanOptions.CalcBudget = func(int64, int64, *mergeplan.Options) int {
		return math.MaxInt
	}

	writer, err := OpenWriter(config)
	if err != nil {
		tb.Fatalf("open writer: %v", err)
	}
	for start := 0; start < spec.Documents; start += spec.SegmentSize {
		end := start + spec.SegmentSize
		if end > spec.Documents {
			end = spec.Documents
		}
		documents := make([]*Document, 0, end-start)
		for i := start; i < end; i++ {
			documents = append(documents, NewDocument(string(embeddedDocID(i))).
				AddField(NewKeywordField("group", spec.GroupOf(i))).
				AddField(NewTextField("body", "chunk body "+spec.GroupOf(i))).
				AddField(NewVectorField(embeddedBenchField, spec.VectorOf(i))))
		}
		// One batch per segment: the writer persists exactly one segment per
		// batch and the disabled merge budget keeps them separate.
		if err := writer.InsertMany(documents); err != nil {
			_ = writer.Close()
			tb.Fatalf("insert documents: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		tb.Fatalf("close writer: %v", err)
	}

	reader, err := OpenReader(config)
	if err != nil {
		tb.Fatalf("open reader: %v", err)
	}
	corpus := &embeddedCorpus{
		reader:   reader,
		config:   config,
		spec:     spec,
		segments: len(reader.reader.Segments()),
	}
	if corpus.segments != spec.segments() {
		corpus.Close()
		tb.Fatalf("segment count mismatch: got %d want %d", corpus.segments, spec.segments())
	}
	return corpus
}

// embeddedIndex returns the backend for direct phase-level measurement.
func (c *embeddedCorpus) embeddedIndex(tb testing.TB) *embeddedUSearchVectorIndex {
	tb.Helper()
	vector, ok := c.reader.vector.(*embeddedUSearchVectorIndex)
	if !ok {
		tb.Fatalf("reader is not using the embedded usearch backend: %T", c.reader.vector)
	}
	return vector
}

// benchQuery is the deterministic query vector used by every case.
func benchQuery(dimensions int) []float32 {
	return deterministicVector(0x9e3779b97f4a7c15, make([]float32, dimensions))
}

func assertSameHits(tb testing.TB, label string, got, want []VectorHit) {
	tb.Helper()
	if len(got) != len(want) {
		tb.Fatalf("%s: result length differs: got %d want %d\ngot  %v\nwant %v",
			label, len(got), len(want), vectorIDs(got), vectorIDs(want))
	}
	for i := range got {
		if got[i] != want[i] {
			tb.Fatalf("%s: result %d differs: got %+v want %+v (order %v vs %v)",
				label, i, got[i], want[i], vectorIDs(got), vectorIDs(want))
		}
	}
}

// smallSearchCorpus is shared by the equivalence tests. It is small enough to
// scan exhaustively and large enough to span several segments.
func smallSearchCorpus(tb testing.TB, documents, segments int, quantized int) *embeddedCorpus {
	tb.Helper()
	dimensions := 16
	spec := embeddedCorpusSpec{
		Documents:   documents,
		SegmentSize: documents / segments,
		Dimensions:  dimensions,
	}
	if quantized > 0 {
		spec.VectorOf = func(i int) []float32 { return quantizedVector(i, quantized, dimensions) }
	}
	return buildEmbeddedCorpus(tb, spec)
}

// groupFilter returns a filter matching one synthetic group.
func groupFilter(group string) Query {
	return NewTermQuery(group).SetField("group")
}

// vectorIDs extracts the identifiers in result order.
func vectorIDs(hits []VectorHit) []Identifier {
	ids := make([]Identifier, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

// searchEmbedded runs a vector search on the corpus reader.
func (c *embeddedCorpus) search(k int, query []float32, filter Query) ([]VectorHit, error) {
	return c.reader.VectorSearch(context.Background(), embeddedBenchField, query, k, filter)
}

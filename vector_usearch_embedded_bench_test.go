//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"context"
	"fmt"
	"testing"
)

// Benchmarks for the embedded USearch search hot path.
//
// The corpora are synthetic and deterministic: no network embedding calls and
// no real source corpus. Segment counts are exact because the merge budget is
// disabled when the index is built.
//
// Run with the counters enabled to see how many stored _id reads a search
// performs:
//
//	BLUGE_USEARCH_LIBRARY_PATH=/path/vector_engine.dll \
//	  go test -tags blugevectorstats -run '^$' -bench Embedded -benchtime 20x .

type embeddedBenchCase struct {
	name        string
	documents   int
	segments    int
	dimensions  int
	quantized   int // when > 0, vectors come from a small alphabet to force ties
	description string
}

var embeddedBenchCases = []embeddedBenchCase{
	{name: "27k/1seg", documents: 27000, segments: 1, dimensions: 64,
		description: "single segment, 27k vectors"},
	{name: "27k/8seg", documents: 27000, segments: 8, dimensions: 64,
		description: "8 segments, 27k vectors"},
	{name: "27k/26seg", documents: 27000, segments: 26, dimensions: 64,
		description: "26 segments, 27k vectors"},
	{name: "41k/26seg", documents: 41378, segments: 26, dimensions: 64,
		description: "26 segments, 41378 vectors"},
	{name: "27k/26seg-ties", documents: 27000, segments: 26, dimensions: 64, quantized: 8,
		description: "26 segments, heavy score ties at the K boundary"},
}

func embeddedBenchSpec(c embeddedBenchCase) embeddedCorpusSpec {
	dims := c.dimensions
	spec := embeddedCorpusSpec{
		Documents:   c.documents,
		SegmentSize: (c.documents + c.segments - 1) / c.segments,
		Dimensions:  dims,
	}
	if c.quantized > 0 {
		spec.VectorOf = func(i int) []float32 { return quantizedVector(i, c.quantized, dims) }
	}
	return spec
}

func BenchmarkEmbeddedVectorSearch(b *testing.B) {
	for _, c := range embeddedBenchCases {
		c := c
		b.Run(c.name, func(b *testing.B) {
			corpus := buildEmbeddedCorpus(b, embeddedBenchSpec(c))
			defer corpus.Close()
			b.Logf("%s: %d documents in %d segments", c.description,
				c.documents, corpus.segments)
			query := benchQuery(c.dimensions)

			for _, k := range []int{192, 512} {
				k := k
				b.Run(fmt.Sprintf("k=%d", k), func(b *testing.B) {
					b.Run("nofilter", func(b *testing.B) {
						benchSearch(b, corpus, k, query, nil)
					})
					b.Run("docnum-filter", func(b *testing.B) {
						benchSearch(b, corpus, k, query, groupFilter("g1"))
					})
					b.Run("id-filter", func(b *testing.B) {
						benchIDSearcher(b, corpus, k, query, "g1")
					})
				})
			}
		})
	}
}

// embeddedWarmupRuns are executed before the timer starts so the measured
// iterations do not include first-touch page-cache and allocator warmup.
const embeddedWarmupRuns = 5

func benchSearch(b *testing.B, corpus *embeddedCorpus, k int, query []float32, filter Query) {
	b.Helper()
	for i := 0; i < embeddedWarmupRuns; i++ {
		if _, err := corpus.search(k, query, filter); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, err := corpus.search(k, query, filter)
		if err != nil {
			b.Fatal(err)
		}
		if len(hits) == 0 {
			b.Fatal("search returned no hits")
		}
	}
}

// benchIDSearcher measures the exported SearchCandidates path, which filters by
// stored identifier instead of by document number.
func benchIDSearcher(b *testing.B, corpus *embeddedCorpus, k int, query []float32, group string) {
	b.Helper()
	vector := corpus.embeddedIndex(b)
	allowed, err := corpus.reader.vectorFilterIDs(context.Background(), groupFilter(group))
	if err != nil {
		b.Fatal(err)
	}
	if len(allowed) == 0 {
		b.Fatal("identifier filter matched no documents")
	}
	for i := 0; i < embeddedWarmupRuns; i++ {
		if _, err := vector.SearchCandidates(embeddedBenchField, query, k, allowed); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, err := vector.SearchCandidates(embeddedBenchField, query, k, allowed)
		if err != nil {
			b.Fatal(err)
		}
		if len(hits) == 0 {
			b.Fatal("search returned no hits")
		}
	}
}

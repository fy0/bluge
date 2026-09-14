//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

// Phase-level benchmarks for the embedded USearch search hot path.
//
// These benchmarks reach into the current implementation (rankCandidates,
// collectCandidates, the per-segment native API), so unlike
// vector_usearch_embedded_bench_test.go they do not compile against earlier
// revisions of vector_usearch_embedded.go. Keep them separate so an A/B run
// against a stashed baseline can still build and run the end-to-end
// benchmarks; see docs/run-embedded-bench.sh.

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"testing"
)

func BenchmarkEmbeddedVectorPhases(b *testing.B) {
	cases := []embeddedBenchCase{
		{name: "27k/1seg", documents: 27000, segments: 1, dimensions: 64},
		{name: "27k/26seg", documents: 27000, segments: 26, dimensions: 64},
	}
	for _, c := range cases {
		c := c
		b.Run(c.name, func(b *testing.B) {
			corpus := buildEmbeddedCorpus(b, embeddedBenchSpec(c))
			defer corpus.Close()
			vector := corpus.embeddedIndex(b)
			segments := vector.fields[embeddedBenchField]
			query := benchQuery(c.dimensions)

			for _, k := range []int{192, 512} {
				k := k
				b.Run(fmt.Sprintf("native-ann/k=%d", k), func(b *testing.B) {
					benchNativeSearch(b, segments, query, k, false)
				})
				b.Run(fmt.Sprintf("native-ann-filtered/k=%d", k), func(b *testing.B) {
					benchNativeSearch(b, segments, query, k, true)
				})
				b.Run(fmt.Sprintf("stored-id-reads/k=%d", k), func(b *testing.B) {
					benchStoredIDReads(b, corpus, segments, query, k, false)
				})
				b.Run(fmt.Sprintf("stored-id-reads-sorted/k=%d", k), func(b *testing.B) {
					benchStoredIDReads(b, corpus, segments, query, k, true)
				})
				b.Run(fmt.Sprintf("rank-candidates/k=%d", k), func(b *testing.B) {
					benchCandidateRank(b, vector, len(segments), k)
				})
				b.Run(fmt.Sprintf("rank-previous/k=%d", k), func(b *testing.B) {
					benchPreviousRank(b, len(segments), k)
				})
			}
		})
	}
}

// benchNativeSearch measures only the native ANN call for every segment.
func benchNativeSearch(b *testing.B, segments []embeddedUSearchSegment, query []float32,
	k int, filtered bool) {
	b.Helper()
	keys := make([]uint64, k)
	distances := make([]float32, k)
	var allowed []uint64
	if filtered {
		allowed = make([]uint64, 0, k)
		for i := 0; i < k; i++ {
			allowed = append(allowed, uint64(i+1))
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, segment := range segments {
			var status int32
			if filtered {
				status, _ = segment.api.searchFiltered(segment.handle, query, k, allowed, keys, distances)
			} else {
				status, _ = segment.api.search(segment.handle, query, k, keys, distances)
			}
			if status != 0 {
				b.Fatalf("native search failed with status %d", status)
			}
		}
	}
}

// benchStoredIDReads measures resolving the stored identifier of every
// candidate a search considers, in native result order (unsorted) or in
// document order (sorted). USearch returns hits by distance, so the unsorted
// order is effectively random with respect to stored blocks.
func benchStoredIDReads(b *testing.B, corpus *embeddedCorpus,
	segments []embeddedUSearchSegment, query []float32, k int, sorted bool) {
	b.Helper()
	docs := collectCandidateDocs(b, segments, query, k)
	if sorted {
		sort.Slice(docs, func(i, j int) bool { return docs[i] < docs[j] })
	}
	snapshot := corpus.reader.reader
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, doc := range docs {
			if _, err := embeddedDocumentID(snapshot, doc); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(len(docs)), "ids/op")
}

// collectCandidateDocs replays the per-segment native search and returns the
// global document numbers a search would have to resolve.
func collectCandidateDocs(b *testing.B, segments []embeddedUSearchSegment,
	query []float32, k int) []uint64 {
	b.Helper()
	var docs []uint64
	keys := make([]uint64, k)
	distances := make([]float32, k)
	for _, segment := range segments {
		status, count := segment.api.search(segment.handle, query, k, keys, distances)
		if status != 0 {
			b.Fatalf("native search failed with status %d", status)
		}
		for i := 0; i < count; i++ {
			localDoc := segment.payload.DocIDs[keys[i]-1]
			docs = append(docs, segment.offset+uint64(localDoc))
		}
	}
	return docs
}

// benchCandidateRank measures the production ranking step - rankCandidates -
// with the identifiers already resolved, which is what the ranking itself costs
// after the deferred-identifier change.
//
// The input mimics what a real search hands over: one already-descending run
// of scores per segment, with the runs overlapping each other and a tie group
// sitting on the global K boundary. Every iteration copies the full candidate
// list, so no iteration can be short-circuited by the previous one.
func benchCandidateRank(b *testing.B, index *embeddedUSearchVectorIndex,
	segments, k int) {
	b.Helper()
	source := mergedCandidates(segments, k)
	work := make([]embeddedVectorCandidate, len(source))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(work, source)
		if _, err := index.rankCandidates(work, k); err != nil {
			b.Fatal(err)
		}
	}
}

// benchPreviousRank measures the ranking the implementation used before the
// deferred-identifier change: resolve every candidate, sort the whole
// []VectorHit with sort.SliceStable, then truncate to k. It runs on the same
// candidate multiset as benchCandidateRank so the two are comparable.
func benchPreviousRank(b *testing.B, segments, k int) {
	b.Helper()
	work := make([]VectorHit, 0, segments*k)
	previous := previousImplementationHits(segments, k)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		work = append(work[:0], previous...)
		sort.SliceStable(work, func(i, j int) bool {
			if work[i].Score == work[j].Score {
				return work[i].ID < work[j].ID
			}
			return work[i].Score > work[j].Score
		})
		truncated := work
		if len(truncated) > k {
			truncated = truncated[:k]
		}
		_ = truncated
	}
}

// previousImplementationHits is the []VectorHit form of mergedCandidates, which
// is what the previous implementation ranked.
func previousImplementationHits(segments, k int) []VectorHit {
	candidates := mergedCandidates(segments, k)
	hits := make([]VectorHit, 0, len(candidates))
	for _, candidate := range candidates {
		hits = append(hits, VectorHit{ID: candidate.id, Score: candidate.score})
	}
	return hits
}

// mergedCandidates builds the candidate list a multi-segment search hands to
// the ranking step: one run of k candidates per segment, each run descending
// because the native search returns hits by distance. The runs overlap, so the
// concatenation is not globally sorted, and the scores on the global K
// boundary collide so the tie group is exercised.
func mergedCandidates(segments, k int) []embeddedVectorCandidate {
	total := segments * k
	if total == 0 {
		return nil
	}
	ordered := make([]float64, total)
	state := uint64(0x243f6a8885a308d3)
	for i := range ordered {
		state = state*6364136223846793005 + 1442695040888963407
		ordered[i] = float64(int64(state>>11)) / float64(int64(1)<<52)
	}
	slices.SortFunc(ordered, func(a, b float64) int { return cmp.Compare(b, a) })
	if total > k {
		// Force the global K boundary to be a tie group instead of a single
		// score, the shape a corpus with duplicate vectors produces.
		cutoff := ordered[k-1]
		for i := k - 1; i < total && i < k-1+k/8; i++ {
			ordered[i] = cutoff
		}
	}
	runs := make([][]embeddedVectorCandidate, segments)
	for i, score := range ordered {
		segment := i % segments
		runs[segment] = append(runs[segment], embeddedVectorCandidate{
			globalDoc: uint64(i),
			score:     score,
			id:        Identifier(fmt.Sprintf("doc-%06d", i)),
			idSet:     true,
		})
	}
	candidates := make([]embeddedVectorCandidate, 0, total)
	for _, run := range runs {
		candidates = append(candidates, run...)
	}
	return candidates
}

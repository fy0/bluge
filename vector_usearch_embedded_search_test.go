//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
)

// referenceEmbeddedSearch is a copy of the search implementation as it existed
// before candidate identifiers were resolved lazily. It reads the stored
// identifier of every candidate, then orders and truncates the whole list. It
// is the semantic oracle the optimized implementation must match.
func referenceEmbeddedSearch(tb testing.TB, e *embeddedUSearchVectorIndex, field string,
	query []float32, k int, allowedIDs map[Identifier]struct{}, allowedDocs []uint64) []VectorHit {
	tb.Helper()
	segments := e.fields[field]
	hits := make([]VectorHit, 0, len(segments)*k)
	for _, segment := range segments {
		count := k
		if allowedIDs != nil {
			count = len(segment.payload.DocIDs)
		}
		if count == 0 {
			continue
		}
		allowedKeys, filtered := segment.allowedKeys(allowedDocs)
		if filtered && len(allowedKeys) == 0 {
			continue
		}
		keys := make([]uint64, count)
		distances := make([]float32, count)
		var status int32
		var resultCount int
		if filtered {
			status, resultCount = segment.api.searchFiltered(segment.handle, query, count,
				allowedKeys, keys, distances)
		} else {
			status, resultCount = segment.api.search(segment.handle, query, count, keys, distances)
		}
		if status != 0 {
			tb.Fatalf("reference search failed with status %d", status)
		}
		for i := 0; i < resultCount; i++ {
			if keys[i] == 0 || keys[i]-1 >= uint64(len(segment.payload.DocIDs)) {
				tb.Fatalf("reference search returned invalid key %d", keys[i])
			}
			localDoc := segment.payload.DocIDs[keys[i]-1]
			if segment.deleted != nil && segment.deleted.Contains(localDoc) {
				continue
			}
			id, err := embeddedDocumentID(e.snapshot, segment.offset+uint64(localDoc))
			if err != nil {
				tb.Fatal(err)
			}
			if allowedIDs != nil {
				if _, ok := allowedIDs[id]; !ok {
					continue
				}
			}
			hits = append(hits, VectorHit{
				ID:    id,
				Score: usearchScore(VectorSimilarity(segment.payload.Similarity), float64(distances[i])),
			})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// groundTruthScores scans every vector in every segment so the exact global
// ranking can be computed independently of the per-segment top-k retrieval.
func groundTruthScores(tb testing.TB, e *embeddedUSearchVectorIndex, field string,
	query []float32) map[uint64]float64 {
	tb.Helper()
	scores := make(map[uint64]float64)
	for _, segment := range e.fields[field] {
		size := len(segment.payload.DocIDs)
		if size == 0 {
			continue
		}
		keys := make([]uint64, size)
		distances := make([]float32, size)
		status, count := segment.api.search(segment.handle, query, size, keys, distances)
		if status != 0 {
			tb.Fatalf("ground truth scan failed with status %d", status)
		}
		for i := 0; i < count; i++ {
			localDoc := segment.payload.DocIDs[keys[i]-1]
			if segment.deleted != nil && segment.deleted.Contains(localDoc) {
				continue
			}
			scores[segment.offset+uint64(localDoc)] =
				usearchScore(VectorSimilarity(segment.payload.Similarity), float64(distances[i]))
		}
	}
	return scores
}

// groundTruthHits turns the full score map into the documented ordering.
func groundTruthHits(tb testing.TB, corpus *embeddedCorpus, scores map[uint64]float64) []VectorHit {
	tb.Helper()
	hits := make([]VectorHit, 0, len(scores))
	for doc, score := range scores {
		id, err := embeddedDocumentID(corpus.reader.reader, doc)
		if err != nil {
			tb.Fatal(err)
		}
		hits = append(hits, VectorHit{ID: id, Score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	return hits
}

func assertOrdering(tb testing.TB, label string, hits []VectorHit) {
	tb.Helper()
	for i := 1; i < len(hits); i++ {
		previous, current := hits[i-1], hits[i]
		if current.Score > previous.Score {
			tb.Fatalf("%s: score increased at %d: %v then %v", label, i, previous, current)
		}
		if current.Score == previous.Score && current.ID < previous.ID {
			tb.Fatalf("%s: identifier order violated at %d: %v then %v", label, i, previous, current)
		}
	}
}

func TestEmbeddedSearchMatchesReferenceImplementation(t *testing.T) {
	corpus := smallSearchCorpus(t, 800, 4, 0)
	defer corpus.Close()
	index := corpus.embeddedIndex(t)
	query := benchQuery(16)

	allowedIDs, err := corpus.reader.vectorFilterIDs(context.Background(), groupFilter("g1"))
	if err != nil {
		t.Fatal(err)
	}
	allowedDocs, err := corpus.reader.vectorFilterDocumentNumbers(context.Background(), groupFilter("g1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(allowedIDs) == 0 || len(allowedDocs) == 0 {
		t.Fatal("filter matched no documents")
	}

	for _, k := range []int{1, 5, 192, 512, 800, 5000} {
		k := k
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			got, err := corpus.search(k, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertSameHits(t, "no filter",
				got, referenceEmbeddedSearch(t, index, embeddedBenchField, query, k, nil, nil))
			assertOrdering(t, "no filter", got)

			got, err = corpus.search(k, query, groupFilter("g1"))
			if err != nil {
				t.Fatal(err)
			}
			assertSameHits(t, "document filter",
				got, referenceEmbeddedSearch(t, index, embeddedBenchField, query, k, nil, allowedDocs))
			assertOrdering(t, "document filter", got)

			got, err = index.SearchCandidates(embeddedBenchField, query, k, allowedIDs)
			if err != nil {
				t.Fatal(err)
			}
			assertSameHits(t, "identifier filter",
				got, referenceEmbeddedSearch(t, index, embeddedBenchField, query, k, allowedIDs, nil))
			assertOrdering(t, "identifier filter", got)
		})
	}
}

// TestEmbeddedSearchMatchesExhaustiveGroundTruth checks the optimized path
// against a full scan of every vector, which is independent of the per-segment
// top-k retrieval. Scores in this corpus are distinct, so the global ordering
// is unambiguous.
func TestEmbeddedSearchMatchesExhaustiveGroundTruth(t *testing.T) {
	corpus := smallSearchCorpus(t, 800, 4, 0)
	defer corpus.Close()
	index := corpus.embeddedIndex(t)
	query := benchQuery(16)

	scores := groundTruthScores(t, index, embeddedBenchField, query)
	if len(scores) != 800 {
		t.Fatalf("ground truth covered %d documents, want 800", len(scores))
	}

	ordered := groundTruthHits(t, corpus, scores)
	for _, k := range []int{1, 3, 200, 512, 799} {
		k := k
		want := ordered
		if k < len(want) {
			if want[k-1].Score == want[k].Score {
				t.Fatalf("k=%d: this corpus is not tie-free at the boundary, so the "+
					"per-segment top-k cannot reproduce the exact global order", k)
			}
			want = want[:k]
		}
		got, err := corpus.search(k, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertSameHits(t, fmt.Sprintf("ground truth k=%d", k), got, want)
	}
}

// TestEmbeddedSearchTieBreaking covers heavy score ties at the global K
// boundary, where the identifier decides which candidates survive.
func TestEmbeddedSearchTieBreaking(t *testing.T) {
	corpus := smallSearchCorpus(t, 800, 4, 4)
	defer corpus.Close()
	index := corpus.embeddedIndex(t)
	query := benchQuery(16)

	allowedDocs, err := corpus.reader.vectorFilterDocumentNumbers(context.Background(), groupFilter("g2"))
	if err != nil {
		t.Fatal(err)
	}

	// Every quantized vector is orthogonal to the others, so all documents in
	// the same group share one exact score and ties dominate the boundary.
	for _, k := range []int{1, 7, 200, 201, 512} {
		k := k
		got, err := corpus.search(k, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertOrdering(t, "ties", got)
		assertSameHits(t, fmt.Sprintf("ties k=%d", k),
			got, referenceEmbeddedSearch(t, index, embeddedBenchField, query, k, nil, nil))
		for i := 1; i < len(got); i++ {
			if got[i-1].Score < got[i].Score {
				t.Fatalf("k=%d: not sorted by score", k)
			}
		}
		// The corpus must actually produce ties, otherwise this test would not
		// exercise the boundary tie group at all.
		counts := map[float64]int{}
		for _, hit := range got {
			counts[hit.Score]++
		}
		tied := false
		for _, count := range counts {
			if count > 1 {
				tied = true
			}
		}
		if len(got) > 1 && !tied {
			t.Fatalf("k=%d: expected shared scores in a tie-dominated corpus: %v", k, counts)
		}

		got, err = corpus.search(k, query, groupFilter("g2"))
		if err != nil {
			t.Fatal(err)
		}
		assertOrdering(t, "ties filtered", got)
		assertSameHits(t, fmt.Sprintf("ties filtered k=%d", k),
			got, referenceEmbeddedSearch(t, index, embeddedBenchField, query, k, nil, allowedDocs))
	}
}

// TestEmbeddedSearchBestResultsInOneSegment covers the case a naive
// "k divided by segment count" split would break: all of the best vectors live
// in a single segment.
func TestEmbeddedSearchBestResultsInOneSegment(t *testing.T) {
	dimensions := 16
	documents := 800
	segmentSize := 200
	// Documents 200..399 fill the second segment and match the query exactly.
	bestStart, bestEnd := 200, 400
	spec := embeddedCorpusSpec{
		Documents:   documents,
		SegmentSize: segmentSize,
		Dimensions:  dimensions,
		VectorOf: func(i int) []float32 {
			vector := make([]float32, dimensions)
			if i >= bestStart && i < bestEnd {
				vector[0] = 1
				return vector
			}
			// Everything else is orthogonal to the query direction.
			slot := 1 + i%(dimensions-1)
			vector[slot] = 1
			return vector
		},
	}
	corpus := buildEmbeddedCorpus(t, spec)
	defer corpus.Close()
	if corpus.segments != 4 {
		t.Fatalf("expected 4 segments, got %d", corpus.segments)
	}
	query := make([]float32, dimensions)
	query[0] = 1

	// Every document in the best segment scores 1 and every other document
	// scores 0, so every best result must come from that one segment. The
	// native search picks arbitrarily among its own ties and can even return
	// fewer hits than requested when a segment is full of identical vectors,
	// so the assertions below cover membership and scores rather than a
	// specific identifier order inside the tie. The top score is compared
	// with a tolerance because the SIMD distance kernels leave residual
	// floating-point noise (about 2e-16 on arm64 NEON).
	for _, k := range []int{1, 50, 192, 512} {
		k := k
		hits, err := corpus.search(k, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertOrdering(t, "concentrated", hits)
		if len(hits) != k {
			t.Fatalf("k=%d: got %d hits, want %d", k, len(hits), k)
		}
		best := 0
		for i, hit := range hits {
			number := documentNumberOf(t, hit.ID)
			inBestSegment := number >= bestStart && number < bestEnd
			if hit.Score >= 1-1e-6 {
				best++
				if !inBestSegment {
					t.Fatalf("k=%d: hit %d is %q with the top score but lives outside the "+
						"segment holding the best vectors", k, i, hit.ID)
				}
				continue
			}
			if inBestSegment {
				t.Fatalf("k=%d: hit %d is %q from the best segment with score %v",
					k, i, hit.ID, hit.Score)
			}
		}
		// A "k divided by segment count" split would cap this segment at
		// k/len(segments) results.
		if minimumBest := k / corpus.segments; best <= minimumBest {
			t.Fatalf("k=%d: only %d results came from the best segment, at most as many "+
				"as an even per-segment split would give (%d)", k, best, minimumBest)
		}
		assertSameHits(t, fmt.Sprintf("concentrated k=%d", k),
			hits, referenceEmbeddedSearch(t, corpus.embeddedIndex(t),
				embeddedBenchField, query, k, nil, nil))
	}
}

func documentNumberOf(tb testing.TB, id Identifier) int {
	tb.Helper()
	var number int
	if _, err := fmt.Sscanf(string(id), "doc-%06d", &number); err != nil {
		tb.Fatalf("unexpected identifier %q: %v", id, err)
	}
	return number
}

func TestEmbeddedSearchDeletionsAndFilters(t *testing.T) {
	corpus := smallSearchCorpus(t, 600, 3, 0)
	defer corpus.Close()
	index := corpus.embeddedIndex(t)
	query := benchQuery(16)

	// Every document of group g1 is excluded by the document filter, so a
	// search filtered on an unknown group must return an empty, non-nil slice.
	hits, err := corpus.search(10, query, groupFilter("does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if hits == nil || len(hits) != 0 {
		t.Fatalf("expected an empty non-nil result, got %#v", hits)
	}

	allowed, err := corpus.reader.vectorFilterDocumentNumbers(context.Background(), groupFilter("g3"))
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 150 {
		t.Fatalf("expected 150 allowed documents, got %d", len(allowed))
	}
	hits, err = corpus.search(500, query, groupFilter("g3"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != len(allowed) {
		t.Fatalf("k above the result count returned %d hits, want %d", len(hits), len(allowed))
	}
	for _, hit := range hits {
		if !strings.HasPrefix(string(hit.ID), "doc-") {
			t.Fatalf("unexpected identifier %q", hit.ID)
		}
	}
	assertOrdering(t, "filtered", hits)
	assertSameHits(t, "filtered exhaustive",
		hits, referenceEmbeddedSearch(t, index, embeddedBenchField, query, 500, nil, allowed))

	// An identifier filter that matches nothing keeps its own empty contract.
	empty, err := index.SearchCandidates(embeddedBenchField, query, 10, map[Identifier]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no identifiers to match, got %v", vectorIDs(empty))
	}
}

func TestEmbeddedSearchBestResultsRemainAfterDeletion(t *testing.T) {
	dimensions := 8
	spec := embeddedCorpusSpec{
		Documents:   120,
		SegmentSize: 40,
		Dimensions:  dimensions,
		VectorOf: func(i int) []float32 {
			vector := make([]float32, dimensions)
			vector[i%dimensions] = 1
			return vector
		},
	}
	corpus := buildEmbeddedCorpus(t, spec)
	defer corpus.Close()
	query := make([]float32, dimensions)
	query[0] = 1

	hits, err := corpus.search(5, query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 || hits[0].ID != embeddedDocID(0) {
		t.Fatalf("unexpected initial results: %v", vectorIDs(hits))
	}

	writer, err := OpenWriter(corpus.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Delete(embeddedDocID(0)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := corpus.reader.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(corpus.config)
	if err != nil {
		t.Fatal(err)
	}
	corpus.reader = reader

	hits, err = corpus.search(5, query, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.ID == embeddedDocID(0) {
			t.Fatalf("deleted document %q was returned", hit.ID)
		}
	}
	if len(hits) != 5 {
		t.Fatalf("deletion shrank the result set: %v", vectorIDs(hits))
	}
	assertSameHits(t, "after deletion",
		hits, referenceEmbeddedSearch(t, corpus.embeddedIndex(t), embeddedBenchField, query, 5, nil, nil))
}

func TestEmbeddedSearchValidationErrors(t *testing.T) {
	corpus := smallSearchCorpus(t, 120, 2, 0)
	defer corpus.Close()
	index := corpus.embeddedIndex(t)
	query := benchQuery(16)

	if _, err := corpus.search(0, query, nil); !errors.Is(err, ErrVectorInvalidK) {
		t.Fatalf("expected ErrVectorInvalidK, got %v", err)
	}
	if _, err := corpus.search(-1, query, nil); !errors.Is(err, ErrVectorInvalidK) {
		t.Fatalf("expected ErrVectorInvalidK, got %v", err)
	}
	if _, err := corpus.search(1, nil, nil); !errors.Is(err, ErrVectorInvalidDimension) {
		t.Fatalf("expected ErrVectorInvalidDimension, got %v", err)
	}
	invalid := append([]float32(nil), query...)
	invalid[0] = float32(math.Inf(1))
	if _, err := corpus.search(1, invalid, nil); !errors.Is(err, ErrVectorInvalidValue) {
		t.Fatalf("expected ErrVectorInvalidValue, got %v", err)
	}
	short := query[:len(query)-1]
	if _, err := corpus.search(1, short, nil); !errors.Is(err, ErrVectorInvalidDimension) {
		t.Fatalf("expected ErrVectorInvalidDimension, got %v", err)
	}
	if _, err := index.searchCandidates("missing", query, 1, nil, nil); !errors.Is(err, ErrVectorFieldNotFound) {
		t.Fatalf("expected ErrVectorFieldNotFound, got %v", err)
	}

	// The exported Search entry point without filters must reject a filter,
	// matching the pre-existing contract.
	if _, err := index.Search(embeddedBenchField, query, 1, groupFilter("g0")); !errors.Is(err, ErrVectorFilterUnsupported) {
		t.Fatalf("expected ErrVectorFilterUnsupported, got %v", err)
	}
	// Closing the reader closes the embedded vector index it owns.
	if err := corpus.reader.Close(); err != nil {
		t.Fatal(err)
	}
	corpus.reader = nil
	if _, err := index.searchCandidates(embeddedBenchField, query, 1, nil, nil); err == nil {
		t.Fatal("expected a closed index error")
	}
	if _, err := index.SearchCandidates(embeddedBenchField, query, 1, nil); err == nil {
		t.Fatal("expected a closed index error from the exported entry point")
	}
}

// TestEmbeddedSearchReaderLifecycleAndConcurrency exercises concurrent searches
// on one reader, an independent reader over the same index, and closing.
func TestEmbeddedSearchReaderLifecycleAndConcurrency(t *testing.T) {
	corpus := smallSearchCorpus(t, 600, 3, 0)
	defer corpus.Close()
	query := benchQuery(16)

	second, err := OpenReader(corpus.config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	baseline, err := corpus.search(20, query, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			reader := corpus.reader
			if worker%2 == 1 {
				reader = second
			}
			for i := 0; i < 25; i++ {
				var filter Query
				if i%2 == 0 {
					filter = groupFilter(fmt.Sprintf("g%d", i%4))
				}
				if _, err := reader.VectorSearch(context.Background(),
					embeddedBenchField, query, 20, filter); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent search failed: %v", err)
	}

	after, err := corpus.search(20, query, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSameHits(t, "concurrent stability", after, baseline)

	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.VectorSearch(context.Background(),
		embeddedBenchField, query, 20, nil); err == nil {
		t.Fatal("expected a closed index error from the closed reader")
	}
}

// TestEmbeddedSearchDefersIdentifierReads documents the point of the
// optimization: identifiers are resolved for the final results and the tie
// group on the K boundary only, not for every candidate. Run with
// -tags blugevectorstats to enable the counter.
func TestEmbeddedSearchDefersIdentifierReads(t *testing.T) {
	if !vectorStatsEnabled {
		t.Skip("run with -tags blugevectorstats to measure stored identifier reads")
	}
	corpus := smallSearchCorpus(t, 800, 4, 0)
	defer corpus.Close()
	query := benchQuery(16)
	k := 40

	resetVectorStoredIDReadCount()
	hits, err := corpus.search(k, query, nil)
	if err != nil {
		t.Fatal(err)
	}
	reads := vectorStoredIDReadCount()
	candidates := len(corpus.embeddedIndex(t).fields[embeddedBenchField]) * k
	if int(reads) > len(hits)+1 {
		t.Fatalf("resolved %d identifiers for %d results (%d candidates): identifiers "+
			"were not deferred", reads, len(hits), candidates)
	}

	resetVectorStoredIDReadCount()
	referenceEmbeddedSearch(t, corpus.embeddedIndex(t),
		embeddedBenchField, query, k, nil, nil)
	referenceReads := vectorStoredIDReadCount()
	if referenceReads <= reads {
		t.Fatalf("reference read %d identifiers, optimized read %d: expected the "+
			"reference to read more", referenceReads, reads)
	}
	t.Logf("k=%d: optimized search resolved %d identifiers, reference resolved %d "+
		"(%d candidate slots)", k, reads, referenceReads, candidates)
}

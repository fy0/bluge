//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/fy0/bluge/search"
)

// TestPreparedVectorFilterMatchesUnpreparedSearch checks that a prepared
// filter reuses the filter evaluation without changing any result.
func TestPreparedVectorFilterMatchesUnpreparedSearch(t *testing.T) {
	corpus := smallSearchCorpus(t, 800, 4, 0)
	defer corpus.Close()
	query := benchQuery(16)

	prepared, err := corpus.reader.PrepareVectorFilter(context.Background(), groupFilter("g1"))
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()

	for _, k := range []int{1, 5, 192, 512, 5000} {
		want, err := corpus.search(k, query, groupFilter("g1"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := corpus.reader.VectorSearchPrepared(context.Background(),
			embeddedBenchField, query, k, prepared)
		if err != nil {
			t.Fatal(err)
		}
		assertSameHits(t, "prepared filter", got, want)
	}
}

// TestPreparedVectorFilterEmptyVersusUnfiltered covers the distinction the API
// must preserve: a filter that matches nothing must not turn into an
// unfiltered search.
func TestPreparedVectorFilterEmptyVersusUnfiltered(t *testing.T) {
	corpus := smallSearchCorpus(t, 400, 2, 0)
	defer corpus.Close()
	query := benchQuery(16)

	empty, err := corpus.reader.PrepareVectorFilter(context.Background(),
		groupFilter("matches-nothing"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	hits, err := corpus.reader.VectorSearchPrepared(context.Background(),
		embeddedBenchField, query, 10, empty)
	if err != nil {
		t.Fatal(err)
	}
	if hits == nil || len(hits) != 0 {
		t.Fatalf("an empty prepared filter must return an empty non-nil result, got %#v", hits)
	}

	unfiltered, err := corpus.reader.PrepareVectorFilter(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unfiltered.Close()
	hits, err = corpus.reader.VectorSearchPrepared(context.Background(),
		embeddedBenchField, query, 10, unfiltered)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 10 {
		t.Fatalf("a nil prepared filter must search without a filter, got %d hits", len(hits))
	}
	plain, err := corpus.search(10, query, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSameHits(t, "nil prepared filter", hits, plain)
}

// TestPreparedVectorFilterLifetime rejects cross-reader use and use after the
// filter or its reader was closed.
func TestPreparedVectorFilterLifetime(t *testing.T) {
	corpus := smallSearchCorpus(t, 300, 2, 0)
	query := benchQuery(16)

	prepared, err := corpus.reader.PrepareVectorFilter(context.Background(), groupFilter("g2"))
	if err != nil {
		t.Fatal(err)
	}

	other, err := OpenReader(corpus.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.VectorSearchPrepared(context.Background(),
		embeddedBenchField, query, 5, prepared); !errors.Is(err, ErrVectorPreparedFilter) {
		t.Fatalf("cross-reader use was not rejected: %v", err)
	}
	if _, err := corpus.reader.VectorSearchPrepared(context.Background(),
		embeddedBenchField, query, 5, nil); !errors.Is(err, ErrVectorPreparedFilter) {
		t.Fatalf("nil prepared filter was not rejected: %v", err)
	}
	if _, err := corpus.reader.VectorSearchPrepared(context.Background(),
		embeddedBenchField, query, 5, prepared); err != nil {
		t.Fatalf("valid prepared filter was rejected: %v", err)
	}

	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.reader.VectorSearchPrepared(context.Background(),
		embeddedBenchField, query, 5, prepared); !errors.Is(err, ErrVectorPreparedFilter) {
		t.Fatalf("closed prepared filter was not rejected: %v", err)
	}

	// A filter must not outlive the reader that owns it.
	second, err := corpus.reader.PrepareVectorFilter(context.Background(), groupFilter("g2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.reader.Close(); err != nil {
		t.Fatal(err)
	}
	corpus.reader = nil
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.reader.VectorSearchPrepared(context.Background(),
		embeddedBenchField, query, 5, second); !errors.Is(err, ErrVectorPreparedFilter) {
		t.Fatalf("use after reader close was not rejected: %v", err)
	}
}

// TestPreparedVectorFilterConcurrentUse shares one prepared filter across
// concurrent searches and checks that the results stay stable.
func TestPreparedVectorFilterConcurrentUse(t *testing.T) {
	corpus := smallSearchCorpus(t, 600, 3, 0)
	defer corpus.Close()
	query := benchQuery(16)

	prepared, err := corpus.reader.PrepareVectorFilter(context.Background(), groupFilter("g0"))
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	want, err := corpus.reader.VectorSearchPrepared(context.Background(),
		embeddedBenchField, query, 25, prepared)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				got, err := corpus.reader.VectorSearchPrepared(context.Background(),
					embeddedBenchField, query, 25, prepared)
				if err != nil {
					failures <- err
					return
				}
				if len(got) != len(want) {
					failures <- errors.New("concurrent prepared search returned a different length")
					return
				}
				for j := range got {
					if got[j] != want[j] {
						failures <- errors.New("concurrent prepared search returned different hits")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}

// TestPreparedVectorFilterReuseSkipsFilterEvaluation checks the point of the
// API: the filter query is evaluated once, not once per search.
func TestPreparedVectorFilterReuseSkipsFilterEvaluation(t *testing.T) {
	corpus := smallSearchCorpus(t, 400, 2, 0)
	defer corpus.Close()
	query := benchQuery(16)

	checks := 0
	counting := &countingQuery{inner: groupFilter("g1"), checks: &checks}
	prepared, err := corpus.reader.PrepareVectorFilter(context.Background(), counting)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if checks == 0 {
		t.Fatal("preparing the filter did not evaluate the query")
	}
	evaluations := checks

	for i := 0; i < 5; i++ {
		if _, err := corpus.reader.VectorSearchPrepared(context.Background(),
			embeddedBenchField, query, 10, prepared); err != nil {
			t.Fatal(err)
		}
	}
	if checks != evaluations {
		t.Fatalf("the filter query was re-evaluated %d extra times", checks-evaluations)
	}
}

// TestPreparedVectorFilterRejectsClosedReader covers the preparation entry
// point, not just later searches: a closed reader must not be read again, and
// must not hand out a filter that can never be used.
func TestPreparedVectorFilterRejectsClosedReader(t *testing.T) {
	corpus := smallSearchCorpus(t, 200, 2, 0)
	if err := corpus.reader.Close(); err != nil {
		t.Fatal(err)
	}
	closed := corpus.reader
	corpus.reader = nil

	if _, err := closed.PrepareVectorFilter(context.Background(), groupFilter("g0")); !errors.Is(err, ErrVectorPreparedFilter) {
		t.Fatalf("preparing a filtered query on a closed reader was not rejected: %v", err)
	}
	if _, err := closed.PrepareVectorFilter(context.Background(), nil); !errors.Is(err, ErrVectorPreparedFilter) {
		t.Fatalf("preparing an unfiltered query on a closed reader was not rejected: %v", err)
	}
}

// TestPreparedVectorFilterFlatBackend checks the identifier-based backends:
// the filter is frozen into an identifier set at preparation time, so mutating
// the original query afterwards cannot change the results.
func TestPreparedVectorFilterFlatBackend(t *testing.T) {
	config := InMemoryOnlyConfig().WithVectorBackend(NewFlatVectorBackend(""))
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	documents := []*Document{
		NewDocument("alpha").AddField(NewKeywordField("group", "g0")).
			AddField(NewVectorField("embedding", []float32{1, 0})),
		NewDocument("beta").AddField(NewKeywordField("group", "g0")).
			AddField(NewVectorField("embedding", []float32{0.9, 0.1})),
		NewDocument("gamma").AddField(NewKeywordField("group", "g1")).
			AddField(NewVectorField("embedding", []float32{0, 1})),
	}
	if err := writer.InsertMany(documents); err != nil {
		t.Fatal(err)
	}
	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	query := []float32{1, 0}

	filter := NewTermQuery("g0").SetField("group")
	prepared, err := reader.PrepareVectorFilter(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	got, err := reader.VectorSearchPrepared(context.Background(), "embedding", query, 5, prepared)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, got, "alpha", "beta")

	// The same search through the ordinary entry point must agree.
	want, err := reader.VectorSearch(context.Background(), "embedding", query, 5, filter)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, want, "alpha", "beta")
	assertSameHits(t, "flat backend", got, want)

	// Mutating the original query must not change a prepared filter's results.
	filter.SetField("group-mutated")
	after, err := reader.VectorSearchPrepared(context.Background(), "embedding", query, 5, prepared)
	if err != nil {
		t.Fatal(err)
	}
	assertSameHits(t, "flat backend after query mutation", after, got)

	// An empty match set stays empty rather than becoming unfiltered.
	empty, err := reader.PrepareVectorFilter(context.Background(),
		NewTermQuery("nothing").SetField("group"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	hits, err := reader.VectorSearchPrepared(context.Background(), "embedding", query, 5, empty)
	if err != nil {
		t.Fatal(err)
	}
	if hits == nil || len(hits) != 0 {
		t.Fatalf("an empty flat-backend filter must return an empty non-nil result, got %#v", hits)
	}
}

// countingQuery counts how often a query is evaluated by the searcher.
type countingQuery struct {
	inner  Query
	checks *int
}

func (q *countingQuery) Searcher(i search.Reader,
	options search.SearcherOptions) (search.Searcher, error) {
	*q.checks++
	return q.inner.Searcher(i, options)
}

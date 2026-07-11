package bluge

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/fy0/bluge/search"
	"github.com/fy0/bluge/search/similarity"
)

type bm25GoldenDocument struct {
	id    string
	title string
	body  string
}

var bm25GoldenDocuments = []bm25GoldenDocument{
	{id: "short", title: "Location", body: "location"},
	{id: "repeated", title: "Reference", body: "location location location location location location location location alpha"},
	{id: "mixed", title: "Alpha location", body: "alpha beta location"},
	{id: "alpha", title: "Alpha", body: "alpha alpha beta"},
	{id: "phrase", title: "Guide", body: "find location alpha quickly"},
	{id: "sparse", title: "Unrelated", body: "beta gamma delta epsilon"},
}

// Expected orders were generated with github.com/blevesearch/bleve/v2 v2.5.7
// using its BM25 scoring model. Raw scores are intentionally not compared.
func TestBleveBM25SimilarityGoldenOrder(t *testing.T) {
	path := createTmpIndexPath(t)
	defer cleanupTmpIndexPath(t, path)

	writeBM25GoldenIndex(t, path)
	config := DefaultConfig(path)
	config.DefaultSimilarity = similarity.NewBleveBM25Similarity()
	reader, err := OpenReader(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	title := NewMatchQuery("location").SetField("title").SetBoost(2)
	body := NewMatchQuery("location").SetField("body")
	tests := []struct {
		name  string
		query Query
		want  []string
	}{
		{name: "term", query: NewTermQuery("location").SetField("body"), want: []string{"short", "repeated", "mixed", "phrase"}},
		{name: "match_or", query: NewMatchQuery("location alpha").SetField("body"), want: []string{"mixed", "phrase", "repeated", "short", "alpha"}},
		{name: "match_and", query: NewMatchQuery("location alpha").SetField("body").SetOperator(MatchQueryOperatorAnd), want: []string{"mixed", "phrase", "repeated"}},
		{name: "phrase", query: NewMatchPhraseQuery("location alpha").SetField("body"), want: []string{"phrase", "repeated"}},
		{name: "nested_boost", query: NewBooleanQuery().AddShould(title, body), want: []string{"short", "mixed", "repeated", "phrase"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := bm25ResultIDs(t, reader, test.query)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("result order: got %v want %v", got, test.want)
			}
		})
	}
}

func TestBM25ModesShareNormEncoding(t *testing.T) {
	path := createTmpIndexPath(t)
	defer cleanupTmpIndexPath(t, path)
	writeBM25GoldenIndex(t, path)

	for _, sim := range []search.Similarity{
		similarity.NewBM25Similarity(),
		similarity.NewLegacyBM25Similarity(),
		similarity.NewBleveBM25Similarity(),
	} {
		config := DefaultConfig(path)
		config.DefaultSimilarity = sim
		reader, err := OpenReader(config)
		if err != nil {
			t.Fatal(err)
		}
		ids := bm25ResultIDs(t, reader, NewTermQuery("location").SetField("body"))
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if len(ids) != 4 {
			t.Fatalf("mode %T returned %d matches, want 4", sim, len(ids))
		}
	}
}

func TestStandardBM25MatchPhraseBoostAppliedOnce(t *testing.T) {
	path := createTmpIndexPath(t)
	defer cleanupTmpIndexPath(t, path)
	writeBM25GoldenIndex(t, path)

	reader, err := OpenReader(DefaultConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	base := bm25TopScore(t, reader, NewMatchPhraseQuery("location alpha").SetField("body"))
	boosted := bm25TopScore(t, reader, NewMatchPhraseQuery("location alpha").SetField("body").SetBoost(3))
	if math.Abs(boosted-base*3) > 1e-12 {
		t.Fatalf("boosted phrase score: got %v want %v", boosted, base*3)
	}
}

func writeBM25GoldenIndex(t *testing.T, path string) {
	t.Helper()
	writer, err := OpenWriter(DefaultConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range bm25GoldenDocuments {
		doc := NewDocument(fixture.id).
			AddField(NewTextField("title", fixture.title).SearchTermPositions()).
			AddField(NewTextField("body", fixture.body).SearchTermPositions())
		if err := writer.Insert(doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func bm25ResultIDs(t *testing.T, reader *Reader, query Query) []string {
	t.Helper()
	iterator, err := reader.Search(context.Background(), NewTopNSearch(10, query))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for {
		match, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		if match == nil {
			break
		}
		var id string
		if err := match.VisitStoredFields(func(field string, value []byte) bool {
			if field == "_id" {
				id = string(value)
				return false
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func bm25TopScore(t *testing.T, reader *Reader, query Query) float64 {
	t.Helper()
	iterator, err := reader.Search(context.Background(), NewTopNSearch(1, query))
	if err != nil {
		t.Fatal(err)
	}
	match, err := iterator.Next()
	if err != nil {
		t.Fatal(err)
	}
	if match == nil {
		t.Fatal("expected a match")
	}
	return match.Score
}

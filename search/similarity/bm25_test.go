package similarity

import (
	"math"
	"testing"

	segment "github.com/blugelabs/bluge_segment_api"
)

type testCollectionStats struct {
	totalDocs uint64
	fieldDocs uint64
	totalTF   uint64
}

func (s *testCollectionStats) TotalDocumentCount() uint64    { return s.totalDocs }
func (s *testCollectionStats) DocumentCount() uint64         { return s.fieldDocs }
func (s *testCollectionStats) SumTotalTermFrequency() uint64 { return s.totalTF }
func (s *testCollectionStats) Merge(segment.CollectionStats) {}

type testTermStats uint64

func (s testTermStats) DocumentFrequency() uint64 { return uint64(s) }

func TestBM25SimilarityIdfMatchesBM25Formula(t *testing.T) {
	sim := NewBM25Similarity()

	const docFreq = 10
	const docCount = 100

	want := math.Log(1.0 + (float64(docCount-docFreq)+0.5)/(float64(docFreq)+0.5))
	got := sim.Idf(docFreq, docCount)

	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("unexpected idf: got %v want %v", got, want)
	}
}

func TestBM25SimilarityIdfDoesNotReturnNegativeValue(t *testing.T) {
	sim := NewBM25Similarity()

	got := sim.Idf(101, 100)

	if got != 0 {
		t.Fatalf("unexpected idf: got %v want 0", got)
	}
}

func TestBM25SimilarityModes(t *testing.T) {
	stats := &testCollectionStats{totalDocs: 10, fieldDocs: 5, totalTF: 21}
	termStats := testTermStats(2)
	const boost = 1.5
	const freq = 3
	const docLen = 7

	standard := NewBM25SimilarityBK1(defaultB, defaultK1)
	standardScorer := standard.Scorer(boost, stats, termStats)
	standardScore := standardScorer.Score(freq, float64(standard.ComputeNorm(docLen)))
	idf := standard.Idf(2, 5)
	wantStandard := boost * idf * freq * (defaultK1 + 1) /
		(float64(freq) + defaultK1*(1-defaultB+defaultB*docLen/(21.0/5.0)))
	if math.Abs(standardScore-wantStandard) > 1e-12 {
		t.Fatalf("standard score: got %v want %v", standardScore, wantStandard)
	}

	legacy := NewLegacyBM25SimilarityBK1(defaultB, defaultK1)
	legacyScore := legacy.Scorer(boost, stats, termStats).Score(freq, float64(legacy.ComputeNorm(docLen)))
	if math.Abs(standardScore-legacyScore*(defaultK1+1)) > 1e-12 {
		t.Fatalf("legacy score %v is not the historical scale of standard score %v", legacyScore, standardScore)
	}

	bleve := NewBleveBM25Similarity()
	if got, want := bleve.AverageFieldLength(stats), 3.0; got != want {
		t.Fatalf("Bleve avgdl: got %v want %v", got, want)
	}
	bleveScorer := bleve.Scorer(boost, stats, termStats)
	tf := math.Sqrt(freq)
	bleveIDF := bleve.Idf(2, 10)
	wantBleve := bleveIDF * tf * defaultK1 /
		(tf + defaultK1*(1-defaultB+defaultB*docLen/3.0))
	if got := bleveScorer.Score(freq, float64(bleve.ComputeNorm(docLen))); math.Abs(got-wantBleve) > 1e-12 {
		t.Fatalf("Bleve score before query norm: got %v want %v", got, wantBleve)
	}
}

func TestBM25SimilarityEmptyStatsAreFinite(t *testing.T) {
	for _, sim := range []*BM25Similarity{
		NewBM25Similarity(), NewLegacyBM25Similarity(), NewBleveBM25Similarity(),
	} {
		scorer := sim.Scorer(1, &testCollectionStats{}, testTermStats(0))
		score := scorer.Score(1, float64(sim.ComputeNorm(1)))
		if score != 0 || math.IsNaN(score) || math.IsInf(score, 0) {
			t.Fatalf("unexpected empty-stat score for mode %d: %v", sim.mode, score)
		}
	}
}

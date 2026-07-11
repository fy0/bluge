//  Copyright (c) 2020 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package similarity

import (
	"fmt"
	"math"

	segment "github.com/blugelabs/bluge_segment_api"

	"github.com/fy0/bluge/search"
)

const defaultB = 0.75
const defaultK1 = 1.2

type bm25Mode uint8

const (
	bm25Standard bm25Mode = iota
	bm25Legacy
	bm25Bleve
)

type BM25Similarity struct {
	b    float64
	k1   float64
	mode bm25Mode
}

// NewBM25Similarity returns the standard BM25 similarity used by default.
func NewBM25Similarity() *BM25Similarity {
	return NewBM25SimilarityBK1(defaultB, defaultK1)
}

func NewBM25SimilarityBK1(b, k1 float64) *BM25Similarity {
	return &BM25Similarity{b: b, k1: k1, mode: bm25Standard}
}

// NewLegacyBM25Similarity preserves Bluge's historical BM25 score formula.
func NewLegacyBM25Similarity() *BM25Similarity {
	return NewLegacyBM25SimilarityBK1(defaultB, defaultK1)
}

func NewLegacyBM25SimilarityBK1(b, k1 float64) *BM25Similarity {
	return &BM25Similarity{b: b, k1: k1, mode: bm25Legacy}
}

// NewBleveBM25Similarity approximates the ranking behavior of Bleve v2.5.7.
// It intentionally retains Bluge's norm encoding so an index can be searched
// with any of the built-in BM25 modes without rebuilding it.
func NewBleveBM25Similarity() *BM25Similarity {
	return &BM25Similarity{b: defaultB, k1: defaultK1, mode: bm25Bleve}
}

// UsesQueryNorm reports whether searchers should apply Bleve-style query normalization.
func (b *BM25Similarity) UsesQueryNorm() bool {
	return b.mode == bm25Bleve
}

func (b *BM25Similarity) ComputeNorm(numTerms int) float32 {
	return math.Float32frombits(uint32(numTerms))
}

func (b *BM25Similarity) Idf(docFreq, docCount uint64) float64 {
	if docFreq > docCount {
		return 0
	}
	return math.Log(1.0 + (float64(docCount-docFreq)+0.5)/(float64(docFreq)+0.5))
}

func (b *BM25Similarity) IdfExplainTerm(collectionStats segment.CollectionStats, termStats segment.TermStats) *search.Explanation {
	docFreq := termStats.DocumentFrequency()
	docCount := b.documentCount(collectionStats)
	idf := b.Idf(docFreq, docCount)
	return search.NewExplanation(idf, "idf, computed as log(1 + (N - n + 0.5) / (n + 0.5)) from:",
		search.NewExplanation(float64(docFreq), "n, number of documents containing term"),
		search.NewExplanation(float64(docCount), "N, document count used by the scoring mode"))
}

func (b *BM25Similarity) documentCount(stats segment.CollectionStats) uint64 {
	if stats == nil {
		return 0
	}
	if b.mode == bm25Bleve {
		return stats.TotalDocumentCount()
	}
	return stats.DocumentCount()
}

func (b *BM25Similarity) AverageFieldLength(stats segment.CollectionStats) float64 {
	docCount := b.documentCount(stats)
	if stats == nil || docCount == 0 {
		return 0
	}
	totalTerms := stats.SumTotalTermFrequency()
	if b.mode == bm25Bleve {
		// Bleve v2.5.7 calls the per-segment term dictionary cardinality
		// "field cardinality" and uses it as the avgdl numerator.
		if extended, ok := stats.(interface{ UniqueTermCount() uint64 }); ok {
			totalTerms = extended.UniqueTermCount()
		}
		return math.Ceil(float64(totalTerms) / float64(docCount))
	}
	return float64(totalTerms) / float64(docCount)
}

func (b *BM25Similarity) Scorer(boost float64, collectionStats segment.CollectionStats, termStats segment.TermStats) search.Scorer {
	idf := b.IdfExplainTerm(collectionStats, termStats)
	return newBM25ScorerMode(boost, b.k1, b.b, b.AverageFieldLength(collectionStats), idf, b.mode)
}

type BM25Scorer struct {
	boost       float64
	k1          float64
	b           float64
	avgDocLen   float64
	weight      float64
	queryWeight float64
	idf         *search.Explanation
	mode        bm25Mode
}

func NewBM25Scorer(boost, k1, b, avgDocLen float64, idf *search.Explanation) *BM25Scorer {
	return newBM25ScorerMode(boost, k1, b, avgDocLen, idf, bm25Standard)
}

func newBM25ScorerMode(boost, k1, b, avgDocLen float64, idf *search.Explanation, mode bm25Mode) *BM25Scorer {
	return &BM25Scorer{
		boost:       boost,
		k1:          k1,
		b:           b,
		avgDocLen:   avgDocLen,
		idf:         idf,
		weight:      boost * idf.Value,
		queryWeight: 1,
		mode:        mode,
	}
}

func (b *BM25Scorer) Score(freq int, norm float64) float64 {
	docLen := math.Float32bits(float32(norm))
	return b.ScoreRawNorm(freq, uint64(docLen))
}

func (b *BM25Scorer) ScoreRawNorm(freq int, normBits uint64) float64 {
	return b.score(freq, float64(normBits))
}

func (b *BM25Scorer) score(freq int, docLen float64) float64 {
	if freq <= 0 || b.avgDocLen <= 0 {
		return 0
	}
	tf := float64(freq)
	if b.mode == bm25Bleve {
		tf = math.Sqrt(tf)
	}
	denominator := tf + b.k1*((1-b.b)+b.b*docLen/b.avgDocLen)
	if denominator <= 0 {
		return 0
	}

	switch b.mode {
	case bm25Legacy:
		normInverse := 1 / (b.k1 * ((1 - b.b) + b.b*docLen/b.avgDocLen))
		return b.weight - b.weight/(1+tf*normInverse)
	case bm25Bleve:
		return b.idf.Value * tf * b.k1 / denominator * b.queryWeight
	default:
		return b.weight * tf * (b.k1 + 1) / denominator
	}
}

// QueryNormWeight and SetQueryNorm are consumed by compatible searchers when
// Bleve-style query normalization is enabled.
func (b *BM25Scorer) QueryNormWeight() (float64, bool) {
	if b.mode != bm25Bleve {
		return 0, false
	}
	return b.weight * b.weight, true
}

func (b *BM25Scorer) SetQueryNorm(queryNorm float64) {
	if b.mode == bm25Bleve {
		b.queryWeight = b.weight * queryNorm
	}
}

func (b *BM25Scorer) tfExplanation(freq int, docLen float64) *search.Explanation {
	tf := float64(freq)
	if b.mode == bm25Bleve {
		tf = math.Sqrt(tf)
	}
	value := 0.0
	if b.avgDocLen > 0 {
		denominator := tf + b.k1*((1-b.b)+b.b*docLen/b.avgDocLen)
		multiplier := 1.0
		switch b.mode {
		case bm25Standard:
			multiplier = b.k1 + 1
		case bm25Bleve:
			multiplier = b.k1
		}
		if denominator > 0 {
			value = tf * multiplier / denominator
		}
	}
	return search.NewExplanation(value,
		"tf saturation from:",
		search.NewExplanation(tf, "term frequency used by scoring mode"),
		search.NewExplanation(b.k1, "k1, term saturation parameter"),
		search.NewExplanation(b.b, "b, length normalization parameter"),
		search.NewExplanation(docLen, "dl, length of field"),
		search.NewExplanation(b.avgDocLen, "avgdl, average length of field"))
}

func (b *BM25Scorer) modeName() string {
	switch b.mode {
	case bm25Legacy:
		return "legacy-bluge"
	case bm25Bleve:
		return "bleve-v2.5.7"
	default:
		return "standard"
	}
}

const noBoost = 1.0

func (b *BM25Scorer) Explain(freq int, norm float64) *search.Explanation {
	docLen := float64(math.Float32bits(float32(norm)))
	children := []*search.Explanation{b.idf}
	if b.boost != noBoost {
		children = append(children, search.NewExplanation(b.boost, "boost"))
	}
	children = append(children, b.tfExplanation(freq, docLen))
	if b.mode == bm25Bleve && b.queryWeight != 1 {
		children = append(children, search.NewExplanation(b.queryWeight, "Bleve query weight"))
	}
	return search.NewExplanation(b.score(freq, docLen),
		fmt.Sprintf("score(freq=%d), computed using %s BM25 from:", freq, b.modeName()),
		children...)
}

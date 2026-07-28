//  Copyright (c) 2020 Couchbase, Inc.
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

package searcher

import (
	"context"
	"math"

	"github.com/fy0/bluge/search"
	segment "github.com/fy0/bluge/segment"
)

type TermSearcher struct {
	indexReader search.Reader
	reader      segment.PostingsIterator
	options     search.SearcherOptions
	scorer      search.Scorer
	rawScorer   rawNormScorer
	queryTerm   string
}

type rawNormScorer interface {
	ScoreRawNorm(freq int, normBits uint64) float64
}

type impactScorer interface {
	rawNormScorer
	ScoreImpactUpperBound([]segment.Impact) float64
}

type rawNormPosting interface {
	NormUint64() uint64
}

type queryNormScorer interface {
	QueryNormWeight() (float64, bool)
	SetQueryNorm(queryNorm float64)
}

func NewTermSearcher(indexReader search.Reader, term, field string, boost float64, scorer search.Scorer,
	options search.SearcherOptions) (*TermSearcher, error) {
	return NewTermSearcherBytes(indexReader, []byte(term), field, boost, scorer, options)
}

func NewTermSearcherBytes(indexReader search.Reader, term []byte, field string, boost float64, scorer search.Scorer,
	options search.SearcherOptions) (*TermSearcher, error) {
	needFreqNorm := options.Score != "none"
	reader, err := indexReader.PostingsIterator(term, field, needFreqNorm, needFreqNorm, options.IncludeTermVectors)
	if err != nil {
		return nil, err
	}
	return newTermSearcherFromReader(indexReader, reader, term, field, boost, scorer, options)
}

type termStatsWrapper struct {
	docFreq uint64
}

func (t *termStatsWrapper) DocumentFrequency() uint64 {
	return t.docFreq
}

func newTermSearcherFromReader(indexReader search.Reader, reader segment.PostingsIterator,
	term []byte, field string, boost float64, scorer search.Scorer, options search.SearcherOptions) (*TermSearcher, error) {
	if scorer == nil {
		collStats, err := indexReader.CollectionStats(field)
		if err != nil {
			return nil, err
		}
		scorer = options.SimilarityForField(field).Scorer(boost, collStats, &termStatsWrapper{docFreq: reader.Count()})
	}
	return &TermSearcher{
		indexReader: indexReader,
		reader:      reader,
		scorer:      scorer,
		rawScorer:   rawScorerFor(scorer),
		options:     options,
		queryTerm:   string(term),
	}, nil
}

func rawScorerFor(scorer search.Scorer) rawNormScorer {
	rawScorer, _ := scorer.(rawNormScorer)
	return rawScorer
}

func (s *TermSearcher) Size() int {
	return reflectStaticSizeTermSearcher + sizeOfPtr + s.reader.Size()
}

func (s *TermSearcher) Count() uint64 {
	return s.reader.Count()
}

func (s *TermSearcher) QueryNormWeight() (float64, bool) {
	weighted, ok := s.scorer.(queryNormScorer)
	if !ok {
		return 0, false
	}
	return weighted.QueryNormWeight()
}

func (s *TermSearcher) SetQueryNorm(queryNorm float64) {
	if weighted, ok := s.scorer.(queryNormScorer); ok {
		weighted.SetQueryNorm(queryNorm)
	}
}

func (s *TermSearcher) Next(ctx *search.Context) (*search.DocumentMatch, error) {
	termMatch, err := s.reader.Next()
	if err != nil {
		return nil, err
	}

	if termMatch == nil {
		return nil, nil
	}

	// score match
	docMatch := s.buildDocumentMatch(ctx, termMatch)

	// return doc match
	return docMatch, nil
}

func (s *TermSearcher) Advance(ctx *search.Context, number uint64) (*search.DocumentMatch, error) {
	termMatch, err := s.reader.Advance(number)
	if err != nil {
		return nil, err
	}

	if termMatch == nil {
		return nil, nil
	}

	// score match
	docMatch := s.buildDocumentMatch(ctx, termMatch)

	// return doc match
	return docMatch, nil
}

func (s *TermSearcher) Close() error {
	return s.reader.Close()
}

func (s *TermSearcher) Min() int {
	return 0
}

func (s *TermSearcher) DocumentMatchPoolSize() int {
	return 1
}

func (s *TermSearcher) RawTopN(ctx context.Context, searchContext *search.Context,
	size int) (search.RawTopNResult, bool, error) {
	result := search.RawTopNResult{TotalCandidates: s.reader.Count()}
	blockReader, ok := s.reader.(segment.ImpactPostingsIterator)
	blockScorer, scorerOK := s.scorer.(impactScorer)
	if !ok || !scorerOK || !blockReader.HasImpacts() || s.options.Explain ||
		s.options.IncludeTermVectors || s.options.Score == "none" {
		return result, false, nil
	}
	if size <= 0 {
		return result, true, nil
	}

	matches := make(rawTopNHeap, 0, size)
	var posting segment.Posting
	var target uint64
	var blocksVisited uint64
	for {
		blocksVisited++
		if blocksVisited&1023 == 0 {
			select {
			case <-ctx.Done():
				return result, true, ctx.Err()
			default:
			}
		}
		blockEnd, impacts, exists, err := blockReader.AdvanceShallow(target)
		if err != nil {
			return result, true, err
		}
		if !exists {
			break
		}
		if len(impacts) > 0 && len(matches) == size &&
			blockScorer.ScoreImpactUpperBound(impacts) <= matches[0].Score {
			if blockEnd == math.MaxUint64 {
				break
			}
			posting = nil
			target = blockEnd + 1
			continue
		}

		if posting == nil {
			posting, err = s.reader.Advance(target)
			if err != nil {
				return result, true, err
			}
		}
		for posting != nil && posting.Number() <= blockEnd {
			if result.ScoredCandidates&1023 == 0 {
				select {
				case <-ctx.Done():
					return result, true, ctx.Err()
				default:
				}
			}
			score := s.scorePosting(posting)
			result.ScoredCandidates++
			if len(matches) < size || rawTopNBetter(score, posting.Number(), matches[0]) {
				match := searchContext.DocumentMatchPool.Get()
				match.SetReader(s.indexReader)
				match.Number = posting.Number()
				match.Score = score
				match.HitNumber = int(match.Number) + 1
				if removed := rawTopNAdd(&matches, size, match); removed != nil {
					searchContext.DocumentMatchPool.Put(removed)
				}
			}
			posting, err = s.reader.Next()
			if err != nil {
				return result, true, err
			}
		}
		if posting == nil {
			break
		}
		target = posting.Number()
	}
	result.Matches = rawTopNSorted(matches)
	return result, true, nil
}

func (s *TermSearcher) scorePosting(posting segment.Posting) float64 {
	if s.rawScorer != nil {
		if rawPosting, ok := posting.(rawNormPosting); ok {
			return s.rawScorer.ScoreRawNorm(posting.Frequency(), rawPosting.NormUint64())
		}
	}
	return s.scorer.Score(posting.Frequency(), posting.Norm())
}

func (s *TermSearcher) Optimize(kind string, octx segment.OptimizableContext) (
	segment.OptimizableContext, error) {
	o, ok := s.reader.(segment.Optimizable)
	if ok {
		return o.Optimize(kind, octx)
	}

	return nil, nil
}

func (s *TermSearcher) buildDocumentMatch(ctx *search.Context, termMatch segment.Posting) *search.DocumentMatch {
	rv := ctx.DocumentMatchPool.Get()
	rv.SetReader(s.indexReader)
	rv.Number = termMatch.Number()

	if s.options.Explain {
		rv.Explanation = s.scorer.Explain(termMatch.Frequency(), termMatch.Norm())
		rv.Score = rv.Explanation.Value
	} else {
		rv.Score = s.scorePosting(termMatch)
	}

	if s.options.IncludeTermVectors {
		locations := termMatch.Locations()
		if cap(rv.FieldTermLocations) < len(locations) {
			rv.FieldTermLocations = make([]search.FieldTermLocation, 0, len(locations))
		}

		for _, v := range locations {
			rv.FieldTermLocations =
				append(rv.FieldTermLocations, search.FieldTermLocation{
					Field: v.Field(),
					Term:  s.queryTerm,
					Location: search.Location{
						Pos:   v.Pos(),
						Start: v.Start(),
						End:   v.End(),
					},
				})
		}
	}

	return rv
}

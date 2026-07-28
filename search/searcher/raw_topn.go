// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package searcher

import (
	"container/heap"
	"context"
	"math"
	"sort"

	"github.com/fy0/bluge/search"
	"github.com/fy0/bluge/segment"
)

type rawCompositeScorer interface {
	ScoreCompositeSum(float64) (float64, bool)
}

type rawTopNHeap search.DocumentMatchCollection

func (h rawTopNHeap) Len() int { return len(h) }
func (h rawTopNHeap) Less(i, j int) bool {
	if h[i].Score != h[j].Score {
		return h[i].Score < h[j].Score
	}
	return h[i].Number > h[j].Number
}
func (h rawTopNHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *rawTopNHeap) Push(value interface{}) {
	*h = append(*h, value.(*search.DocumentMatch))
}
func (h *rawTopNHeap) Pop() interface{} {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = nil
	*h = old[:last]
	return value
}

func rawTopNBetter(score float64, number uint64, worst *search.DocumentMatch) bool {
	return score > worst.Score || score == worst.Score && number < worst.Number
}

func rawTopNSorted(matches rawTopNHeap) search.DocumentMatchCollection {
	rv := search.DocumentMatchCollection(matches)
	sort.Slice(rv, func(i, j int) bool {
		if rv[i].Score != rv[j].Score {
			return rv[i].Score > rv[j].Score
		}
		return rv[i].Number < rv[j].Number
	})
	return rv
}

func rawTopNAdd(h *rawTopNHeap, size int, match *search.DocumentMatch) *search.DocumentMatch {
	if len(*h) < size {
		heap.Push(h, match)
		return nil
	}
	removed := heap.Pop(h).(*search.DocumentMatch)
	heap.Push(h, match)
	return removed
}

type wandTermCursor struct {
	term        *TermSearcher
	blocks      segment.ImpactPostingsIterator
	blockScorer impactScorer
	posting     segment.Posting
}

func rawTopNDisjunction(ctx context.Context, searchContext *search.Context, size int,
	searchers []search.Searcher, scorer search.CompositeScorer, options search.SearcherOptions,
	min int) (search.RawTopNResult, bool, error) {
	var result search.RawTopNResult
	composite, ok := scorer.(rawCompositeScorer)
	if !ok || min > 1 || options.Explain || options.IncludeTermVectors || options.Score == "none" {
		return result, false, nil
	}
	if _, ok := composite.ScoreCompositeSum(1); !ok {
		return result, false, nil
	}

	cursors := make([]wandTermCursor, len(searchers))
	for i, child := range searchers {
		term, ok := child.(*TermSearcher)
		if !ok {
			return result, false, nil
		}
		blocks, ok := term.reader.(segment.ImpactPostingsIterator)
		blockScorer, scorerOK := term.scorer.(impactScorer)
		if !ok || !scorerOK || !blocks.HasImpacts() {
			return result, false, nil
		}
		result.TotalCandidates += term.reader.Count()
		cursors[i] = wandTermCursor{term: term, blocks: blocks, blockScorer: blockScorer}
	}
	if size <= 0 || len(cursors) == 0 {
		return result, true, nil
	}

	matches := make(rawTopNHeap, 0, size)
	var target uint64
	var blocksVisited uint64
	for len(cursors) > 0 {
		blocksVisited++
		if blocksVisited&1023 == 0 {
			select {
			case <-ctx.Done():
				return result, true, ctx.Err()
			default:
			}
		}
		rangeEnd := uint64(math.MaxUint64)
		upperSum := 0.0
		unbounded := false
		active := cursors[:0]
		for i := range cursors {
			cursor := cursors[i]
			blockEnd, impacts, exists, err := cursor.blocks.AdvanceShallow(target)
			if err != nil {
				return result, true, err
			}
			if !exists {
				continue
			}
			if blockEnd < rangeEnd {
				rangeEnd = blockEnd
			}
			if len(impacts) == 0 {
				unbounded = true
			} else {
				upperSum += cursor.blockScorer.ScoreImpactUpperBound(impacts)
			}
			active = append(active, cursor)
		}
		cursors = active
		if len(cursors) == 0 {
			break
		}
		upperBound, _ := composite.ScoreCompositeSum(upperSum)
		if !unbounded && len(matches) == size && upperBound <= matches[0].Score {
			if rangeEnd == math.MaxUint64 {
				break
			}
			target = rangeEnd + 1
			continue
		}

		for i := range cursors {
			cursor := &cursors[i]
			if cursor.posting == nil || cursor.posting.Number() < target {
				posting, err := cursor.term.reader.Advance(target)
				if err != nil {
					return result, true, err
				}
				cursor.posting = posting
			}
		}
		for {
			candidate := uint64(math.MaxUint64)
			for i := range cursors {
				posting := cursors[i].posting
				if posting != nil && posting.Number() <= rangeEnd && posting.Number() < candidate {
					candidate = posting.Number()
				}
			}
			if candidate == math.MaxUint64 {
				break
			}

			scoreSum := 0.0
			for i := range cursors {
				cursor := &cursors[i]
				if cursor.posting == nil || cursor.posting.Number() != candidate {
					continue
				}
				scoreSum += cursor.term.scorePosting(cursor.posting)
				next, err := cursor.term.reader.Next()
				if err != nil {
					return result, true, err
				}
				cursor.posting = next
			}
			score, _ := composite.ScoreCompositeSum(scoreSum)
			result.ScoredCandidates++
			if len(matches) < size || rawTopNBetter(score, candidate, matches[0]) {
				match := searchContext.DocumentMatchPool.Get()
				match.SetReader(cursors[0].term.indexReader)
				match.Number = candidate
				match.Score = score
				match.HitNumber = int(candidate) + 1
				if removed := rawTopNAdd(&matches, size, match); removed != nil {
					searchContext.DocumentMatchPool.Put(removed)
				}
			}
			if result.ScoredCandidates&1023 == 0 {
				select {
				case <-ctx.Done():
					return result, true, ctx.Err()
				default:
				}
			}
		}
		if rangeEnd == math.MaxUint64 {
			break
		}
		target = rangeEnd + 1
	}
	result.Matches = rawTopNSorted(matches)
	return result, true, nil
}

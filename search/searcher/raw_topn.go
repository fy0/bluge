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

type rawTopNState struct {
	result  search.RawTopNResult
	matches rawTopNHeap
	context *search.Context
	size    int
}

func (s *rawTopNState) collect(reader search.Reader, number uint64, score float64) {
	s.result.ScoredCandidates++
	if len(s.matches) == s.size && !rawTopNBetter(score, number, s.matches[0]) {
		return
	}
	match := s.context.DocumentMatchPool.Get()
	match.SetReader(reader)
	match.Number, match.Score = number, score
	match.HitNumber = int(number) + 1
	if removed := rawTopNAdd(&s.matches, s.size, match); removed != nil {
		s.context.DocumentMatchPool.Put(removed)
	}
}

func (s *rawTopNState) finish() search.RawTopNResult {
	s.result.Matches = rawTopNSorted(s.matches)
	return s.result
}

func rawTopNContext(ctx context.Context, count uint64) error {
	if count&1023 == 0 {
		return ctx.Err()
	}
	return nil
}

func rawTopNComposite(scorer search.CompositeScorer, options search.SearcherOptions, min int) (rawCompositeScorer, bool) {
	composite, ok := scorer.(rawCompositeScorer)
	if !ok || min > 1 || options.Explain || options.IncludeTermVectors || options.Score == optionScoringNone {
		return nil, false
	}
	_, ok = composite.ScoreCompositeSum(1)
	return composite, ok
}

func rawTopNCursors(searchers []search.Searcher) ([]wandTermCursor, uint64, bool) {
	cursors := make([]wandTermCursor, len(searchers))
	var total uint64
	for i, child := range searchers {
		term, ok := child.(*TermSearcher)
		if !ok {
			return nil, 0, false
		}
		blocks, ok := term.reader.(segment.ImpactPostingsIterator)
		blockScorer, scorerOK := term.scorer.(impactScorer)
		if !ok || !scorerOK || !blocks.HasImpacts() {
			return nil, 0, false
		}
		total += term.reader.Count()
		cursors[i] = wandTermCursor{term: term, blocks: blocks, blockScorer: blockScorer}
	}
	return cursors, total, true
}

type wandBlock struct {
	end       uint64
	upperSum  float64
	unbounded bool
}

func wandBlockBounds(cursors []wandTermCursor, target uint64) ([]wandTermCursor, wandBlock, error) {
	bounds := wandBlock{end: math.MaxUint64}
	active := cursors[:0]
	for _, cursor := range cursors {
		end, impacts, exists, err := cursor.blocks.AdvanceShallow(target)
		if err != nil {
			return nil, bounds, err
		}
		if !exists {
			continue
		}
		bounds.end = min(bounds.end, end)
		if len(impacts) == 0 {
			bounds.unbounded = true
		} else {
			bounds.upperSum += cursor.blockScorer.ScoreImpactUpperBound(impacts)
		}
		active = append(active, cursor)
	}
	return active, bounds, nil
}

func advanceWandCursors(cursors []wandTermCursor, target uint64) error {
	for i := range cursors {
		cursor := &cursors[i]
		if cursor.posting == nil || cursor.posting.Number() < target {
			posting, err := cursor.term.reader.Advance(target)
			if err != nil {
				return err
			}
			cursor.posting = posting
		}
	}
	return nil
}

func wandCandidate(cursors []wandTermCursor, end uint64) uint64 {
	candidate := uint64(math.MaxUint64)
	for _, cursor := range cursors {
		if cursor.posting != nil && cursor.posting.Number() <= end {
			candidate = min(candidate, cursor.posting.Number())
		}
	}
	return candidate
}

func scoreWandCandidate(cursors []wandTermCursor, candidate uint64) (float64, error) {
	var score float64
	for i := range cursors {
		cursor := &cursors[i]
		if cursor.posting == nil || cursor.posting.Number() != candidate {
			continue
		}
		score += cursor.term.scorePosting(cursor.posting)
		next, err := cursor.term.reader.Next()
		if err != nil {
			return 0, err
		}
		cursor.posting = next
	}
	return score, nil
}

func scoreWandBlock(ctx context.Context, state *rawTopNState, cursors []wandTermCursor,
	end uint64, composite rawCompositeScorer) error {
	for {
		candidate := wandCandidate(cursors, end)
		if candidate == math.MaxUint64 {
			return nil
		}
		sum, err := scoreWandCandidate(cursors, candidate)
		if err != nil {
			return err
		}
		score, _ := composite.ScoreCompositeSum(sum)
		state.collect(cursors[0].term.indexReader, candidate, score)
		if err := rawTopNContext(ctx, state.result.ScoredCandidates); err != nil {
			return err
		}
	}
}

func rawTopNDisjunction(ctx context.Context, searchContext *search.Context, size int,
	searchers []search.Searcher, scorer search.CompositeScorer, options search.SearcherOptions,
	min int) (search.RawTopNResult, bool, error) {
	state := rawTopNState{context: searchContext, size: size}
	composite, ok := rawTopNComposite(scorer, options, min)
	if !ok {
		return state.result, false, nil
	}
	cursors, total, ok := rawTopNCursors(searchers)
	state.result.TotalCandidates = total
	if !ok || size <= 0 || len(cursors) == 0 {
		return state.result, ok, nil
	}
	state.matches = make(rawTopNHeap, 0, size)
	var target, visited uint64
	for len(cursors) > 0 {
		visited++
		if err := rawTopNContext(ctx, visited); err != nil {
			return state.result, true, err
		}
		active, bounds, err := wandBlockBounds(cursors, target)
		if err != nil {
			return state.result, true, err
		}
		cursors = active
		if len(cursors) == 0 {
			break
		}
		upperBound, _ := composite.ScoreCompositeSum(bounds.upperSum)
		if bounds.unbounded || len(state.matches) < size || upperBound > state.matches[0].Score {
			if err := advanceWandCursors(cursors, target); err != nil {
				return state.result, true, err
			}
			if err := scoreWandBlock(ctx, &state, cursors, bounds.end, composite); err != nil {
				return state.result, true, err
			}
		}
		if bounds.end == math.MaxUint64 {
			break
		}
		target = bounds.end + 1
	}
	return state.finish(), true, nil
}

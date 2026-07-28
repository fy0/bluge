// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fy0/bluge/index/mergeplan"
	"github.com/fy0/bluge/search"
	"github.com/fy0/bluge/search/collector"
	"github.com/fy0/bluge/search/similarity"
	"github.com/fy0/bluge/segment"
)

type noImpactSimilarity struct{ search.Similarity }

func (s noImpactSimilarity) UsesQueryNorm() bool {
	weighted, ok := s.Similarity.(interface{ UsesQueryNorm() bool })
	return ok && weighted.UsesQueryNorm()
}

func (s noImpactSimilarity) Scorer(boost float64, collectionStats segment.CollectionStats,
	termStats segment.TermStats) search.Scorer {
	return noImpactScorer{s.Similarity.Scorer(boost, collectionStats, termStats)}
}

type noImpactScorer struct{ inner search.Scorer }

func (s noImpactScorer) Score(freq int, norm float64) float64 {
	return s.inner.Score(freq, norm)
}

func (s noImpactScorer) Explain(freq int, norm float64) *search.Explanation {
	return s.inner.Explain(freq, norm)
}

func (s noImpactScorer) QueryNormWeight() (float64, bool) {
	weighted, ok := s.inner.(interface {
		QueryNormWeight() (float64, bool)
	})
	if !ok {
		return 0, false
	}
	return weighted.QueryNormWeight()
}

func (s noImpactScorer) SetQueryNorm(queryNorm float64) {
	if weighted, ok := s.inner.(interface{ SetQueryNorm(float64) }); ok {
		weighted.SetQueryNorm(queryNorm)
	}
}

type wandTestHit struct {
	id    string
	score float64
}

func TestBM25RawTopNMatchesBaselineAndSkipsCandidates(t *testing.T) {
	config := InMemoryOnlyConfig()
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	batch := NewBatch()
	for i := 0; i < 1000; i++ {
		body := ""
		if i < 64 {
			body = strings.TrimSpace(strings.Repeat("alpha ", 20))
		} else {
			body = "alpha " + strings.TrimSpace(strings.Repeat("padding ", 100))
		}
		batch.Insert(NewDocument(fmt.Sprintf("%04d", i)).AddField(NewTextField("body", body)))
	}
	if err := writer.Batch(batch); err != nil {
		t.Fatal(err)
	}
	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	baselineConfig := config
	baselineConfig.DefaultSimilarity = noImpactSimilarity{config.DefaultSimilarity}
	baseline, baselineCollector := collectWANDTestHits(t, reader, baselineConfig)
	if baselineCollector.TotalCandidates() != 0 || baselineCollector.ScoredCandidates() != 0 {
		t.Fatal("baseline unexpectedly used Raw Top-N")
	}

	for run := 0; run < 2; run++ {
		got, fastCollector := collectWANDTestHits(t, reader, config)
		if len(got) != len(baseline) {
			t.Fatalf("run %d: got %d hits want %d", run, len(got), len(baseline))
		}
		for i := range baseline {
			if got[i] != baseline[i] {
				t.Fatalf("run %d hit %d: got %+v want %+v", run, i, got[i], baseline[i])
			}
		}
		if got, total := fastCollector.ScoredCandidates(), fastCollector.TotalCandidates(); total != 1000 || got > total/5 {
			t.Fatalf("run %d: scored %d of %d candidates, want at least 80%% reduction", run, got, total)
		}
	}
}

func TestBM25RawTopNExactAcrossSegmentsAndDeletions(t *testing.T) {
	config := InMemoryOnlyConfig()
	config.indexConfig.MinSegmentsForInMemoryMerge = 1000
	config.indexConfig.MergePlanOptions.CalcBudget = func(_ int64, _ int64, _ *mergeplan.Options) int {
		return 1000
	}
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	for segmentNumber := 0; segmentNumber < 10; segmentNumber++ {
		batch := NewBatch()
		for docNumber := 0; docNumber < 256; docNumber++ {
			id := fmt.Sprintf("s%02d-d%03d", segmentNumber, docNumber)
			body := "alpha " + strings.TrimSpace(strings.Repeat("padding ", 100))
			if docNumber < 16 {
				body = strings.TrimSpace(strings.Repeat("alpha ", 20))
			}
			batch.Insert(NewDocument(id).AddField(NewTextField("body", body)))
		}
		if err := writer.Batch(batch); err != nil {
			t.Fatal(err)
		}
	}

	lowCardinality := NewBatch()
	for docNumber := 0; docNumber < 40; docNumber++ {
		id := fmt.Sprintf("rare-segment-%03d", docNumber)
		lowCardinality.Insert(NewDocument(id).AddField(NewTextField("body", "alpha padding")))
	}
	if err := writer.Batch(lowCardinality); err != nil {
		t.Fatal(err)
	}

	deletions := NewBatch()
	for docNumber := 0; docNumber < 10; docNumber++ {
		deletions.Delete(Identifier(fmt.Sprintf("s00-d%03d", docNumber)))
	}
	deletions.Update(Identifier("s01-d000"),
		NewDocument("s01-d000").AddField(NewTextField("body", "alpha replacement padding")))
	if err := writer.Batch(deletions); err != nil {
		t.Fatal(err)
	}

	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	segments := reader.reader.Segments()
	if len(segments) < 10 {
		t.Fatalf("expected a multi-segment snapshot, got %d segments", len(segments))
	}
	var sawDeletion bool
	for _, snapshot := range segments {
		if deleted := snapshot.Deleted(); deleted != nil && !deleted.IsEmpty() {
			sawDeletion = true
			break
		}
	}
	if !sawDeletion {
		t.Fatal("expected logical deletions in snapshot")
	}

	modes := []search.Similarity{
		similarity.NewBM25Similarity(),
		similarity.NewLegacyBM25Similarity(),
		similarity.NewBleveBM25Similarity(),
	}
	for modeNumber, mode := range modes {
		fastConfig := config
		fastConfig.DefaultSimilarity = mode
		baselineConfig := fastConfig
		baselineConfig.DefaultSimilarity = noImpactSimilarity{mode}
		for _, size := range []int{1, 5, 20, 100} {
			baseline, _ := collectWANDQueryHits(t, reader, baselineConfig, "alpha", size)
			got, fastCollector := collectWANDQueryHits(t, reader, fastConfig, "alpha", size)
			if len(got) != len(baseline) {
				t.Fatalf("mode %d size %d: got %d hits want %d", modeNumber, size, len(got), len(baseline))
			}
			for i := range baseline {
				if got[i] != baseline[i] {
					t.Fatalf("mode %d size %d hit %d: got %+v want %+v",
						modeNumber, size, i, got[i], baseline[i])
				}
			}
			if size == 5 && fastCollector.ScoredCandidates()*5 > fastCollector.TotalCandidates() {
				t.Fatalf("mode %d: scored %d of %d candidates, want at least 80%% reduction",
					modeNumber, fastCollector.ScoredCandidates(), fastCollector.TotalCandidates())
			}
		}

		orQuery := NewMatchQuery("alpha padding").SetField("body").SetBoost(2.5)
		baseline, _ := collectWANDRequestHits(t, reader, baselineConfig,
			NewTopNSearch(20, orQuery))
		got, fastCollector := collectWANDRequestHits(t, reader, fastConfig,
			NewTopNSearch(20, orQuery))
		if len(got) != len(baseline) {
			t.Fatalf("OR mode %d: got %d hits want %d", modeNumber, len(got), len(baseline))
		}
		for i := range baseline {
			if got[i] != baseline[i] {
				t.Fatalf("OR mode %d hit %d: got %+v want %+v", modeNumber, i, got[i], baseline[i])
			}
		}
		if fastCollector.TotalCandidates() == 0 || fastCollector.ScoredCandidates() == 0 {
			t.Fatalf("OR mode %d did not use BM25 WAND", modeNumber)
		}
	}
}

func collectWANDTestHits(t *testing.T, reader *Reader, config Config) (
	[]wandTestHit, *collector.TopNCollector) {
	return collectWANDQueryHits(t, reader, config, "alpha", 10)
}

func collectWANDQueryHits(t *testing.T, reader *Reader, config Config, term string, size int) (
	[]wandTestHit, *collector.TopNCollector) {
	return collectWANDRequestHits(t, reader, config,
		NewTopNSearch(size, NewTermQuery(term).SetField("body")))
}

func collectWANDRequestHits(t *testing.T, reader *Reader, config Config,
	request *TopNSearch) ([]wandTestHit, *collector.TopNCollector) {
	t.Helper()
	searcher, err := request.Searcher(reader.reader, config)
	if err != nil {
		t.Fatal(err)
	}
	topN := request.Collector().(*collector.TopNCollector)
	iterator, err := topN.Collect(context.Background(), request.Aggregations(), searcher)
	if err != nil {
		t.Fatal(err)
	}

	var hits []wandTestHit
	for {
		match, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		if match == nil {
			break
		}
		hit := wandTestHit{score: match.Score}
		if err := match.VisitStoredFields(func(field string, value []byte) bool {
			if field == "_id" {
				hit.id = string(value)
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		hits = append(hits, hit)
	}
	return hits, topN
}

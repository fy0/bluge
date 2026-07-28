// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package zap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	scorchseg "github.com/blevesearch/scorch_segment_api/v2"
	blugeseg "github.com/fy0/bluge/segment"
)

func TestV2ImpactPostingsRoundTrip(t *testing.T) {
	documents := makeImpactTestDocuments(130)
	segment := newStatsTestSegment(t, documents...)

	var encoded bytes.Buffer
	if _, err := segment.(*SegmentBase).WriteTo(&encoded); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBytes(encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.(*SegmentBase).FormatVersion(); got != Version {
		t.Fatalf("format version: got %d want %d", got, Version)
	}

	iterator := impactTestIterator(t, loaded.(*SegmentBase), "alpha")
	if !iterator.HasImpacts() {
		t.Fatal("expected block impacts for high-cardinality term")
	}

	ends := []uint64{63, 127, 129}
	starts := []uint64{0, 64, 128}
	for block := range ends {
		end, impacts, ok, err := iterator.AdvanceShallow(starts[block])
		if err != nil {
			t.Fatal(err)
		}
		if !ok || end != ends[block] {
			t.Fatalf("block %d: got end=%d ok=%v want end=%d", block, end, ok, ends[block])
		}
		assertExactImpactFrontier(t, impacts, starts[block], ends[block])
	}
	if _, _, ok, err := iterator.AdvanceShallow(130); err != nil || ok {
		t.Fatalf("expected exhausted impacts, ok=%v err=%v", ok, err)
	}
}

func TestV2PersistedSegmentFormatVersionAndImpacts(t *testing.T) {
	segment := newStatsTestSegment(t, makeImpactTestDocuments(130)...)
	path := filepath.Join(t.TempDir(), "segment.zap")
	if err := segment.(*SegmentBase).Persist(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := new(ZapPlugin).Open(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted := loaded.(*Segment)
	defer func() {
		if err := persisted.Close(); err != nil {
			t.Errorf("close persisted segment: %v", err)
		}
	}()

	if got := persisted.FormatVersion(); got != Version {
		t.Fatalf("format version: got %d want %d", got, Version)
	}
	if got := persisted.Version(); got != Version {
		t.Fatalf("footer version: got %d want %d", got, Version)
	}
	if iterator := impactTestIterator(t, &persisted.SegmentBase, "alpha"); !iterator.HasImpacts() {
		t.Fatal("persisted high-cardinality term lost impacts")
	}
}

func TestV2RareTermDoesNotCarryImpacts(t *testing.T) {
	segment := newStatsTestSegment(t, makeImpactTestDocuments(impactMinPostings-1)...)
	iterator := impactTestIterator(t, segment.(*SegmentBase), "alpha")
	if iterator.HasImpacts() {
		t.Fatal("rare term unexpectedly has impact metadata")
	}
	posting, err := iterator.Next()
	if err != nil || posting == nil {
		t.Fatalf("ordinary postings fallback failed: posting=%v err=%v", posting, err)
	}
}

func TestV2RejectsV1Segment(t *testing.T) {
	segment := newStatsTestSegment(t, newStatsTestDocument("1", "alpha"))
	var encoded bytes.Buffer
	if _, err := segment.(*SegmentBase).WriteTo(&encoded); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	binary.BigEndian.PutUint32(data[len(data)-8:len(data)-4], 1)
	_, err := LoadBytes(data)
	if err == nil || !strings.Contains(err.Error(), "unsupported version 1 != 2") {
		t.Fatalf("unexpected v1 load result: %v", err)
	}

	path := filepath.Join(t.TempDir(), "v1-segment.zap")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = new(ZapPlugin).Open(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported version 1 != 2") {
		t.Fatalf("unexpected persisted v1 load result: %v", err)
	}
}

func TestV2ImpactPostingsSurviveMergeAndDrops(t *testing.T) {
	first := newStatsTestSegment(t, makeImpactTestDocuments(130)...)
	second := newStatsTestSegment(t, makeImpactTestDocuments(130)...)
	var encoded bytes.Buffer
	_, _, err := MergeToWriter([]scorchseg.Segment{first, second},
		[]*roaring.Bitmap{roaring.BitmapOf(0, 64), nil}, &encoded, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := LoadBytes(encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	segment := merged.(*SegmentBase)
	shallow := impactTestIterator(t, segment, "alpha")
	if !shallow.HasImpacts() {
		t.Fatal("merged high-cardinality term lost impacts")
	}

	var target uint64
	var blocks int
	for {
		end, impacts, ok, err := shallow.AdvanceShallow(target)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		assertImpactFrontierBoundsPostings(t, segment, target, end, impacts)
		blocks++
		target = end + 1
	}
	if blocks != 5 {
		t.Fatalf("merged impact blocks: got %d want 5", blocks)
	}
}

func TestV2DensePostingsSequentialCursorAfterAdvance(t *testing.T) {
	segment := newStatsTestSegment(t, makeImpactTestDocuments(256)...).(*SegmentBase)
	iterator := impactTestIterator(t, segment, "alpha")

	posting, err := iterator.Advance(130)
	if err != nil {
		t.Fatal(err)
	}
	assertImpactTestPosting(t, posting, 130)
	posting, err = iterator.Next()
	if err != nil {
		t.Fatal(err)
	}
	assertImpactTestPosting(t, posting, 131)

	posting, err = iterator.Advance(190)
	if err != nil {
		t.Fatal(err)
	}
	assertImpactTestPosting(t, posting, 190)
	posting, err = iterator.Next()
	if err != nil {
		t.Fatal(err)
	}
	assertImpactTestPosting(t, posting, 191)
}

func assertImpactTestPosting(t *testing.T, posting scorchseg.Posting, docNum uint64) {
	t.Helper()
	if posting == nil {
		t.Fatalf("posting %d is nil", docNum)
	}
	if got := posting.Number(); got != docNum {
		t.Fatalf("posting number: got %d want %d", got, docNum)
	}
	if got, want := posting.Frequency(), docNum%5+1; got != want {
		t.Fatalf("posting %d frequency: got %d want %d", docNum, got, want)
	}
}

func makeImpactTestDocuments(count int) []blugeseg.Document {
	documents := make([]blugeseg.Document, count)
	for i := range documents {
		freq := i%5 + 1
		padding := i % 7
		body := strings.TrimSpace(strings.Repeat("alpha ", freq) + strings.Repeat("pad ", padding))
		documents[i] = newStatsTestDocument(fmt.Sprintf("%d", i), body)
	}
	return documents
}

func impactTestIterator(t *testing.T, segment *SegmentBase, term string) *PostingsIterator {
	t.Helper()
	dictionary, err := segment.dictionary("body")
	if err != nil {
		t.Fatal(err)
	}
	postings, err := dictionary.postingsList([]byte(term), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return postings.iterator(true, true, false, nil)
}

func assertExactImpactFrontier(t *testing.T, impacts []blugeseg.Impact, start, end uint64) {
	t.Helper()
	actual := make(map[blugeseg.Impact]bool)
	for docNum := start; docNum <= end; docNum++ {
		freq := docNum%5 + 1
		norm := freq + docNum%7
		actual[blugeseg.Impact{Frequency: freq, Norm: norm}] = true
	}

	for _, impact := range impacts {
		if !actual[impact] {
			t.Fatalf("impact %+v does not occur in block %d..%d", impact, start, end)
		}
		for other := range actual {
			if other != impact && other.Frequency >= impact.Frequency && other.Norm <= impact.Norm {
				t.Fatalf("impact %+v is dominated by %+v", impact, other)
			}
		}
	}
	for pair := range actual {
		var dominated bool
		for _, impact := range impacts {
			if impact.Frequency >= pair.Frequency && impact.Norm <= pair.Norm {
				dominated = true
				break
			}
		}
		if !dominated {
			t.Fatalf("pair %+v is not bounded by frontier %v", pair, impacts)
		}
	}
}

func assertImpactFrontierBoundsPostings(t *testing.T, segment *SegmentBase,
	start, end uint64, impacts []blugeseg.Impact) {
	t.Helper()
	iterator := impactTestIterator(t, segment, "alpha")
	posting, err := iterator.Advance(start)
	if err != nil {
		t.Fatal(err)
	}
	for posting != nil && posting.Number() <= end {
		raw, ok := posting.(*Posting)
		if !ok {
			t.Fatalf("unexpected posting type %T", posting)
		}
		var dominated bool
		for _, impact := range impacts {
			if impact.Frequency >= posting.Frequency() && impact.Norm <= raw.NormUint64() {
				dominated = true
				break
			}
		}
		if !dominated {
			t.Fatalf("posting doc=%d freq=%d norm=%d is not bounded by %v",
				posting.Number(), posting.Frequency(), raw.NormUint64(), impacts)
		}
		posting, err = iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
	}
}

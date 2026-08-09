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
	"errors"
	"testing"
)

func TestVectorSearchRequestAndHybridSearch(t *testing.T) {
	config := InMemoryOnlyConfig().WithVectorBackend(NewFlatVectorBackend(""))
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	documents := []*Document{
		NewDocument("red").
			AddField(NewTextField("body", "red apple")).
			AddField(NewKeywordField("kind", "fruit")).
			AddField(NewVectorField("embedding", []float32{1, 0})),
		NewDocument("green").
			AddField(NewTextField("body", "green apple")).
			AddField(NewKeywordField("kind", "fruit")).
			AddField(NewVectorField("embedding", []float32{0.8, 0.2})),
		NewDocument("blue").
			AddField(NewTextField("body", "blue car")).
			AddField(NewKeywordField("kind", "vehicle")).
			AddField(NewVectorField("embedding", []float32{0, 1})),
	}
	for _, document := range documents {
		if err := writer.Insert(document); err != nil {
			t.Fatal(err)
		}
	}

	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	vectorRequest := NewVectorSearchRequest("embedding", []float32{1, 0}).
		SetK(2).
		SetFilter(NewTermQuery("fruit").SetField("kind"))
	vectorHits, err := reader.SearchVectorRequest(context.Background(), vectorRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorIDs(t, vectorHits, "red", "green")

	hybrid := NewHybridSearchRequest(
		NewMatchQuery("apple").SetField("body"),
		NewVectorSearchRequest("embedding", []float32{0, 1}),
	).SetK(3).
		SetFilter(NewTermQuery("fruit").SetField("kind")).
		SetFusion(HybridFusionRRF).
		SetTextCandidates(2).
		SetVectorCandidates(2)
	hybridHits, err := reader.HybridSearch(context.Background(), hybrid)
	if err != nil {
		t.Fatal(err)
	}
	if len(hybridHits) != 2 {
		t.Fatalf("got %d hybrid hits, expected 2: %v", len(hybridHits), hybridHits)
	}
	if hybridHits[0].ID != "green" || hybridHits[1].ID != "red" {
		t.Fatalf("unexpected hybrid order: %v", hybridHits)
	}
	for _, hit := range hybridHits {
		if hit.TextRank == 0 || hit.VectorRank == 0 {
			t.Fatalf("expected both branch ranks in hybrid hit: %+v", hit)
		}
	}
}

func TestSearchRequestValidation(t *testing.T) {
	config := InMemoryOnlyConfig().WithVectorBackend(NewFlatVectorBackend(""))
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := writer.Reader()
	if err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	_, err = reader.SearchVectorRequest(context.Background(), &VectorSearchRequest{K: 1})
	if !errors.Is(err, ErrVectorInvalidRequest) {
		t.Fatalf("expected vector request validation error, got %v", err)
	}
	_, err = reader.HybridSearch(context.Background(), &HybridSearchRequest{K: 1})
	if !errors.Is(err, ErrHybridInvalidRequest) {
		t.Fatalf("expected hybrid request validation error, got %v", err)
	}
}

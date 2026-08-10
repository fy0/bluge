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
	"testing"
)

func TestWriterInsertManyAndUpdateMany(t *testing.T) {
	writer, err := OpenWriter(InMemoryOnlyConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	documents := []*Document{
		NewDocument("one").AddField(NewTextField("body", "first version")),
		NewDocument("two").AddField(NewTextField("body", "second version")),
	}
	if err := writer.InsertMany(documents); err != nil {
		t.Fatal(err)
	}
	if err := writer.UpdateMany([]*Document{
		NewDocument("one").AddField(NewTextField("body", "updated first")),
		NewDocument("two").AddField(NewTextField("body", "updated second")),
	}); err != nil {
		t.Fatal(err)
	}

	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	count, err := reader.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("got %d documents, expected 2", count)
	}
	results, err := reader.Search(context.Background(), NewTopNSearch(10,
		NewTermQuery("updated").SetField("body")).WithStandardAggregations())
	if err != nil {
		t.Fatal(err)
	}
	if results.Aggregations().Count() != 2 {
		t.Fatalf("updated text matched %d documents, expected 2",
			results.Aggregations().Count())
	}
}

func TestWriterBulkInputValidationPrecedesSubmission(t *testing.T) {
	writer, err := OpenWriter(InMemoryOnlyConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	if err := writer.InsertMany([]*Document{NewDocument("valid"), nil}); err == nil {
		t.Fatal("expected nil bulk document to be rejected")
	}
	missingID := Document{NewTextField("body", "missing identifier")}
	if err := writer.UpdateMany([]*Document{&missingID}); err == nil {
		t.Fatal("expected update without _id to be rejected")
	}

	reader, err := writer.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	count, err := reader.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid bulk input submitted %d documents", count)
	}
}

type textFastPathTrackingField struct {
	Field
	analysisStarted      bool
	preAnalysisNameCalls int
}

func (f *textFastPathTrackingField) Index() bool {
	f.analysisStarted = true
	return f.Field.Index()
}

func (f *textFastPathTrackingField) Name() string {
	if !f.analysisStarted {
		f.preAnalysisNameCalls++
	}
	return f.Field.Name()
}

func TestWriterTextOnlyFastPathAvoidsPreAnalysisFieldAccess(t *testing.T) {
	writer, err := OpenWriter(InMemoryOnlyConfig())
	if err != nil {
		t.Fatal(err)
	}

	field := &textFastPathTrackingField{Field: NewTextField("body", "text only")}
	document := NewDocument("text").AddField(field)
	if err := writer.InsertMany([]*Document{document}); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if field.preAnalysisNameCalls != 0 {
		t.Fatalf("text-only vector preflight accessed fields %d times before analysis",
			field.preAnalysisNameCalls)
	}
}

func TestOfflineWriterTextOnlyFastPathAvoidsPreAnalysisFieldAccess(t *testing.T) {
	indexPath := createTmpIndexPath(t)
	defer cleanupTmpIndexPath(t, indexPath)

	writer, err := OpenOfflineWriter(DefaultConfig(indexPath), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	field := &textFastPathTrackingField{Field: NewTextField("body", "text only")}
	document := NewDocument("text").AddField(field)
	if err := writer.InsertMany([]*Document{document}); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if field.preAnalysisNameCalls != 0 {
		t.Fatalf("offline text-only vector preflight accessed fields %d times before analysis",
			field.preAnalysisNameCalls)
	}
}

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

func TestBulkSinglePathStoredFieldsAndIDSort(t *testing.T) {
	tmpIndexPath := createTmpIndexPath(t)
	defer cleanupTmpIndexPath(t, tmpIndexPath)

	config := DefaultConfig(tmpIndexPath)
	writer, err := OpenOfflineWriter(config, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"doc-2", "doc-1"} {
		doc := NewDocument(id).
			AddField(NewTextField("file", "sample.src").StoreValue()).
			AddField(NewKeywordField("side", "src").StoreValue()).
			AddField(NewTextField("body", "LOCATION token").StoreValue())
		if err := writer.Insert(doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReader(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close reader: %v", err)
		}
	}()

	request := NewTopNSearch(10, NewMatchAllQuery()).SortBy([]string{"_id"})
	iterator, err := reader.Search(context.Background(), request)
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
		fields := map[string]string{}
		if err := match.VisitStoredFields(func(field string, value []byte) bool {
			fields[field] = string(value)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, fields["_id"])
		if fields["file"] != "sample.src" || fields["side"] != "src" ||
			fields["body"] != "LOCATION token" {
			t.Fatalf("unexpected stored fields: %#v", fields)
		}
	}
	if len(ids) != 2 || ids[0] != "doc-1" || ids[1] != "doc-2" {
		t.Fatalf("expected _id doc-value order [doc-1 doc-2], got %v", ids)
	}
}

func TestBulkSinglePathDoesNotMutateMultiValueTokens(t *testing.T) {
	tmpIndexPath := createTmpIndexPath(t)
	defer cleanupTmpIndexPath(t, tmpIndexPath)

	first := NewTextField("body", "alpha").SearchTermPositions()
	second := NewTextField("body", "alpha").SearchTermPositions()
	doc := NewDocument("doc-1").AddField(first).AddField(second)
	writer, err := OpenOfflineWriter(DefaultConfig(tmpIndexPath), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Insert(doc); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	for i, field := range []*TermField{first, second} {
		frequency := field.AnalyzedTokenFrequencies()["alpha"]
		if frequency == nil || frequency.Frequency() != 1 {
			t.Fatalf("field %d token frequencies were mutated: %#v", i, frequency)
		}
		if len(frequency.Locations) != 1 || frequency.Locations[0].Field() != "" {
			t.Fatalf("field %d token locations were mutated: %#v", i, frequency.Locations)
		}
	}
}

func TestBulkSinglePathCompositeField(t *testing.T) {
	tmpIndexPath := createTmpIndexPath(t)
	defer cleanupTmpIndexPath(t, tmpIndexPath)

	config := DefaultConfig(tmpIndexPath)
	writer, err := OpenOfflineWriter(config, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	doc := NewDocument("doc-1").
		AddField(NewTextField("title", "search engine")).
		AddField(NewCompositeFieldExcluding("_all", []string{"_id"}))
	if err := writer.Insert(doc); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReader(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	iterator, err := reader.Search(context.Background(),
		NewTopNSearch(10, NewTermQuery("engine").SetField("_all")))
	if err != nil {
		t.Fatal(err)
	}
	match, err := iterator.Next()
	if err != nil {
		t.Fatal(err)
	}
	if match == nil {
		t.Fatal("expected composite field query hit")
	}
}

func TestOfflineWriterValidatesConfigurationAndState(t *testing.T) {
	tmpIndexPath := createTmpIndexPath(t)
	defer cleanupTmpIndexPath(t, tmpIndexPath)
	config := DefaultConfig(tmpIndexPath)

	if _, err := OpenOfflineWriter(config, 0, 2); err == nil {
		t.Fatal("expected invalid batch size error")
	}
	if _, err := OpenOfflineWriter(config, 1, 1); err == nil {
		t.Fatal("expected invalid merge max error")
	}

	writer, err := OpenOfflineWriter(config, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err == nil {
		t.Fatal("expected empty offline writer error")
	}
	if err := writer.Close(); err == nil {
		t.Fatal("expected repeated close error")
	}
	if err := writer.Insert(NewDocument("late")); err == nil {
		t.Fatal("expected insert-after-close error")
	}
}

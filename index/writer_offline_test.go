// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package index

import (
	"runtime"
	"strings"
	"testing"
	"time"

	segment "github.com/fy0/bluge/segment"
)

type blockingOfflineDocument struct {
	document *FakeDocument
	started  chan<- struct{}
	release  <-chan struct{}
}

func (d *blockingOfflineDocument) Analyze() {
	d.started <- struct{}{}
	<-d.release
}

func (d *blockingOfflineDocument) EachField(visitor segment.VisitField) {
	d.document.EachField(visitor)
}

func TestOfflineWriterBuildsBatchesConcurrently(t *testing.T) {
	config := InMemoryOnlyConfig()
	writer, err := OpenOfflineWriterWithMergeMax(config, 2)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	for _, id := range []string{"1", "2"} {
		document := FakeDocument{
			NewFakeField("_id", id, true, false, true),
			NewFakeField("body", "alpha", false, false, false),
		}
		batch := NewBatch()
		batch.Insert(&blockingOfflineDocument{
			document: &document,
			started:  started,
			release:  release,
		})
		if err := writer.Batch(batch); err != nil {
			close(release)
			t.Fatal(err)
		}
	}

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("timed out waiting for concurrent offline builds")
		}
	}
	close(release)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineWriterHonorsConfiguredConcurrency(t *testing.T) {
	previousMaxProcs := runtime.GOMAXPROCS(3)
	defer runtime.GOMAXPROCS(previousMaxProcs)

	config := InMemoryOnlyConfig().WithOfflineWriterConcurrency(3)
	writer, err := OpenOfflineWriterWithMergeMax(config, 2)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, 3)
	release := make(chan struct{})
	for _, id := range []string{"1", "2", "3"} {
		document := FakeDocument{
			NewFakeField("_id", id, true, false, true),
			NewFakeField("body", "alpha", false, false, false),
		}
		batch := NewBatch()
		batch.Insert(&blockingOfflineDocument{
			document: &document,
			started:  started,
			release:  release,
		})
		if err := writer.Batch(batch); err != nil {
			close(release)
			t.Fatal(err)
		}
	}

	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("timed out waiting for configured offline builds")
		}
	}
	close(release)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineWriterReportsAsyncBuildError(t *testing.T) {
	writer, err := OpenOfflineWriterWithMergeMax(InMemoryOnlyConfig(), 2)
	if err != nil {
		t.Fatal(err)
	}
	document := FakeDocument{
		NewFakeField("body", "missing id", false, false, false),
	}
	batch := NewBatch()
	batch.Insert(&document)
	if err := writer.Batch(batch); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err == nil || !strings.Contains(err.Error(), "missing _id") {
		t.Fatalf("expected missing _id build error, got %v", err)
	}
}

func TestOfflineWriterRejectsInvalidMergeMax(t *testing.T) {
	_, err := OpenOfflineWriterWithMergeMax(InMemoryOnlyConfig(), 1)
	if err == nil {
		t.Fatal("expected invalid merge max error")
	}
}

func TestOfflineWriterRejectsInvalidConcurrency(t *testing.T) {
	config := InMemoryOnlyConfig().WithOfflineWriterConcurrency(0)
	_, err := OpenOfflineWriter(config)
	if err == nil || !strings.Contains(err.Error(), "concurrency") {
		t.Fatalf("expected invalid concurrency error, got %v", err)
	}
}

func TestOfflineWriterCloseWithoutSegments(t *testing.T) {
	writer, err := OpenOfflineWriter(InMemoryOnlyConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err == nil || !strings.Contains(err.Error(), "no segments") {
		t.Fatalf("expected no segments error, got %v", err)
	}
}

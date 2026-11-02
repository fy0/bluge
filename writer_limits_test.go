// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/internal/zapxbluge"
	"github.com/fy0/bluge/segment"
)

type inputTestDocument struct {
	*Document
	analyze func()
}

func (d *inputTestDocument) Analyze() {
	d.analyze()
	d.Document.Analyze()
}

// Gates hold a real write at analysis, segment construction, or call completion.
// Cleanup releases the gate even when an assertion fails.
type batchTestGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBatchTestGate(t *testing.T) *batchTestGate {
	g := &batchTestGate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *batchTestGate) block() {
	close(g.entered)
	<-g.release
}

func (g *batchTestGate) open() { g.once.Do(func() { close(g.release) }) }

func awaitBatchSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not reach the expected stage")
	}
}

func awaitBatchResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("write or close did not finish")
		return nil
	}
}

func assertBatchWaiting(t *testing.T, analyzed <-chan struct{}) {
	t.Helper()
	select {
	case <-analyzed:
		t.Fatal("waiting batch reached analysis before a slot was released")
	case <-time.After(30 * time.Millisecond):
	}
}

func assertBatchStats(t *testing.T, w *Writer, count, bytes uint64) {
	t.Helper()
	want := BatchStats{InFlightBatches: count, InFlightInputBytes: bytes}
	if got := w.BatchStats(); got != want {
		t.Fatalf("batch stats = %+v, want %+v", got, want)
	}
}

func TestWriterBatchDefaultAllowsConcurrentCalls(t *testing.T) {
	w, err := OpenWriter(InMemoryOnlyConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	first, second := newBatchTestGate(t), newBatchTestGate(t)
	done := make(chan error, 2)
	go func() { done <- w.Insert(&inputTestDocument{NewDocument("one"), first.block}) }()
	awaitBatchSignal(t, first.entered)
	go func() { done <- w.Insert(&inputTestDocument{NewDocument("two"), second.block}) }()
	awaitBatchSignal(t, second.entered)
	assertBatchStats(t, w, 0, 0) // Statistics are opt-in.
	first.open()
	second.open()
	for i := 0; i < 2; i++ {
		if err := awaitBatchResult(t, done); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWriterBatchSlotCoversWholeCall(t *testing.T) {
	path := t.TempDir()
	var w *Writer
	t.Cleanup(func() {
		if w != nil {
			_ = w.Close()
		}
	})
	analysis, build, finish := newBatchTestGate(t), newBatchTestGate(t), newBatchTestGate(t)
	var builds, finishes atomic.Int32
	config := DefaultConfig(path).WithMaxConcurrentBatches(1).WithBatchStats(true)
	config.indexConfig = config.indexConfig.WithSegmentPlugin(&index.SegmentPlugin{
		Type: zapxbluge.Type, Version: zapxbluge.Version,
		New: func(docs []segment.Document, norm func(string, int) float32) (segment.Segment, uint64, error) {
			if builds.Add(1) == 1 {
				build.block()
			}
			return zapxbluge.New(docs, norm)
		},
		Load: zapxbluge.Load, Merge: zapxbluge.Merge,
	})
	config.indexConfig.EventCallback = func(event index.Event) {
		if event.Kind == index.EventKindBatchIntroduction && finishes.Add(1) == 1 {
			finish.block()
		}
	}
	var err error
	w, err = OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- w.Insert(&inputTestDocument{NewDocument("one"), analysis.block}) }()
	awaitBatchSignal(t, analysis.entered)
	secondAnalyzed, secondCalled := make(chan struct{}), make(chan struct{})
	go func() {
		close(secondCalled)
		secondDone <- w.Insert(&inputTestDocument{NewDocument("two"), func() { close(secondAnalyzed) }})
	}()
	awaitBatchSignal(t, secondCalled)
	for _, gate := range []*batchTestGate{analysis, build, finish} {
		awaitBatchSignal(t, gate.entered)
		assertBatchWaiting(t, secondAnalyzed)
		assertBatchStats(t, w, 1, 6) // len("_id") + len("one").
		gate.open()
	}
	if err := awaitBatchResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitBatchResult(t, secondDone); err != nil {
		t.Fatal(err)
	}
	assertBatchStats(t, w, 0, 0)
}

func TestWriterBatchInputLimitsAreAtomic(t *testing.T) {
	w, err := OpenWriter(InMemoryOnlyConfig().WithMaxConcurrentBatches(1).
		WithMaxDocumentInputBytes(12).WithMaxBatchInputBytes(24).WithBatchStats(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err := w.Insert(NewDocument("old").AddField(NewTextField("body", "ok"))); err != nil {
		t.Fatal(err) // Exactly 12 document bytes.
	}
	var analyses int
	document := func(id, body string) *inputTestDocument {
		return &inputTestDocument{NewDocument(id).AddField(NewTextField("body", body)), func() { analyses++ }}
	}
	reject := func(batch *index.Batch, scope string, actual, limit uint64) {
		t.Helper()
		err := w.Batch(batch)
		var limitErr *InputLimitError
		if !errors.As(err, &limitErr) || limitErr.Scope != scope || limitErr.Actual != actual || limitErr.Limit != limit {
			t.Fatalf("got %v, want %s limit error with actual=%d limit=%d", err, scope, actual, limit)
		}
		if analyses != 0 {
			t.Fatal("rejected input was analyzed")
		}
		assertBatchStats(t, w, 0, 0)
		reader, err := w.Reader()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		count, err := reader.Count()
		if err != nil || count != 1 {
			t.Fatalf("rejected batch changed index: count=%d err=%v", count, err)
		}
		var id string
		if err := reader.VisitStoredFields(0, func(name string, value []byte) bool {
			if name == "_id" {
				id = string(value)
			}
			return true
		}); err != nil || id != "old" {
			t.Fatalf("original document not preserved: id=%q err=%v", id, err)
		}
	}
	tooLargeDocument := NewBatch()
	tooLargeDocument.Delete(Identifier("old"))
	tooLargeDocument.Insert(document("new", "bad"))
	reject(tooLargeDocument, "document", 13, 12)

	tooLargeBatch := NewBatch()
	tooLargeBatch.Delete(Identifier("old"))
	tooLargeBatch.Insert(document("two", "ok"))
	tooLargeBatch.Insert(document("tri", "ok"))
	reject(tooLargeBatch, "batch", 30, 24)

	deleteOnly := NewBatch()
	deleteOnly.Delete(Identifier(strings.Repeat("x", 22)))
	reject(deleteOnly, "batch", 25, 24)

	boundary := NewBatch()
	boundary.Update(Identifier("old"), NewDocument("old").AddField(NewTextField("body", "OK")))
	boundary.Insert(NewDocument("two")) // 12 document + 6 selector + 6 document = 24.
	if err := w.Batch(boundary); err != nil {
		t.Fatal(err)
	}
	assertBatchStats(t, w, 0, 0)
}

func TestWriterBatchErrorsReleaseSlotAndStats(t *testing.T) {
	wantErr := errors.New("segment construction failed")
	var builds atomic.Int32
	config := InMemoryOnlyConfig().WithMaxConcurrentBatches(1).WithBatchStats(true)
	config.indexConfig = config.indexConfig.WithSegmentPlugin(&index.SegmentPlugin{
		Type: zapxbluge.Type, Version: zapxbluge.Version,
		New: func(docs []segment.Document, norm func(string, int) float32) (segment.Segment, uint64, error) {
			if builds.Add(1) == 1 {
				return nil, 0, wantErr
			}
			return zapxbluge.New(docs, norm)
		},
		Load: zapxbluge.Load, Merge: zapxbluge.Merge,
	})
	w, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err := w.Insert(NewDocument("vector").AddField(NewVectorField("v", []float32{1}))); !errors.Is(err, ErrVectorUnsupported) {
		t.Fatalf("expected preflight failure, got %v", err)
	}
	assertBatchStats(t, w, 0, 0)
	if err := w.Insert(NewDocument("failed")); !errors.Is(err, wantErr) {
		t.Fatalf("expected segment failure, got %v", err)
	}
	assertBatchStats(t, w, 0, 0)
	done := make(chan error, 1)
	go func() { done <- w.Insert(NewDocument("success")) }()
	if err := awaitBatchResult(t, done); err != nil {
		t.Fatal(err)
	}
	assertBatchStats(t, w, 0, 0)
}

func TestWriterCloseReleasesBatchWaiters(t *testing.T) {
	w, err := OpenWriter(InMemoryOnlyConfig().WithMaxConcurrentBatches(1).WithBatchStats(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	active := newBatchTestGate(t)
	activeDone, waitingDone, closeDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() { activeDone <- w.Insert(&inputTestDocument{NewDocument("one"), active.block}) }()
	awaitBatchSignal(t, active.entered)
	waiterCalled, waiterAnalyzed := make(chan struct{}), make(chan struct{})
	go func() {
		close(waiterCalled)
		waitingDone <- w.Insert(&inputTestDocument{NewDocument("two"), func() { close(waiterAnalyzed) }})
	}()
	awaitBatchSignal(t, waiterCalled)
	assertBatchWaiting(t, waiterAnalyzed)
	go func() { closeDone <- w.Close() }()
	// Close must wake the waiter while the active analysis is still blocked.
	if err := awaitBatchResult(t, waitingDone); !errors.Is(err, segment.ErrClosed) {
		t.Fatalf("waiting write returned %v, want ErrClosed", err)
	}
	assertBatchStats(t, w, 1, 6)
	active.open()
	if err := awaitBatchResult(t, activeDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitBatchResult(t, closeDone); err != nil {
		t.Fatal(err)
	}
	if err := w.Batch(NewBatch()); !errors.Is(err, segment.ErrClosed) {
		t.Fatalf("write after close returned %v, want ErrClosed", err)
	}
	assertBatchStats(t, w, 0, 0)
}

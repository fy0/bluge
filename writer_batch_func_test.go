package bluge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/segment"
)

func TestBatchFuncDefersConstructionUntilAdmitted(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig().WithMaxConcurrentBatches(1).WithBatchStats(true))
	gate := newBatchTestGate(t)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		first <- w.BatchFunc(context.Background(), func(b *index.Batch) error {
			gate.block()
			b.Insert(NewDocument("one"))
			return nil
		})
	}()
	awaitBatchSignal(t, gate.entered)
	assertBatchStats(t, w, 1, 0)
	called, built := make(chan struct{}), make(chan struct{})
	go func() {
		close(called)
		second <- w.BatchFunc(context.Background(), func(b *index.Batch) error {
			close(built)
			b.Insert(NewDocument("two"))
			return nil
		})
	}()
	awaitBatchSignal(t, called)
	assertBatchWaiting(t, built)
	gate.open()
	if err := awaitBatchResult(t, first); err != nil {
		t.Fatal(err)
	}
	if err := awaitBatchResult(t, second); err != nil {
		t.Fatal(err)
	}
	assertBatchStats(t, w, 0, 0)
	if len(textTestHits(t, w, NewMatchAllQuery())) != 2 {
		t.Fatal("missing built documents")
	}
}

func TestBatchFuncWaitingCancellationAndClose(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig().WithMaxConcurrentBatches(1).WithBatchStats(true))
	gate := newBatchTestGate(t)
	active := make(chan error, 1)
	go func() { active <- w.Insert(&inputTestDocument{NewDocument("active"), gate.block}) }()
	awaitBatchSignal(t, gate.entered)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, 1)
	called, built := make(chan struct{}), make(chan struct{}, 2)
	build := func(*index.Batch) error { built <- struct{}{}; return nil }
	go func() { close(called); waiting <- w.BatchFunc(ctx, build) }()
	awaitBatchSignal(t, called)
	cancel()
	if err := awaitBatchResult(t, waiting); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	go func() { waiting <- w.BatchFunc(context.Background(), build) }()
	closing := make(chan error, 1)
	go func() { closing <- w.Close() }()
	if err := awaitBatchResult(t, waiting); !errors.Is(err, segment.ErrClosed) {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-built:
		t.Fatal("waiting callback ran")
	default:
	}
	gate.open()
	if err := awaitBatchResult(t, active); err != nil {
		t.Fatal(err)
	}
	if err := awaitBatchResult(t, closing); err != nil {
		t.Fatal(err)
	}
	assertBatchStats(t, w, 0, 0)
}

func TestBatchFuncFailuresDiscardUnsubmittedChanges(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig().WithMaxConcurrentBatches(1).
		WithMaxBatchInputBytes(32).WithBatchStats(true))
	if err := w.Insert(NewDocument("old")); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("builder failed")
	if err := w.BatchFunc(context.Background(), func(b *index.Batch) error {
		b.Delete(Identifier("old"))
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("build failure: %v", err)
	}
	var limit *InputLimitError
	if err := w.BatchFunc(context.Background(), func(b *index.Batch) error {
		b.Delete(Identifier("old"))
		b.Insert(NewDocument("new").AddField(NewTextField("body", strings.Repeat("x", 64))))
		return nil
	}); !errors.As(err, &limit) {
		t.Fatalf("limit failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.BatchFunc(ctx, func(b *index.Batch) error {
		b.Delete(Identifier("old"))
		cancel()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled builder: %v", err)
	}
	assertBatchStats(t, w, 0, 0)
	hits := textTestHits(t, w, NewMatchAllQuery())
	if len(hits) != 1 || hits[0][_idField] != "old" {
		t.Fatalf("failed build modified index: %v", hits)
	}
	if err := w.BatchFunc(context.Background(), func(b *index.Batch) error {
		b.Update(Identifier("old"), NewDocument("new"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hits = textTestHits(t, w, NewMatchAllQuery())
	if len(hits) != 1 || hits[0][_idField] != "new" {
		t.Fatalf("slot not reusable: %v", hits)
	}
}

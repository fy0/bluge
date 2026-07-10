// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package index

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	segment "github.com/blugelabs/bluge_segment_api"

	"github.com/fy0/bluge/index/mergeplan"
)

type directoryLoadCounter struct {
	segment atomic.Int64
}

type countingLoadDirectory struct {
	Directory
	loads *directoryLoadCounter
}

func (d *countingLoadDirectory) Load(kind string, id uint64) (*segment.Data, io.Closer, error) {
	if kind == ItemKindSegment {
		d.loads.segment.Add(1)
	}
	return d.Directory.Load(kind, id)
}

func TestDirectPersistReusesInMemorySegment(t *testing.T) {
	path := t.TempDir()
	loads := &directoryLoadCounter{}
	cfg := DefaultConfigWithDirectory(func() Directory {
		return &countingLoadDirectory{
			Directory: NewFileSystemDirectory(path),
			loads:     loads,
		}
	})

	plugin := cfg.supportedSegmentPlugins[cfg.SegmentType][cfg.SegmentVersion]
	var created segment.Segment
	cfg = cfg.WithSegmentPlugin(&SegmentPlugin{
		Type:    plugin.Type,
		Version: plugin.Version,
		New: func(results []segment.Document, normCalc func(string, int) float32) (segment.Segment, uint64, error) {
			var err error
			var count uint64
			created, count, err = plugin.New(results, normCalc)
			return created, count, err
		},
		Load:  plugin.Load,
		Merge: plugin.Merge,
	})

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = idx.Close()
	})
	batch := NewBatch()
	batch.Update(testIdentifier("1"), &FakeDocument{
		NewFakeField("_id", "1", true, false, true),
		NewFakeField("body", "persisted in memory", false, false, true),
	})
	if err = idx.Batch(batch); err != nil {
		t.Fatal(err)
	}

	reader, err := idx.Reader()
	if err != nil {
		t.Fatal(err)
	}
	readerClosed := false
	t.Cleanup(func() {
		if !readerClosed {
			_ = reader.Close()
		}
	})
	if got := loads.segment.Load(); got != 0 {
		t.Fatalf("segment loads before reopen: got %d want 0", got)
	}
	if len(reader.segment) != 1 {
		t.Fatalf("segments: got %d want 1", len(reader.segment))
	}
	if reader.segment[0].segment.Segment != created {
		t.Fatal("persisted root did not reuse the in-memory segment")
	}
	if !reader.segment[0].segment.Persisted() {
		t.Fatal("reused segment is not marked persisted")
	}
	err = reader.Close()
	readerClosed = true
	if err != nil {
		t.Fatal(err)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reopenedClosed := false
	t.Cleanup(func() {
		if !reopenedClosed {
			_ = reopened.Close()
		}
	})
	if got := loads.segment.Load(); got != 1 {
		t.Fatalf("segment loads after reopen: got %d want 1", got)
	}
	count, err := reopened.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reopened count: got %d want 1", count)
	}
	err = reopened.Close()
	reopenedClosed = true
	if err != nil {
		t.Fatal(err)
	}
}

func TestDirectPersistReuseIsBounded(t *testing.T) {
	path := t.TempDir()
	loads := &directoryLoadCounter{}
	cfg := DefaultConfigWithDirectory(func() Directory {
		return &countingLoadDirectory{
			Directory: NewFileSystemDirectory(path),
			loads:     loads,
		}
	})
	cfg.MergePlanOptions.CalcBudget = func(_ int64, _ int64, _ *mergeplan.Options) int {
		return 1000
	}

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = idx.Close()
	})
	for i := 0; i < maxPersistedInMemorySegments+1; i++ {
		id := fmt.Sprintf("%d", i)
		batch := NewBatch()
		batch.Update(testIdentifier(id), &FakeDocument{
			NewFakeField("_id", id, true, false, true),
			NewFakeField("body", "bounded reuse", false, false, true),
		})
		if err = idx.Batch(batch); err != nil {
			t.Fatal(err)
		}
	}

	reader, err := idx.Reader()
	if err != nil {
		t.Fatal(err)
	}
	readerClosed := false
	t.Cleanup(func() {
		if !readerClosed {
			_ = reader.Close()
		}
	})
	var persistedInMemory int
	for _, segmentSnapshot := range reader.segment {
		if segmentSnapshot.segment.Persisted() && segmentSnapshot.segment.inMemory {
			persistedInMemory++
		}
	}
	if persistedInMemory != maxPersistedInMemorySegments {
		t.Fatalf("persisted in-memory segments: got %d want %d",
			persistedInMemory, maxPersistedInMemorySegments)
	}
	if got := loads.segment.Load(); got != 1 {
		t.Fatalf("segment loads: got %d want 1", got)
	}
	err = reader.Close()
	readerClosed = true
	if err != nil {
		t.Fatal(err)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}
}

type countingCloser struct {
	closes atomic.Int64
}

func (c *countingCloser) Close() error {
	c.closes.Add(1)
	return nil
}

func TestPersistedViewSharesOwnership(t *testing.T) {
	closer := &countingCloser{}
	source := &segmentWrapper{
		refCounter: &closeOnLastRefCounter{closer: closer, refs: 1},
	}
	persisted := source.persistedView()
	if !persisted.Persisted() {
		t.Fatal("persisted view is not marked persisted")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closer.closes.Load(); got != 0 {
		t.Fatalf("close count with persisted owner: got %d want 0", got)
	}
	if err := persisted.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closer.closes.Load(); got != 1 {
		t.Fatalf("final close count: got %d want 1", got)
	}
}

func TestPrepareIntroducePersistCleansUnappliedView(t *testing.T) {
	closeCh := make(chan struct{})
	close(closeCh)
	writer := &Writer{closeCh: closeCh}
	closer := &countingCloser{}
	source := &segmentWrapper{
		refCounter: &closeOnLastRefCounter{closer: closer, refs: 1},
	}
	persisted := source.persistedView()

	err := writer.prepareIntroducePersist(make(chan *persistIntroduction), map[uint64]*segmentWrapper{1: persisted})
	if !errors.Is(err, segment.ErrClosed) {
		t.Fatalf("error: got %v want %v", err, segment.ErrClosed)
	}
	if got := closer.closes.Load(); got != 0 {
		t.Fatalf("source closed while still owned: got %d closes", got)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closer.closes.Load(); got != 1 {
		t.Fatalf("final close count: got %d want 1", got)
	}
}

type partialLoadDirectory struct {
	Directory
	snapshot      []byte
	segment       []byte
	segmentCloser io.Closer
}

func (d *partialLoadDirectory) Load(kind string, id uint64) (*segment.Data, io.Closer, error) {
	switch kind {
	case ItemKindSnapshot:
		return segment.NewDataBytes(d.snapshot), nil, nil
	case ItemKindSegment:
		if id == 11 {
			return segment.NewDataBytes(d.segment), d.segmentCloser, nil
		}
		return nil, nil, fmt.Errorf("injected load failure for segment %d", id)
	default:
		return nil, nil, fmt.Errorf("unexpected item kind %q", kind)
	}
}

func TestLoadSnapshotClosesSegmentsAfterPartialFailure(t *testing.T) {
	cfg := defaultConfig()
	plugin := cfg.supportedSegmentPlugins[cfg.SegmentType][cfg.SegmentVersion]
	doc := &FakeDocument{
		NewFakeField("_id", "1", true, false, true),
		NewFakeField("body", "partial load", false, false, true),
	}
	seg, _, err := plugin.New([]segment.Document{doc}, cfg.NormCalc)
	if err != nil {
		t.Fatal(err)
	}
	var segmentBytes bytes.Buffer
	if _, err = seg.WriteTo(&segmentBytes, nil); err != nil {
		t.Fatal(err)
	}

	snapshot := &Snapshot{
		epoch: 7,
		segment: []*segmentSnapshot{
			{id: 11, segment: &segmentWrapper{Segment: seg}},
			{id: 12, segment: &segmentWrapper{Segment: seg}},
		},
	}
	var snapshotBytes bytes.Buffer
	if _, err = snapshot.WriteTo(&snapshotBytes, nil); err != nil {
		t.Fatal(err)
	}

	closer := &countingCloser{}
	directory := &partialLoadDirectory{
		Directory:     NewInMemoryDirectory(),
		snapshot:      snapshotBytes.Bytes(),
		segment:       segmentBytes.Bytes(),
		segmentCloser: closer,
	}
	writer := &Writer{config: cfg, directory: directory}
	if _, err = writer.loadSnapshot(7); err == nil {
		t.Fatal("expected partial segment load to fail")
	}
	if got := closer.closes.Load(); got != 1 {
		t.Fatalf("loaded segment close count: got %d want 1", got)
	}
}

// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package index

import (
	"strings"
	"testing"
)

func TestDefaultIndexSegmentsAreZapV17(t *testing.T) {
	cfg, cleanup := CreateConfig("TestDefaultIndexSegmentsAreZapV17")
	defer func() {
		err := cleanup()
		if err != nil {
			t.Log(err)
		}
	}()

	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err = idx.Close()
		if err != nil {
			t.Fatal(err)
		}
	}()

	doc := &FakeDocument{
		NewFakeField("_id", "1", true, false, false),
		NewFakeField("name", "zap", false, false, true),
	}
	batch := NewBatch()
	batch.Update(testIdentifier("1"), doc)
	err = idx.Batch(batch)
	if err != nil {
		t.Fatal(err)
	}

	reader, err := idx.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
	}()

	if len(reader.segment) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(reader.segment))
	}
	seg := reader.segment[0].segment
	if seg.Type() != "zap" {
		t.Fatalf("expected segment type zap, got %s", seg.Type())
	}
	if seg.Version() != 17 {
		t.Fatalf("expected segment version 17, got %d", seg.Version())
	}
}

func TestLegacyIceSegmentPluginError(t *testing.T) {
	_, err := loadSegmentPlugin(defaultConfig().supportedSegmentPlugins, "ice", 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `unsupported legacy segment type "ice"; migrate the index offline`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

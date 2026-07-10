// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package index

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirectPersistDoesNotHoldWindowsSegmentFile(t *testing.T) {
	path := t.TempDir()
	directory := NewFileSystemDirectory(path)
	cfg := DefaultConfigWithDirectory(func() Directory {
		return directory
	})
	idx, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := idx.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()

	batch := NewBatch()
	batch.Update(testIdentifier("1"), &FakeDocument{
		NewFakeField("_id", "1", true, false, true),
		NewFakeField("body", "windows file ownership", false, false, true),
	})
	if err = idx.Batch(batch); err != nil {
		t.Fatal(err)
	}
	segmentIDs, err := directory.List(ItemKindSegment)
	if err != nil {
		t.Fatal(err)
	}
	if len(segmentIDs) != 1 {
		t.Fatalf("segment files: got %d want 1", len(segmentIDs))
	}

	segmentPath := filepath.Join(path, directory.fileName(ItemKindSegment, segmentIDs[0]))
	locked, err := directory.openExclusive(segmentPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("segment file still held after direct persist: %v", err)
	}
	if err = locked.Close(); err != nil {
		t.Fatal(err)
	}
}

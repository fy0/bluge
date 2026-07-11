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
	"fmt"
	"testing"
)

func TestStoredBlocksRoundTrip(t *testing.T) {
	var output bytes.Buffer
	fileWriter := NewFileWriterEmpty(NewCountHashWriter(&output))
	writer := newStoredBlockWriter(fileWriter, 130)

	for docNum := 0; docNum < 130; docNum++ {
		meta := []byte{byte(docNum), byte(docNum >> 8)}
		data := []byte(fmt.Sprintf("shared-prefix/document-%03d", docNum))
		if err := writer.Add(meta, data); err != nil {
			t.Fatal(err)
		}
	}
	storedIndexOffset, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	fileReader, err := NewFileReader("", nil)
	if err != nil {
		t.Fatal(err)
	}
	sb := &SegmentBase{
		mem:               output.Bytes(),
		numDocs:           130,
		storedIndexOffset: storedIndexOffset,
		fileReader:        fileReader,
	}
	vdc := &visitDocumentCtx{}
	for docNum := 0; docNum < 130; docNum++ {
		meta, data, err := sb.getDocStoredMetaAndData(vdc, uint64(docNum))
		if err != nil {
			t.Fatal(err)
		}
		expectedMeta := []byte{byte(docNum), byte(docNum >> 8)}
		expectedData := []byte(fmt.Sprintf("shared-prefix/document-%03d", docNum))
		if !bytes.Equal(meta, expectedMeta) || !bytes.Equal(data, expectedData) {
			t.Fatalf("doc %d mismatch: meta=%v data=%q", docNum, meta, data)
		}
	}
	if expected := uint64(output.Len()); sb.getEdgeListOffset() != expected {
		t.Fatalf("expected edge list offset %d, got %d", expected, sb.getEdgeListOffset())
	}
}

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
	"testing"

	index "github.com/blevesearch/bleve_index_api"
	"github.com/golang/snappy"
)

func TestChunkedContentCoderDeltaMetadata(t *testing.T) {
	var output bytes.Buffer
	coder := newChunkedContentCoder(1024, 1023, &output, false, false)
	values := map[uint64][]byte{
		0:    []byte("zero"),
		7:    []byte("seven"),
		1023: []byte("last"),
	}
	for _, docNum := range []uint64{0, 7, 1023} {
		if err := coder.Add(docNum, values[docNum]); err != nil {
			t.Fatal(err)
		}
	}
	if err := coder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := coder.Write(); err != nil {
		t.Fatal(err)
	}

	segment := &SegmentBase{
		mem:        output.Bytes(),
		fileReader: &FileReader{},
		fieldsOptions: map[string]index.FieldIndexingOptions{
			"field": index.DocValues,
		},
	}
	reader, err := segment.loadFieldDocValueReader("field", 0, uint64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.loadDvChunk(0, segment); err != nil {
		t.Fatal(err)
	}
	reader.uncompressed, err = snappy.Decode(nil, reader.curChunkData)
	if err != nil {
		t.Fatal(err)
	}
	for i, docNum := range []uint64{0, 7, 1023} {
		entry := reader.curChunkHeader[i]
		if entry.DocNum != docNum {
			t.Fatalf("entry %d doc number: got %d want %d", i, entry.DocNum, docNum)
		}
		start, end := ReadDocValueBoundary(i, reader.curChunkHeader)
		if got := reader.uncompressed[start:end]; !bytes.Equal(got, values[docNum]) {
			t.Fatalf("entry %d value: got %q want %q", i, got, values[docNum])
		}
	}
}

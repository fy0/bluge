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
	"encoding/binary"
	"testing"
)

func TestChunkedIntCoderAdaptiveCompression(t *testing.T) {
	raw := encodeIntChunks(t, 4, 4, func(docNum uint64) []uint64 {
		return []uint64{docNum + 1}
	})
	if flag := firstIntChunkFlag(t, raw); flag != intChunkRaw {
		t.Fatalf("expected small chunk to stay raw, got flag %d", flag)
	}

	compressed := encodeIntChunks(t, 1024, 1024, func(uint64) []uint64 {
		return []uint64{1, 0xffffffff}
	})
	if flag := firstIntChunkFlag(t, compressed); flag != intChunkSnappy {
		t.Fatalf("expected repetitive chunk to use snappy, got flag %d", flag)
	}

	fileReader, err := NewFileReader("", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Offset zero is the on-disk sentinel for an absent integer stream.
	stored := append([]byte{0}, compressed...)
	decoder := newChunkedIntDecoder(stored, 1, nil, fileReader)
	if err := decoder.loadChunk(0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1024; i++ {
		first, err := decoder.readUvarint()
		if err != nil {
			t.Fatal(err)
		}
		second, err := decoder.readUvarint()
		if err != nil {
			t.Fatal(err)
		}
		if first != 1 || second != 0xffffffff {
			t.Fatalf("entry %d: got %d/%d", i, first, second)
		}
	}
}

func encodeIntChunks(t *testing.T, chunkSize, numDocs uint64,
	values func(uint64) []uint64) []byte {
	t.Helper()
	coder := newChunkedIntCoder(chunkSize, numDocs-1)
	for docNum := uint64(0); docNum < numDocs; docNum++ {
		if err := coder.Add(docNum, values(docNum)...); err != nil {
			t.Fatal(err)
		}
	}
	coder.Close()
	var output bytes.Buffer
	if _, err := coder.Write(&output); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func firstIntChunkFlag(t *testing.T, data []byte) byte {
	t.Helper()
	pos := 0
	numChunks, n := binary.Uvarint(data[pos:])
	if n <= 0 || numChunks == 0 {
		t.Fatal("missing integer chunks")
	}
	pos += n
	for i := uint64(0); i < numChunks; i++ {
		_, n = binary.Uvarint(data[pos:])
		if n <= 0 {
			t.Fatal("invalid chunk offset")
		}
		pos += n
	}
	return data[pos]
}

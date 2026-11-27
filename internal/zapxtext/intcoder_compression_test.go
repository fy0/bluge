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
	"math/rand"
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

	// Offset zero is the on-disk sentinel for an absent integer stream.
	stored := append([]byte{0}, compressed...)
	decoder := newChunkedIntDecoder(stored, 1, nil)
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

func TestChunkedIntCoderSparseChunks(t *testing.T) {
	chunks := [][]byte{nil, {1, 2, 3}, nil, bytes.Repeat([]byte{4, 5}, 4096), {6}, nil}
	coder := newChunkedIntCoder(1, uint64(len(chunks))-1)
	for _, chunkSize := range []uint64{1, 4, 2} {
		coder.Reset()
		coder.SetChunkSize(chunkSize, chunkSize*uint64(len(chunks))-1)
		for i, chunk := range chunks {
			if err := coder.AddBytes(uint64(i)*chunkSize, chunk); err != nil {
				t.Fatal(err)
			}
		}
		coder.Close()
		var output bytes.Buffer
		if _, err := coder.Write(&output); err != nil {
			t.Fatal(err)
		}
		checkIntChunks(t, output.Bytes(), chunks)
	}
}

func checkIntChunks(t *testing.T, data []byte, chunks [][]byte) {
	t.Helper()
	// Offset zero means absent; pad the tail for the decoder's varint lookahead.
	stored := append([]byte{0}, data...)
	stored = append(stored, make([]byte, binary.MaxVarintLen64)...)
	decoder := newChunkedIntDecoder(stored, 1, nil)
	if len(decoder.chunkOffsets) != len(chunks) {
		t.Fatalf("got %d chunks, want %d", len(decoder.chunkOffsets), len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) == 0 {
			start, end := readChunkBoundary(i, decoder.chunkOffsets)
			if start != end {
				t.Fatalf("empty chunk %d has bytes", i)
			}
			continue
		}
		if err := decoder.loadChunk(i); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoder.curChunkBytes, chunk) {
			t.Fatalf("chunk %d failed round trip", i)
		}
	}
	end := decoder.dataStartOffset + decoder.chunkOffsets[len(chunks)-1]
	if end != 1+uint64(len(data)) {
		t.Fatalf("end offset %d does not cover stream of length %d", end, len(data))
	}
}

func TestChunkedIntCoderReleasesLargeBuffers(t *testing.T) {
	data := make([]byte, 2*maxIntCoderRetainedBytes)
	_, _ = rand.New(rand.NewSource(1)).Read(data)
	coder := newChunkedIntCoder(1, 0)
	if err := coder.AddBytes(0, data); err != nil {
		t.Fatal(err)
	}
	coder.Close()
	var output bytes.Buffer
	if _, err := coder.Write(&output); err != nil {
		t.Fatal(err)
	}
	checkIntChunks(t, output.Bytes(), [][]byte{data})
	coder.Reset()
	if cap(coder.final) > maxIntCoderRetainedBytes || coder.chunkBuf.Cap() > maxIntCoderRetainedBytes ||
		cap(coder.compressed) > maxIntCoderRetainedBytes || cap(coder.buf) > maxIntCoderRetainedBytes {
		t.Fatal("Reset retained an oversized buffer")
	}
	if err := coder.Add(0, 42); err != nil {
		t.Fatal(err)
	}
	coder.Close()
	output.Reset()
	if _, err := coder.Write(&output); err != nil {
		t.Fatal(err)
	}
	checkIntChunks(t, output.Bytes(), [][]byte{{42}})
}

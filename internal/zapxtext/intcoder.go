//  Copyright (c) 2017 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zap

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/golang/snappy"
)

const (
	intChunkRaw byte = iota
	intChunkSnappy
)

const maxIntCoderRetainedBytes = 1 << 20

const termNotEncoded = 0

type chunkedIntCoder struct {
	final     []byte
	chunkSize uint64
	chunkBuf  bytes.Buffer
	chunkLens []uint64
	currChunk uint64

	buf []byte

	compressed []byte
}

func newChunkedIntCoder(chunkSize uint64, maxDocNum uint64) *chunkedIntCoder {
	total := maxDocNum/chunkSize + 1
	rv := &chunkedIntCoder{
		chunkSize: chunkSize,
		chunkLens: make([]uint64, total),
		final:     make([]byte, 0, 64),
	}

	return rv
}

func (c *chunkedIntCoder) Reset() {
	c.final = c.final[:0]
	if cap(c.final) > maxIntCoderRetainedBytes {
		c.final = nil
	}
	c.chunkBuf.Reset()
	if c.chunkBuf.Cap() > maxIntCoderRetainedBytes {
		c.chunkBuf = bytes.Buffer{}
	}
	if cap(c.compressed) > maxIntCoderRetainedBytes {
		c.compressed = nil
	}
	if cap(c.buf) > maxIntCoderRetainedBytes {
		c.buf = nil
	}
	c.currChunk = 0
	for i := range c.chunkLens {
		c.chunkLens[i] = 0
	}
}

func (c *chunkedIntCoder) SetChunkSize(chunkSize uint64, maxDocNum uint64) {
	total := maxDocNum/chunkSize + 1
	c.chunkSize = chunkSize
	if uint64(cap(c.chunkLens)) < total {
		c.chunkLens = make([]uint64, total)
	} else {
		c.chunkLens = c.chunkLens[:total]
	}
}

func (c *chunkedIntCoder) Add(docNum uint64, vals ...uint64) error {
	chunk := docNum / c.chunkSize
	if chunk != c.currChunk {
		c.Close()
		c.chunkBuf.Reset()
		c.currChunk = chunk
	}

	if len(c.buf) < binary.MaxVarintLen64 {
		c.buf = make([]byte, binary.MaxVarintLen64)
	}

	for _, val := range vals {
		wb := binary.PutUvarint(c.buf, val)
		_, err := c.chunkBuf.Write(c.buf[:wb])
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *chunkedIntCoder) AddBytes(docNum uint64, buf []byte) error {
	chunk := docNum / c.chunkSize
	if chunk != c.currChunk {
		c.Close()
		c.chunkBuf.Reset()
		c.currChunk = chunk
	}

	_, err := c.chunkBuf.Write(buf)
	return err
}

func (c *chunkedIntCoder) Close() {
	encodingBytes := c.chunkBuf.Bytes()
	if len(encodingBytes) > 0 {
		c.compressed = snappy.Encode(c.compressed[:cap(c.compressed)], encodingBytes)
		flag := intChunkRaw
		if len(c.compressed) < len(encodingBytes) {
			flag = intChunkSnappy
			encodingBytes = c.compressed
		}
		c.final = append(c.final, flag)
		c.final = append(c.final, encodingBytes...)
		c.chunkLens[c.currChunk] = 1 + uint64(len(encodingBytes))
	} else {
		c.chunkLens[c.currChunk] = 0
	}
	c.currChunk = uint64(cap(c.chunkLens)) // sentinel to detect double close
}

func (c *chunkedIntCoder) Write(w io.Writer) (int, error) {
	bufNeeded := binary.MaxVarintLen64 * (1 + len(c.chunkLens))
	if len(c.buf) < bufNeeded {
		c.buf = make([]byte, bufNeeded)
	}
	buf := c.buf

	chunkOffsets := modifyLengthsToEndOffsets(c.chunkLens)

	n := binary.PutUvarint(buf, uint64(len(chunkOffsets)))
	for _, chunkOffset := range chunkOffsets {
		n += binary.PutUvarint(buf[n:], chunkOffset)
	}

	tw, err := w.Write(buf[:n])
	if err != nil {
		return tw, err
	}

	nw, err := w.Write(c.final)
	tw += nw
	if err != nil {
		return tw, err
	}
	return tw, nil
}

func (c *chunkedIntCoder) writeAt(w *CountHashWriter) (uint64, error) {
	if len(c.final) == 0 {
		return termNotEncoded, nil
	}
	offset := w.Count()
	_, err := c.Write(w)
	return offset, err
}

func (c *chunkedIntCoder) FinalSize() int {
	return len(c.final)
}

func modifyLengthsToEndOffsets(lengths []uint64) []uint64 {
	var runningOffset uint64
	var index, i int
	for i = 1; i <= len(lengths); i++ {
		runningOffset += lengths[i-1]
		lengths[index] = runningOffset
		index++
	}
	return lengths
}

func readChunkBoundary(chunk int, offsets []uint64) (uint64, uint64) {
	var start uint64
	if chunk > 0 {
		start = offsets[chunk-1]
	}
	return start, offsets[chunk]
}

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
	"unsafe"

	"github.com/golang/snappy"
)

const reflectStaticSizeMetaData = int(unsafe.Sizeof(MetaData{}))

type chunkedContentCoder struct {
	final     []byte
	currChunk uint64
	chunkLens []uint64

	compressed []byte // temp buf for snappy compression

	w io.Writer

	chunkMeta    []MetaData
	chunkMetaBuf bytes.Buffer
	chunkBuf     bytes.Buffer
}

type MetaData struct {
	DocNum      uint64 // docNum of the data inside the chunk
	DocDvOffset uint64 // offset of data inside the chunk for the given docid
}

func newChunkedContentCoder(maxDocNum uint64, w io.Writer) *chunkedContentCoder {
	total := maxDocNum/docValueChunkSize + 1
	return &chunkedContentCoder{
		chunkLens: make([]uint64, total),
		chunkMeta: make([]MetaData, 0, total),
		w:         w,
	}
}

func (c *chunkedContentCoder) Close() error {
	return c.flushContents()
}

func (c *chunkedContentCoder) writeChunkMeta() ([]byte, error) {
	buf := make([]byte, binary.MaxVarintLen64)
	var metaData []byte
	n := binary.PutUvarint(buf, uint64(len(c.chunkMeta)))
	_, err := c.chunkMetaBuf.Write(buf[:n])
	if err != nil {
		return nil, err
	}

	var previousDocNum, previousOffset uint64
	for _, meta := range c.chunkMeta {
		err := writeUvarints(&c.chunkMetaBuf,
			meta.DocNum-previousDocNum, meta.DocDvOffset-previousOffset)
		if err != nil {
			return nil, err
		}
		previousDocNum = meta.DocNum
		previousOffset = meta.DocDvOffset
	}

	metaData = c.chunkMetaBuf.Bytes()
	c.final = append(c.final, metaData...)

	return metaData, nil
}

func (c *chunkedContentCoder) flushContents() error {
	metaData, err := c.writeChunkMeta()
	if err != nil {
		return err
	}
	c.compressed = snappy.Encode(c.compressed[:cap(c.compressed)], c.chunkBuf.Bytes())
	c.final = append(c.final, c.compressed...)
	c.chunkLens[c.currChunk] = uint64(len(c.compressed)) + uint64(len(metaData))
	_, err = c.w.Write(c.final)
	c.final = c.final[:0]
	return err
}

func (c *chunkedContentCoder) Add(docNum uint64, vals []byte) error {
	chunk := docNum / docValueChunkSize
	if chunk != c.currChunk {
		err := c.flushContents()
		if err != nil {
			return err
		}
		c.chunkBuf.Reset()
		c.chunkMetaBuf.Reset()
		c.chunkMeta = c.chunkMeta[:0]
		c.currChunk = chunk
	}

	_, err := c.chunkBuf.Write(vals)
	if err != nil {
		return err
	}

	c.chunkMeta = append(c.chunkMeta, MetaData{
		DocNum:      docNum,
		DocDvOffset: uint64(c.chunkBuf.Len()),
	})
	return nil
}

func (c *chunkedContentCoder) Write() (int, error) {
	var tw int

	if cap(c.final) < binary.MaxVarintLen64 {
		c.final = make([]byte, binary.MaxVarintLen64)
	} else {
		c.final = c.final[0:binary.MaxVarintLen64]
	}
	chunkOffsets := modifyLengthsToEndOffsets(c.chunkLens)
	for _, chunkOffset := range chunkOffsets {
		n := binary.PutUvarint(c.final, chunkOffset)
		nw, err := c.w.Write(c.final[:n])
		tw += nw
		if err != nil {
			return tw, err
		}
	}

	chunkOffsetsLen := uint64(tw)

	c.final = c.final[0:8]
	binary.BigEndian.PutUint64(c.final, chunkOffsetsLen)
	nw, err := c.w.Write(c.final)
	tw += nw
	if err != nil {
		return tw, err
	}

	binary.BigEndian.PutUint64(c.final, uint64(len(c.chunkLens)))
	nw, err = c.w.Write(c.final)
	tw += nw
	if err != nil {
		return tw, err
	}

	c.final = c.final[:0]

	return tw, nil
}

func ReadDocValueBoundary(chunk int, metaHeaders []MetaData) (uint64, uint64) {
	var start uint64
	if chunk > 0 {
		start = metaHeaders[chunk-1].DocDvOffset
	}
	return start, metaHeaders[chunk].DocDvOffset
}

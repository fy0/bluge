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
	"fmt"

	"github.com/golang/snappy"
)

const storedBlockSize = uint64(32)

func storedBlockCount(numDocs uint64) uint64 {
	return (numDocs + storedBlockSize - 1) / storedBlockSize
}

type storedBlockWriter struct {
	w *FileWriter

	blockOffsets []uint64
	recordEnds   []uint64
	records      bytes.Buffer
	header       bytes.Buffer
	block        bytes.Buffer
	compressed   []byte
	varintBuf    [binary.MaxVarintLen64]byte
	bytesWritten uint64
}

func newStoredBlockWriter(w *FileWriter, numDocs uint64) *storedBlockWriter {
	return &storedBlockWriter{
		w:            w,
		blockOffsets: make([]uint64, 0, storedBlockCount(numDocs)),
		recordEnds:   make([]uint64, 0, storedBlockSize),
	}
}

func (w *storedBlockWriter) Add(meta, data []byte) error {
	if uint64(len(w.recordEnds)) == storedBlockSize {
		if err := w.flush(); err != nil {
			return err
		}
	}

	n := binary.PutUvarint(w.varintBuf[:], uint64(len(meta)))
	_, _ = w.records.Write(w.varintBuf[:n])
	_, _ = w.records.Write(meta)
	_, _ = w.records.Write(data)
	w.recordEnds = append(w.recordEnds, uint64(w.records.Len()))
	return nil
}

func (w *storedBlockWriter) flush() error {
	if len(w.recordEnds) == 0 {
		return nil
	}

	w.header.Reset()
	for _, recordEnd := range w.recordEnds {
		n := binary.PutUvarint(w.varintBuf[:], recordEnd)
		_, _ = w.header.Write(w.varintBuf[:n])
	}

	w.block.Reset()
	n := binary.PutUvarint(w.varintBuf[:], uint64(w.header.Len()))
	_, _ = w.block.Write(w.varintBuf[:n])
	_, _ = w.block.Write(w.header.Bytes())
	_, _ = w.block.Write(w.records.Bytes())

	w.compressed = snappy.Encode(w.compressed[:cap(w.compressed)], w.block.Bytes())
	processed := w.w.process(w.compressed)
	w.blockOffsets = append(w.blockOffsets, uint64(w.w.Count()))
	written, err := w.w.Write(processed)
	w.bytesWritten += uint64(written)
	if err != nil {
		return err
	}

	w.records.Reset()
	w.recordEnds = w.recordEnds[:0]
	return nil
}

func (w *storedBlockWriter) Close() (uint64, error) {
	if err := w.flush(); err != nil {
		return 0, err
	}

	storedIndexOffset := uint64(w.w.Count())
	for _, blockOffset := range w.blockOffsets {
		if err := binary.Write(w.w, binary.BigEndian, blockOffset); err != nil {
			return 0, err
		}
	}
	return storedIndexOffset, nil
}

func (sb *SegmentBase) storedBlockBounds(blockNum uint64) (uint64, uint64) {
	indexPos := sb.storedIndexOffset + 8*blockNum
	start := binary.BigEndian.Uint64(sb.mem[indexPos : indexPos+8])
	if blockNum+1 == storedBlockCount(sb.numDocs) {
		return start, sb.storedIndexOffset
	}
	end := binary.BigEndian.Uint64(sb.mem[indexPos+8 : indexPos+16])
	return start, end
}

func (sb *SegmentBase) getDocStoredMetaAndData(vdc *visitDocumentCtx, docNum uint64) ([]byte, []byte, error) {
	blockNum := docNum / storedBlockSize
	if vdc.segment != sb || vdc.blockNum != blockNum {
		start, end := sb.storedBlockBounds(blockNum)
		compressed, err := sb.fileReader.process(sb.mem[start:end])
		if err != nil {
			return nil, nil, err
		}
		decoded, err := snappy.Decode(vdc.buf[:cap(vdc.buf)], compressed)
		if err != nil {
			return nil, nil, err
		}
		vdc.buf = decoded
		vdc.segment = sb
		vdc.blockNum = blockNum
	}

	headerLen, n := binary.Uvarint(vdc.buf)
	if n <= 0 {
		return nil, nil, fmt.Errorf("invalid stored block header length")
	}
	headerStart := uint64(n)
	recordsStart := headerStart + headerLen
	header := vdc.buf[headerStart:recordsStart]
	withinBlock := docNum % storedBlockSize

	var recordStart, recordEnd uint64
	var headerPos uint64
	for i := uint64(0); i <= withinBlock; i++ {
		end, read := binary.Uvarint(header[headerPos:])
		if read <= 0 {
			return nil, nil, fmt.Errorf("invalid stored record offset")
		}
		headerPos += uint64(read)
		recordStart = recordEnd
		recordEnd = end
	}

	record := vdc.buf[recordsStart+recordStart : recordsStart+recordEnd]
	metaLen, read := binary.Uvarint(record)
	if read <= 0 {
		return nil, nil, fmt.Errorf("invalid stored metadata length")
	}
	metaStart := uint64(read)
	dataStart := metaStart + metaLen
	return record[metaStart:dataStart], record[dataStart:], nil
}

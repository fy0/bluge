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
	"fmt"
	"math"
	"sort"
	"unsafe"

	index "github.com/blevesearch/bleve_index_api"
	segment "github.com/blevesearch/scorch_segment_api/v2"
	"github.com/golang/snappy"
)

const reflectStaticSizedocValueReader = int(unsafe.Sizeof(docValueReader{}))

type docNumTermsVisitor func(docNum uint64, terms []byte) error

type docVisitState struct {
	dvrs    []*docValueReader
	segment *SegmentBase

	bytesRead uint64
}

func (d *docVisitState) BytesRead() uint64 {
	return d.bytesRead
}

func (d *docVisitState) BytesWritten() uint64 {
	return 0
}

func (d *docVisitState) ResetBytesRead(val uint64) {
	d.bytesRead = val
}

type docValueReader struct {
	field          string
	curChunkNum    uint64
	chunkOffsets   []uint64
	dvDataLoc      uint64
	curChunkHeader []MetaData
	curChunkData   []byte // compressed data cache
	uncompressed   []byte // temp buf for snappy decompression

	bytesRead uint64
}

func (di *docValueReader) size() int {
	return reflectStaticSizedocValueReader + SizeOfPtr +
		len(di.field) +
		len(di.chunkOffsets)*SizeOfUint64 +
		len(di.curChunkHeader)*reflectStaticSizeMetaData +
		len(di.curChunkData)
}

func (di *docValueReader) cloneInto(rv *docValueReader) *docValueReader {
	if rv == nil {
		rv = &docValueReader{}
	}

	rv.field = di.field
	rv.curChunkNum = math.MaxUint64
	rv.chunkOffsets = di.chunkOffsets // immutable, so it's sharable
	rv.dvDataLoc = di.dvDataLoc
	rv.curChunkHeader = rv.curChunkHeader[:0]
	rv.curChunkData = nil
	rv.uncompressed = rv.uncompressed[:0]

	return rv
}

func (di *docValueReader) curChunkNumber() uint64 {
	return di.curChunkNum
}

func (sb *SegmentBase) loadFieldDocValueReader(field string,
	fieldDvLocStart, fieldDvLocEnd uint64) (*docValueReader, error) {
	if fieldDvLocStart == fieldNotUninverted {
		return nil, nil
	}

	var numChunks, chunkOffsetsPosition uint64

	if fieldDvLocEnd-fieldDvLocStart > 16 {
		numChunks = binary.BigEndian.Uint64(sb.mem[fieldDvLocEnd-8 : fieldDvLocEnd])
		chunkOffsetsLen := binary.BigEndian.Uint64(sb.mem[fieldDvLocEnd-16 : fieldDvLocEnd-8])
		chunkOffsetsPosition = (fieldDvLocEnd - 16) - chunkOffsetsLen

		sb.incrementBytesRead(16)
	} else {
		return nil, fmt.Errorf("loadFieldDocValueReader: fieldDvLoc too small: %d-%d", fieldDvLocEnd, fieldDvLocStart)
	}

	fdvIter := &docValueReader{
		curChunkNum:  math.MaxUint64,
		field:        field,
		chunkOffsets: make([]uint64, numChunks),
	}

	var offset uint64
	for i := range fdvIter.chunkOffsets {
		loc, read := binary.Uvarint(sb.mem[chunkOffsetsPosition+offset : chunkOffsetsPosition+offset+binary.MaxVarintLen64])
		if read <= 0 {
			return nil, fmt.Errorf("corrupted chunk offset during segment load")
		}
		fdvIter.chunkOffsets[i] = loc
		offset += uint64(read)
	}
	sb.incrementBytesRead(offset)
	fdvIter.dvDataLoc = fieldDvLocStart
	return fdvIter, nil
}

func (d *docValueReader) getBytesRead() uint64 {
	return d.bytesRead
}

func (d *docValueReader) incrementBytesRead(val uint64) {
	d.bytesRead += val
}

func (di *docValueReader) loadDvChunk(chunkNumber uint64, s *SegmentBase) error {
	destChunkDataLoc, curChunkEnd := di.dvDataLoc, di.dvDataLoc
	start, end := readChunkBoundary(int(chunkNumber), di.chunkOffsets)
	if start >= end {
		di.curChunkHeader = di.curChunkHeader[:0]
		di.curChunkData = nil
		di.curChunkNum = chunkNumber
		di.uncompressed = di.uncompressed[:0]
		return nil
	}

	destChunkDataLoc += start
	curChunkEnd += end

	numDocs, read := binary.Uvarint(s.mem[destChunkDataLoc : destChunkDataLoc+binary.MaxVarintLen64])
	if read <= 0 {
		return fmt.Errorf("failed to read the chunk")
	}
	chunkMetaLoc := destChunkDataLoc + uint64(read)
	di.incrementBytesRead(uint64(read))
	offset := uint64(0)
	if uint64(cap(di.curChunkHeader)) < numDocs {
		di.curChunkHeader = make([]MetaData, numDocs)
	} else {
		di.curChunkHeader = di.curChunkHeader[:numDocs]
	}
	var previousDocNum, previousOffset uint64
	for i := range di.curChunkHeader {
		docNumDelta, read := binary.Uvarint(s.mem[chunkMetaLoc+offset : chunkMetaLoc+offset+binary.MaxVarintLen64])
		offset += uint64(read)
		valueOffsetDelta, read := binary.Uvarint(s.mem[chunkMetaLoc+offset : chunkMetaLoc+offset+binary.MaxVarintLen64])
		offset += uint64(read)
		previousDocNum += docNumDelta
		previousOffset += valueOffsetDelta
		di.curChunkHeader[i] = MetaData{
			DocNum:      previousDocNum,
			DocDvOffset: previousOffset,
		}
	}

	compressedDataLoc := chunkMetaLoc + offset
	dataLength := curChunkEnd - compressedDataLoc
	di.incrementBytesRead(dataLength + offset)
	di.curChunkData = s.mem[compressedDataLoc : compressedDataLoc+dataLength]
	di.curChunkNum = chunkNumber
	di.uncompressed = di.uncompressed[:0]
	return nil
}

func (di *docValueReader) iterateAllDocValues(s *SegmentBase, visitor docNumTermsVisitor) error {
	for i := uint64(0); i < uint64(len(di.chunkOffsets)); i++ {
		err := di.loadDvChunk(i, s)
		if err != nil {
			return err
		}

		if len(di.curChunkHeader) == 0 {
			continue
		}
		uncompressed, err := snappy.Decode(di.uncompressed[:cap(di.uncompressed)], di.curChunkData)
		if err != nil {
			return err
		}
		di.uncompressed = uncompressed

		start := uint64(0)
		for _, entry := range di.curChunkHeader {
			err = visitor(entry.DocNum, uncompressed[start:entry.DocDvOffset])
			if err != nil {
				return err
			}

			start = entry.DocDvOffset
		}
	}

	return nil
}

func (di *docValueReader) visitDocValues(docNum uint64,
	visitor index.DocValueVisitor) error {
	start, end := di.getDocValueLocs(docNum)
	if start == end {
		return nil
	}
	if len(di.uncompressed) == 0 {
		var err error
		di.uncompressed, err = snappy.Decode(di.uncompressed[:cap(di.uncompressed)], di.curChunkData)
		if err != nil {
			return err
		}
	}
	uncompressed := di.uncompressed[start:end]
	for {
		i := bytes.IndexByte(uncompressed, index.DocValueTermSeparator)
		if i < 0 {
			break
		}

		visitor(di.field, uncompressed[0:i])
		uncompressed = uncompressed[i+1:]
	}

	return nil
}

func (di *docValueReader) getDocValueLocs(docNum uint64) (uint64, uint64) {
	i := sort.Search(len(di.curChunkHeader), func(i int) bool {
		return di.curChunkHeader[i].DocNum >= docNum
	})
	if i < len(di.curChunkHeader) && di.curChunkHeader[i].DocNum == docNum {
		return ReadDocValueBoundary(i, di.curChunkHeader)
	}
	return math.MaxUint64, math.MaxUint64
}

func (sb *SegmentBase) VisitDocValues(localDocNum uint64, fields []string,
	visitor index.DocValueVisitor, dvsIn segment.DocVisitState) (
	segment.DocVisitState, error) {
	dvs, ok := dvsIn.(*docVisitState)
	if !ok || dvs == nil {
		dvs = &docVisitState{}
	} else {
		if dvs.segment != sb {
			dvs.segment = sb
			dvs.dvrs = nil
			dvs.bytesRead = 0
		}
	}

	var initDvReaders bool
	if dvs.dvrs == nil {
		dvs.dvrs = make([]*docValueReader, len(sb.fieldsInv))
		initDvReaders = true
	}

	var fieldIDPlus1, fieldID uint16
	var dvr, dvIter *docValueReader
	docInChunk := localDocNum / docValueChunkSize
	for _, field := range fields {
		if fieldIDPlus1, ok = sb.fieldsMap[field]; !ok {
			continue
		}
		fieldID = fieldIDPlus1 - 1

		if initDvReaders {
			dvIter = sb.fieldDvReaders[SectionInvertedTextIndex][fieldID]
			if dvIter != nil {
				dvs.dvrs[fieldID] = dvIter.cloneInto(dvs.dvrs[fieldID])
			}
		}

		dvr = dvs.dvrs[fieldID]
		if dvr != nil {
			if docInChunk != dvr.curChunkNumber() {
				err := dvr.loadDvChunk(docInChunk, sb)
				if err != nil {
					return dvs, err
				}
				dvs.ResetBytesRead(dvr.getBytesRead())
			} else {
				dvs.ResetBytesRead(0)
			}

			_ = dvr.visitDocValues(localDocNum, visitor)
		}
	}
	return dvs, nil
}

func (sb *SegmentBase) VisitableDocValueFields() ([]string, error) {
	return sb.fieldDvNames, nil
}

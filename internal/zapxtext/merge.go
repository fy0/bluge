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
	"errors"
	"math"
	"sort"

	"github.com/RoaringBitmap/roaring/v2"
	index "github.com/blevesearch/bleve_index_api"
	seg "github.com/blevesearch/scorch_segment_api/v2"

	blugeseg "github.com/fy0/bluge/segment"
)

var DefaultFileMergerBufferSize = 1024 * 1024

const docDropped = math.MaxUint64 // sentinel docNum to represent a deleted doc

func mergeToWriter(segments []*SegmentBase, drops []*roaring.Bitmap,
	chunkMode uint32, w *CountHashWriter, closeCh chan struct{}, options blugeseg.BuildOptions) (
	newDocNums [][]uint64, numDocs, storedIndexOffset uint64,
	fieldsInv []string, fieldsMap map[string]uint16, sectionsIndexOffset uint64,
	err error) {
	var fieldsSame bool
	var fieldsOptions map[string]index.FieldIndexingOptions
	fieldsSame, fieldsInv, fieldsOptions = mergeFields(segments)
	fieldsMap = mapFields(fieldsInv)

	numDocs = computeNewDocCount(segments, drops)
	if numDocs > math.MaxUint32 {
		return nil, 0, 0, nil, nil, 0, errors.New("too many segment documents")
	}

	if isClosed(closeCh) {
		return nil, 0, 0, nil, nil, 0, seg.ErrClosed
	}

	mergeOpaque := map[int]resetable{}
	args := sectionConfig{
		chunkMode:     chunkMode,
		fieldsSame:    fieldsSame,
		fieldsMap:     fieldsMap,
		numDocs:       numDocs,
		fieldsOptions: fieldsOptions,
		options:       options,
	}

	if numDocs > 0 {
		storedIndexOffset, newDocNums, err = mergeStoredAndRemap(segments, drops,
			fieldsMap, fieldsInv, fieldsOptions, fieldsSame, numDocs, w, closeCh)
		if err != nil {
			return nil, 0, 0, nil, nil, 0, err
		}

		for i, x := range segmentSections {
			mergeOpaque[i] = x.InitOpaque(args)
			err = x.Merge(mergeOpaque, segments, drops, fieldsInv, newDocNums, w, closeCh)
			if err != nil {
				return nil, 0, 0, nil, nil, 0, err
			}
		}
	}

	var stats []fieldStats
	if inverted, ok := mergeOpaque[SectionInvertedTextIndex].(*invertedIndexOpaque); ok {
		stats = inverted.fieldStats
	}
	sectionsIndexOffset, err = persistFieldsSection(fieldsInv, fieldsOptions, w, mergeOpaque, stats)
	if err != nil {
		return nil, 0, 0, nil, nil, 0, err
	}

	return newDocNums, numDocs, storedIndexOffset, fieldsInv, fieldsMap, sectionsIndexOffset, nil
}

func mapFields(fields []string) map[string]uint16 {
	rv := make(map[string]uint16, len(fields))
	for i, fieldName := range fields {
		rv[fieldName] = uint16(i) + 1
	}
	return rv
}

func computeNewDocCount(segments []*SegmentBase, drops []*roaring.Bitmap) uint64 {
	var newDocCount uint64
	for segI, segment := range segments {
		newDocCount += segment.numDocs
		if drops[segI] != nil {
			newDocCount -= drops[segI].GetCardinality()
		}
	}
	return newDocCount
}

func mergeTermFreqNormLocsByCopying(postItr *PostingsIterator,
	newDocNums []uint64, newRoaring *roaring.Bitmap,
	tfEncoder *chunkedIntCoder, locEncoder *chunkedIntCoder, impactEncoder *impactCoder,
	totalTermFrequency *uint64) (
	lastDocNum uint64, lastFreq uint64, lastNorm uint64, err error) {
	nextDocNum, nextFreq, nextNorm, nextFreqNormBytes, nextLocBytes, err :=
		postItr.nextBytes()
	for err == nil && len(nextFreqNormBytes) > 0 {
		hitNewDocNum := newDocNums[nextDocNum]

		newRoaring.Add(uint32(hitNewDocNum))
		if totalTermFrequency != nil {
			*totalTermFrequency += nextFreq
		}

		err = tfEncoder.AddBytes(hitNewDocNum, nextFreqNormBytes)
		if err != nil {
			return 0, 0, 0, err
		}
		impactEncoder.Add(hitNewDocNum, nextFreq, nextNorm)

		if len(nextLocBytes) > 0 {
			err = locEncoder.AddBytes(hitNewDocNum, nextLocBytes)
			if err != nil {
				return 0, 0, 0, err
			}
		}

		lastDocNum = hitNewDocNum
		lastFreq = nextFreq
		lastNorm = nextNorm

		nextDocNum, nextFreq, nextNorm, nextFreqNormBytes, nextLocBytes, err =
			postItr.nextBytes()
	}

	return lastDocNum, lastFreq, lastNorm, err
}

func mergeTermFreqNormLocs(fieldsMap map[string]uint16, postItr *PostingsIterator,
	newDocNums []uint64, newRoaring *roaring.Bitmap,
	tfEncoder, locEncoder *chunkedIntCoder, impactEncoder *impactCoder,
	bufLoc []interimLoc, totalTermFrequency *uint64) (
	lastDocNum, lastFreq, lastNorm uint64, bufLocOut []interimLoc, err error) {
	next, err := postItr.Next()
	for next != nil && err == nil {
		docNum := newDocNums[next.Number()]
		freq := next.Frequency()
		norm := next.(*Posting).NormUint64()
		newRoaring.Add(uint32(docNum))
		if totalTermFrequency != nil {
			*totalTermFrequency += freq
		}
		bufLoc = bufLoc[:0]
		for _, loc := range next.Locations() {
			bufLoc = append(bufLoc, interimLoc{
				fieldID: fieldsMap[loc.Field()] - 1, pos: loc.Pos(),
				start: loc.Start(), end: loc.End(), arrayposs: loc.ArrayPositions(),
			})
		}
		if err := encodePostingFrequency(tfEncoder, docNum, freq, norm, len(bufLoc) > 0); err != nil {
			return 0, 0, 0, nil, err
		}
		if err := encodePostingLocations(locEncoder, docNum, bufLoc); err != nil {
			return 0, 0, 0, nil, err
		}
		impactEncoder.Add(docNum, freq, norm)
		lastDocNum, lastFreq, lastNorm = docNum, freq, norm
		next, err = postItr.Next()
	}
	return lastDocNum, lastFreq, lastNorm, bufLoc, err
}

type varintEncoder func(uint64) (int, error)

type storedFieldsMerger struct {
	fields     map[string]uint16
	options    map[string]index.FieldIndexingOptions
	sameFields bool
	values     [][][]byte
	meta       bytes.Buffer
	data       []byte
	varint     [binary.MaxVarintLen64]byte
	context    *visitDocumentCtx
	writer     *storedBlockWriter
	nextDoc    uint64
}

func mergeStoredAndRemap(segments []*SegmentBase, drops []*roaring.Bitmap,
	fields map[string]uint16, names []string, options map[string]index.FieldIndexingOptions,
	sameFields bool, numDocs uint64, w *CountHashWriter, closeCh chan struct{}) (uint64, [][]uint64, error) {
	m := storedFieldsMerger{
		fields: fields, options: options, sameFields: sameFields,
		values:  make([][][]byte, len(names)),
		context: visitDocumentCtxPool.Get().(*visitDocumentCtx),
		writer:  newStoredBlockWriter(w, numDocs),
	}
	defer visitDocumentCtxPool.Put(m.context)
	mappings := make([][]uint64, len(segments))
	for i, segment := range segments {
		if isClosed(closeCh) {
			return 0, nil, seg.ErrClosed
		}
		mapping, err := m.mergeSegment(segment, drops[i])
		if err != nil {
			return 0, nil, err
		}
		mappings[i] = mapping
	}
	offset, err := m.writer.Close()
	return offset, mappings, err
}

func (m *storedFieldsMerger) mergeSegment(segment *SegmentBase, drops *roaring.Bitmap) ([]uint64, error) {
	mapping := make([]uint64, segment.numDocs)
	for docNum := range mapping {
		if drops != nil && drops.Contains(uint32(docNum)) {
			mapping[docNum] = docDropped
			continue
		}
		if err := m.writeDocument(segment, uint64(docNum)); err != nil {
			return nil, err
		}
		mapping[docNum] = m.nextDoc
		m.nextDoc++
	}
	return mapping, nil
}

func (m *storedFieldsMerger) writeDocument(segment *SegmentBase, docNum uint64) error {
	if m.sameFields {
		meta, data, err := segment.getDocStoredMetaAndData(m.context, docNum)
		if err != nil {
			return err
		}
		return m.writer.Add(meta, data)
	}
	for id := range m.values {
		m.values[id] = m.values[id][:0]
	}
	if err := segment.visitStoredFields(m.context, docNum, m.collectField); err != nil {
		return err
	}
	m.meta.Reset()
	m.data = m.data[:0]
	var offset int
	for fieldID, values := range m.values {
		var err error
		offset, m.data, err = persistStoredFieldValues(fieldID, values, offset, m.encodeMeta, m.data)
		if err != nil {
			return err
		}
	}
	return m.writer.Add(m.meta.Bytes(), m.data)
}

func (m *storedFieldsMerger) collectField(field string, _ byte, value []byte, _ []uint64) bool {
	if m.options[field].IsStored() {
		id := m.fields[field] - 1
		m.values[id] = append(m.values[id], value)
	}
	return true
}

func (m *storedFieldsMerger) encodeMeta(value uint64) (int, error) {
	n := binary.PutUvarint(m.varint[:], value)
	return m.meta.Write(m.varint[:n])
}

func mergeFields(segments []*SegmentBase) (bool, []string, map[string]index.FieldIndexingOptions) {
	fieldsSame := true

	var segment0Fields []string
	if len(segments) > 0 {
		segment0Fields = segments[0].Fields()
	}

	fieldsExist := map[string]struct{}{}
	fieldOptions := map[string]index.FieldIndexingOptions{}
	for _, segment := range segments {
		fields := segment.Fields()
		for fieldi, field := range fields {
			fieldsExist[field] = struct{}{}

			if prev, ok := fieldOptions[field]; ok {
				fieldOptions[field] = prev & segment.fieldsOptions[field]
				if fieldOptions[field] != prev {
					fieldsSame = false
				}
			} else {
				fieldOptions[field] = segment.fieldsOptions[field]
			}
			if len(segment0Fields) != len(fields) || segment0Fields[fieldi] != field {
				fieldsSame = false
			}
		}
	}

	rv := make([]string, 0, len(fieldsExist))
	rv = append(rv, "_id")
	for k := range fieldsExist {
		if k != "_id" {
			rv = append(rv, k)
		}
	}

	sort.Strings(rv[1:]) // leave _id as first

	return fieldsSame, rv, fieldOptions
}

func isClosed(closeCh chan struct{}) bool {
	select {
	case <-closeCh:
		return true
	default:
		return false
	}
}

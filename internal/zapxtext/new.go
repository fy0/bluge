//  Copyright (c) 2018 Couchbase, Inc.
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
	"sync"
	"sync/atomic"

	index "github.com/blevesearch/bleve_index_api"
	segment "github.com/blevesearch/scorch_segment_api/v2"

	"github.com/fy0/bluge/internal/blugeidx"
	blugeseg "github.com/fy0/bluge/segment"
)

var NewSegmentBufferNumResultsBump int = 100
var NewSegmentBufferNumResultsFactor float64 = 1.0
var NewSegmentBufferAvgBytesPerDocFactor float64 = 1.0

const maxNewSegmentPrealloc = 1 << 20

func New(results []*blugeidx.Document, normCalc func(string, int) float32,
	options blugeseg.BuildOptions) (*SegmentBase, uint64, error) {
	if len(results) > math.MaxUint32 {
		return nil, 0, errors.New("too many segment documents")
	}
	chunkMode := DefaultChunkMode
	s := interimPool.Get().(*interim)

	var br bytes.Buffer
	if s.lastNumDocs > 0 {
		estimateAvgBytesPerDoc := int(float64(s.lastOutSize/s.lastNumDocs) *
			NewSegmentBufferNumResultsFactor)
		extraResults := min(NewSegmentBufferNumResultsBump, len(results))
		estimateNumResults := int(float64(len(results)+extraResults) *
			NewSegmentBufferAvgBytesPerDocFactor)
		if estimateAvgBytesPerDoc > 0 && estimateNumResults > 0 {
			estimate := maxNewSegmentPrealloc
			if estimateAvgBytesPerDoc <= maxNewSegmentPrealloc/estimateNumResults {
				estimate = estimateAvgBytesPerDoc * estimateNumResults
			}
			br.Grow(estimate)
		}
	}

	var err error
	s.results = results
	s.options = options
	s.chunkMode = chunkMode
	s.normCalc = normCalc

	s.w = NewCountHashWriter(&br)

	storedIndexOffset, sectionsIndexOffset, err := s.convert()
	if err != nil {
		return nil, uint64(0), err
	}

	sb, err := InitSegmentBase(br.Bytes(), s.w.Sum32(), chunkMode,
		uint64(len(s.results)), storedIndexOffset, sectionsIndexOffset)

	totalBytesWritten := s.getBytesWritten()
	if err == nil && s.reset() == nil {
		s.lastNumDocs = len(results)
		s.lastOutSize = len(br.Bytes())
		sb.setBytesWritten(totalBytesWritten)
		interimPool.Put(s)
	}

	return sb, uint64(len(br.Bytes())), err
}

var interimPool = sync.Pool{New: func() interface{} { return &interim{} }}

type interim struct {
	bytesWritten atomic.Uint64

	results []*blugeidx.Document

	chunkMode uint32

	w *CountHashWriter

	options blugeseg.BuildOptions

	FieldsMap map[string]uint16

	FieldsOptions map[string]index.FieldIndexingOptions

	FieldsInv []string

	metaBuf bytes.Buffer

	tmp0 []byte

	lastNumDocs int
	lastOutSize int

	opaque   map[int]resetable
	normCalc func(string, int) float32
}

func (s *interim) reset() (err error) {
	s.results = nil
	s.chunkMode = 0
	s.w = nil
	clear(s.FieldsMap)
	clear(s.FieldsOptions)
	s.FieldsInv = s.FieldsInv[:0]
	s.metaBuf.Reset()
	s.tmp0 = s.tmp0[:0]
	s.lastNumDocs = 0
	s.lastOutSize = 0
	s.normCalc = nil

	s.setBytesWritten(0)

	if s.opaque != nil {
		for _, v := range s.opaque {
			err = v.Reset()
		}
	} else {
		s.opaque = map[int]resetable{}
	}

	return err
}

type interimStoredField struct {
	vals [][]byte
}

type interimFreqNorm struct {
	freq    uint64
	norm    float32
	numLocs int
}

type interimLoc struct {
	fieldID   uint16
	pos       uint64
	start     uint64
	end       uint64
	arrayposs []uint64
}

func (s *interim) convert() (uint64, uint64, error) {
	if s.FieldsMap == nil {
		s.FieldsMap = map[string]uint16{}
	}
	if s.FieldsOptions == nil {
		s.FieldsOptions = map[string]index.FieldIndexingOptions{}
	}

	s.getOrDefineField(blugeidx.IDFieldName) // _id field is fieldID 0
	s.FieldsOptions[blugeidx.IDFieldName] = index.IndexField | index.StoreField

	var fName string
	for _, result := range s.results {
		result.VisitFields(func(field *blugeidx.Field) {
			fName = field.Name()
			s.getOrDefineField(fName)
			s.FieldsOptions[fName] |= field.Options()
		})
		if s.options.VectorBackend != nil {
			result.VisitVectors(func(vector *blugeidx.Vector) {
				fName = vector.Name()
				s.getOrDefineField(fName)
				s.FieldsOptions[fName] |= index.IndexField
			})
		}
	}

	sort.Strings(s.FieldsInv[1:]) // keep _id as first field

	for fieldID, fieldName := range s.FieldsInv {
		s.FieldsMap[fieldName] = uint16(fieldID + 1)
	}

	args := sectionConfig{
		results:       s.results,
		chunkMode:     s.chunkMode,
		fieldsMap:     s.FieldsMap,
		fieldsInv:     s.FieldsInv,
		fieldsOptions: s.FieldsOptions,
		normCalc:      s.normCalc,
		options:       s.options,
	}
	if s.opaque == nil {
		s.opaque = map[int]resetable{}
		for i, x := range segmentSections {
			s.opaque[i] = x.InitOpaque(args)
		}
	} else {
		for _, op := range s.opaque {
			op.Configure(args)
		}
	}

	s.processDocuments()

	storedIndexOffset, err := s.writeStoredFields()
	if err != nil {
		return 0, 0, err
	}

	for _, x := range segmentSections {
		err = x.Persist(s.opaque, s.w)
		if err != nil {
			return 0, 0, err
		}
	}

	for _, opaque := range s.opaque {
		opaqueIO, ok := opaque.(segment.DiskStatsReporter)
		if ok {
			s.incrementBytesWritten(opaqueIO.BytesWritten())
		}
	}

	stats := s.opaque[SectionInvertedTextIndex].(*invertedIndexOpaque).fieldStats
	sectionsIndexOffset, err := persistFieldsSection(s.FieldsInv, s.FieldsOptions, s.w, s.opaque, stats)
	if err != nil {
		return 0, 0, err
	}

	return storedIndexOffset, sectionsIndexOffset, nil
}

func (s *interim) getOrDefineField(fieldName string) uint16 {
	fieldIDPlus1, exists := s.FieldsMap[fieldName]
	if !exists {
		fieldIDPlus1 = uint16(len(s.FieldsInv) + 1)
		s.FieldsMap[fieldName] = fieldIDPlus1
		s.FieldsInv = append(s.FieldsInv, fieldName)
	}

	return fieldIDPlus1 - 1
}

func (s *interim) processDocuments() {
	for docNum, result := range s.results {
		s.processDocument(uint32(docNum), result)
	}
}

func (s *interim) processDocument(docNum uint32,
	result *blugeidx.Document) {
	visitField := func(field *blugeidx.Field) {
		fieldID := s.getOrDefineField(field.Name())

		for _, section := range segmentSections {
			section.Process(s.opaque, docNum, field, fieldID)
		}
	}
	result.VisitFields(visitField)

	if s.options.VectorBackend != nil {
		result.VisitVectors(func(vector *blugeidx.Vector) {
			fieldID := s.getOrDefineField(vector.Name())
			segmentSections[SectionVectorIndex].(*vectorIndexSection).ProcessVector(s.opaque, docNum, vector, fieldID)
		})
	}

	for _, section := range segmentSections {
		section.Process(s.opaque, docNum, nil, math.MaxUint16)
	}
}

func (s *interim) getBytesWritten() uint64 {
	return s.bytesWritten.Load()
}

func (s *interim) incrementBytesWritten(val uint64) {
	s.bytesWritten.Add(val)
}

func (s *interim) writeStoredFields() (
	storedIndexOffset uint64, err error) {
	varBuf := make([]byte, binary.MaxVarintLen64)
	metaEncode := func(val uint64) (int, error) {
		wb := binary.PutUvarint(varBuf, val)
		return s.metaBuf.Write(varBuf[:wb])
	}

	data := s.tmp0[:0]
	defer func() { s.tmp0 = data }()
	blockWriter := newStoredBlockWriter(s.w, uint64(len(s.results)))

	docStoredFields := map[uint16]interimStoredField{}

	for _, result := range s.results {
		for fieldID := range docStoredFields { // reset for next doc
			delete(docStoredFields, fieldID)
		}

		result.VisitFields(func(field *blugeidx.Field) {
			fieldID := s.getOrDefineField(field.Name())

			if field.Options().IsStored() {
				isf := docStoredFields[fieldID]
				isf.vals = append(isf.vals, field.Value())
				docStoredFields[fieldID] = isf
			}
		})

		var curr int

		s.metaBuf.Reset()
		data = data[:0]

		for fieldID := 0; fieldID < len(s.FieldsInv); fieldID++ {
			isf, exists := docStoredFields[uint16(fieldID)]
			if exists {
				curr, data, err = persistStoredFieldValues(
					fieldID, isf.vals,
					curr, metaEncode, data)
				if err != nil {
					return 0, err
				}
			}
		}

		metaBytes := s.metaBuf.Bytes()

		err = blockWriter.Add(metaBytes, data)
		if err != nil {
			return 0, err
		}
	}

	storedIndexOffset, err = blockWriter.Close()
	if err != nil {
		return 0, err
	}
	s.incrementBytesWritten(blockWriter.bytesWritten)

	return storedIndexOffset, nil
}

func (s *interim) setBytesWritten(val uint64) {
	s.bytesWritten.Store(val)
}

func totalUvarintBytes(a, b, c, d, e uint64, more []uint64) (n uint64) {
	n = numUvarintBytes(a)
	n += numUvarintBytes(b)
	n += numUvarintBytes(c)
	n += numUvarintBytes(d)
	n += numUvarintBytes(e)
	for _, v := range more {
		n += numUvarintBytes(v)
	}
	return n
}

func numUvarintBytes(x uint64) (n uint64) {
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n + 1
}

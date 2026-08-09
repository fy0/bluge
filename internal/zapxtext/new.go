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
	"math"
	"sort"
	"sync"
	"sync/atomic"

	index "github.com/blevesearch/bleve_index_api"
	segment "github.com/blevesearch/scorch_segment_api/v2"
	"github.com/fy0/bluge/internal/blugeidx"
)

var NewSegmentBufferNumResultsBump int = 100
var NewSegmentBufferNumResultsFactor float64 = 1.0
var NewSegmentBufferAvgBytesPerDocFactor float64 = 1.0

// ValidateDocFields can be set by applications to perform additional checks
// on fields in a document being added to a new segment, by default it does
// nothing.
// This API is experimental and may be removed at any time.
var ValidateDocFields = func(field *blugeidx.Field) error {
	return nil
}

// New creates an in-memory zap-encoded SegmentBase from a set of Documents
func (z *ZapPlugin) New(results []*blugeidx.Document) (
	segment.Segment, uint64, error) {
	return z.newWithChunkMode(results, DefaultChunkMode, nil, nil)
}

func (z *ZapPlugin) NewUsing(results []*blugeidx.Document, config map[string]interface{}) (
	segment.Segment, uint64, error) {
	return z.newWithChunkMode(results, DefaultChunkMode, config, nil)
}

func (*ZapPlugin) newWithChunkMode(results []*blugeidx.Document,
	chunkMode uint32, config map[string]interface{}, normCalc func(string, int) float32) (
	segment.Segment, uint64, error) {
	s := interimPool.Get().(*interim)

	var br bytes.Buffer
	if s.lastNumDocs > 0 {
		// use previous results to initialize the buf with an estimate
		// size, but note that the interim instance comes from a
		// global interimPool, so multiple scorch instances indexing
		// different docs can lead to low quality estimates
		estimateAvgBytesPerDoc := int(float64(s.lastOutSize/s.lastNumDocs) *
			NewSegmentBufferNumResultsFactor)
		estimateNumResults := int(float64(len(results)+NewSegmentBufferNumResultsBump) *
			NewSegmentBufferAvgBytesPerDocFactor)
		br.Grow(estimateAvgBytesPerDoc * estimateNumResults)
	}

	var err error
	s.results = results
	clear(s.edgeList)
	s.config = config
	s.chunkMode = chunkMode
	s.normCalc = normCalc

	s.w = NewFileWriterEmpty(NewCountHashWriter(&br))

	storedIndexOffset, sectionsIndexOffset, err := s.convert()
	if err != nil {
		return nil, uint64(0), err
	}

	sb, err := InitSegmentBase(br.Bytes(), s.w.Sum32(), chunkMode,
		uint64(len(s.results)), storedIndexOffset, sectionsIndexOffset, config)

	// get the bytes written before the interim's reset() call
	// write it to the newly formed segment base.
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

// interim holds temporary working data used while converting from
// analysis results to a zap-encoded segment
type interim struct {
	bytesWritten atomic.Uint64

	results []*blugeidx.Document

	// edge list for nested documents: child -> parent
	edgeList map[uint64]uint64

	chunkMode uint32

	w *FileWriter

	config map[string]interface{}

	// FieldsMap adds 1 to field id to avoid zero value issues
	//  name -> field id + 1
	FieldsMap map[string]uint16

	// FieldsOptions holds the indexing options for each field
	FieldsOptions map[string]index.FieldIndexingOptions

	// FieldsInv is the inverse of FieldsMap
	//  field id -> name
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
	clear(s.edgeList)
	clear(s.FieldsMap)
	clear(s.FieldsOptions)
	s.FieldsInv = s.FieldsInv[:0]
	s.metaBuf.Reset()
	s.tmp0 = s.tmp0[:0]
	s.lastNumDocs = 0
	s.lastOutSize = 0
	s.normCalc = nil

	// reset the bytes written stat count
	// to avoid leaking of bytesWritten across reuse cycles.
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
	// special case _id field options: the _id is the canonical document identifier and
	// must always be both indexed and stored so that it can be used for lookups/queries
	// and retrieved back from the stored fields, regardless of user-specified field options.
	s.FieldsOptions[blugeidx.IDFieldName] = index.IndexField | index.StoreField

	var fName string
	for _, result := range s.results {
		result.VisitFields(func(field *blugeidx.Field) {
			fName = field.Name()
			s.getOrDefineField(fName)
			s.FieldsOptions[fName] |= field.Options()
		})
		if vectorSegmentBackend(s.config) != nil {
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

	args := map[string]interface{}{
		"results":       s.results,
		"chunkMode":     s.chunkMode,
		"fieldsMap":     s.FieldsMap,
		"fieldsInv":     s.FieldsInv,
		"fieldsOptions": s.FieldsOptions,
		"normCalc":      s.normCalc,
	}
	if s.config != nil {
		args["config"] = s.config
	}
	if s.opaque == nil {
		s.opaque = map[int]resetable{}
		for i, x := range segmentSections {
			s.opaque[int(i)] = x.InitOpaque(args)
		}
	} else {
		for k, v := range args {
			for _, op := range s.opaque {
				op.Set(k, v)
			}
		}
	}

	s.processDocuments()

	storedIndexOffset, err := s.writeStoredFields()
	if err != nil {
		return 0, 0, err
	}

	// we can persist the various sections at this point.
	// the rule of thumb here is that each section must persist field wise.
	for _, x := range segmentSections {
		err = x.Persist(s.opaque, s.w)
		if err != nil {
			return 0, 0, err
		}
	}

	// after persisting the sections to the writer, account corresponding
	for _, opaque := range s.opaque {
		opaqueIO, ok := opaque.(segment.DiskStatsReporter)
		if ok {
			s.incrementBytesWritten(opaqueIO.BytesWritten())
		}
	}

	// we can persist a new fields section here
	// this new fields section will point to the various indexes available
	stats := s.opaque[SectionInvertedTextIndex].(*invertedIndexOpaque).fieldStats
	sectionsIndexOffset, err := persistFieldsSection(s.FieldsInv, s.FieldsOptions, s.w, s.opaque, stats)
	if err != nil {
		return 0, 0, err
	}

	return storedIndexOffset, sectionsIndexOffset, nil
}

func (s *interim) getOrDefineField(fieldName string) int {
	fieldIDPlus1, exists := s.FieldsMap[fieldName]
	if !exists {
		fieldIDPlus1 = uint16(len(s.FieldsInv) + 1)
		s.FieldsMap[fieldName] = fieldIDPlus1
		s.FieldsInv = append(s.FieldsInv, fieldName)
	}

	return int(fieldIDPlus1 - 1)
}

func (s *interim) processDocuments() {
	for docNum, result := range s.results {
		s.processDocument(uint32(docNum), result)
	}
}

func (s *interim) processDocument(docNum uint32,
	result *blugeidx.Document) {
	visitField := func(field *blugeidx.Field) {
		fieldID := uint16(s.getOrDefineField(field.Name()))

		for _, section := range segmentSections {
			section.Process(s.opaque, docNum, field, fieldID)
		}
	}
	result.VisitFields(visitField)

	if vectorSegmentBackend(s.config) != nil {
		result.VisitVectors(func(vector *blugeidx.Vector) {
			fieldID := uint16(s.getOrDefineField(vector.Name()))
			for _, section := range segmentSections {
				if vectorProcessor, ok := section.(interface {
					ProcessVector(map[int]resetable, uint32, *blugeidx.Vector, uint16)
				}); ok {
					vectorProcessor.ProcessVector(s.opaque, docNum, vector, fieldID)
				}
			}
		})
	}

	// given that as part of visiting each field, there may some kind of totalling
	// or accumulation that can be updated, it becomes necessary to commit or
	// put that totalling/accumulation into effect. However, for certain section
	// types this particular step need not be valid, in which case it would be a
	// no-op in the implmentation of the section's process API.
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

	// keyed by fieldID, for the current doc in the loop
	docStoredFields := map[uint16]interimStoredField{}

	for _, result := range s.results {
		for fieldID := range docStoredFields { // reset for next doc
			delete(docStoredFields, fieldID)
		}

		var validationErr error
		result.VisitFields(func(field *blugeidx.Field) {
			fieldID := uint16(s.getOrDefineField(field.Name()))

			if field.Options().IsStored() {
				isf := docStoredFields[fieldID]
				isf.vals = append(isf.vals, field.Value())
				docStoredFields[fieldID] = isf
			}

			err := ValidateDocFields(field)
			if err != nil && validationErr == nil {
				validationErr = err
			}
		})
		if validationErr != nil {
			return 0, validationErr
		}

		var curr int

		s.metaBuf.Reset()
		data = data[:0]

		// Store every field, including _id, in one compressed payload so repeated
		// values within a document can share Snappy back-references.
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

	// write the number of edges in the child -> parent edge list
	// this will be zero if there are no nested documents
	// and this number also reflects the number of nested documents
	// in the segment
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, uint64(len(s.edgeList)))
	_, err = s.w.Write(buf[:n])
	if err != nil {
		return 0, err
	}
	// write the child -> parent edge list
	// child and parent are both flattened doc ids
	for child, parent := range s.edgeList {
		n = binary.PutUvarint(buf, child)
		_, err = s.w.Write(buf[:n])
		if err != nil {
			return 0, err
		}
		n = binary.PutUvarint(buf, parent)
		_, err = s.w.Write(buf[:n])
		if err != nil {
			return 0, err
		}
	}

	return storedIndexOffset, nil
}

func (s *interim) setBytesWritten(val uint64) {
	s.bytesWritten.Store(val)
}

// returns the total # of bytes needed to encode the given uint64's
// into binary.PutUVarint() encoding
func totalUvarintBytes(a, b, c, d, e uint64, more []uint64) (n int) {
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

// returns # of bytes needed to encode x in binary.PutUvarint() encoding
func numUvarintBytes(x uint64) (n int) {
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n + 1
}

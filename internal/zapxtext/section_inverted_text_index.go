//  Copyright (c) 2023 Couchbase, Inc.
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
	"math"
	"slices"
	"sort"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2"
	index "github.com/blevesearch/bleve_index_api"
	"github.com/blevesearch/vellum"

	blugeseg "github.com/fy0/bluge/segment"

	"github.com/fy0/bluge/analysis"
	"github.com/fy0/bluge/internal/blugeidx"
	"github.com/fy0/bluge/numeric"
)

const (
	docValueModeUnknown uint8 = iota
	docValueModeCanonical
	docValueModeAll
)

type invertedTextIndexSection struct {
}

func (i *invertedTextIndexSection) Process(opaque map[int]resetable, docNum uint32,
	field *blugeidx.Field, fieldID uint16) {
	io := i.getInvertedIndexOpaque(opaque)
	io.process(field, fieldID, docNum)
}

func (i *invertedTextIndexSection) Persist(opaque map[int]resetable, w *CountHashWriter) error {
	io := i.getInvertedIndexOpaque(opaque)
	return io.writeDicts(w)
}

func (i *invertedTextIndexSection) AddrForField(opaque map[int]resetable, fieldID int) uint64 {
	io := i.getInvertedIndexOpaque(opaque)
	return io.fieldAddrs[fieldID]
}

func (i *invertedTextIndexSection) Merge(opaque map[int]resetable, segments []*SegmentBase,
	drops []*roaring.Bitmap, fieldsInv []string, newDocNumsIn [][]uint64,
	w *CountHashWriter, closeCh chan struct{}) error {
	io := i.getInvertedIndexOpaque(opaque)
	if cap(io.fieldStats) >= len(fieldsInv) {
		io.fieldStats = io.fieldStats[:len(fieldsInv)]
		clear(io.fieldStats)
	} else {
		io.fieldStats = make([]fieldStats, len(fieldsInv))
	}
	fieldAddrs, err := mergeAndPersistInvertedSection(segments, drops, fieldsInv, newDocNumsIn, io, w, closeCh)
	if err != nil {
		return err
	}

	io.fieldAddrs = fieldAddrs
	return nil
}

func (i *invertedIndexOpaque) incrementBytesWritten(bytes uint64) {
	i.bytesWritten += bytes
}

func (i *invertedIndexOpaque) BytesWritten() uint64 {
	return i.bytesWritten
}

func (i *invertedIndexOpaque) BytesRead() uint64 {
	return 0
}

func (i *invertedIndexOpaque) ResetBytesRead(uint64) {}

func (io *invertedIndexOpaque) writeDicts(w *CountHashWriter) error {
	if len(io.results) == 0 {
		return nil
	}
	var err error
	if io.builder == nil {
		io.builder, err = vellum.New(&io.builderBuf, nil)
		if err != nil {
			return err
		}
	}
	start := w.Count()
	encoder := newPostingsEncoder(uint64(len(io.results)))
	var docTerms [][]byte
	for fieldID, terms := range io.DictKeys {
		docTerms = prepareDocTerms(docTerms, len(io.results), io.IncludeDocValues[fieldID])
		for _, term := range terms {
			if err := io.writeTerm(fieldID, term, docTerms, encoder, w); err != nil {
				return err
			}
		}
		dictOffset, err := writeDictionary(io.builder, &io.builderBuf, w)
		if err != nil {
			return err
		}
		dvStart, dvEnd, err := io.writeDocValues(fieldID, docTerms, w)
		if err != nil {
			return err
		}
		io.fieldAddrs[fieldID], err = writeInvertedFieldHeader(w, dictOffset, dvStart, dvEnd)
		if err != nil {
			return err
		}
	}
	io.incrementBytesWritten(w.Count() - start)
	return nil
}

func prepareDocTerms(terms [][]byte, count int, include bool) [][]byte {
	if !include {
		return terms[:0]
	}
	if cap(terms) < count {
		return make([][]byte, count)
	}
	terms = terms[:count]
	for i := range terms {
		terms[i] = terms[i][:0]
	}
	return terms
}

func (io *invertedIndexOpaque) writeTerm(fieldID int, term string, docTerms [][]byte,
	encoder *postingsEncoder, w *CountHashWriter) error {
	pid := io.Dicts[fieldID][term] - 1
	postings := io.Postings[pid]
	if err := encoder.reset(io.chunkMode, postings.GetCardinality(), uint64(len(io.results))); err != nil {
		return err
	}
	freqNorms, locs := io.FreqNorms[pid], io.Locs[pid]
	iterator := postings.Iterator()
	freqOffset, locOffset := 0, 0
	for iterator.HasNext() {
		docNum := uint64(iterator.Next())
		freqNorm := freqNorms[freqOffset]
		norm := uint64(math.Float32bits(freqNorm.norm))
		nextLocOffset := locOffset + freqNorm.numLocs
		if err := encoder.add(docNum, freqNorm.freq, norm, locs[locOffset:nextLocOffset]); err != nil {
			return err
		}
		freqOffset++
		locOffset = nextLocOffset
		if len(docTerms) > 0 && includeDocValueTerm(io.DocValueModes[fieldID], term) {
			docTerms[docNum] = append(append(docTerms[docNum], term...), index.DocValueTermSeparator)
		}
	}
	return encoder.finishTerm(postings, []byte(term), io.builder, w)
}

func includeDocValueTerm(mode uint8, term string) bool {
	if mode != docValueModeCanonical {
		return true
	}
	valid, shift := numeric.ValidPrefixCodedTerm(term)
	return valid && shift == 0
}

func (io *invertedIndexOpaque) writeDocValues(fieldID int, docTerms [][]byte,
	w *CountHashWriter) (uint64, uint64, error) {
	if !io.IncludeDocValues[fieldID] {
		return fieldNotUninverted, fieldNotUninverted, nil
	}
	start := w.Count()
	encoder := newChunkedContentCoder(uint64(len(io.results))-1, w)
	for docNum, terms := range docTerms {
		if len(terms) > 0 {
			if err := encoder.Add(uint64(docNum), terms); err != nil {
				return 0, 0, err
			}
		}
	}
	return finishDocValues(encoder, w, start)
}

type accumulatedTerm struct {
	frequency int
	locations []interimLoc
}

type fieldTermAccumulator struct {
	native    analysis.TokenFrequencies
	nativeSet bool
	terms     map[string]*accumulatedTerm
	usingMap  bool
}

func (a *fieldTermAccumulator) reset() {
	a.native = nil
	a.nativeSet = false
	a.usingMap = false
	clear(a.terms)
}

func (a *fieldTermAccumulator) empty() bool {
	if a.nativeSet {
		return len(a.native) == 0
	}
	return !a.usingMap || len(a.terms) == 0
}

func (io *invertedIndexOpaque) process(field *blugeidx.Field, fieldID uint16, docNum uint32) {
	if !io.init && io.results != nil {
		io.realloc()
		io.init = true
	}

	if fieldID == math.MaxUint16 {
		for fid := range io.reusableFieldTerms {
			accumulator := &io.reusableFieldTerms[fid]
			if accumulator.empty() {
				continue
			}
			dict := io.Dicts[fid]
			fieldID := uint16(fid)
			norm := math.Float32frombits(uint32(io.reusableFieldLens[fid]))
			if io.normCalc != nil {
				norm = io.normCalc(io.FieldsInv[fid], io.reusableFieldLens[fid])
			}
			io.fieldStats[fid].documentCount++

			if accumulator.nativeSet {
				for term, tf := range accumulator.native {
					io.appendNativeTerm(fieldID, docNum, dict, term, tf, norm)
				}
			} else {
				for term, tf := range accumulator.terms {
					io.appendAccumulatedTerm(fieldID, docNum, dict, term, tf, norm)
				}
			}
		}
		for i := 0; i < len(io.FieldsInv); i++ { // clear these for reuse
			io.reusableFieldLens[i] = 0
			io.reusableFieldTerms[i].reset()
		}
		return
	}

	io.reusableFieldLens[fieldID] += field.AnalyzedLength()
	accumulator := &io.reusableFieldTerms[fieldID]
	native, isNative := field.NativeTokenFrequencies()
	if !accumulator.nativeSet && !accumulator.usingMap && isNative {
		accumulator.native = native
		accumulator.nativeSet = true
		return
	}
	if accumulator.nativeSet {
		io.materializeNativeTerms(accumulator, fieldID)
	}
	if isNative {
		for term, tf := range native {
			io.accumulateTerm(accumulator, fieldID, term, tf)
		}
		return
	}
	field.EachTerm(func(term blugeseg.FieldTerm) {
		io.accumulateTerm(accumulator, fieldID, string(term.Term()), term)
	})
}

func (io *invertedIndexOpaque) appendNativeTerm(fieldID uint16, docNum uint32,
	dict map[string]uint64, term string, tf *analysis.TokenFreq, norm float32) {
	pid := dict[term] - 1
	frequency := uint64(tf.Frequency())
	io.fieldStats[fieldID].sumTotalTermFrequency += frequency
	io.Postings[pid].Add(docNum)
	io.FreqNorms[pid] = append(io.FreqNorms[pid], interimFreqNorm{
		freq:    frequency,
		norm:    norm,
		numLocs: len(tf.Locations),
	})
	for _, location := range tf.Locations {
		io.Locs[pid] = append(io.Locs[pid], io.makeInterimLocation(fieldID, location))
	}
}

func (io *invertedIndexOpaque) appendAccumulatedTerm(fieldID uint16, docNum uint32,
	dict map[string]uint64, term string, tf *accumulatedTerm, norm float32) {
	pid := dict[term] - 1
	frequency := uint64(tf.frequency)
	io.fieldStats[fieldID].sumTotalTermFrequency += frequency
	io.Postings[pid].Add(docNum)
	io.FreqNorms[pid] = append(io.FreqNorms[pid], interimFreqNorm{
		freq:    frequency,
		norm:    norm,
		numLocs: len(tf.locations),
	})
	io.Locs[pid] = append(io.Locs[pid], tf.locations...)
}

func (io *invertedIndexOpaque) materializeNativeTerms(accumulator *fieldTermAccumulator,
	fieldID uint16) {
	for term, tf := range accumulator.native {
		io.accumulateTerm(accumulator, fieldID, term, tf)
	}
	accumulator.native = nil
	accumulator.nativeSet = false
}

func (io *invertedIndexOpaque) accumulateTerm(accumulator *fieldTermAccumulator,
	fieldID uint16, term string, tf blugeseg.FieldTerm) {
	if accumulator.terms == nil {
		accumulator.terms = make(map[string]*accumulatedTerm)
	}
	accumulator.usingMap = true
	entry := accumulator.terms[term]
	if entry == nil {
		entry = &accumulatedTerm{}
		accumulator.terms[term] = entry
	}
	entry.frequency += tf.Frequency()
	tf.EachLocation(func(location blugeseg.Location) {
		entry.locations = append(entry.locations, io.makeInterimLocation(fieldID, location))
	})
}

func (io *invertedIndexOpaque) makeInterimLocation(fieldID uint16,
	location blugeseg.Location) interimLoc {
	locationFieldID := fieldID
	if location.Field() != "" {
		locationFieldID = io.getOrDefineField(location.Field())
	}
	return interimLoc{
		fieldID: locationFieldID,
		pos:     uint64(location.Pos()),
		start:   uint64(location.Start()),
		end:     uint64(location.End()),
	}
}

func resizeSlice[T any](slice []T, count int) []T {
	return slices.Grow(slice, max(0, count-len(slice)))[:count]
}

func (i *invertedIndexOpaque) initFieldStorage() {
	n := len(i.FieldsInv)
	i.Dicts = resizeSlice(i.Dicts, n)
	i.DictKeys = resizeSlice(i.DictKeys, n)
	for id := range i.Dicts {
		if i.Dicts[id] == nil {
			i.Dicts[id] = make(map[string]uint64)
		} else {
			clear(i.Dicts[id])
		}
		i.DictKeys[id] = i.DictKeys[id][:0]
	}
	i.IncludeDocValues = resizeSlice(i.IncludeDocValues, n)
	i.DocValueModes = resizeSlice(i.DocValueModes, n)
	clear(i.IncludeDocValues)
	clear(i.DocValueModes)
	i.reusableFieldTerms = resizeSlice(i.reusableFieldTerms, n)
	i.reusableFieldLens = resizeSlice(i.reusableFieldLens, n)
	i.fieldStats = resizeSlice(i.fieldStats, n)
	clear(i.fieldStats)
}

func (i *invertedIndexOpaque) realloc() {
	i.initFieldStorage()
	var frequencies, locations int
	for _, result := range i.results {
		result.VisitFields(func(field *blugeidx.Field) {
			freqs, locs := i.countFieldPostings(field)
			frequencies += freqs
			locations += locs
		})
	}
	i.initPostingStorage(frequencies, locations)
	for _, keys := range i.DictKeys {
		sort.Strings(keys)
	}
}

func (i *invertedIndexOpaque) countFieldPostings(field *blugeidx.Field) (frequencies, locations int) {
	fieldID := i.getOrDefineField(field.Name())
	dict := i.Dicts[fieldID]
	visitTerm := func(term string, numLocations int) {
		pidPlus1 := dict[term]
		if pidPlus1 == 0 {
			pidPlus1 = uint64(len(i.numTermsPerPostingsList)) + 1
			dict[term] = pidPlus1
			i.DictKeys[fieldID] = append(i.DictKeys[fieldID], term)
			i.numTermsPerPostingsList = append(i.numTermsPerPostingsList, 0)
			i.numLocsPerPostingsList = append(i.numLocsPerPostingsList, 0)
		}
		i.numTermsPerPostingsList[pidPlus1-1]++
		i.numLocsPerPostingsList[pidPlus1-1] += numLocations
		frequencies++
		locations += numLocations
	}
	if native, ok := field.NativeTokenFrequencies(); ok {
		for term, tf := range native {
			visitTerm(term, len(tf.Locations))
		}
	} else {
		field.EachTerm(func(term blugeseg.FieldTerm) {
			var count int
			term.EachLocation(func(blugeseg.Location) { count++ })
			visitTerm(string(term.Term()), count)
		})
	}
	i.setDocValueMode(fieldID, field)
	return frequencies, locations
}

func (i *invertedIndexOpaque) setDocValueMode(fieldID uint16, field *blugeidx.Field) {
	if !field.Options().IncludeDocValues() {
		return
	}
	i.IncludeDocValues[fieldID] = true
	if !field.CanonicalDocValues() {
		i.DocValueModes[fieldID] = docValueModeAll
	} else if i.DocValueModes[fieldID] == docValueModeUnknown {
		i.DocValueModes[fieldID] = docValueModeCanonical
	}
}

func (i *invertedIndexOpaque) initPostingStorage(frequencies, locations int) {
	count := len(i.numTermsPerPostingsList)
	i.Postings = resizeSlice(i.Postings, count)
	for id := range i.Postings {
		if i.Postings[id] == nil {
			i.Postings[id] = roaring.New()
		}
	}
	i.FreqNorms = resizeSlice(i.FreqNorms, count)
	i.freqNormsBacking = resizeSlice(i.freqNormsBacking, frequencies)
	freqs := i.freqNormsBacking
	for pid, count := range i.numTermsPerPostingsList {
		i.FreqNorms[pid] = freqs[:0:count]
		freqs = freqs[count:]
	}
	i.Locs = resizeSlice(i.Locs, count)
	i.locsBacking = resizeSlice(i.locsBacking, locations)
	locs := i.locsBacking
	for pid, count := range i.numLocsPerPostingsList {
		i.Locs[pid] = locs[:0:count]
		locs = locs[count:]
	}
}

func (i *invertedTextIndexSection) getInvertedIndexOpaque(opaque map[int]resetable) *invertedIndexOpaque {
	return opaque[SectionInvertedTextIndex].(*invertedIndexOpaque)
}

func (i *invertedIndexOpaque) getOrDefineField(fieldName string) uint16 {
	fieldIDPlus1, exists := i.FieldsMap[fieldName]
	if !exists {
		fieldIDPlus1 = uint16(len(i.FieldsInv) + 1)
		i.FieldsMap[fieldName] = fieldIDPlus1
		i.FieldsInv = append(i.FieldsInv, fieldName)

		i.Dicts = append(i.Dicts, make(map[string]uint64))

		n := len(i.DictKeys)
		if n < cap(i.DictKeys) {
			i.DictKeys = i.DictKeys[:n+1]
			i.DictKeys[n] = i.DictKeys[n][:0]
		} else {
			i.DictKeys = append(i.DictKeys, []string(nil))
		}
	}

	return fieldIDPlus1 - 1
}

func (i *invertedTextIndexSection) InitOpaque(args sectionConfig) resetable {
	rv := &invertedIndexOpaque{fieldAddrs: map[int]uint64{}}
	rv.Configure(args)
	return rv
}

type invertedIndexOpaque struct {
	bytesWritten uint64 // atomic access to this variable, moved to top to correct alignment issues on ARM, 386 and 32-bit MIPS.

	results []*blugeidx.Document

	chunkMode uint32

	init bool

	FieldsMap map[string]uint16

	FieldsInv []string

	FieldsOptions map[string]index.FieldIndexingOptions

	Dicts []map[string]uint64

	DictKeys [][]string

	IncludeDocValues []bool
	DocValueModes    []uint8

	Postings []*roaring.Bitmap

	FreqNorms        [][]interimFreqNorm
	freqNormsBacking []interimFreqNorm

	Locs        [][]interimLoc
	locsBacking []interimLoc

	numTermsPerPostingsList []int // key is postings list id
	numLocsPerPostingsList  []int // key is postings list id

	builder    *vellum.Builder
	builderBuf bytes.Buffer

	reusableFieldLens  []int
	reusableFieldTerms []fieldTermAccumulator
	fieldStats         []fieldStats
	normCalc           func(string, int) float32

	fieldAddrs map[int]uint64

	fieldsSame bool
	numDocs    uint64
}

func (io *invertedIndexOpaque) Reset() (err error) {
	io.results = nil
	io.init = false
	io.chunkMode = 0
	io.FieldsMap = nil
	io.FieldsOptions = nil
	io.FieldsInv = nil
	for i := range io.Dicts {
		io.Dicts[i] = nil
	}
	io.Dicts = io.Dicts[:0]
	for i := range io.DictKeys {
		io.DictKeys[i] = io.DictKeys[i][:0]
	}
	io.DictKeys = io.DictKeys[:0]
	for i := range io.IncludeDocValues {
		io.IncludeDocValues[i] = false
	}
	io.IncludeDocValues = io.IncludeDocValues[:0]
	clear(io.DocValueModes)
	io.DocValueModes = io.DocValueModes[:0]
	for _, idn := range io.Postings {
		idn.Clear()
	}
	io.Postings = io.Postings[:0]
	io.FreqNorms = io.FreqNorms[:0]
	for i := range io.freqNormsBacking {
		io.freqNormsBacking[i] = interimFreqNorm{}
	}
	io.freqNormsBacking = io.freqNormsBacking[:0]
	io.Locs = io.Locs[:0]
	for i := range io.locsBacking {
		io.locsBacking[i] = interimLoc{}
	}
	io.locsBacking = io.locsBacking[:0]
	io.numTermsPerPostingsList = io.numTermsPerPostingsList[:0]
	io.numLocsPerPostingsList = io.numLocsPerPostingsList[:0]
	io.builderBuf.Reset()
	if io.builder != nil {
		err = io.builder.Reset(&io.builderBuf)
	}

	io.reusableFieldLens = io.reusableFieldLens[:0]
	for i := range io.reusableFieldTerms {
		io.reusableFieldTerms[i].reset()
	}
	io.reusableFieldTerms = io.reusableFieldTerms[:0]
	io.fieldStats = io.fieldStats[:0]
	io.normCalc = nil

	atomic.StoreUint64(&io.bytesWritten, 0)
	io.fieldsSame = false
	io.numDocs = 0

	clear(io.fieldAddrs)

	return err
}
func (i *invertedIndexOpaque) Configure(args sectionConfig) {
	i.results = args.results
	i.chunkMode = args.chunkMode
	i.fieldsSame = args.fieldsSame
	i.FieldsMap = args.fieldsMap
	i.FieldsOptions = args.fieldsOptions
	i.FieldsInv = args.fieldsInv
	i.numDocs = args.numDocs
	i.normCalc = args.normCalc
}

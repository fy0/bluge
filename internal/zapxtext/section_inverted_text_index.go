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
	"encoding/binary"
	"math"
	"sort"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2"
	index "github.com/blevesearch/bleve_index_api"
	seg "github.com/blevesearch/scorch_segment_api/v2"
	"github.com/blevesearch/vellum"
	blugeseg "github.com/blugelabs/bluge_segment_api"

	"github.com/fy0/bluge/analysis"
	"github.com/fy0/bluge/internal/blugeidx"
)

func init() {
	registerSegmentSection(SectionInvertedTextIndex, &invertedTextIndexSection{})
}

type invertedTextIndexSection struct {
}

func (i *invertedTextIndexSection) Process(opaque map[int]resetable, docNum uint32,
	field *blugeidx.Field, fieldID uint16) {
	io := i.getInvertedIndexOpaque(opaque)
	io.process(field, fieldID, docNum)
}

func (i *invertedTextIndexSection) Persist(opaque map[int]resetable, w *FileWriter) error {
	io := i.getInvertedIndexOpaque(opaque)
	return io.writeDicts(w)
}

func (i *invertedTextIndexSection) AddrForField(opaque map[int]resetable, fieldID int) int {
	io := i.getInvertedIndexOpaque(opaque)
	return io.fieldAddrs[fieldID]
}

func mergeAndPersistInvertedSection(segments []*SegmentBase, dropsIn []*roaring.Bitmap,
	fieldsInv []string, fieldsMap map[string]uint16, fieldsOptions map[string]index.FieldIndexingOptions,
	fieldsSame bool, newDocNumsIn [][]uint64, newSegDocCount uint64, chunkMode uint32, stats []fieldStats, w *FileWriter,
	closeCh chan struct{}) (map[int]int, error) {
	var bufMaxVarintLen64 []byte = make([]byte, binary.MaxVarintLen64)
	var bufLoc []uint64

	var postings *PostingsList
	var postItr *PostingsIterator

	fieldAddrs := make(map[int]int)
	dictOffsets := make([]uint64, len(fieldsInv))
	fieldDvLocsStart := make([]uint64, len(fieldsInv))
	fieldDvLocsEnd := make([]uint64, len(fieldsInv))

	recomputeStats := false
	for _, drop := range dropsIn {
		if drop != nil && !drop.IsEmpty() {
			recomputeStats = true
			break
		}
	}
	if !recomputeStats {
		for fieldID, fieldName := range fieldsInv {
			if !fieldsOptions[fieldName].IsIndexed() {
				continue
			}
			for _, segment := range segments {
				documentCount, sumTotalTermFrequency, ok := segment.FieldStats(fieldName)
				if ok {
					stats[fieldID].add(fieldStats{
						documentCount:         documentCount,
						sumTotalTermFrequency: sumTotalTermFrequency,
					})
				}
			}
		}
	}

	// copying data directly is safe only if there are no
	// file callbacks that might modify the data in all
	// of the involved segments and the current writer
	copyFlag := true
	for _, segment := range segments {
		if segment.fileReader.id != "" {
			copyFlag = false
			break
		}
	}
	if w.id != "" {
		copyFlag = false
	}

	// these int coders are initialized with chunk size 1024
	// however this will be reset to the correct chunk size
	// while processing each individual field-term section
	tfEncoder := newChunkedIntCoder(1024, newSegDocCount-1)
	locEncoder := newChunkedIntCoder(1024, newSegDocCount-1)

	var vellumBuf bytes.Buffer
	newVellum, err := vellum.New(&vellumBuf, nil)
	if err != nil {
		return nil, err
	}

	newRoaring := roaring.NewBitmap()
	var fieldDocs *roaring.Bitmap
	if recomputeStats {
		fieldDocs = roaring.NewBitmap()
	}
	newDocNums := make([][]uint64, 0, len(segments))
	drops := make([]*roaring.Bitmap, 0, len(segments))
	dicts := make([]*Dictionary, 0, len(segments))
	itrs := make([]vellum.Iterator, 0, len(segments))
	segmentsInFocus := make([]*SegmentBase, 0, len(segments))
	// for each field
	for fieldID, fieldName := range fieldsInv {
		var totalTermFrequency *uint64
		if recomputeStats {
			fieldDocs.Clear()
			totalTermFrequency = &stats[fieldID].sumTotalTermFrequency
		}
		// collect FST iterators from all active segments for this field
		newDocNums = newDocNums[:0]
		drops = drops[:0]
		dicts = dicts[:0]
		itrs = itrs[:0]
		segmentsInFocus = segmentsInFocus[:0]
		for segmentI, segment := range segments {
			// check for the closure in meantime
			if isClosed(closeCh) {
				return nil, seg.ErrClosed
			}
			// early exit if the field's index option is false
			if !fieldsOptions[fieldName].IsIndexed() {
				continue
			}

			dict, err2 := segment.dictionary(fieldName)
			if err2 != nil {
				return nil, err2
			}
			if dict != nil && dict.fst != nil {
				itr, err2 := dict.fst.Iterator(nil, nil)
				if err2 != nil && err2 != vellum.ErrIteratorDone {
					return nil, err2
				}
				if itr != nil {
					newDocNums = append(newDocNums, newDocNumsIn[segmentI])
					if dropsIn[segmentI] != nil && !dropsIn[segmentI].IsEmpty() {
						drops = append(drops, dropsIn[segmentI])
					} else {
						drops = append(drops, nil)
					}
					dicts = append(dicts, dict)
					itrs = append(itrs, itr)
					segmentsInFocus = append(segmentsInFocus, segment)
				}
			}
		}

		var prevTerm []byte

		newRoaring.Clear()

		var lastDocNum, lastFreq, lastNorm uint64

		// determines whether to use "1-hit" encoding optimization
		// when a term appears in only 1 doc, with no loc info,
		// has freq of 1, and the docNum fits into 31-bits
		use1HitEncoding := func(termCardinality uint64) (bool, uint64, uint64) {
			if termCardinality == uint64(1) && locEncoder.FinalSize() <= 0 {
				docNum := uint64(newRoaring.Minimum())
				if under32Bits(docNum) && docNum == lastDocNum && lastFreq == 1 {
					return true, docNum, lastNorm
				}
			}
			return false, 0, 0
		}

		finishTerm := func(term []byte) error {
			tfEncoder.Close()
			locEncoder.Close()

			postingsOffset, err := writePostings(newRoaring,
				tfEncoder, locEncoder, use1HitEncoding, w, bufMaxVarintLen64)
			if err != nil {
				return err
			}

			if postingsOffset > 0 {
				err = newVellum.Insert(term, postingsOffset)
				if err != nil {
					return err
				}
			}
			if recomputeStats {
				fieldDocs.Or(newRoaring)
			}

			newRoaring.Clear()

			tfEncoder.Reset()
			locEncoder.Reset()

			lastDocNum = 0
			lastFreq = 0
			lastNorm = 0

			return nil
		}

		enumerator, err := newEnumerator(itrs)

		for err == nil {
			term, itrI, postingsOffset := enumerator.Current()

			if !bytes.Equal(prevTerm, term) {
				// check for the closure in meantime
				if isClosed(closeCh) {
					return nil, seg.ErrClosed
				}

				// if the term changed, write out the info collected
				// for the previous term
				err = finishTerm(prevTerm)
				if err != nil {
					return nil, err
				}
			}
			if !bytes.Equal(prevTerm, term) || prevTerm == nil {
				// compute cardinality of field-term in new seg
				var newCard uint64
				lowItrIdxs, lowItrVals := enumerator.GetLowIdxsAndValues()
				for i, idx := range lowItrIdxs {
					pl, err := dicts[idx].postingsListFromOffset(lowItrVals[i], drops[idx], nil)
					if err != nil {
						return nil, err
					}
					newCard += pl.Count()
				}
				// compute correct chunk size with this
				chunkSize, err := getChunkSize(chunkMode, newCard, newSegDocCount)
				if err != nil {
					return nil, err
				}
				// update encoders chunk
				tfEncoder.SetChunkSize(chunkSize, newSegDocCount-1)
				locEncoder.SetChunkSize(chunkSize, newSegDocCount-1)
			}

			postings, err = dicts[itrI].postingsListFromOffset(
				postingsOffset, drops[itrI], postings)
			if err != nil {
				return nil, err
			}

			postItr = postings.iterator(true, true, true, postItr)

			// can only safely copy data if all segments have same fields and all have an empty
			// writer id (i.e. no callbacks)
			if fieldsSame && copyFlag {
				// can optimize by copying freq/norm/loc bytes directly
				lastDocNum, lastFreq, lastNorm, err = mergeTermFreqNormLocsByCopying(
					term, postItr, newDocNums[itrI], newRoaring,
					tfEncoder, locEncoder, totalTermFrequency)
			} else {
				lastDocNum, lastFreq, lastNorm, bufLoc, err = mergeTermFreqNormLocs(
					fieldsMap, term, postItr, newDocNums[itrI], newRoaring,
					tfEncoder, locEncoder, bufLoc, totalTermFrequency)
			}
			if err != nil {
				return nil, err
			}

			prevTerm = prevTerm[:0] // copy to prevTerm in case Next() reuses term mem
			prevTerm = append(prevTerm, term...)

			err = enumerator.Next()
		}
		if err != vellum.ErrIteratorDone {
			return nil, err
		}
		// close the enumerator to free the underlying iterators
		err = enumerator.Close()
		if err != nil {
			return nil, err
		}

		err = finishTerm(prevTerm)
		if err != nil {
			return nil, err
		}
		if recomputeStats {
			stats[fieldID].documentCount = fieldDocs.GetCardinality()
		}

		dictOffset := uint64(w.Count())

		err = newVellum.Close()
		if err != nil {
			return nil, err
		}
		vellumData := w.process(vellumBuf.Bytes())

		// write out the length of the vellum data
		n := binary.PutUvarint(bufMaxVarintLen64, uint64(len(vellumData)))
		_, err = w.Write(bufMaxVarintLen64[:n])
		if err != nil {
			return nil, err
		}

		// write this vellum to disk
		_, err = w.Write(vellumData)
		if err != nil {
			return nil, err
		}

		dictOffsets[fieldID] = dictOffset

		fieldDvLocsStart[fieldID] = uint64(w.Count())

		// update the field doc values
		// NOTE: doc values continue to use legacy chunk mode
		chunkSize, err := getChunkSize(LegacyChunkMode, 0, 0)
		if err != nil {
			return nil, err
		}
		if fieldsOptions[fieldName].SkipDVChunking() {
			chunkSize = 1
		}
		fdvEncoder := newChunkedContentCoder(chunkSize, newSegDocCount-1, w, true, fieldsOptions[fieldName].SkipDVCompression())

		fdvReadersAvailable := false
		var dvIterClone *docValueReader
		var dvIter *docValueReader
		for segmentI, segment := range segmentsInFocus {
			// check for the closure in meantime
			if isClosed(closeCh) {
				return nil, seg.ErrClosed
			}
			// early exit if docvalues are not wanted for this field
			if !fieldsOptions[fieldName].IncludeDocValues() {
				continue
			}
			fieldIDPlus1 := uint16(segment.fieldsMap[fieldName])
			dvIter = segment.fieldDvReaders[SectionInvertedTextIndex][fieldIDPlus1-1]
			if dvIter != nil {
				fdvReadersAvailable = true
				dvIterClone = dvIter.cloneInto(dvIterClone)
				err = dvIterClone.iterateAllDocValues(segment, func(docNum uint64, terms []byte) error {
					if newDocNums[segmentI][docNum] == docDropped {
						return nil
					}
					err := fdvEncoder.Add(newDocNums[segmentI][docNum], terms)
					if err != nil {
						return err
					}
					return nil
				})
				if err != nil {
					return nil, err
				}
			}
		}

		if fdvReadersAvailable {
			err = fdvEncoder.Close()
			if err != nil {
				return nil, err
			}

			// persist the doc value details for this field
			_, err = fdvEncoder.Write()
			if err != nil {
				return nil, err
			}

			// get the field doc value offset (end)
			fieldDvLocsEnd[fieldID] = uint64(w.Count())
		} else {
			fieldDvLocsStart[fieldID] = fieldNotUninverted
			fieldDvLocsEnd[fieldID] = fieldNotUninverted
		}

		fieldStart := w.Count()

		n = binary.PutUvarint(bufMaxVarintLen64, fieldDvLocsStart[fieldID])
		_, err = w.Write(bufMaxVarintLen64[:n])
		if err != nil {
			return nil, err
		}

		n = binary.PutUvarint(bufMaxVarintLen64, fieldDvLocsEnd[fieldID])
		_, err = w.Write(bufMaxVarintLen64[:n])
		if err != nil {
			return nil, err
		}

		n = binary.PutUvarint(bufMaxVarintLen64, dictOffsets[fieldID])
		_, err = w.Write(bufMaxVarintLen64[:n])
		if err != nil {
			return nil, err
		}

		fieldAddrs[fieldID] = fieldStart

		// reset vellum buffer and vellum builder
		vellumBuf.Reset()
		err = newVellum.Reset(&vellumBuf)
		if err != nil {
			return nil, err
		}
	}
	return fieldAddrs, nil
}

func (i *invertedTextIndexSection) Merge(opaque map[int]resetable, segments []*SegmentBase,
	drops []*roaring.Bitmap, fieldsInv []string, newDocNumsIn [][]uint64,
	w *FileWriter, closeCh chan struct{}) error {
	io := i.getInvertedIndexOpaque(opaque)
	if cap(io.fieldStats) >= len(fieldsInv) {
		io.fieldStats = io.fieldStats[:len(fieldsInv)]
		clear(io.fieldStats)
	} else {
		io.fieldStats = make([]fieldStats, len(fieldsInv))
	}
	fieldAddrs, err := mergeAndPersistInvertedSection(segments, drops, fieldsInv,
		io.FieldsMap, io.FieldsOptions, io.fieldsSame, newDocNumsIn, io.numDocs, io.chunkMode,
		io.fieldStats, w, closeCh)
	if err != nil {
		return err
	}

	io.fieldAddrs = fieldAddrs
	return nil
}

func (i *invertedIndexOpaque) grabBuf(size int) []byte {
	buf := i.tmp0
	if cap(buf) < size {
		buf = make([]byte, size)
		i.tmp0 = buf
	}
	return buf[:size]
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

func (io *invertedIndexOpaque) writeDicts(w *FileWriter) error {
	if len(io.results) == 0 {
		return nil
	}

	dictOffsets := make([]uint64, len(io.FieldsInv))
	var err error

	fdvOffsetsStart := make([]uint64, len(io.FieldsInv))
	fdvOffsetsEnd := make([]uint64, len(io.FieldsInv))

	buf := io.grabBuf(binary.MaxVarintLen64)

	// these int coders are initialized with chunk size 1024
	// however this will be reset to the correct chunk size
	// while processing each individual field-term section
	tfEncoder := newChunkedIntCoder(1024, uint64(len(io.results)-1))
	locEncoder := newChunkedIntCoder(1024, uint64(len(io.results)-1))

	var docTermMap [][]byte

	if io.builder == nil {
		io.builder, err = vellum.New(&io.builderBuf, nil)
		if err != nil {
			return err
		}
	}

	for fieldID, terms := range io.DictKeys {
		if cap(docTermMap) < len(io.results) {
			docTermMap = make([][]byte, len(io.results))
		} else {
			docTermMap = docTermMap[:len(io.results)]
			for docNum := range docTermMap { // reset the docTermMap
				docTermMap[docNum] = docTermMap[docNum][:0]
			}
		}

		dict := io.Dicts[fieldID]

		for _, term := range terms { // terms are already sorted
			pid := dict[term] - 1

			postingsBS := io.Postings[pid]

			freqNorms := io.FreqNorms[pid]
			freqNormOffset := 0

			locs := io.Locs[pid]
			locOffset := 0

			var cardinality uint64
			if postingsBS != nil {
				cardinality = postingsBS.GetCardinality()
			}
			chunkSize, err := getChunkSize(io.chunkMode, cardinality, uint64(len(io.results)))
			if err != nil {
				return err
			}
			tfEncoder.SetChunkSize(chunkSize, uint64(len(io.results)-1))
			locEncoder.SetChunkSize(chunkSize, uint64(len(io.results)-1))

			postingsItr := postingsBS.Iterator()
			for postingsItr.HasNext() {
				docNum := uint64(postingsItr.Next())

				freqNorm := freqNorms[freqNormOffset]

				// check if freq/norm is enabled
				if freqNorm.freq > 0 {
					err = tfEncoder.Add(docNum,
						encodeFreqHasLocs(freqNorm.freq, freqNorm.numLocs > 0),
						uint64(math.Float32bits(freqNorm.norm)))
				} else {
					// if disabled, then skip the norm part
					err = tfEncoder.Add(docNum,
						encodeFreqHasLocs(freqNorm.freq, freqNorm.numLocs > 0))
				}
				if err != nil {
					return err
				}

				if freqNorm.numLocs > 0 {
					numBytesLocs := 0
					for _, loc := range locs[locOffset : locOffset+freqNorm.numLocs] {
						numBytesLocs += totalUvarintBytes(
							uint64(loc.fieldID), loc.pos, loc.start, loc.end,
							uint64(len(loc.arrayposs)), loc.arrayposs)
					}

					err = locEncoder.Add(docNum, uint64(numBytesLocs))
					if err != nil {
						return err
					}
					for _, loc := range locs[locOffset : locOffset+freqNorm.numLocs] {
						err = locEncoder.Add(docNum,
							uint64(loc.fieldID), loc.pos, loc.start, loc.end,
							uint64(len(loc.arrayposs)))
						if err != nil {
							return err
						}

						err = locEncoder.Add(docNum, loc.arrayposs...)
						if err != nil {
							return err
						}
					}
					locOffset += freqNorm.numLocs
				}

				freqNormOffset++

				docTermMap[docNum] = append(
					append(docTermMap[docNum], term...),
					index.DocValueTermSeparator)
			}

			tfEncoder.Close()
			locEncoder.Close()
			io.incrementBytesWritten(locEncoder.getBytesWritten())
			io.incrementBytesWritten(tfEncoder.getBytesWritten())

			postingsOffset, err :=
				writePostings(postingsBS, tfEncoder, locEncoder, nil, w, buf)
			if err != nil {
				return err
			}

			if postingsOffset > uint64(0) {
				err = io.builder.Insert([]byte(term), postingsOffset)
				if err != nil {
					return err
				}
			}

			tfEncoder.Reset()
			locEncoder.Reset()
		}

		err = io.builder.Close()
		if err != nil {
			return err
		}

		// record where this dictionary starts
		dictOffsets[fieldID] = uint64(w.Count())

		vellumData := w.process(io.builderBuf.Bytes())

		// write out the length of the vellum data
		n := binary.PutUvarint(buf, uint64(len(vellumData)))
		_, err = w.Write(buf[:n])
		if err != nil {
			return err
		}

		io.incrementBytesWritten(uint64(len(vellumData)))

		// write this vellum to disk
		_, err = w.Write(vellumData)
		if err != nil {
			return err
		}

		// reset vellum for reuse
		io.builderBuf.Reset()

		err = io.builder.Reset(&io.builderBuf)
		if err != nil {
			return err
		}

		// write the field doc values
		// NOTE: doc values continue to use legacy chunk mode
		chunkSize, err := getChunkSize(LegacyChunkMode, 0, 0)
		if err != nil {
			return err
		}
		if io.FieldsOptions[io.FieldsInv[fieldID]].SkipDVChunking() {
			chunkSize = 1
		}
		fdvEncoder := newChunkedContentCoder(chunkSize, uint64(len(io.results)-1), w, false, io.FieldsOptions[io.FieldsInv[fieldID]].SkipDVCompression())
		if io.IncludeDocValues[fieldID] {
			for docNum, docTerms := range docTermMap {
				if fieldTermMap, ok := io.extraDocValues[docNum]; ok {
					if sTerms, ok := fieldTermMap[uint16(fieldID)]; ok {
						for _, sTerm := range sTerms {
							docTerms = append(append(docTerms, sTerm...), index.DocValueTermSeparator)
						}
					}
				}
				if len(docTerms) > 0 {
					err = fdvEncoder.Add(uint64(docNum), docTerms)
					if err != nil {
						return err
					}
				}
			}
			err = fdvEncoder.Close()
			if err != nil {
				return err
			}

			io.incrementBytesWritten(fdvEncoder.getBytesWritten())

			fdvOffsetsStart[fieldID] = uint64(w.Count())

			_, err = fdvEncoder.Write()
			if err != nil {
				return err
			}

			fdvOffsetsEnd[fieldID] = uint64(w.Count())
			fdvEncoder.Reset()
		} else {
			fdvOffsetsStart[fieldID] = fieldNotUninverted
			fdvOffsetsEnd[fieldID] = fieldNotUninverted
		}

		fieldStart := w.Count()

		n = binary.PutUvarint(buf, fdvOffsetsStart[fieldID])
		_, err = w.Write(buf[:n])
		if err != nil {
			return err
		}

		n = binary.PutUvarint(buf, fdvOffsetsEnd[fieldID])
		_, err = w.Write(buf[:n])
		if err != nil {
			return err
		}

		n = binary.PutUvarint(buf, dictOffsets[fieldID])
		_, err = w.Write(buf[:n])
		if err != nil {
			return err
		}

		io.fieldAddrs[fieldID] = fieldStart
	}

	return nil
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

	// if the fieldID is MaxUint16, it's mainly indicated that the caller has
	// finished invoking the process() for every field on that doc.
	if fieldID == math.MaxUint16 {
		for fid := range io.reusableFieldTerms {
			accumulator := &io.reusableFieldTerms[fid]
			if accumulator.empty() {
				continue
			}
			dict := io.Dicts[fid]
			norm := math.Float32frombits(uint32(io.reusableFieldLens[fid]))
			if io.normCalc != nil {
				norm = io.normCalc(io.FieldsInv[fid], io.reusableFieldLens[fid])
			}
			io.fieldStats[fid].documentCount++

			if accumulator.nativeSet {
				for term, tf := range accumulator.native {
					io.appendNativeTerm(uint16(fid), uint32(docNum), dict, term, tf, norm)
				}
			} else {
				for term, tf := range accumulator.terms {
					io.appendAccumulatedTerm(uint16(fid), uint32(docNum), dict, term, tf, norm)
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
	io.fieldStats[fieldID].sumTotalTermFrequency += uint64(tf.Frequency())
	io.Postings[pid].Add(docNum)
	io.FreqNorms[pid] = append(io.FreqNorms[pid], interimFreqNorm{
		freq:    uint64(tf.Frequency()),
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
	io.fieldStats[fieldID].sumTotalTermFrequency += uint64(tf.frequency)
	io.Postings[pid].Add(docNum)
	io.FreqNorms[pid] = append(io.FreqNorms[pid], interimFreqNorm{
		freq:    uint64(tf.frequency),
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
		locationFieldID = uint16(io.getOrDefineField(location.Field()))
	}
	return interimLoc{
		fieldID: locationFieldID,
		pos:     uint64(location.Pos()),
		start:   uint64(location.Start()),
		end:     uint64(location.End()),
	}
}

func (i *invertedIndexOpaque) initDictsAndKeysFromFields() {
	numFields := len(i.FieldsInv)

	// Resize or allocate Dicts
	if cap(i.Dicts) >= numFields {
		i.Dicts = i.Dicts[:numFields]
	} else {
		i.Dicts = make([]map[string]uint64, numFields)
	}

	// Resize or allocate DictKeys
	if cap(i.DictKeys) >= numFields {
		i.DictKeys = i.DictKeys[:numFields]
	} else {
		i.DictKeys = make([][]string, numFields)
	}

	for idx := 0; idx < numFields; idx++ {
		// --- Dicts ---
		if i.Dicts[idx] == nil {
			i.Dicts[idx] = make(map[string]uint64)
		} else {
			clear(i.Dicts[idx])
		}

		// --- DictKeys ---
		if i.DictKeys[idx] != nil {
			i.DictKeys[idx] = i.DictKeys[idx][:0]
		} else {
			i.DictKeys[idx] = nil
		}
	}
}

func (i *invertedIndexOpaque) realloc() {
	var pidNext int

	var totTFs int
	var totLocs int

	// initialize dicts and dict keys from fieldsMap
	i.initDictsAndKeysFromFields()
	visitField := func(field *blugeidx.Field) {
		fieldID := uint16(i.getOrDefineField(field.Name()))
		dict := i.Dicts[fieldID]
		dictKeys := i.DictKeys[fieldID]
		var fieldTermCount int
		visitTerm := func(term string, numLocations int) {
			pidPlus1, exists := dict[term]
			if !exists {
				pidNext++
				pidPlus1 = uint64(pidNext)
				dict[term] = pidPlus1
				dictKeys = append(dictKeys, term)
				i.numTermsPerPostingsList = append(i.numTermsPerPostingsList, 0)
				i.numLocsPerPostingsList = append(i.numLocsPerPostingsList, 0)
			}
			pid := pidPlus1 - 1
			i.numTermsPerPostingsList[pid]++
			i.numLocsPerPostingsList[pid] += numLocations
			totLocs += numLocations
			fieldTermCount++
		}
		if native, ok := field.NativeTokenFrequencies(); ok {
			for term, tf := range native {
				visitTerm(term, len(tf.Locations))
			}
		} else {
			field.EachTerm(func(term blugeseg.FieldTerm) {
				var numLocations int
				term.EachLocation(func(blugeseg.Location) {
					numLocations++
				})
				visitTerm(string(term.Term()), numLocations)
			})
		}
		totTFs += fieldTermCount
		i.DictKeys[fieldID] = dictKeys
		if field.Options().IncludeDocValues() {
			i.IncludeDocValues[fieldID] = true
		}
	}

	if cap(i.IncludeDocValues) >= len(i.FieldsInv) {
		i.IncludeDocValues = i.IncludeDocValues[:len(i.FieldsInv)]
	} else {
		i.IncludeDocValues = make([]bool, len(i.FieldsInv))
	}

	if i.extraDocValues == nil {
		i.extraDocValues = map[int]map[uint16][][]byte{}
	}

	for _, result := range i.results {
		result.VisitFields(func(field *blugeidx.Field) {
			visitField(field)
		})
	}

	numPostingsLists := pidNext

	if cap(i.Postings) >= numPostingsLists {
		i.Postings = i.Postings[:numPostingsLists]
	} else {
		postings := make([]*roaring.Bitmap, numPostingsLists)
		copy(postings, i.Postings[:cap(i.Postings)])
		for i := 0; i < numPostingsLists; i++ {
			if postings[i] == nil {
				postings[i] = roaring.New()
			}
		}
		i.Postings = postings
	}

	if cap(i.FreqNorms) >= numPostingsLists {
		i.FreqNorms = i.FreqNorms[:numPostingsLists]
	} else {
		i.FreqNorms = make([][]interimFreqNorm, numPostingsLists)
	}

	if cap(i.freqNormsBacking) >= totTFs {
		i.freqNormsBacking = i.freqNormsBacking[:totTFs]
	} else {
		i.freqNormsBacking = make([]interimFreqNorm, totTFs)
	}

	freqNormsBacking := i.freqNormsBacking
	for pid, numTerms := range i.numTermsPerPostingsList {
		i.FreqNorms[pid] = freqNormsBacking[0:0]
		freqNormsBacking = freqNormsBacking[numTerms:]
	}

	if cap(i.Locs) >= numPostingsLists {
		i.Locs = i.Locs[:numPostingsLists]
	} else {
		i.Locs = make([][]interimLoc, numPostingsLists)
	}

	if cap(i.locsBacking) >= totLocs {
		i.locsBacking = i.locsBacking[:totLocs]
	} else {
		i.locsBacking = make([]interimLoc, totLocs)
	}

	locsBacking := i.locsBacking
	for pid, numLocs := range i.numLocsPerPostingsList {
		i.Locs[pid] = locsBacking[0:0]
		locsBacking = locsBacking[numLocs:]
	}

	for _, dict := range i.DictKeys {
		sort.Strings(dict)
	}

	if cap(i.reusableFieldTerms) >= len(i.FieldsInv) {
		i.reusableFieldTerms = i.reusableFieldTerms[:len(i.FieldsInv)]
	} else {
		i.reusableFieldTerms = make([]fieldTermAccumulator, len(i.FieldsInv))
	}

	if cap(i.reusableFieldLens) >= len(i.FieldsInv) {
		i.reusableFieldLens = i.reusableFieldLens[:len(i.FieldsInv)]
	} else {
		i.reusableFieldLens = make([]int, len(i.FieldsInv))
	}

	if cap(i.fieldStats) >= len(i.FieldsInv) {
		i.fieldStats = i.fieldStats[:len(i.FieldsInv)]
		clear(i.fieldStats)
	} else {
		i.fieldStats = make([]fieldStats, len(i.FieldsInv))
	}
}

func (i *invertedTextIndexSection) getInvertedIndexOpaque(opaque map[int]resetable) *invertedIndexOpaque {
	if _, ok := opaque[SectionInvertedTextIndex]; !ok {
		opaque[SectionInvertedTextIndex] = i.InitOpaque(nil)
	}
	return opaque[SectionInvertedTextIndex].(*invertedIndexOpaque)
}

func (i *invertedIndexOpaque) getOrDefineField(fieldName string) int {
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

	return int(fieldIDPlus1 - 1)
}

func (i *invertedTextIndexSection) InitOpaque(args map[string]interface{}) resetable {
	rv := &invertedIndexOpaque{
		fieldAddrs: map[int]int{},
	}
	for k, v := range args {
		rv.Set(k, v)
	}

	return rv
}

type invertedIndexOpaque struct {
	bytesWritten uint64 // atomic access to this variable, moved to top to correct alignment issues on ARM, 386 and 32-bit MIPS.

	results []*blugeidx.Document

	chunkMode uint32

	// indicates whethere the following structs are initialized
	init bool

	// FieldsMap adds 1 to field id to avoid zero value issues
	//  name -> field id + 1
	FieldsMap map[string]uint16

	// FieldsInv is the inverse of FieldsMap
	//  field id -> name
	FieldsInv []string

	// Field indexing options
	//  field name -> options
	FieldsOptions map[string]index.FieldIndexingOptions

	// Term dictionaries for each field
	//  field id -> term -> postings list id + 1
	Dicts []map[string]uint64

	// Terms for each field, where terms are sorted ascending
	//  field id -> []term
	DictKeys [][]string

	// Fields whose IncludeDocValues is true
	//  field id -> bool
	IncludeDocValues []bool

	// postings id -> bitmap of docNums
	Postings []*roaring.Bitmap

	// postings id -> freq/norm's, one for each docNum in postings
	FreqNorms        [][]interimFreqNorm
	freqNormsBacking []interimFreqNorm

	// postings id -> locs, one for each freq
	Locs        [][]interimLoc
	locsBacking []interimLoc

	numTermsPerPostingsList []int // key is postings list id
	numLocsPerPostingsList  []int // key is postings list id

	// store terms that are unnecessary for the term dictionaries but needed in doc values
	// eg - encoded geoshapes
	// docNum -> fieldID -> terms
	extraDocValues map[int]map[uint16][][]byte

	builder    *vellum.Builder
	builderBuf bytes.Buffer

	// reusable stuff for processing fields etc.
	reusableFieldLens  []int
	reusableFieldTerms []fieldTermAccumulator
	fieldStats         []fieldStats
	normCalc           func(string, int) float32

	tmp0 []byte

	fieldAddrs map[int]int

	fieldsSame bool
	numDocs    uint64
}

func (io *invertedIndexOpaque) Reset() (err error) {
	// cleanup stuff over here
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

	io.tmp0 = io.tmp0[:0]
	io.extraDocValues = nil
	atomic.StoreUint64(&io.bytesWritten, 0)
	io.fieldsSame = false
	io.numDocs = 0

	clear(io.fieldAddrs)

	return err
}
func (i *invertedIndexOpaque) Set(key string, val interface{}) {
	switch key {
	case "results":
		i.results = val.([]*blugeidx.Document)
	case "chunkMode":
		i.chunkMode = val.(uint32)
	case "fieldsSame":
		i.fieldsSame = val.(bool)
	case "fieldsMap":
		i.FieldsMap = val.(map[string]uint16)
	case "fieldsOptions":
		i.FieldsOptions = val.(map[string]index.FieldIndexingOptions)
	case "fieldsInv":
		i.FieldsInv = val.([]string)
	case "numDocs":
		i.numDocs = val.(uint64)
	case "normCalc":
		if val != nil {
			i.normCalc = val.(func(string, int) float32)
		}
	}
}

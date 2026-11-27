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
	"encoding/binary"
	"fmt"
	"math"
	"unsafe"

	"github.com/RoaringBitmap/roaring/v2"
	segment "github.com/blevesearch/scorch_segment_api/v2"

	blugeseg "github.com/fy0/bluge/segment"
)

const (
	reflectStaticSizePostingsList     = int(unsafe.Sizeof(PostingsList{}))
	reflectStaticSizePostingsIterator = int(unsafe.Sizeof(PostingsIterator{}))
	reflectStaticSizePosting          = int(unsafe.Sizeof(Posting{}))
	reflectStaticSizeLocation         = int(unsafe.Sizeof(Location{}))
)

const FSTValEncodingMask = uint64(0xc000000000000000)
const FSTValEncodingGeneral = uint64(0x0000000000000000)
const FSTValEncoding1Hit = uint64(0x8000000000000000)

func FSTValEncode1Hit(docNum uint64, normBits uint64) uint64 {
	return FSTValEncoding1Hit | ((mask31Bits & normBits) << 31) | (mask31Bits & docNum)
}

func FSTValDecode1Hit(v uint64) (docNum uint64, normBits uint64) {
	return (mask31Bits & v), (mask31Bits & (v >> 31))
}

const mask31Bits = uint64(0x000000007fffffff)

func under32Bits(x uint64) bool {
	return x <= mask31Bits
}

const DocNum1HitFinished = math.MaxUint64

var NormBits1Hit = uint64(1)

type PostingsList struct {
	sb             *SegmentBase
	postingsOffset uint64
	freqOffset     uint64
	locOffset      uint64
	impactOffset   uint64
	postings       *roaring.Bitmap
	except         *roaring.Bitmap

	docNum1Hit   uint64
	normBits1Hit uint64

	chunkSize uint32

	bytesRead uint64
}

var emptyPostingsList = &PostingsList{}

func (p *PostingsList) Size() int {
	sizeInBytes := reflectStaticSizePostingsList + SizeOfPtr

	if p.except != nil {
		sizeInBytes += int(p.except.GetSizeInBytes())
	}

	return sizeInBytes
}

func (p *PostingsList) OrInto(receiver *roaring.Bitmap) {
	if p.normBits1Hit != 0 {
		receiver.Add(uint32(p.docNum1Hit))
		return
	}

	if p.postings != nil {
		receiver.Or(p.postings)
	}
}

func (p *PostingsList) Iterator(includeFreq, includeNorm, includeLocs bool,
	prealloc segment.PostingsIterator) segment.PostingsIterator {
	if p.normBits1Hit == 0 && p.postings == nil {
		return emptyPostingsIterator
	}

	var preallocPI *PostingsIterator
	pi, ok := prealloc.(*PostingsIterator)
	if ok && pi != nil {
		preallocPI = pi
	}
	if preallocPI == emptyPostingsIterator {
		preallocPI = nil
	}

	return p.iterator(includeFreq, includeNorm, includeLocs, preallocPI)
}

func (p *PostingsList) iterator(includeFreq, includeNorm, includeLocs bool,
	rv *PostingsIterator) *PostingsIterator {
	if rv == nil {
		rv = &PostingsIterator{}
	} else {
		freqNormReader := rv.freqNormReader
		if freqNormReader != nil {
			freqNormReader.reset()
		}

		locReader := rv.locReader
		if locReader != nil {
			locReader.reset()
		}

		nextLocs := rv.nextLocs[:0]
		nextSegmentLocs := rv.nextSegmentLocs[:0]

		buf := rv.buf
		impactDecoder := rv.impactDecoder

		*rv = PostingsIterator{} // clear the struct

		rv.freqNormReader = freqNormReader
		rv.locReader = locReader

		rv.nextLocs = nextLocs
		rv.nextSegmentLocs = nextSegmentLocs

		rv.buf = buf
		rv.impactDecoder = impactDecoder
		if rv.impactDecoder != nil {
			rv.impactDecoder.initialized = false
			rv.impactDecoder.data = nil
		}
	}

	rv.postings = p
	rv.includeFreqNorm = includeFreq || includeNorm || includeLocs
	rv.includeLocs = includeLocs

	if p.normBits1Hit != 0 {
		rv.docNum1Hit = p.docNum1Hit
		rv.normBits1Hit = p.normBits1Hit

		if p.except != nil && p.except.Contains(uint32(rv.docNum1Hit)) {
			rv.docNum1Hit = DocNum1HitFinished
		}

		return rv
	}

	if p.postings == nil {
		return rv
	}

	if rv.includeFreqNorm {
		rv.freqNormReader = newChunkedIntDecoder(p.sb.mem, p.freqOffset, rv.freqNormReader)
		rv.incrementBytesRead(rv.freqNormReader.getBytesRead())
	}

	if rv.includeLocs {
		rv.locReader = newChunkedIntDecoder(p.sb.mem, p.locOffset, rv.locReader)
		rv.incrementBytesRead(rv.locReader.getBytesRead())
	}

	rv.all = p.postings.Iterator()
	if p.except != nil {
		rv.ActualBM = roaring.AndNot(p.postings, p.except)
		rv.Actual = rv.ActualBM.Iterator()
	} else {
		rv.ActualBM = p.postings
		rv.Actual = rv.all // Optimize to use same iterator for all & Actual.
	}

	return rv
}

func (p *PostingsList) Count() uint64 {
	var n, e uint64
	if p.normBits1Hit != 0 {
		n = 1
		if p.except != nil && p.except.Contains(uint32(p.docNum1Hit)) {
			e = 1
		}
	} else if p.postings != nil {
		n = p.postings.GetCardinality()
		if p.except != nil {
			e = p.postings.AndCardinality(p.except)
		}
	}
	return n - e
}

func (p *PostingsList) ResetBytesRead(val uint64) {
	p.bytesRead = val
}

func (p *PostingsList) BytesRead() uint64 {
	return p.bytesRead
}

func (p *PostingsList) incrementBytesRead(val uint64) {
	p.bytesRead += val
}

func (p *PostingsList) BytesWritten() uint64 {
	return 0
}

func (rv *PostingsList) read(postingsOffset uint64, d *Dictionary) error {
	rv.postingsOffset = postingsOffset

	if rv.postingsOffset&FSTValEncodingMask == FSTValEncoding1Hit {
		return rv.init1Hit(postingsOffset)
	}

	var n uint64
	var read int

	rv.freqOffset, read = binary.Uvarint(d.sb.mem[postingsOffset+n : postingsOffset+binary.MaxVarintLen64])
	n += uint64(read)

	rv.locOffset, read = binary.Uvarint(d.sb.mem[postingsOffset+n : postingsOffset+n+binary.MaxVarintLen64])
	n += uint64(read)

	rv.impactOffset, read = binary.Uvarint(d.sb.mem[postingsOffset+n : postingsOffset+n+binary.MaxVarintLen64])
	n += uint64(read)

	var postingsLen uint64
	postingsLen, read = binary.Uvarint(d.sb.mem[postingsOffset+n : postingsOffset+n+binary.MaxVarintLen64])
	n += uint64(read)

	roaringBytes := d.sb.mem[postingsOffset+n : postingsOffset+n+postingsLen]

	rv.incrementBytesRead(n + postingsLen)

	if rv.postings == nil {
		rv.postings = roaring.NewBitmap()
	}
	_, err := rv.postings.FromBuffer(roaringBytes)
	if err != nil {
		return fmt.Errorf("error loading roaring bitmap: %v", err)
	}

	chunkSize, err := getChunkSize(d.sb.chunkMode,
		rv.postings.GetCardinality(), d.sb.numDocs)
	if err != nil {
		return fmt.Errorf("failed to get chunk size: %v", err)
	}

	rv.chunkSize = uint32(chunkSize)

	return nil
}

func (rv *PostingsList) init1Hit(fstVal uint64) error {
	docNum, normBits := FSTValDecode1Hit(fstVal)

	rv.docNum1Hit = docNum
	rv.normBits1Hit = normBits

	return nil
}

type PostingsIterator struct {
	postings *PostingsList
	all      roaring.IntPeekable
	Actual   roaring.IntPeekable
	ActualBM *roaring.Bitmap

	currChunk      uint32
	freqNormReader *chunkedIntDecoder
	locReader      *chunkedIntDecoder

	next            Posting            // reused across Next() calls
	nextLocs        []Location         // reused across Next() calls
	nextSegmentLocs []segment.Location // reused across Next() calls

	docNum1Hit   uint64
	normBits1Hit uint64

	buf []byte

	impactDecoder *impactDecoder

	includeFreqNorm bool
	includeLocs     bool

	bytesRead uint64
}

var emptyPostingsIterator = &PostingsIterator{}

func (i *PostingsIterator) Size() int {
	sizeInBytes := reflectStaticSizePostingsIterator + SizeOfPtr +
		i.next.Size()
	for _, entry := range i.nextLocs {
		sizeInBytes += entry.Size()
	}

	return sizeInBytes
}

func (i *PostingsIterator) ResetBytesRead(val uint64) {
	i.bytesRead = val
}

func (i *PostingsIterator) BytesRead() uint64 {
	return i.bytesRead
}

func (i *PostingsIterator) incrementBytesRead(val uint64) {
	i.bytesRead += val
}

func (i *PostingsIterator) BytesWritten() uint64 {
	return 0
}

func (i *PostingsIterator) HasImpacts() bool {
	return i != nil && i.postings != nil && i.postings.impactOffset != 0
}

func (i *PostingsIterator) AdvanceShallow(docNum uint64) (
	uint64, []blugeseg.Impact, bool, error) {
	if !i.HasImpacts() {
		return 0, nil, false, nil
	}
	if i.impactDecoder == nil {
		i.impactDecoder = &impactDecoder{}
	}
	if !i.impactDecoder.initialized {
		offset := i.postings.impactOffset
		mem := i.postings.sb.mem
		if offset >= uint64(len(mem)) {
			return 0, nil, false, fmt.Errorf("impact offset %d out of bounds", offset)
		}
		length, n := binary.Uvarint(mem[offset:])
		if n <= 0 {
			return 0, nil, false, fmt.Errorf("invalid impact length at offset %d", offset)
		}
		start := offset + uint64(n)
		if length > uint64(len(mem))-start {
			return 0, nil, false, fmt.Errorf("impact length %d out of bounds", length)
		}
		encoded := mem[start : start+length]

		i.incrementBytesRead(uint64(n) + length)
		if err := i.impactDecoder.reset(encoded); err != nil {
			return 0, nil, false, err
		}
	}
	return i.impactDecoder.advanceShallow(docNum)
}

func (i *PostingsIterator) loadChunk(chunk int) error {
	if i.includeFreqNorm {
		err := i.freqNormReader.loadChunk(chunk)
		if err != nil {
			return err
		}

		i.ResetBytesRead(i.freqNormReader.getBytesRead())
	}

	if i.includeLocs {
		err := i.locReader.loadChunk(chunk)
		if err != nil {
			return err
		}
		i.ResetBytesRead(i.locReader.getBytesRead())
	}

	i.currChunk = uint32(chunk)
	return nil
}

func (i *PostingsIterator) readFreqNormHasLocs() (uint64, uint64, bool, error) {
	if i.normBits1Hit != 0 {
		return 1, i.normBits1Hit, false, nil
	}

	freqHasLocs, err := i.freqNormReader.readUvarint()
	if err != nil {
		return 0, 0, false, fmt.Errorf("error reading frequency: %v", err)
	}

	freq, hasLocs := decodeFreqHasLocs(freqHasLocs)
	if freq == 0 {
		return freq, 0, hasLocs, nil
	}

	normBits, err := i.freqNormReader.readUvarint()
	if err != nil {
		return 0, 0, false, fmt.Errorf("error reading norm: %v", err)
	}

	return freq, normBits, hasLocs, nil
}

func (i *PostingsIterator) skipFreqNormReadHasLocs() (bool, error) {
	if i.normBits1Hit != 0 {
		return false, nil
	}

	freqHasLocs, err := i.freqNormReader.readUvarint()
	if err != nil {
		return false, fmt.Errorf("error reading freqHasLocs: %v", err)
	}

	freq, hasLocs := decodeFreqHasLocs(freqHasLocs)
	if freq == 0 {
		return hasLocs, nil
	}

	i.freqNormReader.SkipUvarint() // Skip normBits.

	return hasLocs, nil // See decodeFreqHasLocs() / hasLocs.
}

func encodeFreqHasLocs(freq uint64, hasLocs bool) uint64 {
	rv := freq << 1
	if hasLocs {
		rv = rv | 0x01 // 0'th LSB encodes whether there are locations
	}
	return rv
}

func decodeFreqHasLocs(freqHasLocs uint64) (uint64, bool) {
	freq := freqHasLocs >> 1
	hasLocs := freqHasLocs&0x01 != 0
	return freq, hasLocs
}

func (i *PostingsIterator) readLocation(l *Location) error {
	fieldID, err := i.locReader.readUvarint()
	if err != nil {
		return fmt.Errorf("error reading location field: %v", err)
	}
	pos, err := i.locReader.readUvarint()
	if err != nil {
		return fmt.Errorf("error reading location pos: %v", err)
	}
	start, err := i.locReader.readUvarint()
	if err != nil {
		return fmt.Errorf("error reading location start: %v", err)
	}
	end, err := i.locReader.readUvarint()
	if err != nil {
		return fmt.Errorf("error reading location end: %v", err)
	}
	numArrayPos, err := i.locReader.readUvarint()
	if err != nil {
		return fmt.Errorf("error reading location num array pos: %v", err)
	}

	l.field = i.postings.sb.fieldsInv[fieldID]
	l.pos = pos
	l.start = start
	l.end = end

	if uint64(cap(l.ap)) < numArrayPos {
		l.ap = make([]uint64, numArrayPos)
	} else {
		l.ap = l.ap[:numArrayPos]
	}

	for k := range l.ap {
		ap, err := i.locReader.readUvarint()
		if err != nil {
			return fmt.Errorf("error reading array position: %v", err)
		}

		l.ap[k] = ap
	}

	return nil
}

func (i *PostingsIterator) Next() (segment.Posting, error) {
	return i.nextAtOrAfter(0)
}

func (i *PostingsIterator) Advance(docNum uint64) (segment.Posting, error) {
	return i.nextAtOrAfter(docNum)
}

func (i *PostingsIterator) nextAtOrAfter(atOrAfter uint64) (segment.Posting, error) {
	docNum, exists, err := i.nextDocNumAtOrAfter(atOrAfter)
	if err != nil || !exists {
		return nil, err
	}

	i.next = Posting{} // clear the struct
	rv := &i.next
	rv.docNum = docNum

	if !i.includeFreqNorm {
		return rv, nil
	}

	var normBits uint64
	var hasLocs bool

	rv.freq, normBits, hasLocs, err = i.readFreqNormHasLocs()
	if err != nil {
		return nil, err
	}

	rv.norm = math.Float32frombits(uint32(normBits))

	if i.includeLocs && hasLocs {
		if rv.freq > 0 {
			if uint64(cap(i.nextLocs)) >= rv.freq {
				i.nextLocs = i.nextLocs[0:rv.freq]
			} else {
				i.nextLocs = make([]Location, rv.freq, rv.freq*2)
			}
			if uint64(cap(i.nextSegmentLocs)) < rv.freq {
				i.nextSegmentLocs = make([]segment.Location, rv.freq, rv.freq*2)
			}
			rv.locs = i.nextSegmentLocs[:0]
		}

		numLocsBytes, err := i.locReader.readUvarint()
		if err != nil {
			return nil, fmt.Errorf("error reading location numLocsBytes: %v", err)
		}

		j := 0
		var nextLoc *Location
		startBytesRemaining := i.locReader.Len() // # bytes remaining in the locReader
		for startBytesRemaining-i.locReader.Len() < int(numLocsBytes) {
			if len(i.nextLocs) > j {
				nextLoc = &i.nextLocs[j]
			} else {
				nextLoc = &Location{}
			}

			err := i.readLocation(nextLoc)
			if err != nil {
				return nil, err
			}

			rv.locs = append(rv.locs, nextLoc)
			j++
		}
	}

	return rv, nil
}

func (i *PostingsIterator) nextSingleHit(atOrAfter uint64) (uint64, bool, error) {
	if i.docNum1Hit == DocNum1HitFinished {
		return 0, false, nil
	}
	docNum := i.docNum1Hit
	i.docNum1Hit = DocNum1HitFinished
	if docNum < atOrAfter {
		return 0, false, nil
	}
	return docNum, true, nil
}

func (i *PostingsIterator) nextDocNumAtOrAfter(atOrAfter uint64) (uint64, bool, error) {
	if i.normBits1Hit != 0 {
		return i.nextSingleHit(atOrAfter)
	}

	if i.Actual == nil || !i.Actual.HasNext() {
		return 0, false, nil
	}

	if i.postings == nil || i.postings == emptyPostingsList {
		return 0, false, nil
	}

	if i.postings.postings == i.ActualBM {
		return i.nextDocNumAtOrAfterClean(atOrAfter)
	}

	if i.postings.chunkSize == 0 {
		return 0, false, ErrChunkSizeZero
	}

	i.Actual.AdvanceIfNeeded(uint32(atOrAfter))

	if !i.Actual.HasNext() || !i.all.HasNext() {
		return 0, false, nil
	}

	n := i.Actual.Next()
	allN := i.all.Next()
	nChunk := n / i.postings.chunkSize

	allNReachesNChunk := nChunk * i.postings.chunkSize

	for allN != n {
		if i.includeFreqNorm && allN >= allNReachesNChunk {
			err := i.currChunkNext(nChunk)
			if err != nil {
				return 0, false, err
			}
		}

		if !i.all.HasNext() {
			return 0, false, nil
		}

		allN = i.all.Next()
	}

	if i.includeFreqNorm && (i.currChunk != nChunk || i.freqNormReader.isNil()) {
		err := i.loadChunk(int(nChunk))
		if err != nil {
			return 0, false, fmt.Errorf("error loading chunk: %v", err)
		}
	}

	return uint64(n), true, nil
}

var freqHasLocs1Hit = encodeFreqHasLocs(1, false)

func (i *PostingsIterator) nextBytes() (
	docNumOut uint64, freq uint64, normBits uint64,
	bytesFreqNorm []byte, bytesLoc []byte, err error) {
	docNum, exists, err := i.nextDocNumAtOrAfter(0)
	if err != nil || !exists {
		return 0, 0, 0, nil, nil, err
	}

	if i.normBits1Hit != 0 {
		if i.buf == nil {
			i.buf = make([]byte, binary.MaxVarintLen64*2)
		}
		n := binary.PutUvarint(i.buf, freqHasLocs1Hit)
		n += binary.PutUvarint(i.buf[n:], i.normBits1Hit)
		return docNum, uint64(1), i.normBits1Hit, i.buf[:n], nil, nil
	}

	startFreqNorm := i.freqNormReader.remainingLen()

	var hasLocs bool

	freq, normBits, hasLocs, err = i.readFreqNormHasLocs()
	if err != nil {
		return 0, 0, 0, nil, nil, err
	}

	endFreqNorm := i.freqNormReader.remainingLen()
	bytesFreqNorm = i.freqNormReader.readBytes(startFreqNorm, endFreqNorm)

	if hasLocs {
		startLoc := i.locReader.remainingLen()

		numLocsBytes, err := i.locReader.readUvarint()
		if err != nil {
			return 0, 0, 0, nil, nil,
				fmt.Errorf("error reading location nextBytes numLocs: %v", err)
		}

		i.locReader.SkipBytes(int(numLocsBytes))

		endLoc := i.locReader.remainingLen()
		bytesLoc = i.locReader.readBytes(startLoc, endLoc)
	}

	return docNum, freq, normBits, bytesFreqNorm, bytesLoc, nil
}

func (i *PostingsIterator) nextDocNumAtOrAfterClean(
	atOrAfter uint64) (uint64, bool, error) {
	if !i.includeFreqNorm {
		i.Actual.AdvanceIfNeeded(uint32(atOrAfter))

		if !i.Actual.HasNext() {
			return 0, false, nil // couldn't find anything
		}

		return uint64(i.Actual.Next()), true, nil
	}

	if i.postings != nil && i.postings.chunkSize == 0 {
		return 0, false, ErrChunkSizeZero
	}
	if atOrAfter == 0 {
		n := i.Actual.Next()
		nChunk := n / i.postings.chunkSize
		if i.currChunk != nChunk || i.freqNormReader.isNil() {
			if err := i.loadChunk(int(nChunk)); err != nil {
				return 0, false, fmt.Errorf("load sequential freq-norm chunk: %v", err)
			}
		}
		return uint64(n), true, nil
	}
	if i.postings.postings.GetCardinality()*2 < i.postings.sb.numDocs {
		return i.nextDocNumAtOrAfterCleanSparse(atOrAfter)
	}

	i.Actual.AdvanceIfNeeded(uint32(atOrAfter))
	if !i.Actual.HasNext() {
		return 0, false, nil
	}

	n := i.Actual.Next()
	nChunk := n / i.postings.chunkSize
	if err := i.loadChunk(int(nChunk)); err != nil {
		return 0, false, fmt.Errorf("error loading chunk: %v", err)
	}

	chunkStart := nChunk * i.postings.chunkSize
	var beforeTarget uint64
	if n > 0 {
		beforeTarget = i.postings.postings.Rank(n - 1)
	}
	if chunkStart > 0 {
		beforeTarget -= i.postings.postings.Rank(chunkStart - 1)
	}
	for j := uint64(0); j < beforeTarget; j++ {
		if err := i.currChunkNext(nChunk); err != nil {
			return 0, false, fmt.Errorf("seek freq-norm within chunk: %v", err)
		}
	}

	return uint64(n), true, nil
}

func (i *PostingsIterator) nextDocNumAtOrAfterCleanSparse(
	atOrAfter uint64) (uint64, bool, error) {
	sameChunkNexts := 0
	n := i.Actual.Next()
	nChunk := n / i.postings.chunkSize

	for uint64(n) < atOrAfter && i.Actual.HasNext() {
		n = i.Actual.Next()
		previousChunk := nChunk
		nChunk = n / i.postings.chunkSize
		if nChunk != previousChunk {
			sameChunkNexts = 0
		} else {
			sameChunkNexts++
		}
	}
	if uint64(n) < atOrAfter {
		return 0, false, nil
	}
	for j := 0; j < sameChunkNexts; j++ {
		if err := i.currChunkNext(nChunk); err != nil {
			return 0, false, fmt.Errorf("seek sparse freq-norm within chunk: %v", err)
		}
	}
	if i.currChunk != nChunk || i.freqNormReader.isNil() {
		if err := i.loadChunk(int(nChunk)); err != nil {
			return 0, false, fmt.Errorf("error loading chunk: %v", err)
		}
	}
	return uint64(n), true, nil
}

func (i *PostingsIterator) currChunkNext(nChunk uint32) error {
	if i.currChunk != nChunk || i.freqNormReader.isNil() {
		err := i.loadChunk(int(nChunk))
		if err != nil {
			return fmt.Errorf("error loading chunk: %v", err)
		}
	}

	hasLocs, err := i.skipFreqNormReadHasLocs()
	if err != nil {
		return err
	}

	if i.includeLocs && hasLocs {
		numLocsBytes, err := i.locReader.readUvarint()
		if err != nil {
			return fmt.Errorf("error reading location numLocsBytes: %v", err)
		}

		i.locReader.SkipBytes(int(numLocsBytes))
	}

	return nil
}

func (p *PostingsIterator) DocNum1Hit() (uint64, bool) {
	if p.normBits1Hit != 0 && p.docNum1Hit != DocNum1HitFinished {
		return p.docNum1Hit, true
	}
	return 0, false
}

func (p *PostingsIterator) ActualBitmap() *roaring.Bitmap {
	return p.ActualBM
}

func (p *PostingsIterator) ReplaceActual(abm *roaring.Bitmap) {
	p.ActualBM = abm
	p.Actual = abm.Iterator()
}

func PostingsIteratorFromBitmap(bm *roaring.Bitmap,
	includeFreqNorm, includeLocs bool) (segment.PostingsIterator, error) {
	return &PostingsIterator{
		ActualBM:        bm,
		Actual:          bm.Iterator(),
		includeFreqNorm: includeFreqNorm,
		includeLocs:     includeLocs,
	}, nil
}

func PostingsIteratorFrom1Hit(docNum1Hit uint64,
	includeFreqNorm, includeLocs bool) (segment.PostingsIterator, error) {
	return &PostingsIterator{
		docNum1Hit:      docNum1Hit,
		normBits1Hit:    NormBits1Hit,
		includeFreqNorm: includeFreqNorm,
		includeLocs:     includeLocs,
	}, nil
}

type Posting struct {
	docNum uint64
	freq   uint64
	norm   float32
	locs   []segment.Location
}

func (p *Posting) Size() int {
	sizeInBytes := reflectStaticSizePosting

	for _, entry := range p.locs {
		sizeInBytes += entry.Size()
	}

	return sizeInBytes
}

func (p *Posting) Number() uint64 {
	return p.docNum
}

func (p *Posting) Frequency() uint64 {
	return p.freq
}

func (p *Posting) Norm() float64 {
	return float64(p.norm)
}

func (p *Posting) Locations() []segment.Location {
	return p.locs
}

func (p *Posting) NormUint64() uint64 {
	return uint64(math.Float32bits(p.norm))
}

type Location struct {
	field string
	pos   uint64
	start uint64
	end   uint64
	ap    []uint64
}

func (l *Location) Size() int {
	return reflectStaticSizeLocation +
		len(l.field) +
		len(l.ap)*SizeOfUint64
}

func (l *Location) Field() string {
	return l.field
}

func (l *Location) Start() uint64 {
	return l.start
}

func (l *Location) End() uint64 {
	return l.end
}

func (l *Location) Pos() uint64 {
	return l.pos
}

func (l *Location) ArrayPositions() []uint64 {
	return l.ap
}

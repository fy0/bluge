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
	"io"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/RoaringBitmap/roaring/v2"
	index "github.com/blevesearch/bleve_index_api"
	mmap "github.com/blevesearch/mmap-go"
	segment "github.com/blevesearch/scorch_segment_api/v2"
)

const reflectStaticSizeSegmentBase = int(unsafe.Sizeof(SegmentBase{}))

func Open(path string) (*Segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	mm, err := mmap.Map(f, mmap.RDONLY, 0)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	sb, err := LoadBytes(mm)
	if err != nil {
		_ = mm.Unmap()
		_ = f.Close()
		return nil, err
	}
	return &Segment{
		SegmentBase: *sb,
		f:           f,
		mm:          mm,
		path:        path,
		refs:        1,
		version:     sb.version,
		crc:         sb.memCRC,
	}, nil
}

type SegmentBase struct {
	bytesRead    uint64
	bytesWritten uint64

	mem                 []byte
	memCRC              uint32
	version             uint32
	chunkMode           uint32
	fieldsMap           map[string]uint16                     // fieldName -> fieldID+1
	fieldsOptions       map[string]index.FieldIndexingOptions // fieldName -> fieldOptions
	fieldsInv           []string                              // fieldID -> fieldName
	fieldStats          []fieldStats                          // fieldID -> collection statistics
	fieldsSectionsMap   [][]uint64                            // fieldID -> section -> address
	numDocs             uint64
	storedIndexOffset   uint64
	sectionsIndexOffset uint64
	fieldDvReaders      [][]*docValueReader // naive chunk cache per field; section->fieldID->reader
	fieldDvNames        []string            // field names cached in fieldDvReaders
	size                int

	invIndexCache *invertedIndexCache
}

func (sb *SegmentBase) FormatVersion() uint32 {
	return sb.version
}

func (sb *SegmentBase) Size() int {
	return sb.size
}

func (sb *SegmentBase) updateSize() {
	sizeInBytes := reflectStaticSizeSegmentBase +
		cap(sb.mem)

	for k := range sb.fieldsMap {
		sizeInBytes += (len(k) + SizeOfString) + SizeOfUint16
	}

	for k := range sb.fieldsOptions {
		sizeInBytes += (len(k) + SizeOfString) + SizeOfUint64
	}

	for _, entry := range sb.fieldsInv {
		sizeInBytes += len(entry) + SizeOfString
	}
	sizeInBytes += len(sb.fieldStats) * 2 * SizeOfUint64

	for _, secDvReaders := range sb.fieldDvReaders {
		for _, v := range secDvReaders {
			sizeInBytes += SizeOfUint16 + SizeOfPtr
			if v != nil {
				sizeInBytes += v.size()
			}
		}
	}

	sb.size = sizeInBytes
}

func (sb *SegmentBase) AddRef()             {}
func (sb *SegmentBase) DecRef() (err error) { return nil }
func (sb *SegmentBase) Close() (err error) {
	sb.invIndexCache.Clear()
	return nil
}

type Segment struct {
	SegmentBase

	f       *os.File
	mm      mmap.MMap
	path    string
	version uint32
	crc     uint32

	m    sync.Mutex // Protects the fields that follow.
	refs int64
}

func (s *Segment) Size() int {
	sizeOfUints := 16

	sizeInBytes := (len(s.path) + SizeOfString) + sizeOfUints

	sizeInBytes += 16

	return sizeInBytes + s.SegmentBase.Size() - cap(s.mem)
}

func (s *Segment) AddRef() {
	s.m.Lock()
	s.refs++
	s.m.Unlock()
}

func (s *Segment) DecRef() (err error) {
	s.m.Lock()
	s.refs--
	if s.refs == 0 {
		err = s.closeActual()
	}
	s.m.Unlock()
	return err
}

func (s *Segment) ResetBytesRead(val uint64) {
	atomic.StoreUint64(&s.bytesRead, val)
}

func (s *Segment) BytesRead() uint64 {
	return atomic.LoadUint64(&s.bytesRead)
}

func (s *Segment) BytesWritten() uint64 {
	return 0
}

func (sb *SegmentBase) BytesWritten() uint64 {
	return atomic.LoadUint64(&sb.bytesWritten)
}

func (sb *SegmentBase) setBytesWritten(val uint64) {
	atomic.AddUint64(&sb.bytesWritten, val)
}

func (sb *SegmentBase) BytesRead() uint64 {
	return 0
}

func (sb *SegmentBase) ResetBytesRead(val uint64) {}

func (sb *SegmentBase) incrementBytesRead(val uint64) {
	atomic.AddUint64(&sb.bytesRead, val)
}

func (sb *SegmentBase) loadFields() error {
	pos := sb.sectionsIndexOffset

	if pos == 0 {
		return fmt.Errorf("no sections index present")
	}

	seek := pos + binary.MaxVarintLen64
	if seek > uint64(len(sb.mem)) {
		seek = uint64(len(sb.mem))
	}

	numFields, sz := binary.Uvarint(sb.mem[pos:seek])
	pos += uint64(sz)
	sb.incrementBytesRead(uint64(sz))

	var fieldID uint64
	for fieldID < numFields {
		addr := binary.BigEndian.Uint64(sb.mem[pos : pos+8])
		sb.incrementBytesRead(8)

		fieldSectionMap, err := sb.loadField(uint16(fieldID), addr)
		if err != nil {
			return err
		}

		sb.fieldsSectionsMap = append(sb.fieldsSectionsMap, fieldSectionMap)

		fieldID++
		pos += 8
	}

	return nil
}

func (sb *SegmentBase) loadField(fieldID uint16, pos uint64) ([]uint64, error) {
	if pos == 0 {
		return nil, nil
	}

	fieldStartPos := pos // to track the number of bytes read
	fieldNameLen, sz := binary.Uvarint(sb.mem[pos : pos+binary.MaxVarintLen64])
	pos += uint64(sz)

	fieldName := sb.mem[pos : pos+fieldNameLen]
	pos += fieldNameLen

	fieldOptions, sz := binary.Uvarint(sb.mem[pos : pos+binary.MaxVarintLen64])
	pos += uint64(sz)

	sb.fieldsInv = append(sb.fieldsInv, string(fieldName))
	sb.fieldsMap[string(fieldName)] = fieldID + 1
	sb.fieldsOptions[string(fieldName)] = index.FieldIndexingOptions(fieldOptions)

	fieldNumSections, sz := binary.Uvarint(sb.mem[pos : pos+binary.MaxVarintLen64])
	if sz <= 0 {
		return nil, fmt.Errorf("invalid section count for field %q", fieldName)
	}
	pos += uint64(sz)
	fieldSectionMap := make([]uint64, NumSections)
	for sectionIdx := uint64(0); sectionIdx < fieldNumSections; sectionIdx++ {
		fieldSectionType := binary.BigEndian.Uint16(sb.mem[pos : pos+2])
		pos += 2
		fieldSectionAddr := binary.BigEndian.Uint64(sb.mem[pos : pos+8])
		pos += 8
		fieldSectionMap[fieldSectionType] = fieldSectionAddr
	}

	documentCount, sz := binary.Uvarint(sb.mem[pos : pos+binary.MaxVarintLen64])
	if sz <= 0 {
		return nil, fmt.Errorf("invalid document count for field %q", fieldName)
	}
	pos += uint64(sz)
	sumTotalTermFrequency, sz := binary.Uvarint(sb.mem[pos : pos+binary.MaxVarintLen64])
	if sz <= 0 {
		return nil, fmt.Errorf("invalid total term frequency for field %q", fieldName)
	}
	pos += uint64(sz)
	sb.fieldStats = append(sb.fieldStats, fieldStats{
		documentCount:         documentCount,
		sumTotalTermFrequency: sumTotalTermFrequency,
	})

	sb.incrementBytesRead((pos - fieldStartPos) + fieldNameLen)
	return fieldSectionMap, nil
}

func (sb *SegmentBase) Dictionary(field string) (segment.TermDictionary, error) {
	dict, err := sb.dictionary(field)
	if err == nil && dict == nil {
		return emptyDictionary, nil
	}
	return dict, err
}

func (sb *SegmentBase) dictionary(field string) (rv *Dictionary, err error) {
	fieldIDPlus1 := sb.fieldsMap[field]
	if fieldIDPlus1 == 0 {
		return nil, nil
	}
	pos := sb.fieldsSectionsMap[fieldIDPlus1-1][SectionInvertedTextIndex]
	if pos > 0 {
		rv = &Dictionary{
			sb:      sb,
			field:   field,
			fieldID: fieldIDPlus1 - 1,
		}
		for i := 0; i < 2; i++ {
			_, n := binary.Uvarint(sb.mem[pos : pos+binary.MaxVarintLen64])
			pos += uint64(n)
		}
		dictLoc, _ := binary.Uvarint(sb.mem[pos : pos+binary.MaxVarintLen64])
		fst, bytesRead, err := sb.invIndexCache.loadOrCreate(rv.fieldID, sb.mem[dictLoc:])
		if err != nil {
			return nil, fmt.Errorf("dictionary for field %s err: %v", field, err)
		}
		rv.fst = fst
		rv.fstReader, err = rv.fst.Reader()
		if err != nil {
			return nil, fmt.Errorf("dictionary for field %s, vellum reader err: %v", field, err)
		}
		rv.bytesRead += bytesRead
	}

	return rv, nil
}

type visitDocumentCtx struct {
	buf      []byte
	reader   bytes.Reader
	segment  *SegmentBase
	blockNum uint64
}

var visitDocumentCtxPool = sync.Pool{
	New: func() interface{} {
		reuse := &visitDocumentCtx{}
		return reuse
	},
}

func (sb *SegmentBase) VisitStoredFields(num uint64, visitor segment.StoredFieldValueVisitor) error {
	vdc := visitDocumentCtxPool.Get().(*visitDocumentCtx)
	defer visitDocumentCtxPool.Put(vdc)
	return sb.visitStoredFields(vdc, num, visitor)
}

func (sb *SegmentBase) visitStoredFields(vdc *visitDocumentCtx, num uint64,
	visitor segment.StoredFieldValueVisitor) error {
	if num < sb.numDocs {
		meta, uncompressed, err := sb.getDocStoredMetaAndData(vdc, num)
		if err != nil {
			return err
		}

		vdc.reader.Reset(meta)
		keepGoing := true
		for keepGoing {
			field, err := binary.ReadUvarint(&vdc.reader)
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			offset, err := binary.ReadUvarint(&vdc.reader)
			if err != nil {
				return err
			}
			l, err := binary.ReadUvarint(&vdc.reader)
			if err != nil {
				return err
			}
			value := uncompressed[offset : offset+l]
			keepGoing = visitor(sb.fieldsInv[field], byte('t'), value, nil)
		}
	}
	return nil
}

func (sb *SegmentBase) DocID(num uint64) ([]byte, error) {
	if num >= sb.numDocs {
		return nil, nil
	}

	vdc := visitDocumentCtxPool.Get().(*visitDocumentCtx)
	defer visitDocumentCtxPool.Put(vdc)

	meta, uncompressed, err := sb.getDocStoredMetaAndData(vdc, num)
	if err != nil {
		return nil, err
	}

	vdc.reader.Reset(meta)
	field, err := binary.ReadUvarint(&vdc.reader)
	if err != nil {
		return nil, err
	}
	if field != 0 {
		return nil, fmt.Errorf("stored _id field missing for doc %d", num)
	}
	offset, err := binary.ReadUvarint(&vdc.reader)
	if err != nil {
		return nil, err
	}
	l, err := binary.ReadUvarint(&vdc.reader)
	if err != nil {
		return nil, err
	}
	idFieldVal := append([]byte(nil), uncompressed[offset:offset+l]...)

	return idFieldVal, nil
}

func (sb *SegmentBase) Count() uint64 {
	return sb.numDocs
}

func (sb *SegmentBase) DocNumbers(ids []string) (*roaring.Bitmap, error) {
	rv := roaring.New()

	if len(sb.fieldsMap) > 0 {
		idDict, err := sb.dictionary("_id")
		if err != nil {
			return nil, err
		}

		postingsList := emptyPostingsList

		sMax, err := idDict.fst.GetMaxKey()
		if err != nil {
			return nil, err
		}
		sMaxStr := string(sMax)
		for _, id := range ids {
			if id <= sMaxStr {
				postingsList, err = idDict.postingsList([]byte(id), nil, postingsList)
				if err != nil {
					return nil, err
				}
				postingsList.OrInto(rv)
			}
		}
	}

	return rv, nil
}

func (sb *SegmentBase) Fields() []string {
	return sb.fieldsInv
}

func (s *Segment) Path() string {
	return s.path
}

func (s *Segment) Close() (err error) {
	return s.DecRef()
}

func (s *Segment) closeActual() (err error) {
	s.invIndexCache.Clear()

	if s.mm != nil {
		err = s.mm.Unmap()
	}
	if s.f != nil {
		err2 := s.f.Close()
		if err == nil {
			err = err2
		}
	}

	return
}

func (s *Segment) Data() []byte {
	return s.mm
}

func (s *Segment) CRC() uint32 {
	return s.crc
}

func (s *Segment) Version() uint32 {
	return s.version
}

func (s *Segment) ChunkMode() uint32 {
	return s.chunkMode
}

func (s *Segment) SectionsIndexOffset() uint64 {
	return s.sectionsIndexOffset
}

func (s *Segment) StoredIndexOffset() uint64 {
	return s.storedIndexOffset
}

func (s *Segment) NumDocs() uint64 {
	return s.numDocs
}

func (s *Segment) DictAddr(field string) (uint64, error) {
	fieldIDPlus1, ok := s.fieldsMap[field]
	if !ok {
		return 0, fmt.Errorf("no such field '%s'", field)
	}
	dictStart := s.fieldsSectionsMap[fieldIDPlus1-1][SectionInvertedTextIndex]
	if dictStart == 0 {
		return 0, fmt.Errorf("no dictionary for field '%s'", field)
	}
	for i := 0; i < 2; i++ {
		_, n := binary.Uvarint(s.mem[dictStart : dictStart+binary.MaxVarintLen64])
		dictStart += uint64(n)
	}
	dictLoc, _ := binary.Uvarint(s.mem[dictStart : dictStart+binary.MaxVarintLen64])
	return dictLoc, nil
}

func (sb *SegmentBase) loadDvReaders() error {
	if sb.numDocs == 0 {
		return nil
	}
	readers := make([]*docValueReader, len(sb.fieldsInv))
	sb.fieldDvReaders[SectionInvertedTextIndex] = readers
	for fieldID, sections := range sb.fieldsSectionsMap {
		field := sb.fieldsInv[fieldID]
		pos := sections[SectionInvertedTextIndex]
		if pos == 0 || !sb.fieldsOptions[field].IncludeDocValues() {
			continue
		}
		start, n := binary.Uvarint(sb.mem[pos:])
		if n <= 0 {
			return fmt.Errorf("invalid docvalue start")
		}
		end, m := binary.Uvarint(sb.mem[pos+uint64(n):])
		if m <= 0 {
			return fmt.Errorf("invalid docvalue end")
		}
		sb.incrementBytesRead(uint64(n + m))
		reader, err := sb.loadFieldDocValueReader(field, start, end)
		if err != nil {
			return err
		}
		if reader != nil {
			readers[fieldID] = reader
			sb.fieldDvNames = append(sb.fieldDvNames, field)
		}
	}
	return nil
}

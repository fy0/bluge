package zap

import (
	"bytes"

	"github.com/RoaringBitmap/roaring/v2"
	seg "github.com/blevesearch/scorch_segment_api/v2"
	"github.com/blevesearch/vellum"
)

type invertedMergeInput struct {
	dictionary *Dictionary
	drop       *roaring.Bitmap
	docNums    []uint64
}

type invertedFieldMerger struct {
	segments   []*SegmentBase
	drops      []*roaring.Bitmap
	docNums    [][]uint64
	config     *invertedIndexOpaque
	writer     *CountHashWriter
	closeCh    chan struct{}
	encoder    *postingsEncoder
	bitmap     *roaring.Bitmap
	fieldDocs  *roaring.Bitmap
	builder    *vellum.Builder
	builderBuf bytes.Buffer
	inputs     []invertedMergeInput
	iterators  []vellum.Iterator
	postings   *PostingsList
	postItr    *PostingsIterator
	locations  []interimLoc
	dvReader   *docValueReader
}

func mergeAndPersistInvertedSection(segments []*SegmentBase, drops []*roaring.Bitmap,
	fields []string, docNums [][]uint64, config *invertedIndexOpaque,
	w *CountHashWriter, closeCh chan struct{}) (map[int]uint64, error) {
	m := &invertedFieldMerger{
		segments: segments, drops: drops, docNums: docNums,
		config: config, writer: w, closeCh: closeCh,
		encoder: newPostingsEncoder(config.numDocs), bitmap: roaring.NewBitmap(),
	}
	for _, drop := range drops {
		if drop != nil && !drop.IsEmpty() {
			m.fieldDocs = roaring.NewBitmap()
			break
		}
	}
	var err error
	m.builder, err = vellum.New(&m.builderBuf, nil)
	if err != nil {
		return nil, err
	}
	addrs := make(map[int]uint64, len(fields))
	for fieldID, field := range fields {
		addrs[fieldID], err = m.mergeField(fieldID, field)
		if err != nil {
			return nil, err
		}
	}
	return addrs, nil
}

func (m *invertedFieldMerger) mergeField(fieldID int, field string) (uint64, error) {
	if err := m.collectInputs(field); err != nil {
		return 0, err
	}
	stats := &m.config.fieldStats[fieldID]
	if m.fieldDocs == nil {
		m.sumStats(field, stats)
	} else {
		m.fieldDocs.Clear()
	}
	if err := m.mergeTerms(stats); err != nil {
		return 0, err
	}
	if m.fieldDocs != nil {
		stats.documentCount = m.fieldDocs.GetCardinality()
	}
	dictOffset, err := writeDictionary(m.builder, &m.builderBuf, m.writer)
	if err != nil {
		return 0, err
	}
	dvStart, dvEnd, err := m.mergeDocValues(field)
	if err != nil {
		return 0, err
	}
	return writeInvertedFieldHeader(m.writer, dictOffset, dvStart, dvEnd)
}

func (m *invertedFieldMerger) sumStats(field string, stats *fieldStats) {
	if !m.config.FieldsOptions[field].IsIndexed() {
		return
	}
	for _, segment := range m.segments {
		docs, terms, ok := segment.FieldStats(field)
		if ok {
			stats.add(fieldStats{documentCount: docs, sumTotalTermFrequency: terms})
		}
	}
}

func (m *invertedFieldMerger) collectInputs(field string) error {
	m.inputs = m.inputs[:0]
	m.iterators = m.iterators[:0]
	if !m.config.FieldsOptions[field].IsIndexed() {
		return nil
	}
	for i, segment := range m.segments {
		if isClosed(m.closeCh) {
			return seg.ErrClosed
		}
		dict, err := segment.dictionary(field)
		if err != nil {
			return err
		}
		if dict == nil || dict.fst == nil {
			continue
		}
		iterator, err := dict.fst.Iterator(nil, nil)
		if err != nil && err != vellum.ErrIteratorDone {
			return err
		}
		if iterator != nil {
			m.inputs = append(m.inputs, invertedMergeInput{
				dictionary: dict, drop: m.drops[i], docNums: m.docNums[i],
			})
			m.iterators = append(m.iterators, iterator)
		}
	}
	return nil
}

func (m *invertedFieldMerger) mergeTerms(stats *fieldStats) (err error) {
	e, err := newEnumerator(m.iterators)
	if err != nil && err != vellum.ErrIteratorDone {
		return err
	}
	defer func() {
		if closeErr := e.Close(); err == nil {
			err = closeErr
		}
	}()
	var previous []byte
	active := false
	for err == nil {
		term, input, offset := e.Current()
		if !active || !bytes.Equal(previous, term) {
			if isClosed(m.closeCh) {
				return seg.ErrClosed
			}
			if active {
				if err := m.finishTerm(previous); err != nil {
					return err
				}
			}
			if err := m.beginTerm(e); err != nil {
				return err
			}
			active = true
		}
		if err := m.mergePosting(input, offset, stats); err != nil {
			return err
		}
		previous = append(previous[:0], term...)
		err = e.Next()
	}
	if err != vellum.ErrIteratorDone {
		return err
	}
	if active {
		return m.finishTerm(previous)
	}
	return nil
}

func (m *invertedFieldMerger) beginTerm(e *enumerator) error {
	var count uint64
	indices, offsets := e.GetLowIdxsAndValues()
	for i, index := range indices {
		input := &m.inputs[index]
		postings, err := input.dictionary.postingsListFromOffset(offsets[i], input.drop, nil)
		if err != nil {
			return err
		}
		count += postings.Count()
	}
	m.bitmap.Clear()
	return m.encoder.reset(m.config.chunkMode, count, m.config.numDocs)
}

func (m *invertedFieldMerger) mergePosting(inputID int, offset uint64, stats *fieldStats) error {
	input := &m.inputs[inputID]
	var err error
	m.postings, err = input.dictionary.postingsListFromOffset(offset, input.drop, m.postings)
	if err != nil {
		return err
	}
	m.postItr = m.postings.iterator(true, true, true, m.postItr)
	var total *uint64
	if m.fieldDocs != nil {
		total = &stats.sumTotalTermFrequency
	}
	p := m.encoder
	if m.config.fieldsSame {
		p.lastDoc, p.lastFreq, p.lastNorm, err = mergeTermFreqNormLocsByCopying(
			m.postItr, input.docNums, m.bitmap, p.freq, p.loc, &p.impacts, total)
	} else {
		p.lastDoc, p.lastFreq, p.lastNorm, m.locations, err = mergeTermFreqNormLocs(
			m.config.FieldsMap, m.postItr, input.docNums, m.bitmap,
			p.freq, p.loc, &p.impacts, m.locations, total)
	}
	return err
}

func (m *invertedFieldMerger) finishTerm(term []byte) error {
	if m.fieldDocs != nil {
		m.fieldDocs.Or(m.bitmap)
	}
	return m.encoder.finishTerm(m.bitmap, term, m.builder, m.writer)
}

func (m *invertedFieldMerger) mergeDocValues(field string) (uint64, uint64, error) {
	options := m.config.FieldsOptions[field]
	if !options.IncludeDocValues() {
		return fieldNotUninverted, fieldNotUninverted, nil
	}
	start := m.writer.Count()
	encoder := newChunkedContentCoder(m.config.numDocs-1, m.writer)
	for i, segment := range m.segments {
		if isClosed(m.closeCh) {
			return 0, 0, seg.ErrClosed
		}
		fieldID := segment.fieldsMap[field]
		readers := segment.fieldDvReaders[SectionInvertedTextIndex]
		if fieldID == 0 || len(readers) == 0 || readers[fieldID-1] == nil {
			continue
		}
		m.dvReader = readers[fieldID-1].cloneInto(m.dvReader)
		docNums := m.docNums[i]
		err := m.dvReader.iterateAllDocValues(segment, func(docNum uint64, terms []byte) error {
			mapped := docNums[docNum]
			if mapped == docDropped {
				return nil
			}
			return encoder.Add(mapped, terms)
		})
		if err != nil {
			return 0, 0, err
		}
	}
	return finishDocValues(encoder, m.writer, start)
}

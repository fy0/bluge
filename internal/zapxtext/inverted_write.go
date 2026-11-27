package zap

import (
	"bytes"
	"encoding/binary"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/blevesearch/vellum"
)

type postingsEncoder struct {
	freq                        *chunkedIntCoder
	loc                         *chunkedIntCoder
	impacts                     impactCoder
	lastDoc, lastFreq, lastNorm uint64
	buf                         [binary.MaxVarintLen64]byte
}

func newPostingsEncoder(numDocs uint64) *postingsEncoder {
	return &postingsEncoder{
		freq: newChunkedIntCoder(1024, numDocs-1),
		loc:  newChunkedIntCoder(1024, numDocs-1),
	}
}

func (p *postingsEncoder) reset(chunkMode uint32, count, numDocs uint64) error {
	chunkSize, err := getChunkSize(chunkMode, count, numDocs)
	if err != nil {
		return err
	}
	p.freq.Reset()
	p.loc.Reset()
	p.freq.SetChunkSize(chunkSize, numDocs-1)
	p.loc.SetChunkSize(chunkSize, numDocs-1)
	p.impacts.Reset(count)
	p.lastDoc, p.lastFreq, p.lastNorm = 0, 0, 0
	return nil
}

func encodePostingFrequency(encoder *chunkedIntCoder, docNum, freq, norm uint64, hasLocs bool) error {
	encodedFreq := encodeFreqHasLocs(freq, hasLocs)
	if freq == 0 {
		return encoder.Add(docNum, encodedFreq)
	}
	return encoder.Add(docNum, encodedFreq, norm)
}

func encodePostingLocations(encoder *chunkedIntCoder, docNum uint64, locs []interimLoc) error {
	if len(locs) == 0 {
		return nil
	}
	var numBytes uint64
	for _, loc := range locs {
		numBytes += totalUvarintBytes(uint64(loc.fieldID), loc.pos, loc.start,
			loc.end, uint64(len(loc.arrayposs)), loc.arrayposs)
	}
	if err := encoder.Add(docNum, numBytes); err != nil {
		return err
	}
	for _, loc := range locs {
		if err := encoder.Add(docNum, uint64(loc.fieldID), loc.pos, loc.start,
			loc.end, uint64(len(loc.arrayposs))); err != nil {
			return err
		}
		if err := encoder.Add(docNum, loc.arrayposs...); err != nil {
			return err
		}
	}
	return nil
}

func (p *postingsEncoder) add(docNum, freq, norm uint64, locs []interimLoc) error {
	if err := encodePostingFrequency(p.freq, docNum, freq, norm, len(locs) > 0); err != nil {
		return err
	}
	if err := encodePostingLocations(p.loc, docNum, locs); err != nil {
		return err
	}
	p.impacts.Add(docNum, freq, norm)
	p.lastDoc, p.lastFreq, p.lastNorm = docNum, freq, norm
	return nil
}

func (p *postingsEncoder) use1HitEncoding(postings *roaring.Bitmap) bool {
	// The packed FST value reserves one norm bit and uses zero as the absent marker.
	return postings.GetCardinality() == 1 && p.loc.FinalSize() == 0 &&
		p.lastFreq == 1 && under32Bits(p.lastDoc) &&
		under32Bits(p.lastNorm) && p.lastNorm != 0
}

func (p *postingsEncoder) write(postings *roaring.Bitmap, w *CountHashWriter) (uint64, error) {
	if postings.IsEmpty() {
		return 0, nil
	}
	p.freq.Close()
	p.loc.Close()
	if p.use1HitEncoding(postings) {
		return FSTValEncode1Hit(p.lastDoc, p.lastNorm), nil
	}
	freqOffset, err := p.freq.writeAt(w)
	if err != nil {
		return 0, err
	}
	impactOffset, err := p.impacts.writeAt(w)
	if err != nil {
		return 0, err
	}
	locOffset, err := p.loc.writeAt(w)
	if err != nil {
		return 0, err
	}
	offset := w.Count()
	if err = writeUvarints(w, freqOffset, locOffset, impactOffset); err != nil {
		return 0, err
	}
	if _, err = writeRoaringWithLen(postings, w, p.buf[:]); err != nil {
		return 0, err
	}
	return offset, nil
}

func (p *postingsEncoder) finishTerm(postings *roaring.Bitmap, term []byte,
	builder *vellum.Builder, w *CountHashWriter) error {
	offset, err := p.write(postings, w)
	if err != nil || offset == 0 {
		return err
	}
	return builder.Insert(term, offset)
}

func writeDictionary(builder *vellum.Builder, buf *bytes.Buffer, w *CountHashWriter) (uint64, error) {
	if err := builder.Close(); err != nil {
		return 0, err
	}
	offset := w.Count()
	if err := writeUvarints(w, uint64(buf.Len())); err != nil {
		return 0, err
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	buf.Reset()
	return offset, builder.Reset(buf)
}

func finishDocValues(encoder *chunkedContentCoder, w *CountHashWriter, start uint64) (uint64, uint64, error) {
	if err := encoder.Close(); err != nil {
		return 0, 0, err
	}
	if _, err := encoder.Write(); err != nil {
		return 0, 0, err
	}
	return start, w.Count(), nil
}

func writeInvertedFieldHeader(w *CountHashWriter, dict, dvStart, dvEnd uint64) (uint64, error) {
	offset := w.Count()
	err := writeUvarints(w, dvStart, dvEnd, dict)
	return offset, err
}

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
	"io"

	"github.com/RoaringBitmap/roaring/v2"
	index "github.com/blevesearch/bleve_index_api"
)

func writeRoaringWithLen(r *roaring.Bitmap, w io.Writer,
	reuseBufVarint []byte) (int, error) {
	buf, err := r.ToBytes()
	if err != nil {
		return 0, err
	}

	var tw int

	n := binary.PutUvarint(reuseBufVarint, uint64(len(buf)))
	nw, err := w.Write(reuseBufVarint[:n])
	tw += nw
	if err != nil {
		return tw, err
	}

	nw, err = w.Write(buf)
	tw += nw
	if err != nil {
		return tw, err
	}

	return tw, nil
}

func persistFieldsSection(fieldsInv []string,
	fieldsOptions map[string]index.FieldIndexingOptions, w *CountHashWriter,
	opaque map[int]resetable, stats []fieldStats) (uint64, error) {
	var rv uint64
	fieldsOffsets := make([]uint64, 0, len(fieldsInv))

	for fieldID, fieldName := range fieldsInv {
		fieldsOffsets = append(fieldsOffsets, w.Count())
		fieldOpts := fieldsOptions[fieldName]
		fieldName := []byte(fieldName)

		err := writeUvarints(w, uint64(len(fieldName)))
		if err != nil {
			return 0, err
		}

		_, err = w.Write(fieldName)
		if err != nil {
			return 0, err
		}

		err = writeUvarints(w, uint64(fieldOpts))
		if err != nil {
			return 0, err
		}

		err = writeUvarints(w, uint64(len(segmentSections)))
		if err != nil {
			return 0, err
		}

		for segmentSectionType, segmentSectionImpl := range segmentSections {
			err = binary.Write(w, binary.BigEndian, uint16(segmentSectionType))
			if err != nil {
				return 0, err
			}
			err = binary.Write(w, binary.BigEndian, segmentSectionImpl.AddrForField(opaque, fieldID))
			if err != nil {
				return 0, err
			}
		}

		var fieldStats fieldStats
		if fieldID < len(stats) {
			fieldStats = stats[fieldID]
		}
		err = writeUvarints(w, fieldStats.documentCount, fieldStats.sumTotalTermFrequency)
		if err != nil {
			return 0, err
		}
	}

	rv = w.Count()
	err := writeUvarints(w, uint64(len(fieldsInv)))
	if err != nil {
		return 0, err
	}
	for fieldID := range fieldsInv {
		err := binary.Write(w, binary.BigEndian, fieldsOffsets[fieldID])
		if err != nil {
			return 0, err
		}
	}

	return rv, nil
}

const FooterSize = 36

func persistFooter(numDocs, storedIndexOffset, sectionsIndexOffset uint64,
	chunkMode, crcBeforeFooter uint32, writerIn io.Writer) error {
	w := NewCountHashWriter(writerIn)
	w.crc = crcBeforeFooter

	err := binary.Write(w, binary.BigEndian, numDocs)
	if err != nil {
		return err
	}

	err = binary.Write(w, binary.BigEndian, storedIndexOffset)
	if err != nil {
		return err
	}

	err = binary.Write(w, binary.BigEndian, sectionsIndexOffset)
	if err != nil {
		return err
	}

	err = binary.Write(w, binary.BigEndian, chunkMode)
	if err != nil {
		return err
	}

	err = binary.Write(w, binary.BigEndian, Version)
	if err != nil {
		return err
	}

	err = binary.Write(w, binary.BigEndian, w.crc)
	if err != nil {
		return err
	}
	return nil
}

func writeUvarints(w io.Writer, vals ...uint64) error {
	var buf [binary.MaxVarintLen64]byte
	for _, value := range vals {
		n := binary.PutUvarint(buf[:], value)
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
	}
	return nil
}

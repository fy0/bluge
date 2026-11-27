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
	"bufio"
	"fmt"
	"io"
	"math"
	"os"

	index "github.com/blevesearch/bleve_index_api"
)

const Version uint32 = 3

const Type string = "zapx-bluge"

const fieldNotUninverted uint64 = math.MaxUint64

func (sb *SegmentBase) Persist(path string) error {
	return PersistSegmentBase(sb, path)
}

func (sb *SegmentBase) WriteTo(w io.Writer) (int64, error) {
	if w == nil {
		return 0, fmt.Errorf("invalid writer found")
	}

	n, err := persistSegmentBaseToWriter(sb, w)
	return int64(n), err
}

func PersistSegmentBase(sb *SegmentBase, path string) error {
	flag := os.O_RDWR | os.O_CREATE | os.O_TRUNC

	f, err := os.OpenFile(path, flag, 0600)
	if err != nil {
		return err
	}

	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}

	_, err = persistSegmentBaseToWriter(sb, f)
	if err != nil {
		cleanup()
		return err
	}

	err = f.Sync()
	if err != nil {
		cleanup()
		return err
	}

	err = f.Close()
	if err != nil {
		cleanup()
		return err
	}

	return err
}

type bufWriter struct {
	w *bufio.Writer
	n int
}

func (br *bufWriter) Write(in []byte) (int, error) {
	n, err := br.w.Write(in)
	br.n += n
	return n, err
}

func persistSegmentBaseToWriter(sb *SegmentBase, w io.Writer) (int, error) {
	br := &bufWriter{w: bufio.NewWriter(w)}

	_, err := br.Write(sb.mem)
	if err != nil {
		return 0, err
	}

	err = persistFooter(sb.numDocs, sb.storedIndexOffset, sb.sectionsIndexOffset,
		sb.chunkMode, sb.memCRC, br)
	if err != nil {
		return 0, err
	}

	err = br.w.Flush()
	if err != nil {
		return 0, err
	}

	return br.n, nil
}

func persistStoredFieldValues(fieldID int,
	storedFieldValues [][]byte,
	curr int, metaEncode varintEncoder, data []byte) (
	int, []byte, error) {
	for i := 0; i < len(storedFieldValues); i++ {
		_, err := metaEncode(uint64(fieldID))
		if err != nil {
			return 0, nil, err
		}
		_, err = metaEncode(uint64(curr))
		if err != nil {
			return 0, nil, err
		}
		_, err = metaEncode(uint64(len(storedFieldValues[i])))
		if err != nil {
			return 0, nil, err
		}
		data = append(data, storedFieldValues[i]...)
		curr += len(storedFieldValues[i])
	}

	return curr, data, nil
}

func InitSegmentBase(mem []byte, memCRC uint32, chunkMode uint32, numDocs uint64,
	storedIndexOffset uint64, sectionsIndexOffset uint64) (*SegmentBase, error) {
	sb := &SegmentBase{
		mem:                 mem,
		memCRC:              memCRC,
		version:             Version,
		chunkMode:           chunkMode,
		numDocs:             numDocs,
		storedIndexOffset:   storedIndexOffset,
		sectionsIndexOffset: sectionsIndexOffset,
		fieldDvReaders:      make([][]*docValueReader, len(segmentSections)),
		invIndexCache:       newInvertedIndexCache(),
		fieldsMap:           make(map[string]uint16),
		fieldsOptions:       make(map[string]index.FieldIndexingOptions),
		fieldsInv:           make([]string, 0),
		fieldStats:          make([]fieldStats, 0),
	}
	sb.updateSize()

	err := sb.loadFields()
	if err != nil {
		return nil, err
	}

	err = sb.loadDvReaders()
	if err != nil {
		return nil, err
	}

	return sb, nil
}

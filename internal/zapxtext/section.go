//	Copyright (c) 2023 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zap

import (
	"github.com/RoaringBitmap/roaring/v2"
	index "github.com/blevesearch/bleve_index_api"

	"github.com/fy0/bluge/internal/blugeidx"
	blugeseg "github.com/fy0/bluge/segment"
)

type sectionConfig struct {
	results       []*blugeidx.Document
	chunkMode     uint32
	fieldsSame    bool
	fieldsMap     map[string]uint16
	fieldsInv     []string
	fieldsOptions map[string]index.FieldIndexingOptions
	numDocs       uint64
	normCalc      func(string, int) float32
	options       blugeseg.BuildOptions
}

type section interface {
	Process(map[int]resetable, uint32, *blugeidx.Field, uint16)
	Persist(map[int]resetable, *CountHashWriter) error
	AddrForField(map[int]resetable, int) uint64
	Merge(map[int]resetable, []*SegmentBase, []*roaring.Bitmap, []string, [][]uint64, *CountHashWriter, chan struct{}) error
	InitOpaque(sectionConfig) resetable
}

type resetable interface {
	Reset() error
	Configure(sectionConfig)
}

const (
	SectionInvertedTextIndex = iota
	SectionVectorIndex
	NumSections
)

var segmentSections = [...]section{
	&invertedTextIndexSection{},
	&vectorIndexSection{},
}

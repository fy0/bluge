// Copyright (c) 2020 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package segment

import (
	"fmt"
	"io"

	"github.com/RoaringBitmap/roaring/v2"
)

var ErrClosed = fmt.Errorf("index closed")

type StoredFieldVisitor func(field string, value []byte) bool

type DocumentValueVisitor func(field string, term []byte)

type Term interface {
	Field() string
	Term() []byte
}

type Segment interface {
	Dictionary(field string) (Dictionary, error)
	VisitStoredFields(num uint64, visitor StoredFieldVisitor) error
	Count() uint64
	DocsMatchingTerms([]Term) (*roaring.Bitmap, error)
	Fields() []string
	CollectionStats(field string) (CollectionStats, error)
	Size() int
	DocumentValueReader(fields []string) (DocumentValueReader, error)
	WriteTo(w io.Writer, closeCh chan struct{}) (int64, error)
	Type() string
	Version() uint32
}

type DictionaryLookup interface {
	Contains(key []byte) (bool, error)
	Close() error
}

type Dictionary interface {
	DictionaryLookup
	PostingsList(term []byte, except *roaring.Bitmap, prealloc PostingsList) (PostingsList, error)
	Iterator(a Automaton, startKeyInclusive, endKeyExclusive []byte) DictionaryIterator
}

type DictionaryEntry interface {
	Term() string
	Count() uint64
}

type DictionaryIterator interface {
	Next() (DictionaryEntry, error)
	Close() error
}

type PostingsList interface {
	Iterator(includeFreq, includeNorm, includeLocations bool, prealloc PostingsIterator) (PostingsIterator, error)
	Size() int
	Count() uint64
}

type PostingsIterator interface {
	Next() (Posting, error)
	Advance(docNum uint64) (Posting, error)
	Size() int
	Empty() bool
	Count() uint64
	Close() error
}

// Impact is one non-dominated frequency and raw-norm pair from a postings
// block.  Scorers use the pairs to compute an exact upper bound for the block.
type Impact struct {
	Frequency uint64
	Norm      uint64
}

// ImpactPostingsIterator is optionally implemented by postings iterators that
// carry block-level impacts.  AdvanceShallow moves only the impact cursor and
// returns the inclusive end document for the block containing docNum.
type ImpactPostingsIterator interface {
	HasImpacts() bool
	AdvanceShallow(docNum uint64) (blockEnd uint64, impacts []Impact, ok bool, err error)
}

type OptimizablePostingsIterator interface {
	ActualBitmap() *roaring.Bitmap
	DocNum1Hit() (uint64, bool)
	ReplaceActual(*roaring.Bitmap)
}

type Posting interface {
	Number() uint64
	SetNumber(uint64)
	Frequency() int
	Norm() float64
	Locations() []Location
	Size() int
}

type Location interface {
	Field() string
	Start() int
	End() int
	Pos() int
	Size() int
}

type Merger interface {
	WriteTo(w io.Writer, closeCh chan struct{}) (n int64, err error)
	DocumentNumbers() [][]uint64
}

type DocumentValueReader interface {
	VisitDocumentValues(number uint64, visitor DocumentValueVisitor) error
}

type DocVisitState interface{}

type Optimizable interface {
	Optimize(kind string, octx OptimizableContext) (OptimizableContext, error)
}

type OptimizableContext interface {
	Finish() (PostingsIterator, error)
}

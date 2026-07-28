//  Copyright (c) 2020 Couchbase, Inc.
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

package index

import (
	"fmt"
	"sync/atomic"

	segment "github.com/fy0/bluge/segment"
)

type postingsIterator struct {
	term               []byte
	field              string
	snapshot           *Snapshot
	dicts              []segment.Dictionary
	postings           []segment.PostingsList
	iterators          []segment.PostingsIterator
	segmentOffset      int
	includeFreq        bool
	includeNorm        bool
	includeTermVectors bool
	currPosting        segment.Posting
	currID             uint64
	impactState        int8
	recycle            bool
}

func (i *postingsIterator) Size() int {
	sizeInBytes := reflectStaticSizeIndexSnapshotTermFieldReader + sizeOfPtr +
		len(i.term) +
		len(i.field) +
		sizeOfInt

	for _, entry := range i.postings {
		sizeInBytes += entry.Size()
	}

	for _, entry := range i.iterators {
		sizeInBytes += entry.Size()
	}

	if i.currPosting != nil {
		sizeInBytes += i.currPosting.Size()
	}

	return sizeInBytes
}

func (i *postingsIterator) Next() (segment.Posting, error) {
	// find the next hit
	for i.segmentOffset < len(i.iterators) {
		next, err := i.iterators[i.segmentOffset].Next()
		if err != nil {
			return nil, err
		}
		if next != nil {
			rvNumber := next.Number() + i.snapshot.offsets[i.segmentOffset]
			next.SetNumber(rvNumber)
			i.currID = rvNumber
			i.currPosting = next
			return next, nil
		}
		i.segmentOffset++
	}
	return nil, nil
}

func (i *postingsIterator) Advance(number uint64) (segment.Posting, error) {
	// FIXME do something better
	// for now, if we need to seek backwards, then restart from the beginning
	if i.currPosting != nil && i.currID >= number {
		i2, err := i.snapshot.PostingsIterator(i.term, i.field,
			i.includeFreq, i.includeNorm, i.includeTermVectors)
		if err != nil {
			return nil, err
		}
		// close the current term field reader before replacing it with a new one
		_ = i.Close()
		*i = *(i2.(*postingsIterator))
	}
	segIndex, ldocNum := i.snapshot.segmentIndexAndLocalDocNumFromGlobal(number)
	if segIndex >= len(i.snapshot.segment) {
		return nil, fmt.Errorf("computed segment index %d out of bounds %d",
			segIndex, len(i.snapshot.segment))
	}
	// skip directly to the target segment
	i.segmentOffset = segIndex
	next, err := i.iterators[i.segmentOffset].Advance(ldocNum)
	if err != nil {
		return nil, err
	}
	if next == nil {
		// we jumped directly to the segment that should have contained it
		// but it wasn't there, so reuse Next() which should correctly
		// get the next hit after it (we moved i.segmentOffset)
		return i.Next()
	}

	rvNumber := next.Number() + i.snapshot.offsets[i.segmentOffset]
	next.SetNumber(rvNumber)
	i.currID = rvNumber
	i.currPosting = next
	return next, nil
}

func (i *postingsIterator) Count() uint64 {
	var rv uint64
	for _, posting := range i.postings {
		rv += posting.Count()
	}
	return rv
}

func (i *postingsIterator) Empty() bool {
	count := i.Count()
	return count == 0
}

func (i *postingsIterator) HasImpacts() bool {
	if i.impactState != 0 {
		return i.impactState > 0
	}
	foundImpacts := false
	for _, iterator := range i.iterators {
		if iterator == nil || iterator.Count() == 0 {
			continue
		}
		impacts, ok := iterator.(segment.ImpactPostingsIterator)
		if ok && impacts.HasImpacts() {
			foundImpacts = true
		}
	}
	if foundImpacts {
		i.impactState = 1
		return true
	}
	i.impactState = -1
	return false
}

func (i *postingsIterator) AdvanceShallow(number uint64) (
	uint64, []segment.Impact, bool, error) {
	if !i.HasImpacts() || len(i.snapshot.segment) == 0 {
		return 0, nil, false, nil
	}
	last := len(i.snapshot.segment) - 1
	end := i.snapshot.offsets[last] + i.snapshot.segment[last].segment.Count()
	if number >= end {
		return 0, nil, false, nil
	}
	segIndex, local := i.snapshot.segmentIndexAndLocalDocNumFromGlobal(number)
	for ; segIndex < len(i.iterators); segIndex++ {
		iterator := i.iterators[segIndex]
		if iterator == nil || iterator.Count() == 0 {
			local = 0
			continue
		}
		if impacts, ok := iterator.(segment.ImpactPostingsIterator); ok && impacts.HasImpacts() {
			blockEnd, values, exists, err := impacts.AdvanceShallow(local)
			if err != nil {
				return 0, nil, false, err
			}
			if exists {
				i.segmentOffset = segIndex
				return blockEnd + i.snapshot.offsets[segIndex], values, true, nil
			}
		} else {
			// A low-cardinality term deliberately carries no impact table.  Expose
			// the remainder of this segment as an unskippable block so other
			// segments can still use Block-Max.
			segmentCount := i.snapshot.segment[segIndex].segment.Count()
			if segmentCount > 0 {
				i.segmentOffset = segIndex
				return i.snapshot.offsets[segIndex] + segmentCount - 1, nil, true, nil
			}
		}
		local = 0
	}
	return 0, nil, false, nil
}

func (i *postingsIterator) Close() error {
	if i.snapshot != nil {
		atomic.AddUint64(&i.snapshot.parent.stats.TotTermSearchersFinished, uint64(1))
		i.snapshot.recyclePostingsIterator(i)
	}
	return nil
}

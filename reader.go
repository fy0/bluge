//  Copyright (c) 2020 The Bluge Authors.
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

package bluge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/fy0/bluge/index"

	segment "github.com/fy0/bluge/segment"

	"github.com/fy0/bluge/search"
)

type Reader struct {
	config Config
	reader *index.Snapshot
	vector VectorIndex

	// closed is set by Close so lifetime-bound objects such as
	// PreparedVectorFilter can reject use after the reader is gone. It is
	// atomic because it is written by Close while searches may read it.
	closed atomic.Bool
}

func OpenReader(config Config) (*Reader, error) {
	rv := &Reader{
		config: config,
	}
	var err error
	rv.reader, err = index.OpenReader(config.indexConfig)
	if err != nil {
		return nil, fmt.Errorf("error opening index: %w", err)
	}
	rv.vector, err = openVectorIndexForSnapshot(config, rv.reader)
	if err != nil {
		_ = rv.reader.Close()
		return nil, err
	}

	return rv, nil
}

func (r *Reader) Count() (count uint64, err error) {
	return r.reader.Count()
}

func (r *Reader) Fields() (fields []string, err error) {
	return r.reader.Fields()
}

type StoredFieldVisitor func(field string, value []byte) bool

func (r *Reader) VisitStoredFields(number uint64, visitor StoredFieldVisitor) error {
	return r.reader.VisitStoredFields(number, segment.StoredFieldVisitor(visitor))
}

func (r *Reader) Search(ctx context.Context, req SearchRequest) (search.DocumentMatchIterator, error) {
	collector := req.Collector()
	searcher, err := req.Searcher(r.reader, r.config)
	if err != nil {
		return nil, err
	}

	memNeeded := memNeededForSearch(searcher, collector)
	if r.config.SearchStartFunc != nil {
		err = r.config.SearchStartFunc(memNeeded)
	}
	if err != nil {
		return nil, err
	}
	if r.config.SearchEndFunc != nil {
		defer r.config.SearchEndFunc(memNeeded)
	}

	var dmItr search.DocumentMatchIterator
	dmItr, err = collector.Collect(ctx, req.Aggregations(), searcher)
	if err != nil {
		return nil, err
	}

	// FIXME search stats on reader?

	return dmItr, nil
}

// VectorSearch returns the nearest vectors for a field. A non-nil filter is
// evaluated by Bluge first, then applied to the vector candidates.
func (r *Reader) VectorSearch(ctx context.Context, field string, query []float32,
	k int, filter Query) ([]VectorHit, error) {
	if r.vector == nil {
		return nil, ErrVectorUnsupported
	}
	if filter == nil {
		return r.vector.Search(field, query, k, nil)
	}
	if candidateSearcher, ok := r.vector.(vectorDocumentCandidateSearcher); ok {
		allowed, err := r.vectorFilterDocumentNumbers(ctx, filter)
		if err != nil {
			return nil, err
		}
		return candidateSearcher.searchDocumentCandidates(field, query, k, allowed)
	}
	if candidateSearcher, ok := r.vector.(VectorCandidateSearcher); ok {
		allowed, err := r.vectorFilterIDs(ctx, filter)
		if err != nil {
			return nil, err
		}
		return candidateSearcher.SearchCandidates(field, query, k, allowed)
	}
	return r.vector.Search(field, query, k, filter)
}

// SearchVector is kept as a discoverable alias for callers that use verb-first
// naming alongside Reader.Search.
func (r *Reader) SearchVector(ctx context.Context, field string, query []float32,
	k int, filter Query) ([]VectorHit, error) {
	return r.VectorSearch(ctx, field, query, k, filter)
}

func (r *Reader) vectorFilterDocumentNumbers(ctx context.Context, filter Query) ([]uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	iterator, err := r.Search(ctx, NewAllMatches(filter))
	if err != nil {
		return nil, err
	}
	allowed := make([]uint64, 0)
	for {
		match, err := iterator.Next()
		if err != nil {
			return nil, err
		}
		if match == nil {
			sort.Slice(allowed, func(i, j int) bool { return allowed[i] < allowed[j] })
			return allowed, nil
		}
		allowed = append(allowed, match.Number)
	}
}

func (r *Reader) vectorFilterIDs(ctx context.Context, filter Query) (map[Identifier]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	iterator, err := r.Search(ctx, NewAllMatches(filter))
	if err != nil {
		return nil, err
	}
	allowed := make(map[Identifier]struct{})
	for {
		match, err := iterator.Next()
		if err != nil {
			return nil, err
		}
		if match == nil {
			return allowed, nil
		}
		var id Identifier
		err = match.VisitStoredFields(func(field string, value []byte) bool {
			if field == _idField {
				id = Identifier(string(value))
				return false
			}
			return true
		})
		if err != nil {
			return nil, err
		}
		allowed[id] = struct{}{}
	}
}

func (r *Reader) DictionaryIterator(field string, automaton segment.Automaton, start, end []byte) (segment.DictionaryIterator, error) {
	return r.reader.DictionaryIterator(field, automaton, start, end)
}

func (r *Reader) Backup(path string, cancel chan struct{}) error {
	dir := index.NewFileSystemDirectory(path)
	return r.reader.Backup(dir, cancel)
}

func (r *Reader) Close() error {
	r.closed.Store(true)
	return errors.Join(r.reader.Close(), closeVectorIndex(r.vector))
}

// PreparedVectorFilter is a filter Query that was evaluated once against one
// Reader, so several vector searches can reuse the outcome instead of
// re-running the text search for every query.
//
// A prepared filter is bound to the Reader that created it. Using it with a
// different Reader, after it was closed, or after its Reader was closed is
// rejected with ErrVectorPreparedFilter. It is read-only once created and can
// be shared by concurrent searches on its own Reader.
//
// The filter is resolved into whatever its backend consumes at preparation
// time - allowed document numbers, or the allowed identifier set - so the
// Query object is not consulted again and mutating it afterwards cannot change
// the results. The unfiltered case (a nil Query) is represented distinctly
// from a filter that matches no documents, so an empty match set yields no
// results instead of turning into an unfiltered search.
type PreparedVectorFilter struct {
	reader *Reader
	// docs is the sorted set of allowed global document numbers, or nil when
	// the filter was nil or when the backend filters by identifier instead. It
	// is never exposed directly: callers must not be able to hold document
	// numbers without the lifetime check.
	docs []uint64
	// allowedIDs is the allowed identifier set for backends that cannot
	// consume document numbers, or nil when the filter was nil or when the
	// backend filters by document number instead.
	allowedIDs map[Identifier]struct{}
	closed     atomic.Bool
}

// PrepareVectorFilter evaluates filter once and returns a reusable filter for
// VectorSearchPrepared. A nil filter prepares the unfiltered case.
//
// A non-nil filter requires a backend that can actually apply filters: the
// outcome is frozen into the form that backend consumes, and a backend that
// supports neither document numbers nor identifier sets returns
// ErrVectorFilterUnsupported rather than deferring the failure to search time.
func (r *Reader) PrepareVectorFilter(ctx context.Context, filter Query) (*PreparedVectorFilter, error) {
	if r.closed.Load() {
		return nil, fmt.Errorf("%w: reader is closed", ErrVectorPreparedFilter)
	}
	if filter == nil {
		return &PreparedVectorFilter{reader: r}, nil
	}
	if r.vector == nil {
		return nil, ErrVectorUnsupported
	}
	if _, ok := r.vector.(vectorDocumentCandidateSearcher); ok {
		docs, err := r.vectorFilterDocumentNumbers(ctx, filter)
		if err != nil {
			return nil, err
		}
		return &PreparedVectorFilter{reader: r, docs: docs}, nil
	}
	if _, ok := r.vector.(VectorCandidateSearcher); ok {
		allowed, err := r.vectorFilterIDs(ctx, filter)
		if err != nil {
			return nil, err
		}
		return &PreparedVectorFilter{reader: r, allowedIDs: allowed}, nil
	}
	return nil, ErrVectorFilterUnsupported
}

// Close releases the prepared filter. Later searches that use it fail instead
// of silently searching without a filter. The resolved document numbers or
// identifiers are left untouched so that closing cannot race with a search
// that already passed the lifetime check.
func (f *PreparedVectorFilter) Close() error {
	if f == nil {
		return nil
	}
	f.closed.Store(true)
	return nil
}

func (f *PreparedVectorFilter) validFor(r *Reader) error {
	if f == nil {
		return fmt.Errorf("%w: filter is nil", ErrVectorPreparedFilter)
	}
	if f.closed.Load() {
		return fmt.Errorf("%w: filter is closed", ErrVectorPreparedFilter)
	}
	if f.reader != r {
		return fmt.Errorf("%w: filter was prepared by a different reader", ErrVectorPreparedFilter)
	}
	if r.closed.Load() {
		return fmt.Errorf("%w: reader is closed", ErrVectorPreparedFilter)
	}
	return nil
}

// VectorSearchPrepared runs a vector search with a filter prepared by
// PrepareVectorFilter. It has the same result semantics as VectorSearch with
// the equivalent filter, but it reuses the filter outcome instead of
// evaluating the query again.
func (r *Reader) VectorSearchPrepared(ctx context.Context, field string, query []float32,
	k int, prepared *PreparedVectorFilter) ([]VectorHit, error) {
	if err := prepared.validFor(r); err != nil {
		return nil, err
	}
	if r.vector == nil {
		return nil, ErrVectorUnsupported
	}
	// The representation stored by PrepareVectorFilter is the one this
	// reader's backend consumes, so exactly one of these branches applies.
	if prepared.docs != nil {
		if candidateSearcher, ok := r.vector.(vectorDocumentCandidateSearcher); ok {
			return candidateSearcher.searchDocumentCandidates(field, query, k, prepared.docs)
		}
	}
	if prepared.allowedIDs != nil {
		if candidateSearcher, ok := r.vector.(VectorCandidateSearcher); ok {
			return candidateSearcher.SearchCandidates(field, query, k, prepared.allowedIDs)
		}
	}
	if prepared.docs == nil && prepared.allowedIDs == nil {
		return r.vector.Search(field, query, k, nil)
	}
	return nil, fmt.Errorf("%w: filter was prepared for a different vector backend",
		ErrVectorPreparedFilter)
}

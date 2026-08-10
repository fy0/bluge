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

	"github.com/fy0/bluge/index"

	segment "github.com/fy0/bluge/segment"

	"github.com/fy0/bluge/search"
)

type Reader struct {
	config Config
	reader *index.Snapshot
	vector VectorIndex
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
	return errors.Join(r.reader.Close(), closeVectorIndex(r.vector))
}

// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/fy0/bluge/index/lock"
	"github.com/fy0/bluge/search"
)

// CollectionReader pins a text snapshot and the published generations of all
// selected spaces. Native indices and revision filters are loaded lazily.
// Search iterators must be consumed before closing the reader.
type CollectionReader struct {
	mu        sync.RWMutex
	cacheMu   sync.Mutex
	config    CollectionConfig
	corpus    string
	text      *Reader
	spaces    map[string]collectionSpace
	views     map[string]*Reader
	metadata  map[string]*collectionVectorMetadata
	revisions map[Identifier]string
	ids       []Identifier
	closed    bool
	leases    []lock.LockedFile
}

// OpenCollectionReader opens only the named spaces, or all spaces if none are named.
func OpenCollectionReader(config CollectionConfig, spaceIDs ...string) (*CollectionReader, error) {
	config, err := config.normalized()
	if err != nil {
		return nil, err
	}
	gate, err := lockCollectionCoordinator(context.Background(), config.Path)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	manifest, err := loadCollectionManifest(config, false)
	if err != nil {
		return nil, err
	}
	spaces, err := loadCollectionSpaces(config.Path, manifest.CorpusID, spaceIDs...)
	if err != nil {
		return nil, err
	}
	r, err := OpenReader(config.TextConfig)
	if err != nil {
		return nil, err
	}
	return newCollectionReader(config, manifest.CorpusID, r, spaces)
}

func newCollectionReader(config CollectionConfig, corpus string, text *Reader, spaces map[string]collectionSpace) (*CollectionReader, error) {
	leases, err := pinCollectionSpaces(config, spaces)
	if err != nil {
		return nil, errors.Join(err, text.Close())
	}
	copySpaces := make(map[string]collectionSpace, len(spaces))
	for id, space := range spaces {
		copySpaces[id] = space
	}
	return &CollectionReader{config: config, corpus: corpus, text: text, spaces: copySpaces,
		views: make(map[string]*Reader), metadata: make(map[string]*collectionVectorMetadata), leases: leases}, nil
}

func (r *CollectionReader) Search(ctx context.Context, request SearchRequest) (search.DocumentMatchIterator, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrCollectionClosed
	}
	return r.text.Search(ctx, request)
}

func (r *CollectionReader) Count() (uint64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return 0, ErrCollectionClosed
	}
	return r.text.Count()
}

func (r *CollectionReader) VisitStoredFields(number uint64, visitor StoredFieldVisitor) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrCollectionClosed
	}
	return r.text.VisitStoredFields(number, visitor)
}

func (r *CollectionReader) VectorSpaces() ([]VectorSpaceSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrCollectionClosed
	}
	return sortedCollectionSpecs(r.spaces), nil
}

func (r *CollectionReader) SearchVectorRequest(ctx context.Context, spaceID string, request *VectorSearchRequest) ([]VectorHit, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrCollectionClosed
	}
	view, err := r.vectorReader(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return view.SearchVectorRequest(ctx, request)
}

func (r *CollectionReader) HybridSearch(ctx context.Context, spaceID string, request *HybridSearchRequest) ([]HybridHit, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrCollectionClosed
	}
	if request == nil || request.Vector == nil {
		return r.text.HybridSearch(ctx, request)
	}
	view, err := r.vectorReader(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return view.HybridSearch(ctx, request)
}

func (r *CollectionReader) PrepareVectorFilter(ctx context.Context, spaceID string, filter Query) (*PreparedVectorFilter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrCollectionClosed
	}
	view, err := r.vectorReader(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return view.PrepareVectorFilter(ctx, filter)
}

func (r *CollectionReader) VectorSearchPrepared(ctx context.Context, spaceID, field string, query []float32, k int, filter *PreparedVectorFilter) ([]VectorHit, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrCollectionClosed
	}
	view, err := r.vectorReader(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return view.VectorSearchPrepared(ctx, field, query, k, filter)
}

// vectorReader is called with the lifetime read lock held. Cache construction is
// serialized independently so subsequent searches on different spaces can run together.
func (r *CollectionReader) vectorReader(ctx context.Context, spaceID string) (*Reader, error) {
	if err := collectionContextError(ctx); err != nil {
		return nil, err
	}
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if view := r.views[spaceID]; view != nil {
		return view, nil
	}
	m, err := r.vectorMetadata(spaceID)
	if err != nil {
		return nil, err
	}
	if err := r.loadRevisions(ctx); err != nil {
		return nil, err
	}
	var allowed map[Identifier]struct{}
	for id, revision := range m.Revisions {
		if r.revisions[id] != revision {
			allowed = make(map[Identifier]struct{}, len(m.Revisions))
			break
		}
	}
	// nil means every stored vector is current, so native search needs no filter.
	if allowed != nil {
		for id, revision := range m.Revisions {
			if r.revisions[id] == revision {
				allowed[id] = struct{}{}
			}
		}
	}
	var raw VectorIndex
	if m.Generation != 0 {
		raw, err = openCollectionVectors(r.config, r.spaces[spaceID], m, "")
		if err != nil {
			return nil, err
		}
	}
	view := r.text.vectorView(&collectionVectorView{spec: m.Spec, raw: raw, allowed: allowed})
	r.views[spaceID] = view
	return view, nil
}

func (r *CollectionReader) vectorMetadata(spaceID string) (*collectionVectorMetadata, error) {
	space, ok := r.spaces[spaceID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrVectorSpaceNotFound, spaceID)
	}
	if m := r.metadata[spaceID]; m != nil {
		return m, nil
	}
	m, err := loadCollectionVectorMetadata(r.config, space)
	if err != nil {
		return nil, err
	}
	r.metadata[spaceID] = m
	return m, nil
}

func (r *CollectionReader) loadRevisions(ctx context.Context) error {
	if r.revisions != nil {
		return nil
	}
	revisions, err := collectionRevisions(ctx, r.text, r.corpus)
	if err != nil {
		return err
	}
	r.revisions = revisions
	r.ids = make([]Identifier, 0, len(revisions))
	for id := range revisions {
		r.ids = append(r.ids, id)
	}
	sort.Slice(r.ids, func(i, j int) bool { return r.ids[i] < r.ids[j] })
	return nil
}

// PendingVectors returns missing or stale embeddings ordered by chunk ID. Pass
// the last returned ID as after for the next page on this same reader. An empty
// page ends the scan. A new reader is required to discover later text changes.
func (r *CollectionReader) PendingVectors(ctx context.Context, spaceID string, after Identifier, limit int) ([]DocumentRevision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrCollectionClosed
	}
	if limit <= 0 {
		return nil, errors.New("pending vector page size must be positive")
	}
	if err := collectionContextError(ctx); err != nil {
		return nil, err
	}
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	m, err := r.vectorMetadata(spaceID)
	if err != nil {
		return nil, err
	}
	if err := r.loadRevisions(ctx); err != nil {
		return nil, err
	}
	result := make([]DocumentRevision, 0)
	start := sort.Search(len(r.ids), func(i int) bool { return r.ids[i] > after })
	for _, id := range r.ids[start:] {
		if err := collectionContextError(ctx); err != nil {
			return nil, err
		}
		if m.Revisions[id] != r.revisions[id] {
			result = append(result, DocumentRevision{ID: id, Revision: r.revisions[id]})
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

type VectorSpaceStatus struct {
	Spec           VectorSpaceSpec
	Generation     uint64
	Documents      int
	ValidVectors   int
	PendingVectors int
	StaleVectors   int
}

func (r *CollectionReader) VectorStatus(ctx context.Context, spaceID string) (VectorSpaceStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var status VectorSpaceStatus
	if r.closed {
		return status, ErrCollectionClosed
	}
	if err := collectionContextError(ctx); err != nil {
		return status, err
	}
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	m, err := r.vectorMetadata(spaceID)
	if err != nil {
		return status, err
	}
	if err := r.loadRevisions(ctx); err != nil {
		return status, err
	}
	status.Spec, status.Generation, status.Documents = m.Spec, m.Generation, len(r.revisions)
	for id, revision := range m.Revisions {
		if r.revisions[id] == revision {
			status.ValidVectors++
		} else {
			status.StaleVectors++
		}
	}
	status.PendingVectors = status.Documents - status.ValidVectors
	return status, nil
}

func (r *CollectionReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var err error
	for _, view := range r.views {
		err = errors.Join(err, view.Close())
	}
	return errors.Join(err, r.text.Close(), closeCollectionLeases(r.leases))
}

func collectionRevisions(ctx context.Context, reader *Reader, corpus string) (map[Identifier]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	iterator, err := reader.Search(ctx, NewAllMatches(NewMatchAllQuery()))
	if err != nil {
		return nil, err
	}
	result := make(map[Identifier]string)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match, err := iterator.Next()
		if err != nil {
			return nil, err
		}
		if match == nil {
			return result, nil
		}
		var id Identifier
		var revision, corpusID string
		if err := match.VisitStoredFields(func(field string, value []byte) bool {
			switch field {
			case _idField:
				id = Identifier(string(value))
			case collectionRevisionField:
				revision = string(value)
			case collectionCorpusField:
				corpusID = string(value)
			}
			return true
		}); err != nil {
			return nil, err
		}
		if id == "" || revision == "" || corpusID != corpus || result[id] != "" {
			return nil, fmt.Errorf("%w: missing revision, duplicate ID or foreign corpus for %q", ErrCollectionCorrupt, id)
		}
		result[id] = revision
	}
}

func collectionContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

type collectionVectorView struct {
	spec    VectorSpaceSpec
	raw     VectorIndex
	allowed map[Identifier]struct{}
}

func (v *collectionVectorView) Close() error { return closeVectorIndex(v.raw) }

func (v *collectionVectorView) Search(field string, query []float32, k int, filter Query) ([]VectorHit, error) {
	if filter != nil {
		return nil, ErrVectorFilterUnsupported
	}
	return v.SearchCandidates(field, query, k, nil)
}

func (v *collectionVectorView) SearchCandidates(field string, query []float32, k int, candidates map[Identifier]struct{}) ([]VectorHit, error) {
	if field != v.spec.Field {
		return nil, ErrVectorFieldNotFound
	}
	if k <= 0 {
		return nil, ErrVectorInvalidK
	}
	if len(query) != v.spec.Dims {
		return nil, ErrVectorInvalidDimension
	}
	for _, value := range query {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, ErrVectorInvalidValue
		}
	}
	allowed := v.allowed
	if allowed == nil {
		allowed = candidates
	} else if candidates != nil {
		allowed = make(map[Identifier]struct{})
		for id := range candidates {
			if _, ok := v.allowed[id]; ok {
				allowed[id] = struct{}{}
			}
		}
	}
	if (allowed != nil && len(allowed) == 0) || v.raw == nil {
		return []VectorHit{}, nil
	}
	return v.raw.(VectorCandidateSearcher).SearchCandidates(field, query, k, allowed)
}

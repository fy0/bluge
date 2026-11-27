// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"errors"
	"maps"
	"sync"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/segment"
)

var ErrVectorUnsupported = errors.New("vector search is not enabled")

var (
	ErrVectorBackendReadOnly   = errors.New("vector backend is read-only")
	ErrVectorFieldNotFound     = errors.New("vector field not found")
	ErrVectorInvalidDimension  = errors.New("invalid dimension")
	ErrVectorInvalidK          = errors.New("invalid k")
	ErrVectorInvalidValue      = errors.New("vector contains NaN or infinity")
	ErrVectorInvalidField      = errors.New("invalid vector field")
	ErrVectorFilterUnsupported = errors.New("vector filter unsupported")
	ErrVectorPreparedFilter    = errors.New("invalid prepared filter")
)

type VectorSimilarity = segment.VectorSimilarity

const (
	VectorL2     VectorSimilarity = "l2_norm"
	VectorDot    VectorSimilarity = "dot_product"
	VectorCosine VectorSimilarity = "cosine"
)

const (
	vectorBackendUnsupportedName = "unsupported"
	vectorBackendUSearchName     = "usearch"
)

type VectorFieldSpec struct {
	Name       string
	Dims       int
	Similarity VectorSimilarity
}

type VectorHit struct {
	ID    Identifier
	Score float64
}

type VectorIndex interface {
	Search(field string, query []float32, k int, filter Query) ([]VectorHit, error)
	Close() error
}

type VectorChange struct {
	ID         Identifier
	Field      string
	Vector     []float32
	Similarity VectorSimilarity
	Delete     bool
}

type VectorBatcher interface {
	ApplyVectorChanges(changes []VectorChange) error
}

type vectorBatchTransaction interface {
	applyVectorBatch(changes []VectorChange, commit func() error) error
}

type vectorFieldSchema interface {
	vectorFieldSpecs() map[string]VectorFieldSpec
}

type VectorSnapshotter interface {
	SnapshotVectorIndex() VectorIndex
}

type VectorCandidateSearcher interface {
	SearchCandidates(field string, query []float32, k int,
		allowed map[Identifier]struct{}) ([]VectorHit, error)
}

type vectorCandidateExecutor interface {
	searchVectorCandidates(field string, query []float32, k int,
		allowed map[Identifier]struct{}) ([]VectorHit, error)
}

func searchVectorCandidates(index VectorIndex, field string, query []float32, k int,
	allowed map[Identifier]struct{}) ([]VectorHit, error) {
	if executor, ok := index.(vectorCandidateExecutor); ok {
		return executor.searchVectorCandidates(field, query, k, allowed)
	}
	return index.(VectorCandidateSearcher).SearchCandidates(field, query, k, allowed)
}

type vectorDocumentCandidateSearcher interface {
	searchDocumentCandidates(field string, query []float32, k int,
		allowed []uint64) ([]VectorHit, error)
}

type VectorBackend interface {
	Name() string
	Open(config Config) (VectorIndex, error)
}

type segmentVectorBackend interface {
	VectorBackend
	segment.VectorSegmentBackend
	OpenSnapshot(*index.Snapshot) (VectorIndex, error)
	VectorFieldSpecs(*index.Snapshot) (map[string]VectorFieldSpec, error)
}

func (config Config) WithVectorBackend(backend VectorBackend) Config {
	config.VectorBackend = backend
	options := segment.BuildOptions{}
	if embedded, ok := backend.(segmentVectorBackend); ok {
		options.VectorBackend = embedded
	}
	config.indexConfig = config.indexConfig.WithSegmentOptions(options)
	return config
}

type vectorCoordinator interface {
	initialize(*index.Writer) error
	batch(*index.Batch, func() error) error
	stage(*index.Batch, func() error) error
	finish(func() error) error
	reader(*index.Writer) (*index.Snapshot, VectorIndex, error)
	close() error
}

type vectorState struct {
	mu       sync.Mutex
	fields   map[string]VectorFieldSpec
	config   Config
	index    VectorIndex
	embedded segmentVectorBackend
	pending  []VectorChange
}

func newVectorState(config Config) *vectorState {
	embedded, _ := config.VectorBackend.(segmentVectorBackend)
	return &vectorState{config: config, embedded: embedded, fields: make(map[string]VectorFieldSpec)}
}

func openVectorCoordinator(config Config) (vectorCoordinator, error) {
	state := newVectorState(config)
	if state.embedded != nil {
		return state, nil
	}
	var err error
	state.index, err = state.open()
	if schema, ok := state.index.(vectorFieldSchema); ok {
		state.fields = schema.vectorFieldSpecs()
	}
	return state, err
}

func (v *vectorState) open() (VectorIndex, error) {
	backend := v.config.VectorBackend
	if backend == nil || backend.Name() == vectorBackendUnsupportedName {
		return nil, nil
	}
	return backend.Open(v.config)
}

func (v *vectorState) changes(batch *index.Batch) ([]VectorChange, error) {
	if v.index == nil && v.embedded == nil {
		hasVectors, err := batchContainsVectors(batch)
		if hasVectors {
			return nil, ErrVectorUnsupported
		}
		return nil, err
	}
	changes, err := vectorChangesForBatch(batch)
	if err != nil {
		return nil, err
	}
	if err := validatePublicVectorChanges(changes); err != nil {
		return nil, err
	}
	return changes, nil
}

func mergeVectorFieldSpecs(changes []VectorChange, fields map[string]VectorFieldSpec) (map[string]VectorFieldSpec, error) {
	next := maps.Clone(fields)
	for _, change := range changes {
		if change.Delete {
			continue
		}
		spec := VectorFieldSpec{Name: change.Field, Dims: len(change.Vector), Similarity: change.Similarity}
		if prior, ok := next[change.Field]; ok {
			if prior.Dims != spec.Dims {
				return nil, ErrVectorInvalidDimension
			}
			if prior.Similarity != spec.Similarity {
				return nil, ErrVectorInvalidField
			}
		}
		next[change.Field] = spec
	}
	return next, nil
}

func (v *vectorState) initialize(writer *index.Writer) error {
	if v.embedded == nil {
		return nil
	}
	snapshot, err := writer.Reader()
	if err != nil {
		return err
	}
	defer snapshot.Close()
	v.fields, err = v.embedded.VectorFieldSpecs(snapshot)
	return err
}

func (v *vectorState) commit(changes []VectorChange, commit func() error) error {
	if v.embedded != nil || v.index == nil || len(changes) == 0 {
		return commit()
	}
	if transaction, ok := v.index.(vectorBatchTransaction); ok {
		return transaction.applyVectorBatch(changes, commit)
	}
	batcher, writable := v.index.(VectorBatcher)
	if !writable {
		for _, change := range changes {
			if !change.Delete {
				return ErrVectorBackendReadOnly
			}
		}
		return commit()
	}
	if err := commit(); err != nil {
		return err
	}
	return batcher.ApplyVectorChanges(changes)
}

func (v *vectorState) snapshot(snapshot *index.Snapshot) (VectorIndex, error) {
	if v.embedded != nil {
		return v.embedded.OpenSnapshot(snapshot)
	}
	if snapshotter, ok := v.index.(VectorSnapshotter); ok {
		return snapshotter.SnapshotVectorIndex(), nil
	}
	return v.open()
}

func (v *vectorState) batch(batch *index.Batch, commit func() error) error {
	return v.write(batch, commit, false)
}

func (v *vectorState) stage(batch *index.Batch, commit func() error) error {
	return v.write(batch, commit, true)
}

func (v *vectorState) write(batch *index.Batch, commit func() error, deferred bool) error {
	if v.embedded != nil || v.index != nil {
		v.mu.Lock()
		defer v.mu.Unlock()
	}
	changes, err := v.changes(batch)
	if err != nil {
		return err
	}
	fields := v.fields
	if len(changes) > 0 {
		fields, err = mergeVectorFieldSpecs(changes, fields)
		if err != nil {
			return err
		}
	}
	if deferred {
		err = commit()
		if err == nil && v.index != nil {
			v.pending = append(v.pending, changes...)
		}
	} else {
		err = v.commit(changes, commit)
	}
	if err == nil && len(changes) > 0 {
		v.fields = fields
	}
	return err
}

func (v *vectorState) finish(commit func() error) error {
	if err := commit(); err != nil {
		return err
	}
	return v.commit(v.pending, func() error { return nil })
}

func (v *vectorState) reader(writer *index.Writer) (*index.Snapshot, VectorIndex, error) {
	if v.embedded != nil || v.index != nil {
		v.mu.Lock()
		defer v.mu.Unlock()
	}
	text, err := writer.Reader()
	if err != nil {
		return nil, nil, err
	}
	vector, err := v.snapshot(text)
	if err != nil {
		_ = text.Close()
		return nil, nil, err
	}
	return text, vector, nil
}

func (v *vectorState) close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return closeVectorIndex(v.index)
}

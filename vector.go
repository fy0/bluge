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
	"fmt"

	"github.com/fy0/bluge/index"
)

var ErrVectorUnsupported = errors.New("vector search is not enabled")

var (
	ErrVectorBackendReadOnly   = errors.New("vector backend is read-only")
	ErrVectorFieldNotFound     = errors.New("vector field was not found")
	ErrVectorInvalidDimension  = errors.New("vector dimension is invalid")
	ErrVectorInvalidK          = errors.New("vector k must be greater than zero")
	ErrVectorInvalidValue      = errors.New("vector contains NaN or infinity")
	ErrVectorInvalidField      = errors.New("vector field is invalid")
	ErrVectorFilterUnsupported = errors.New("vector backend does not support query filters")
)

type VectorSimilarity string

const (
	VectorL2     VectorSimilarity = "l2_norm"
	VectorDot    VectorSimilarity = "dot_product"
	VectorCosine VectorSimilarity = "cosine"
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

// VectorChange is the mutation sent to a writable vector backend after the
// corresponding text batch has been accepted.
type VectorChange struct {
	ID         Identifier
	Field      string
	Vector     []float32
	Similarity VectorSimilarity
	Delete     bool
}

// VectorBatcher is implemented by backends that can receive document changes.
// A delete with an empty Field removes every vector belonging to the ID.
type VectorBatcher interface {
	ApplyVectorChanges(changes []VectorChange) error
}

// VectorBatchValidator can reject a batch before the text index is mutated.
// Backends should implement it when dimension or metric validation depends on
// the current index state.
type VectorBatchValidator interface {
	ValidateVectorChanges(changes []VectorChange) error
}

// VectorChangeValidator is the segment-integrated spelling of
// VectorBatchValidator. It is an alias because both validators run before
// the text batch is submitted and share the same contract.
type VectorChangeValidator = VectorBatchValidator

// VectorSnapshotter creates a read-only point-in-time view for Reader values
// obtained from a live Writer. It is useful for in-memory backends whose Open
// method intentionally returns a shared writer instance.
type VectorSnapshotter interface {
	SnapshotVectorIndex() VectorIndex
}

// VectorCandidateSearcher lets Reader apply a Bluge Query filter before the
// backend ranks candidates. It avoids asking an ANN/native backend to know
// about Bluge's query language.
type VectorCandidateSearcher interface {
	SearchCandidates(field string, query []float32, k int,
		allowed map[Identifier]struct{}) ([]VectorHit, error)
}

type VectorBackend interface {
	Name() string
	Open(config Config) (VectorIndex, error)
}

func (config Config) WithVectorBackend(backend VectorBackend) Config {
	config.VectorBackend = backend
	if _, embedded := backend.(interface{ SegmentVectorBackend() }); embedded {
		config.indexConfig = config.indexConfig.WithSegmentOptions(map[string]interface{}{
			"vector_backend": backend,
		})
	} else {
		config.indexConfig = config.indexConfig.WithSegmentOptions(nil)
	}
	return config
}

func usesSegmentVectorBackend(config Config) bool {
	if config.VectorBackend == nil {
		return false
	}
	_, ok := config.VectorBackend.(interface{ SegmentVectorBackend() })
	return ok
}

func openVectorIndex(config Config) (VectorIndex, error) {
	if config.VectorBackend == nil || config.VectorBackend.Name() == "unsupported" ||
		usesSegmentVectorBackend(config) {
		return nil, nil
	}
	index, err := config.VectorBackend.Open(config)
	if err != nil {
		return nil, fmt.Errorf("open vector backend %q: %w", config.VectorBackend.Name(), err)
	}
	if index == nil {
		return nil, fmt.Errorf("vector backend %q returned a nil index", config.VectorBackend.Name())
	}
	return index, nil
}

func openVectorIndexForSnapshot(config Config, snapshot *index.Snapshot) (VectorIndex, error) {
	if snapshotBackend, ok := config.VectorBackend.(interface {
		OpenSnapshot(*index.Snapshot) (VectorIndex, error)
	}); ok {
		vector, err := snapshotBackend.OpenSnapshot(snapshot)
		if errors.Is(err, ErrVectorUnsupported) {
			return nil, nil
		}
		return vector, err
	}
	return openVectorIndex(config)
}

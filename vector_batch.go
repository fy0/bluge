// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"fmt"

	"github.com/fy0/bluge/index"
	segment "github.com/fy0/bluge/segment"
)

type vectorFieldValue interface {
	VectorValue() []float32
	VectorSimilarity() VectorSimilarity
}

func vectorChangesForBatch(batch *index.Batch) ([]VectorChange, bool, error) {
	if batch == nil {
		return nil, false, fmt.Errorf("nil batch")
	}

	var (
		changes   []VectorChange
		hasVector bool
	)
	for _, operation := range batch.Operations() {
		switch operation.Kind {
		case index.BatchOperationDelete:
			if operation.ID.Field() == _idField {
				changes = append(changes, VectorChange{
					ID:     Identifier(string(operation.ID.Term())),
					Delete: true,
				})
			}
		case index.BatchOperationInsert, index.BatchOperationUpdate:
			docID, hasID := documentIdentifier(operation.Document)
			vectors, hasDocVectors, err := documentVectors(operation.Document)
			if err != nil {
				return nil, false, err
			}
			if hasDocVectors {
				hasVector = true
			}
			if hasDocVectors && !hasID {
				return nil, false, fmt.Errorf("vector document is missing %q", _idField)
			}

			if operation.Kind == index.BatchOperationUpdate && operation.ID.Field() == _idField {
				changes = append(changes, VectorChange{
					ID:     Identifier(string(operation.ID.Term())),
					Delete: true,
				})
			}
			if hasID && (operation.Kind == index.BatchOperationUpdate || hasDocVectors) {
				// Insert and update have replacement semantics for vectors.
				changes = append(changes, VectorChange{ID: docID, Delete: true})
			}
			for _, vector := range vectors {
				changes = append(changes, VectorChange{
					ID:         docID,
					Field:      vector.field,
					Vector:     vector.vector,
					Similarity: vector.similarity,
				})
			}
		default:
			return nil, false, fmt.Errorf("unknown batch operation %d", operation.Kind)
		}
	}
	return changes, hasVector, nil
}

type documentVector struct {
	field      string
	vector     []float32
	similarity VectorSimilarity
}

func documentVectors(doc segment.Document) ([]documentVector, bool, error) {
	if doc == nil {
		return nil, false, fmt.Errorf("nil document")
	}
	var (
		vectors []documentVector
		err     error
	)
	doc.EachField(func(field segment.Field) {
		if err != nil {
			return
		}
		if field == nil {
			err = fmt.Errorf("nil field")
			return
		}
		vectorField, ok := field.(vectorFieldValue)
		if !ok {
			return
		}
		if field.Name() == "" {
			err = ErrVectorInvalidField
			return
		}
		vectors = append(vectors, documentVector{
			field:      field.Name(),
			vector:     append([]float32(nil), vectorField.VectorValue()...),
			similarity: vectorField.VectorSimilarity(),
		})
	})
	return vectors, len(vectors) > 0, err
}

func documentIdentifier(doc segment.Document) (Identifier, bool) {
	if doc == nil {
		return "", false
	}
	var (
		id    Identifier
		found bool
	)
	doc.EachField(func(field segment.Field) {
		if !found && field.Name() == _idField {
			id = Identifier(string(field.Value()))
			found = true
		}
	})
	return id, found
}

func applyVectorChanges(vector VectorIndex, changes []VectorChange, hasVectors bool) error {
	if vector == nil {
		if hasVectors {
			return ErrVectorUnsupported
		}
		return nil
	}
	if len(changes) == 0 {
		return nil
	}
	batcher, ok := vector.(VectorBatcher)
	if !ok {
		if hasVectors {
			return ErrVectorBackendReadOnly
		}
		return nil
	}
	return batcher.ApplyVectorChanges(changes)
}

func validateVectorChanges(vector VectorIndex, changes []VectorChange, hasVectors bool) error {
	if vector == nil {
		if hasVectors {
			return ErrVectorUnsupported
		}
		return nil
	}
	if len(changes) == 0 {
		return nil
	}
	if validator, ok := vector.(VectorBatchValidator); ok {
		return validator.ValidateVectorChanges(changes)
	}
	if _, ok := vector.(VectorBatcher); !ok && hasVectors {
		return ErrVectorBackendReadOnly
	}
	return nil
}

func closeVectorIndex(vector VectorIndex) error {
	if vector == nil {
		return nil
	}
	return vector.Close()
}

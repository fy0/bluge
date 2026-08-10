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
		changes      = make([]VectorChange, 0, batch.OperationCount()*2)
		hasVector    bool
		iterationErr error
	)
	batch.VisitOperations(func(operation index.BatchOperation) bool {
		switch operation.Kind {
		case index.BatchOperationDelete:
			if operation.ID.Field() == _idField {
				changes = append(changes, VectorChange{
					ID:     Identifier(string(operation.ID.Term())),
					Delete: true,
				})
			}
		case index.BatchOperationInsert, index.BatchOperationUpdate:
			docID, hasID, vectors, hasDocVectors, err := documentVectorData(operation.Document)
			if err != nil {
				iterationErr = err
				return false
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
			iterationErr = fmt.Errorf("unknown batch operation %d", operation.Kind)
			return false
		}
		return true
	})
	if iterationErr != nil {
		return nil, false, iterationErr
	}
	return changes, hasVector, nil
}

func batchContainsVectors(batch *index.Batch) (bool, error) {
	if batch == nil {
		return false, fmt.Errorf("nil batch")
	}
	var iterationErr error
	var hasVectors bool
	batch.VisitOperations(func(operation index.BatchOperation) bool {
		switch operation.Kind {
		case index.BatchOperationDelete:
			return true
		case index.BatchOperationInsert, index.BatchOperationUpdate:
			var err error
			hasVectors, err = documentHasVectors(operation.Document)
			if err != nil {
				iterationErr = err
				return false
			}
			return !hasVectors
		default:
			iterationErr = fmt.Errorf("unknown batch operation %d", operation.Kind)
			return false
		}
	})
	return hasVectors, iterationErr
}

type documentVector struct {
	field      string
	vector     []float32
	similarity VectorSimilarity
}

func documentVectorData(doc segment.Document) (Identifier, bool, []documentVector, bool, error) {
	var (
		id      Identifier
		hasID   bool
		vectors []documentVector
		err     error
	)
	visitErr := visitDocumentFields(doc, func(field segment.Field) bool {
		if err != nil {
			return false
		}
		if field == nil {
			err = fmt.Errorf("nil field")
			return false
		}
		name := field.Name()
		if !hasID && name == _idField {
			id = Identifier(string(field.Value()))
			hasID = true
		}
		vectorField, ok := field.(vectorFieldValue)
		if !ok {
			return true
		}
		if name == "" {
			err = ErrVectorInvalidField
			return false
		}
		vectors = append(vectors, documentVector{
			field:      name,
			vector:     append([]float32(nil), vectorField.VectorValue()...),
			similarity: vectorField.VectorSimilarity(),
		})
		return true
	})
	if visitErr != nil {
		return "", false, nil, false, visitErr
	}
	return id, hasID, vectors, len(vectors) > 0, err
}

func documentHasVectors(doc segment.Document) (bool, error) {
	var (
		found bool
		err   error
	)
	visitErr := visitDocumentFields(doc, func(field segment.Field) bool {
		if field == nil {
			err = fmt.Errorf("nil field")
			return false
		}
		if _, ok := field.(vectorFieldValue); ok {
			found = true
			return false
		}
		return true
	})
	if visitErr != nil {
		return false, visitErr
	}
	return found, err
}

func documentIdentifier(doc segment.Document) (Identifier, bool, error) {
	var id Identifier
	var found bool
	err := visitDocumentFields(doc, func(field segment.Field) bool {
		if field == nil {
			return true
		}
		if field.Name() == _idField {
			id = Identifier(string(field.Value()))
			found = true
			return false
		}
		return true
	})
	return id, found, err
}

func visitDocumentFields(doc segment.Document, visitor func(segment.Field) bool) error {
	if doc == nil {
		return fmt.Errorf("nil document")
	}
	switch document := doc.(type) {
	case *Document:
		if document == nil {
			return fmt.Errorf("nil document")
		}
		for _, field := range *document {
			if !visitor(field) {
				break
			}
		}
		return nil
	case Document:
		for _, field := range document {
			if !visitor(field) {
				break
			}
		}
		return nil
	}
	keepVisiting := true
	doc.EachField(func(field segment.Field) {
		if keepVisiting {
			keepVisiting = visitor(field)
		}
	})
	return nil
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

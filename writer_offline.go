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
	"errors"
	"fmt"

	segment "github.com/fy0/bluge/segment"

	"github.com/fy0/bluge/index"
)

type OfflineWriter struct {
	config               Config
	writer               *index.WriterOffline
	vector               VectorIndex
	pendingVectorChanges []VectorChange

	batchSize  int
	batch      *index.Batch
	batchCount int
	closed     bool
}

func OpenOfflineWriter(config Config, batchSize, maxSegmentsToMerge int) (*OfflineWriter, error) {
	if batchSize <= 0 {
		return nil, fmt.Errorf("batch size must be greater than zero")
	}
	if maxSegmentsToMerge < 2 {
		return nil, fmt.Errorf("max segments to merge must be at least 2")
	}
	rv := &OfflineWriter{
		config:    config,
		batchSize: batchSize,
		batch:     index.NewBatch(),
	}

	var err error
	rv.vector, err = openVectorIndex(config)
	if err != nil {
		return nil, err
	}
	rv.writer, err = index.OpenOfflineWriterWithMergeMax(
		config.indexConfigForWriting(), maxSegmentsToMerge)
	if err != nil {
		if rv.vector != nil {
			_ = rv.vector.Close()
		}
		return nil, fmt.Errorf("error opening index: %w", err)
	}

	return rv, nil
}

// Insert transfers the document to the offline writer. Full batches are built
// asynchronously; a background build error is returned by a later Insert or Close.
func (w *OfflineWriter) Insert(doc segment.Document) error {
	if w.closed {
		return fmt.Errorf("offline writer is closed")
	}
	if doc == nil {
		return fmt.Errorf("cannot insert nil document")
	}
	w.batch.Insert(doc)
	w.batchCount++
	if w.batchCount >= w.batchSize {
		return w.flushBatch()
	}
	return nil
}

// InsertMany transfers documents to the offline writer. Documents are grouped
// into the configured batch size so segment construction remains bounded and
// can run concurrently.
func (w *OfflineWriter) InsertMany(documents []*Document) error {
	if w.closed {
		return fmt.Errorf("offline writer is closed")
	}
	for i, document := range documents {
		if document == nil {
			return fmt.Errorf("cannot insert nil document at index %d", i)
		}
	}
	for _, document := range documents {
		w.batch.Insert(document)
		w.batchCount++
		if w.batchCount >= w.batchSize {
			if err := w.flushBatch(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *OfflineWriter) flushBatch() error {
	if w.batchCount == 0 {
		return nil
	}
	segmentVectors := usesSegmentVectorBackend(w.config)
	if w.vector == nil && !segmentVectors {
		hasVectors, err := batchContainsVectors(w.batch)
		if err != nil {
			return err
		}
		if hasVectors {
			return ErrVectorUnsupported
		}
		if err := w.writer.Batch(w.batch); err != nil {
			return err
		}
		w.batch.Reset()
		w.batchCount = 0
		return nil
	}

	changes, hasVectors, err := vectorChangesForBatch(w.batch)
	if err != nil {
		return err
	}
	if hasVectors && w.vector == nil && !segmentVectors {
		return ErrVectorUnsupported
	}
	if segmentVectors {
		if validator, ok := w.config.VectorBackend.(VectorChangeValidator); ok {
			if err := validator.ValidateVectorChanges(changes); err != nil {
				return err
			}
		}
	} else if err := validateVectorChanges(w.vector, changes, hasVectors); err != nil {
		return err
	}
	if err := w.writer.Batch(w.batch); err != nil {
		return err
	}
	if _, ok := w.vector.(VectorBatcher); ok {
		w.pendingVectorChanges = append(w.pendingVectorChanges, changes...)
	}
	w.batch.Reset()
	w.batchCount = 0
	return nil
}

func (w *OfflineWriter) Close() error {
	if w.closed {
		return fmt.Errorf("offline writer is closed")
	}
	w.closed = true
	var batchErr error
	if w.batchCount > 0 {
		batchErr = w.flushBatch()
	}
	closeErr := w.writer.Close()
	var vectorErr error
	if closeErr == nil && len(w.pendingVectorChanges) > 0 {
		vectorErr = applyVectorChanges(w.vector, w.pendingVectorChanges, true)
	}
	vectorCloseErr := closeVectorIndex(w.vector)
	return errors.Join(batchErr, closeErr, vectorErr, vectorCloseErr)
}

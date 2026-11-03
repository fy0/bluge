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
	"sync"

	segment "github.com/fy0/bluge/segment"

	"github.com/fy0/bluge/index"
)

type Writer struct {
	config Config
	chill  *index.Writer
	vector VectorIndex

	batchSlots     chan struct{}
	closeCh        chan struct{}
	closeOnce      sync.Once
	batchStatsLock sync.Mutex
	batchStats     BatchStats
	textImportLock sync.Mutex
}

func OpenWriter(config Config) (*Writer, error) {
	if config.maxConcurrentBatches < 0 {
		return nil, fmt.Errorf("maximum concurrent batches must not be negative")
	}
	if config.indexConfig.NumAnalysisWorkers < 1 {
		return nil, fmt.Errorf("number of analysis workers must be at least 1")
	}
	if config.indexConfig.MergeBufferSize < 1 {
		return nil, fmt.Errorf("merge buffer size must be at least 1 byte")
	}

	rv := &Writer{
		config:  config,
		closeCh: make(chan struct{}),
	}
	if config.maxConcurrentBatches > 0 {
		rv.batchSlots = make(chan struct{}, config.maxConcurrentBatches)
	}

	var err error
	if !usesSegmentVectorBackend(config) {
		rv.vector, err = openVectorIndex(config)
	}
	if err != nil {
		return nil, err
	}
	rv.chill, err = index.OpenWriter(config.indexConfigForWriting())
	if err != nil {
		if rv.vector != nil {
			_ = rv.vector.Close()
		}
		return nil, fmt.Errorf("error opening index: %w", err)
	}

	return rv, nil
}

func (w *Writer) Insert(doc segment.Document) error {
	b := NewBatch()
	b.Insert(doc)
	return w.Batch(b)
}

// InsertMany inserts documents in one atomic text-index batch. Vector-enabled
// backends receive the corresponding mutations as one batch as well.
func (w *Writer) InsertMany(documents []*Document) error {
	if len(documents) == 0 {
		return nil
	}
	batch := NewBatch()
	for i, document := range documents {
		if document == nil {
			return fmt.Errorf("cannot insert nil document at index %d", i)
		}
		batch.Insert(document)
	}
	return w.Batch(batch)
}

func (w *Writer) Update(id segment.Term, doc segment.Document) error {
	b := NewBatch()
	b.Update(id, doc)
	return w.Batch(b)
}

// UpdateMany replaces documents by their _id fields in one atomic text-index
// batch. Every document must contain an _id field.
func (w *Writer) UpdateMany(documents []*Document) error {
	if len(documents) == 0 {
		return nil
	}
	ids := make([]Identifier, len(documents))
	for i, document := range documents {
		if document == nil {
			return fmt.Errorf("cannot update nil document at index %d", i)
		}
		id, found, err := documentIdentifier(document)
		if err != nil {
			return fmt.Errorf("cannot read document ID at index %d: %w", i, err)
		}
		if !found {
			return fmt.Errorf("cannot update document at index %d without %q", i, _idField)
		}
		ids[i] = id
	}

	batch := NewBatch()
	for i, document := range documents {
		batch.Update(ids[i], document)
	}
	return w.Batch(batch)
}

func (w *Writer) Delete(id segment.Term) error {
	b := NewBatch()
	b.Delete(id)
	return w.Batch(b)
}

func (w *Writer) Batch(batch *index.Batch) error {
	if err := w.acquireBatch(context.Background()); err != nil {
		return err
	}
	defer w.releaseBatch()
	return w.batch(batch)
}

// BatchFunc acquires a batch slot before calling build, so waiting callers can
// defer allocating documents and their payloads. Configure WithMaxConcurrentBatches
// to bound simultaneous builders and writes. The callback fills the supplied
// empty batch; it must not submit writes to this Writer itself.
//
// Cancellation while waiting skips build. Cancellation or a build error before
// submission discards the batch. Once submission starts, ordinary Batch semantics
// apply: cancellation cannot roll back a write. A running build must cooperate
// with ctx itself. Close wakes waiters but cannot interrupt a running builder.
// Capturing already-built documents defeats deferred allocation.
func (w *Writer) BatchFunc(ctx context.Context, build func(*index.Batch) error) error {
	if ctx == nil || build == nil {
		return fmt.Errorf("batch context and builder must be provided")
	}
	if err := w.acquireBatch(ctx); err != nil {
		return err
	}
	defer w.releaseBatch()
	batch := NewBatch()
	if err := build(batch); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.batch(batch)
}

func (w *Writer) batch(batch *index.Batch) error {
	var inputBytes uint64
	if w.config.batchStatsEnabled {
		defer func() {
			w.batchStatsLock.Lock()
			w.batchStats.InFlightInputBytes -= inputBytes
			w.batchStatsLock.Unlock()
		}()
	}
	if w.config.maxDocumentInputBytes > 0 || w.config.maxBatchInputBytes > 0 || w.config.batchStatsEnabled {
		var err error
		inputBytes, err = w.checkBatchInput(batch)
		if err != nil {
			return err
		}
		if w.config.batchStatsEnabled {
			w.batchStatsLock.Lock()
			w.batchStats.InFlightInputBytes += inputBytes
			w.batchStatsLock.Unlock()
		}
	}
	segmentVectors := usesSegmentVectorBackend(w.config)
	if w.vector == nil && !segmentVectors {
		hasVectors, err := batchContainsVectors(batch)
		if err != nil {
			return err
		}
		if hasVectors {
			return ErrVectorUnsupported
		}
		return w.chill.Batch(batch)
	}

	changes, hasVectors, err := vectorChangesForBatch(batch)
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

	if err := w.chill.Batch(batch); err != nil {
		return err
	}
	if segmentVectors {
		return nil
	}
	return applyVectorChanges(w.vector, changes, hasVectors)
}

func (w *Writer) Close() error {
	// The index's close signal is sent only after active mutations have drained.
	// Wake admission waiters immediately, then reuse that existing drain lifecycle.
	w.closeOnce.Do(func() { close(w.closeCh) })
	return errors.Join(w.chill.Close(), closeVectorIndex(w.vector))
}

func (w *Writer) Reader() (*Reader, error) {
	r, err := w.chill.Reader()
	if err != nil {
		return nil, fmt.Errorf("error getting nreal time reader: %w", err)
	}
	var vector VectorIndex
	if usesSegmentVectorBackend(w.config) {
		vector, err = openVectorIndexForSnapshot(w.config, r)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
	} else if w.vector != nil {
		if snapshotter, ok := w.vector.(VectorSnapshotter); ok {
			vector = snapshotter.SnapshotVectorIndex()
		} else {
			vector, err = openVectorIndexForSnapshot(w.config, r)
			if err != nil {
				_ = r.Close()
				return nil, err
			}
		}
	}
	return &Reader{
		config: w.config,
		reader: r,
		vector: vector,
	}, nil
}

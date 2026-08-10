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

type Writer struct {
	config Config
	chill  *index.Writer
	vector VectorIndex
}

func OpenWriter(config Config) (*Writer, error) {
	rv := &Writer{
		config: config,
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

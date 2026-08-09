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

func (w *Writer) Update(id segment.Term, doc segment.Document) error {
	b := NewBatch()
	b.Update(id, doc)
	return w.Batch(b)
}

func (w *Writer) Delete(id segment.Term) error {
	b := NewBatch()
	b.Delete(id)
	return w.Batch(b)
}

func (w *Writer) Batch(batch *index.Batch) error {
	changes, hasVectors, err := vectorChangesForBatch(batch)
	if err != nil {
		return err
	}
	if hasVectors && w.vector == nil && !usesSegmentVectorBackend(w.config) {
		return ErrVectorUnsupported
	}
	if usesSegmentVectorBackend(w.config) {
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
	if usesSegmentVectorBackend(w.config) {
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

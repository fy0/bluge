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
	"fmt"

	segment "github.com/fy0/bluge/segment"

	"github.com/fy0/bluge/index"
)

type OfflineWriter struct {
	writer *index.WriterOffline

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
		batchSize: batchSize,
		batch:     index.NewBatch(),
	}

	var err error
	rv.writer, err = index.OpenOfflineWriterWithMergeMax(
		config.indexConfigForWriting(), maxSegmentsToMerge)
	if err != nil {
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
		err := w.writer.Batch(w.batch)
		if err != nil {
			return err
		}
		w.batch.Reset()
		w.batchCount = 0
	}
	return nil
}

func (w *OfflineWriter) Close() error {
	if w.closed {
		return fmt.Errorf("offline writer is closed")
	}
	w.closed = true
	var batchErr error
	if w.batchCount > 0 {
		batchErr = w.writer.Batch(w.batch)
	}
	closeErr := w.writer.Close()
	if batchErr != nil {
		return batchErr
	}
	return closeErr
}

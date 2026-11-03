// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"context"
	"fmt"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/segment"
)

const (
	documentInputScope = "document"
	batchInputScope    = "batch"
)

// InputLimitError reports a rejected document or batch. Actual and Limit are
// input bytes, not heap usage. Scope is either "document" or "batch".
type InputLimitError struct {
	Scope  string
	Actual uint64
	Limit  uint64
}

func (e *InputLimitError) Error() string {
	return fmt.Sprintf("%s input bytes %d exceed limit %d", e.Scope, e.Actual, e.Limit)
}

// BatchStats measures admitted online Writer calls through Batch completion,
// excluding calls waiting for a slot. Input bytes are added after input checks.
// BatchFunc builders contribute to InFlightBatches while constructing a batch.
// These are workload gauges, not heap usage or a memory reservation.
type BatchStats struct {
	InFlightBatches    uint64
	InFlightInputBytes uint64
}

// BatchStats returns a consistent snapshot, or zeros if WithBatchStats is off.
func (w *Writer) BatchStats() BatchStats {
	w.batchStatsLock.Lock()
	defer w.batchStatsLock.Unlock()
	return w.batchStats
}

func (w *Writer) acquireBatch(ctx context.Context) error {
	// Admission holds no index lock, so persistence and active writes can drain.
	if w.batchSlots != nil {
		select {
		case w.batchSlots <- struct{}{}:
		case <-w.closeCh:
			return segment.ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// A slot and a close/cancellation signal may become ready together.
	var err error
	select {
	case <-w.closeCh:
		err = segment.ErrClosed
	default:
		err = ctx.Err()
	}
	if err != nil {
		if w.batchSlots != nil {
			<-w.batchSlots
		}
		return err
	}
	if w.config.batchStatsEnabled {
		w.batchStatsLock.Lock()
		w.batchStats.InFlightBatches++
		w.batchStatsLock.Unlock()
	}
	return nil
}

func (w *Writer) releaseBatch() {
	if w.config.batchStatsEnabled {
		w.batchStatsLock.Lock()
		w.batchStats.InFlightBatches--
		w.batchStatsLock.Unlock()
	}
	if w.batchSlots != nil {
		<-w.batchSlots
	}
}

func (w *Writer) checkBatchInput(batch *index.Batch) (uint64, error) {
	if batch == nil {
		return 0, fmt.Errorf("nil batch")
	}
	var inputBytes uint64
	var err error
	batch.VisitOperations(func(op index.BatchOperation) bool {
		switch op.Kind {
		case index.BatchOperationInsert, index.BatchOperationUpdate:
			var documentBytes uint64
			var fieldErr error
			err = visitDocumentFields(op.Document, func(field segment.Field) bool {
				if field == nil {
					fieldErr = fmt.Errorf("nil field")
					return false
				}
				documentBytes += uint64(len(field.Name())) + uint64(len(field.Value()))
				return true
			})
			if err == nil {
				err = fieldErr
			}
			if err != nil {
				return false
			}
			if limit := w.config.maxDocumentInputBytes; limit > 0 && documentBytes > limit {
				err = &InputLimitError{Scope: documentInputScope, Actual: documentBytes, Limit: limit}
				return false
			}
			inputBytes += documentBytes
		}
		if op.Kind == index.BatchOperationUpdate || op.Kind == index.BatchOperationDelete {
			inputBytes += uint64(len(op.ID.Field())) + uint64(len(op.ID.Term()))
		}
		return true
	})
	if err != nil {
		return 0, err
	}
	if limit := w.config.maxBatchInputBytes; limit > 0 && inputBytes > limit {
		return 0, &InputLimitError{Scope: batchInputScope, Actual: inputBytes, Limit: limit}
	}
	return inputBytes, nil
}

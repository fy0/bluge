// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// These fields are reserved for ImportText documents. Index, start and end are
// stored as decimal integers; byte ranges are zero-based and end-exclusive.
const (
	TextChunkSourceField = "_chunk_source"
	TextChunkIndexField  = "_chunk_index"
	TextChunkStartField  = "_chunk_start"
	TextChunkEndField    = "_chunk_end"
)

// TextChunkOptions explicitly enables chunking for ImportText only. Ordinary
// Insert, Update and Batch calls never split documents.
type TextChunkOptions struct {
	// MaxChunkBytes bounds each chunk's text, in UTF-8 bytes, including overlap.
	// It must be at least 4. Document/Batch input limits also count IDs and fields.
	MaxChunkBytes int
	// OverlapBytes is an upper bound on repeated bytes (default 0). It must be
	// nonnegative and smaller than MaxChunkBytes. Actual overlap rounds down to
	// rune boundaries and may shrink to ensure progress and room for a new rune.
	OverlapBytes int
	// BatchSize bounds documents per submitted batch. Zero means 1; negatives
	// are invalid. The Writer's batch byte limit may cause earlier submission.
	BatchSize int
}

// TextImportResult describes calls to Batch that returned success. Committed
// chunk ordinals are [0, CommittedChunks), covering source bytes
// [0, CommittedEndByte). Overlapping bytes are counted once in that range.
// A failing Batch may already have been applied (e.g. a persistence error);
// the returned error identifies that attempted range separately.
type TextImportResult struct {
	SourceDeleted    bool
	CommittedChunks  uint64
	CommittedEndByte int64
}

// TextChunkID returns the stable ID used by ImportText. Applications must reserve
// the "_bluge_text_chunk:" ID namespace for these text-only documents.
func TextChunkID(sourceID string, index uint64) Identifier {
	return Identifier("_bluge_text_chunk:" +
		base64.RawURLEncoding.EncodeToString([]byte(sourceID)) + ":" +
		strconv.FormatUint(index, 10))
}

type textSourceTerm string

func (s textSourceTerm) Field() string { return TextChunkSourceField }
func (s textSourceTerm) Term() []byte  { return []byte(s) }

// DeleteText deletes all ImportText chunks for sourceID in one ordinary Batch.
// It does not delete an ordinary document whose _id is sourceID.
func (w *Writer) DeleteText(sourceID string) error {
	if sourceID == "" {
		return fmt.Errorf("text source ID must not be empty")
	}
	w.textImportLock.Lock()
	defer w.textImportLock.Unlock()
	return w.Delete(textSourceTerm(sourceID))
}

// ImportText replaces all chunks of a logical source using bounded batches.
// It stores only each chunk's text, with the standard analyzer and term positions,
// plus source/ordinal/byte metadata. No parent document or vectors are created.
//
// After validating the first chunk, it deletes the old source in one Batch,
// then submits new chunks in separate Batches. There is no cross-batch atomicity:
// readers may see a missing or partial source, and errors leave prior successful
// batches in place. Unsubmitted chunks are discarded. Retry from the beginning
// or call DeleteText to clean up. Empty input deletes the source without inserts.
// Invalid UTF-8 is an error, potentially after earlier batches have succeeded.
//
// ImportText and DeleteText calls on a Writer are serialized. Callers must not
// mutate reserved chunk documents through ordinary write APIs concurrently.
// The reader is not closed, and a blocked Read cannot be interrupted by Close.
func (w *Writer) ImportText(sourceID, field string, input io.Reader,
	options TextChunkOptions) (result TextImportResult, err error) {
	if sourceID == "" || field == "" || input == nil {
		return result, fmt.Errorf("text source ID, field and reader must be provided")
	}
	switch field {
	case _idField, TextChunkSourceField, TextChunkIndexField, TextChunkStartField, TextChunkEndField:
		return result, fmt.Errorf("text field %q is reserved", field)
	}
	if err := options.validate(); err != nil {
		return result, err
	}
	if options.BatchSize == 0 {
		options.BatchSize = 1
	}
	w.textImportLock.Lock()
	defer w.textImportLock.Unlock()
	defer func() {
		if err != nil {
			err = fmt.Errorf("import text %q: source deleted=%t, committed chunks [0,%d), bytes [0,%d): %w",
				sourceID, result.SourceDeleted, result.CommittedChunks, result.CommittedEndByte, err)
		}
	}()

	deletion := NewBatch()
	deletion.Delete(textSourceTerm(sourceID))
	if _, err = w.checkBatchInput(deletion); err != nil {
		return result, err
	}
	chunks := newTextChunkReader(input, options)
	batch := NewBatch()
	var batchBytes uint64
	var batchStart, batchEnd int64
	flush := func() error {
		if batch.OperationCount() == 0 {
			return nil
		}
		count := uint64(batch.OperationCount())
		if err := w.Batch(batch); err != nil {
			return fmt.Errorf("submitting chunks [%d,%d), bytes [%d,%d) (may have been applied): %w",
				result.CommittedChunks, result.CommittedChunks+count, batchStart, batchEnd, err)
		}
		result.CommittedChunks += count
		result.CommittedEndByte = batchEnd
		// Reset retains references to analyzed documents; allocate an empty Batch
		// instead so the importer does not retain previous batches' analysis.
		batch = NewBatch()
		batchBytes = 0
		return nil
	}
	for {
		chunk, readErr := chunks.next()
		if readErr != nil && readErr != io.EOF {
			return result, readErr
		}
		var singleBytes uint64
		var doc *Document
		if readErr != io.EOF {
			ordinal := result.CommittedChunks + uint64(batch.OperationCount())
			doc = newTextChunkDocument(sourceID, field, ordinal, chunk)
			single := NewBatch()
			single.Insert(doc)
			// Reuse the same pre-analysis accounting as ordinary Batch, including
			// IDs, field names, and all stored metadata. Never resize a chunk to
			// silently accommodate an incompatible document or batch limit.
			singleBytes, err = w.checkBatchInput(single)
			if err != nil {
				return result, fmt.Errorf("checking chunk %d: %w", ordinal, err)
			}
		}
		if !result.SourceDeleted {
			if err = w.Batch(deletion); err != nil {
				return result, fmt.Errorf("deleting old source chunks (may have been applied): %w", err)
			}
			result.SourceDeleted = true
		}
		if readErr == io.EOF {
			err = flush()
			return result, err
		}
		if limit := w.config.maxBatchInputBytes; limit > 0 && singleBytes > limit-batchBytes {
			if err = flush(); err != nil {
				return result, err
			}
		}
		if batch.OperationCount() == 0 {
			batchStart = chunk.start
		}
		batch.Insert(doc)
		batchBytes += singleBytes
		batchEnd = chunk.end
		if batch.OperationCount() >= options.BatchSize {
			if err = flush(); err != nil {
				return result, err
			}
		}
	}
}

func (o TextChunkOptions) validate() error {
	if o.MaxChunkBytes < utf8.UTFMax || o.MaxChunkBytes > int(^uint(0)>>1)-utf8.UTFMax {
		return fmt.Errorf("maximum text chunk bytes must be at least 4 and allow UTF-8 lookahead")
	}
	if o.OverlapBytes < 0 || o.OverlapBytes >= o.MaxChunkBytes {
		return fmt.Errorf("text overlap bytes must be nonnegative and smaller than maximum chunk bytes")
	}
	if o.BatchSize < 0 {
		return fmt.Errorf("text batch size must not be negative")
	}
	return nil
}

type textChunk struct {
	text       []byte
	start, end int64
}

func newTextChunkDocument(sourceID, field string, ordinal uint64, chunk textChunk) *Document {
	doc := NewDocument(string(TextChunkID(sourceID, ordinal))).
		AddField(NewTextFieldBytes(field, chunk.text).StoreValue().SearchTermPositions()).
		AddField(NewKeywordField(TextChunkSourceField, sourceID).StoreValue())
	for _, entry := range []struct{ name, value string }{
		{TextChunkIndexField, strconv.FormatUint(ordinal, 10)},
		{TextChunkStartField, strconv.FormatInt(chunk.start, 10)},
		{TextChunkEndField, strconv.FormatInt(chunk.end, 10)},
	} {
		stored := NewKeywordField(entry.name, entry.value)
		stored.FieldOptions = Store
		doc.AddField(stored)
	}
	return doc
}

// The only read-ahead is one fixed buffer (MaxChunkBytes + 4, minimum 16 bytes
// imposed by bufio). next copies just the emitted chunk; it never reads all input.
type textChunkReader struct {
	reader     *bufio.Reader
	options    TextChunkOptions
	start, end int64
}

func newTextChunkReader(input io.Reader, options TextChunkOptions) *textChunkReader {
	return &textChunkReader{
		// Hide a caller's *bufio.Reader so NewReaderSize always creates our own
		// bounded buffer. The supplied Reader may still buffer independently.
		reader:  bufio.NewReaderSize(struct{ io.Reader }{input}, options.MaxChunkBytes+utf8.UTFMax),
		options: options,
	}
}

func (r *textChunkReader) next() (textChunk, error) {
	buffer, err := r.reader.Peek(r.options.MaxChunkBytes + utf8.UTFMax)
	if err != nil && err != io.EOF {
		return textChunk{}, fmt.Errorf("reading text near byte %d: %w", r.start, err)
	}
	if len(buffer) == 0 || r.start+int64(len(buffer)) <= r.end {
		return textChunk{}, io.EOF
	}
	cut := len(buffer)
	if cut > r.options.MaxChunkBytes {
		cut = r.options.MaxChunkBytes
		for cut > 0 && !utf8.RuneStart(buffer[cut]) {
			cut--
		}
	}
	if cut == 0 || !utf8.Valid(buffer[:cut]) {
		return textChunk{}, fmt.Errorf("invalid UTF-8 in text near byte %d", r.start)
	}
	if len(buffer) > r.options.MaxChunkBytes {
		// Prefer boundaries in the latter half, and require at least one new
		// rune beyond the previous chunk's end even when using overlap.
		minimum := cut / 2
		if newByte := int(r.end-r.start) + 1; minimum < newByte {
			minimum = newByte
		}
		var paragraph, line, space int
		for pos, char := range string(buffer[:cut]) {
			end := pos + utf8.RuneLen(char)
			if end < minimum {
				continue
			}
			if unicode.IsSpace(char) {
				space = end
			}
			if char == '\r' || char == '\n' {
				line = end
			}
			if char == '\n' && (pos > 0 && buffer[pos-1] == '\n' ||
				pos > 1 && buffer[pos-1] == '\r' && buffer[pos-2] == '\n') {
				paragraph = end
			}
		}
		switch {
		case paragraph > 0:
			cut = paragraph
		case line > 0:
			cut = line
		case space > 0:
			cut = space
		}
	}
	chunk := textChunk{
		text: append([]byte(nil), buffer[:cut]...), start: r.start, end: r.start + int64(cut),
	}
	advance := cut
	if cut < len(buffer) {
		_, nextWidth := utf8.DecodeRune(buffer[cut:])
		overlap := r.options.OverlapBytes
		if overlap >= cut {
			overlap = cut - 1
		}
		if room := r.options.MaxChunkBytes - nextWidth; overlap > room {
			overlap = room
		}
		advance = cut - overlap
		for advance < cut && !utf8.RuneStart(buffer[advance]) {
			advance++
		}
	}
	_, _ = r.reader.Discard(advance) // advance is within the peeked buffer.
	r.start += int64(advance)
	r.end = chunk.end
	return chunk, nil
}

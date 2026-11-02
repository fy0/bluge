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
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/internal/zapxbluge"
	"github.com/fy0/bluge/segment"
)

func TestTextChunkBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, input, first string
		maximum, overlap   int
	}{
		{"empty", "", "", 4, 0},
		{"exact", "abcdefgh", "abcdefgh", 8, 0},
		{"over boundary", "abcdefghi", "abcdefgh", 8, 0},
		{"paragraph", "aaaa\n\nbb cccc dddd", "aaaa\n\n", 12, 0},
		{"CRLF paragraph", "aaaa\r\n\r\nbb cccc dddd", "aaaa\r\n\r\n", 14, 0},
		{"line", "aaaaaa\nbb cc dddd", "aaaaaa\n", 12, 0},
		{"space", "aaaaaa bbcc dddd", "aaaaaa bbcc ", 12, 0},
		{"unicode space", "aaaaaa\u3000bbccdddd", "aaaaaa\u3000", 12, 0},
		{"Chinese", "中文字符测试长段落", "中文", 8, 0},
		{"long paragraph", strings.Repeat("中文🙂abcdef", 1024), "中文🙂ab", 12, 0},
		{"overlap", "abcdefghijklmnop", "abcdefgh", 8, 3},
		{"UTF8 overlap", "中文🙂abcdef中文🙂", "中文🙂", 10, 5},
		{"minimum progress", "abc🙂中xyz🙂\n\n🙂done", "abc", 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTextChunkReader(iotest.OneByteReader(strings.NewReader(tc.input)),
				TextChunkOptions{MaxChunkBytes: tc.maximum, OverlapBytes: tc.overlap})
			var previousStart, previousEnd int64
			for ordinal := 0; ; ordinal++ {
				chunk, err := r.next()
				if err == io.EOF {
					if previousEnd != int64(len(tc.input)) {
						t.Fatalf("missing suffix: end=%d input=%d", previousEnd, len(tc.input))
					}
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if ordinal > len(tc.input) || chunk.end <= previousEnd ||
					ordinal > 0 && chunk.start <= previousStart {
					t.Fatalf("no progress at chunk %d: %+v", ordinal, chunk)
				}
				if len(chunk.text) > tc.maximum || !utf8.Valid(chunk.text) ||
					string(chunk.text) != tc.input[chunk.start:chunk.end] {
					t.Fatalf("invalid chunk: %+v", chunk)
				}
				if chunk.start > previousEnd || previousEnd-chunk.start > int64(tc.overlap) {
					t.Fatalf("missing or excessive overlap: previous end=%d, chunk=%+v", previousEnd, chunk)
				}
				if ordinal == 0 && string(chunk.text) != tc.first {
					t.Fatalf("first = %q, want %q", chunk.text, tc.first)
				}
				previousStart, previousEnd = chunk.start, chunk.end
			}
		})
	}
}

func TestTextChunkUTF8Progress(t *testing.T) {
	// Exercise all overlap values with boundaries at each UTF-8 width, including
	// boundaries inside the retained overlap and a four-byte rune that needs room.
	input := strings.Repeat("a中é🙂\n\r\n\t汉字  xyz", 10)
	for maximum := 4; maximum <= 32; maximum++ {
		for overlap := 0; overlap < maximum; overlap++ {
			r := newTextChunkReader(strings.NewReader(input),
				TextChunkOptions{MaxChunkBytes: maximum, OverlapBytes: overlap})
			var end int64
			for ordinal := 0; ; ordinal++ {
				chunk, err := r.next()
				if err == io.EOF {
					break
				}
				if err != nil || ordinal >= len(input) || chunk.end <= end ||
					chunk.start > end || end-chunk.start > int64(overlap) ||
					len(chunk.text) > maximum || !utf8.Valid(chunk.text) ||
					string(chunk.text) != input[chunk.start:chunk.end] {
					t.Fatalf("max=%d overlap=%d end=%d chunk=%+v err=%v", maximum, overlap, end, chunk, err)
				}
				end = chunk.end
			}
			if end != int64(len(input)) {
				t.Fatalf("max=%d overlap=%d missing bytes after %d", maximum, overlap, end)
			}
		}
	}
}

func textTestWriter(t *testing.T, config Config) *Writer {
	t.Helper()
	w, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	})
	return w
}

func textTestHits(t *testing.T, w *Writer, query Query) []map[string]string {
	t.Helper()
	r, err := w.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	it, err := r.Search(context.Background(), NewAllMatches(query))
	if err != nil {
		t.Fatal(err)
	}
	var hits []map[string]string
	for {
		match, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		if match == nil {
			break
		}
		stored := map[string]string{}
		if err := match.VisitStoredFields(func(field string, value []byte) bool {
			stored[field] = string(value)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		hits = append(hits, stored)
	}
	return hits
}

func TestImportTextDefaultAndSearch(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig()) // No vectors or chunk configuration.
	text := "alpha beta gamma delta 中文 " + strings.Repeat("long text ", 30)
	if err := w.Insert(NewDocument("ordinary").AddField(NewTextField("body", text).StoreValue())); err != nil {
		t.Fatal(err)
	}
	hits := textTestHits(t, w, NewTermQuery("ordinary").SetField(_idField))
	if len(hits) != 1 || hits[0]["body"] != text || hits[0][TextChunkSourceField] != "" {
		t.Fatalf("ordinary writes changed: %v", hits)
	}
	source := "文件:/a:1"
	result, err := w.ImportText(source, "body", strings.NewReader(text), TextChunkOptions{MaxChunkBytes: 24, BatchSize: 3})
	if err != nil || !result.SourceDeleted || result.CommittedChunks < 2 || result.CommittedEndByte != int64(len(text)) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	hits = textTestHits(t, w, NewTermQuery(source).SetField(TextChunkSourceField))
	sort.Slice(hits, func(i, j int) bool {
		a, _ := strconv.Atoi(hits[i][TextChunkIndexField])
		b, _ := strconv.Atoi(hits[j][TextChunkIndexField])
		return a < b
	})
	var joined strings.Builder
	for i, hit := range hits {
		if hit[_idField] != string(TextChunkID(source, uint64(i))) ||
			hit[TextChunkStartField] != strconv.Itoa(joined.Len()) {
			t.Fatalf("invalid identity/range: %v", hit)
		}
		joined.WriteString(hit["body"])
		if hit[TextChunkEndField] != strconv.Itoa(joined.Len()) {
			t.Fatalf("invalid end: %v", hit)
		}
	}
	if joined.String() != text || uint64(len(hits)) != result.CommittedChunks {
		t.Fatal("stored chunks do not reconstruct the source")
	}
	hits = textTestHits(t, w, NewMatchQuery("delta").SetField("body"))
	if len(hits) != 2 { // One ordinary document and one chunk.
		t.Fatalf("plain text search hits=%v", hits)
	}
	if TextChunkID("a:1", 2) == TextChunkID("a", 12) ||
		TextChunkID("a", 12) == TextChunkID("a", 2) {
		t.Fatal("ambiguous chunk IDs")
	}
}

func TestImportTextReplaceDeleteAndEmpty(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig())
	opts := TextChunkOptions{MaxChunkBytes: 8}
	for _, source := range []string{"parent", "other"} {
		if _, err := w.ImportText(source, "body", strings.NewReader(strings.Repeat("old ", 10)), opts); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Insert(NewDocument("parent")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ImportText("parent", "body", strings.NewReader("new"), opts); err != nil {
		t.Fatal(err)
	}
	hits := textTestHits(t, w, NewTermQuery("parent").SetField(TextChunkSourceField))
	if len(hits) != 1 || hits[0]["body"] != "new" || hits[0][_idField] != string(TextChunkID("parent", 0)) {
		t.Fatalf("old tail survived replacement: %v", hits)
	}
	if err := w.Delete(Identifier("parent")); err != nil {
		t.Fatal(err)
	}
	if len(textTestHits(t, w, NewTermQuery("parent").SetField(TextChunkSourceField))) != 1 {
		t.Fatal("ordinary ID delete unexpectedly cascaded")
	}
	if err := w.DeleteText("parent"); err != nil {
		t.Fatal(err)
	}
	if len(textTestHits(t, w, NewTermQuery("parent").SetField(TextChunkSourceField))) != 0 ||
		len(textTestHits(t, w, NewTermQuery("other").SetField(TextChunkSourceField))) == 0 {
		t.Fatal("source deletion removed wrong documents")
	}
	result, err := w.ImportText("other", "body", strings.NewReader(""), opts)
	if err != nil || result != (TextImportResult{SourceDeleted: true}) || len(textTestHits(t, w, NewMatchAllQuery())) != 0 {
		t.Fatalf("empty import: result=%+v err=%v", result, err)
	}
}

func TestImportTextPhraseOverlap(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig())
	options := TextChunkOptions{MaxChunkBytes: 11}
	if _, err := w.ImportText("s", "body", strings.NewReader("alpha beta gamma"), options); err != nil {
		t.Fatal(err)
	}
	query := NewMatchPhraseQuery("beta gamma").SetField("body")
	if len(textTestHits(t, w, query)) != 0 {
		t.Fatal("phrase unexpectedly crossed documents")
	}
	options.MaxChunkBytes = 12
	options.OverlapBytes = 5
	if _, err := w.ImportText("s", "body", strings.NewReader("alpha beta gamma"), options); err != nil {
		t.Fatal(err)
	}
	hits := textTestHits(t, w, query)
	if len(hits) != 1 || hits[0][TextChunkSourceField] != "s" {
		t.Fatalf("overlap did not preserve a phrase inside one chunk: %v", hits)
	}
}

func TestImportTextReopen(t *testing.T) {
	config := DefaultConfig(t.TempDir())
	w, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	_, importErr := w.ImportText("s", "body", strings.NewReader(strings.Repeat("old ", 10)), TextChunkOptions{MaxChunkBytes: 8})
	closeErr := w.Close()
	if importErr != nil || closeErr != nil {
		t.Fatalf("import=%v close=%v", importErr, closeErr)
	}
	w = textTestWriter(t, config)
	if _, err := w.ImportText("s", "body", strings.NewReader("new"), TextChunkOptions{MaxChunkBytes: 8}); err != nil {
		t.Fatal(err)
	}
	if hits := textTestHits(t, w, NewMatchAllQuery()); len(hits) != 1 || hits[0]["body"] != "new" {
		t.Fatalf("replacement after reopen: %v", hits)
	}
	if err := w.DeleteText("s"); err != nil {
		t.Fatal(err)
	}
	if len(textTestHits(t, w, NewMatchAllQuery())) != 0 {
		t.Fatal("deletion after reopen retained chunks")
	}
}

func TestImportTextLimits(t *testing.T) {
	// For source "s", body "abcdefgh", ordinal 0 and [0,8): the complete
	// document is 88 input bytes (25 ID + 12 body + 14 source + 37 metadata).
	for _, tc := range []struct {
		name, scope string
		doc, batch  uint64
	}{
		{"equality", "", 88, 88},
		{"document overhead", "document", 87, 0},
		{"batch overhead", "batch", 0, 87},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := textTestWriter(t, InMemoryOnlyConfig().WithMaxDocumentInputBytes(tc.doc).WithMaxBatchInputBytes(tc.batch))
			if _, err := w.ImportText("s", "body", strings.NewReader("old"), TextChunkOptions{MaxChunkBytes: 8}); err != nil {
				t.Fatal(err)
			}
			result, err := w.ImportText("s", "body", strings.NewReader("abcdefgh"), TextChunkOptions{MaxChunkBytes: 8})
			if tc.scope == "" {
				if err != nil || result.CommittedEndByte != 8 {
					t.Fatalf("boundary: %+v %v", result, err)
				}
			} else {
				var limit *InputLimitError
				if !errors.As(err, &limit) || limit.Scope != tc.scope || limit.Actual != 88 || result.SourceDeleted {
					t.Fatalf("limit: %+v %v", result, err)
				}
				hits := textTestHits(t, w, NewTermQuery("s").SetField(TextChunkSourceField))
				if len(hits) != 1 || hits[0]["body"] != "old" {
					t.Fatal("invalid first chunk deleted old source")
				}
			}
		})
	}
	// Byte-based early flushing applies only to this new import entry point.
	w := textTestWriter(t, InMemoryOnlyConfig().WithMaxBatchInputBytes(90))
	result, err := w.ImportText("s", "body", strings.NewReader("abcdefghijklmnopqrstuvwx"),
		TextChunkOptions{MaxChunkBytes: 8, BatchSize: 8})
	if err != nil || result.CommittedChunks != 3 || result.CommittedEndByte != 24 {
		t.Fatalf("batch byte flushing: %+v %v", result, err)
	}
	w = textTestWriter(t, InMemoryOnlyConfig().WithMaxBatchInputBytes(13))
	result, err = w.ImportText("s", "body", textErrorReader{errors.New("must not read")}, TextChunkOptions{MaxChunkBytes: 8})
	var limit *InputLimitError
	if !errors.As(err, &limit) || limit.Actual != 14 || result.SourceDeleted { // len(_chunk_source) + len(s).
		t.Fatalf("deletion selector limit: %+v %v", result, err)
	}
}

type textErrorReader struct{ err error }

func (r textErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestImportTextFailureProgress(t *testing.T) {
	wantErr := errors.New("second segment failed")
	builds := 0
	config := InMemoryOnlyConfig()
	config.indexConfig = config.indexConfig.WithSegmentPlugin(&index.SegmentPlugin{
		Type: zapxbluge.Type, Version: zapxbluge.Version,
		New: func(docs []segment.Document, norm func(string, int) float32) (segment.Segment, uint64, error) {
			builds++
			if builds == 2 {
				return nil, 0, wantErr
			}
			return zapxbluge.New(docs, norm)
		},
		Load: zapxbluge.Load, Merge: zapxbluge.Merge,
	})
	w := textTestWriter(t, config)
	options := TextChunkOptions{MaxChunkBytes: 8, BatchSize: 2}
	result, err := w.ImportText("s", "body", strings.NewReader(strings.Repeat("x", 32)), options)
	want := TextImportResult{SourceDeleted: true, CommittedChunks: 2, CommittedEndByte: 16}
	if !errors.Is(err, wantErr) || result != want || !strings.Contains(err.Error(), "submitting chunks [2,4), bytes [16,32)") {
		t.Fatalf("batch failure: %+v %v", result, err)
	}
	if len(textTestHits(t, w, NewMatchAllQuery())) != 2 {
		t.Fatal("failed build changed the committed prefix")
	}
	// Retry replaces the incomplete source, including any uncertain failed batch.
	if _, err := w.ImportText("s", "body", strings.NewReader("retry"), options); err != nil {
		t.Fatal(err)
	}
	if len(textTestHits(t, w, NewMatchAllQuery())) != 1 {
		t.Fatal("retry retained old chunks")
	}
	readerErr := errors.New("input failed")
	input := io.MultiReader(strings.NewReader(strings.Repeat("y", 32)), textErrorReader{readerErr})
	result, err = w.ImportText("s", "body", input, options)
	if !errors.Is(err, readerErr) || result != want || len(textTestHits(t, w, NewMatchAllQuery())) != 2 {
		t.Fatalf("read failure or unsubmitted batch was committed: %+v %v", result, err)
	}
	result, err = w.ImportText("s", "body", strings.NewReader("\xffbad"), options)
	if err == nil || result.SourceDeleted || len(textTestHits(t, w, NewMatchAllQuery())) != 2 {
		t.Fatalf("invalid initial UTF-8: %+v %v", result, err)
	}
}

func TestImportTextInvalidOptions(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig())
	for _, options := range []TextChunkOptions{
		{}, {MaxChunkBytes: -1}, {MaxChunkBytes: 3},
		{MaxChunkBytes: int(^uint(0) >> 1)},
		{MaxChunkBytes: 8, OverlapBytes: -1}, {MaxChunkBytes: 8, OverlapBytes: 8},
		{MaxChunkBytes: 8, BatchSize: -1},
	} {
		result, err := w.ImportText("s", "body", textErrorReader{errors.New("must not read")}, options)
		if err == nil || strings.Contains(err.Error(), "must not read") || result.SourceDeleted {
			t.Fatalf("invalid options %+v: %+v %v", options, result, err)
		}
	}
	for _, bad := range []string{"a\xffb", "truncated\xe4\xb8"} {
		r := newTextChunkReader(strings.NewReader(bad), TextChunkOptions{MaxChunkBytes: 4})
		for attempts := 0; ; attempts++ {
			_, err := r.next()
			if err != nil {
				if err == io.EOF {
					t.Fatal("invalid UTF-8 accepted")
				}
				break
			}
			if attempts > len(bad) {
				t.Fatal("invalid UTF-8 did not terminate")
			}
		}
	}
}

func TestImportTextUsesBatchAdmission(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig().WithMaxConcurrentBatches(1).WithBatchStats(true))
	gate := newBatchTestGate(t)
	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- w.Insert(&inputTestDocument{NewDocument("ordinary"), gate.block}) }()
	awaitBatchSignal(t, gate.entered)
	importDone := make(chan error, 1)
	importFinished := make(chan struct{})
	go func() {
		defer close(importFinished)
		_, err := w.ImportText("s", "body", strings.NewReader("text"), TextChunkOptions{MaxChunkBytes: 8})
		importDone <- err
	}()
	assertBatchWaiting(t, importFinished)
	assertBatchStats(t, w, 1, 11)
	gate.open()
	if err := awaitBatchResult(t, ordinaryDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitBatchResult(t, importDone); err != nil {
		t.Fatal(err)
	}
	assertBatchStats(t, w, 0, 0)
}

type gatedTextReader struct {
	gate *batchTestGate
	read io.Reader
}

func (r *gatedTextReader) Read(p []byte) (int, error) {
	if r.gate != nil {
		r.gate.block()
		r.gate = nil
	}
	return r.read.Read(p)
}

func TestImportTextSerializesDeletion(t *testing.T) {
	w := textTestWriter(t, InMemoryOnlyConfig())
	gate := newBatchTestGate(t)
	importDone, deleteDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := w.ImportText("s", "body", &gatedTextReader{gate, strings.NewReader(strings.Repeat("hello ", 10))},
			TextChunkOptions{MaxChunkBytes: 8})
		importDone <- err
	}()
	awaitBatchSignal(t, gate.entered)
	deleteFinished := make(chan struct{})
	go func() {
		defer close(deleteFinished)
		deleteDone <- w.DeleteText("s")
	}()
	assertBatchWaiting(t, deleteFinished)
	gate.open()
	if err := awaitBatchResult(t, importDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitBatchResult(t, deleteDone); err != nil {
		t.Fatal(err)
	}
	if len(textTestHits(t, w, NewMatchAllQuery())) != 0 {
		t.Fatal("import inserted chunks after source deletion returned")
	}
}

type generatedTextReader struct {
	total, read int
	maxRead     int
}

func (r *generatedTextReader) Read(p []byte) (int, error) {
	if r.read == r.total {
		return 0, io.EOF
	}
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	n := len(p)
	if n > r.total-r.read {
		n = r.total - r.read
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
		if (r.read+i)%1024 == 1023 {
			p[i] = '\n'
		}
	}
	r.read += n
	return n, nil
}

func TestImportText50MiBStreaming(t *testing.T) {
	const maximum, batchSize = 64 * 1024, 4
	input := &generatedTextReader{total: 50 * 1024 * 1024}
	config := DefaultConfig(t.TempDir()).WithMaxConcurrentBatches(1).
		WithMaxDocumentInputBytes(maximum + 256).WithMaxBatchInputBytes(batchSize * (maximum + 256))
	built := 0
	config.indexConfig = config.indexConfig.WithSegmentPlugin(&index.SegmentPlugin{
		Type: zapxbluge.Type, Version: zapxbluge.Version,
		New: func(docs []segment.Document, norm func(string, int) float32) (segment.Segment, uint64, error) {
			if len(docs) > batchSize || input.read > (built+len(docs))*maximum+utf8.UTFMax {
				t.Errorf("read/accumulated ahead: read=%d built=%d batch=%d", input.read, built, len(docs))
			}
			if built == 0 && input.read == input.total {
				t.Error("read entire input before first segment")
			}
			built += len(docs)
			return zapxbluge.New(docs, norm)
		},
		Load: zapxbluge.Load, Merge: zapxbluge.Merge,
	})
	w := textTestWriter(t, config)
	result, err := w.ImportText("large", "body", input, TextChunkOptions{MaxChunkBytes: maximum, BatchSize: batchSize})
	if err != nil || result.CommittedEndByte != int64(input.total) || result.CommittedChunks != uint64(input.total/maximum) ||
		input.maxRead > maximum+utf8.UTFMax || built != int(result.CommittedChunks) {
		t.Fatalf("large import: result=%+v reader=%+v built=%d err=%v", result, input, built, err)
	}
}

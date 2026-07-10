// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package zap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	scorchseg "github.com/blevesearch/scorch_segment_api/v2"
	blugeseg "github.com/blugelabs/bluge_segment_api"

	"github.com/fy0/bluge/internal/blugeidx"
)

func TestFieldStatsSurviveMerge(t *testing.T) {
	first := newStatsTestSegment(t,
		newStatsTestDocument("1", "alpha alpha beta"),
		newStatsTestDocument("2", "alpha"))
	second := newStatsTestSegment(t,
		newStatsTestDocument("3", "gamma gamma gamma"))

	tests := []struct {
		name      string
		drops     []*roaring.Bitmap
		wantDocs  uint64
		wantTerms uint64
	}{
		{
			name:      "without drops",
			drops:     []*roaring.Bitmap{nil, nil},
			wantDocs:  3,
			wantTerms: 7,
		},
		{
			name:      "with drops",
			drops:     []*roaring.Bitmap{roaring.BitmapOf(0), nil},
			wantDocs:  2,
			wantTerms: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			_, _, err := MergeToWriter([]scorchseg.Segment{first, second}, test.drops,
				&output, make(chan struct{}))
			if err != nil {
				t.Fatal(err)
			}
			merged, err := LoadBytes(output.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			statsProvider := merged.(interface {
				FieldStats(string) (uint64, uint64, bool)
			})
			documentCount, sumTotalTermFrequency, ok := statsProvider.FieldStats("body")
			if !ok {
				t.Fatal("expected body field statistics")
			}
			if documentCount != test.wantDocs || sumTotalTermFrequency != test.wantTerms {
				t.Fatalf("stats: got docs=%d terms=%d want docs=%d terms=%d",
					documentCount, sumTotalTermFrequency, test.wantDocs, test.wantTerms)
			}
		})
	}
}

func newStatsTestSegment(t *testing.T, documents ...blugeseg.Document) scorchseg.Segment {
	t.Helper()
	buildDocuments := newStatsTestBuildDocuments(t, documents...)
	result, _, err := New(buildDocuments)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func newStatsTestBuildDocuments(t *testing.T,
	documents ...blugeseg.Document) []*blugeidx.Document {
	t.Helper()
	buildDocuments := make([]*blugeidx.Document, len(documents))
	for i, document := range documents {
		var err error
		buildDocuments[i], err = blugeidx.FromSegmentDocument(document)
		if err != nil {
			t.Fatal(err)
		}
	}
	return buildDocuments
}

type statsTestDocument struct {
	id     string
	fields []*statsTestField
}

func newStatsTestDocument(id, body string) *statsTestDocument {
	return &statsTestDocument{
		id: id,
		fields: []*statsTestField{
			newStatsTestField("_id", id, true, true, false),
			newStatsTestField("body", body, true, false, true),
		},
	}
}

func (d *statsTestDocument) Analyze() {}
func (d *statsTestDocument) EachField(visitor blugeseg.VisitField) {
	for _, field := range d.fields {
		visitor(field)
	}
}

type statsTestField struct {
	name      string
	value     []byte
	indexed   bool
	stored    bool
	docValues bool
	length    int
	terms     map[string]*statsTestTerm
}

func newStatsTestField(name, value string, indexed, stored, docValues bool) *statsTestField {
	field := &statsTestField{
		name:      name,
		value:     []byte(value),
		indexed:   indexed,
		stored:    stored,
		docValues: docValues,
		terms:     make(map[string]*statsTestTerm),
	}
	for _, term := range strings.Fields(value) {
		field.length++
		frequency := field.terms[term]
		if frequency == nil {
			frequency = &statsTestTerm{term: []byte(term)}
			field.terms[term] = frequency
		}
		frequency.frequency++
	}
	return field
}

func (f *statsTestField) Name() string         { return f.name }
func (f *statsTestField) Value() []byte        { return f.value }
func (f *statsTestField) Length() int          { return f.length }
func (f *statsTestField) Index() bool          { return f.indexed }
func (f *statsTestField) Store() bool          { return f.stored }
func (f *statsTestField) IndexDocValues() bool { return f.docValues }
func (f *statsTestField) EachTerm(v blugeseg.VisitTerm) {
	for _, term := range f.terms {
		v(term)
	}
}

type statsTestTerm struct {
	term      []byte
	frequency int
}

func (t *statsTestTerm) Term() []byte                        { return t.term }
func (t *statsTestTerm) Frequency() int                      { return t.frequency }
func (t *statsTestTerm) EachLocation(blugeseg.VisitLocation) {}

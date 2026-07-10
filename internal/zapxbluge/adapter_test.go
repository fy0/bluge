// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package zapxbluge

import (
	"testing"

	bleveindex "github.com/blevesearch/bleve_index_api"
	blugeseg "github.com/blugelabs/bluge_segment_api"

	"github.com/blugelabs/bluge/analysis"
	"github.com/blugelabs/bluge/internal/blugeidx"
)

type exporterTestDocument struct {
	native   *blugeidx.Document
	exported bool
}

func (*exporterTestDocument) Analyze() {}

func (*exporterTestDocument) EachField(blugeseg.VisitField) {
	panic("native exporter should bypass segment field iteration")
}

func (d *exporterTestDocument) ToBlugeIndexDocument() (*blugeidx.Document, error) {
	d.exported = true
	return d.native, nil
}

func TestNewPrefersNativeDocumentExporter(t *testing.T) {
	idFrequencies, _ := analysis.TokenFrequency(analysis.TokenStream{
		&analysis.Token{Term: []byte("1"), PositionIncr: 1},
	}, false, 0)
	bodyFrequencies, _ := analysis.TokenFrequency(analysis.TokenStream{
		&analysis.Token{Term: []byte("alpha"), PositionIncr: 1},
	}, false, 0)
	native, err := blugeidx.NewDocument([]*blugeidx.Field{
		blugeidx.NewField("_id", []byte("1"),
			bleveindex.IndexField|bleveindex.StoreField|bleveindex.DocValues,
			1, idFrequencies, 1),
		blugeidx.NewField("body", []byte("alpha"), bleveindex.IndexField,
			1, bodyFrequencies, 5),
	})
	if err != nil {
		t.Fatal(err)
	}
	document := &exporterTestDocument{native: native}
	segment, _, err := New([]blugeseg.Document{document}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !document.exported {
		t.Fatal("expected native exporter to be used")
	}
	if segment.Count() != 1 {
		t.Fatalf("expected one document, got %d", segment.Count())
	}
}

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

	"github.com/RoaringBitmap/roaring/v2"
	bleveindex "github.com/blevesearch/bleve_index_api"
	scorchseg "github.com/blevesearch/scorch_segment_api/v2"
	blugeseg "github.com/fy0/bluge/segment"

	"github.com/fy0/bluge/analysis"
	"github.com/fy0/bluge/internal/blugeidx"
)

type optimizablePostingsIteratorStub struct {
	actual *roaring.Bitmap
}

func (*optimizablePostingsIteratorStub) Next() (scorchseg.Posting, error) {
	return nil, nil
}

func (*optimizablePostingsIteratorStub) Advance(uint64) (scorchseg.Posting, error) {
	return nil, nil
}

func (*optimizablePostingsIteratorStub) Size() int             { return 0 }
func (*optimizablePostingsIteratorStub) BytesRead() uint64     { return 0 }
func (*optimizablePostingsIteratorStub) ResetBytesRead(uint64) {}
func (*optimizablePostingsIteratorStub) BytesWritten() uint64  { return 0 }
func (s *optimizablePostingsIteratorStub) ActualBitmap() *roaring.Bitmap {
	return s.actual
}
func (*optimizablePostingsIteratorStub) DocNum1Hit() (uint64, bool) { return 0, false }
func (s *optimizablePostingsIteratorStub) ReplaceActual(actual *roaring.Bitmap) {
	s.actual = actual
}

func TestBitmapAdaptersPassV2BitmapsThrough(t *testing.T) {
	initial := roaring.BitmapOf(1, 3, 5)
	inner := &optimizablePostingsIteratorStub{actual: initial}
	adapter := &postingsIteratorAdapter{inner: inner}
	if adapter.ActualBitmap() != initial {
		t.Fatal("ActualBitmap did not return the inner v2 bitmap")
	}

	replacement := roaring.BitmapOf(2, 4, 6)
	adapter.ReplaceActual(replacement)
	if inner.actual != replacement {
		t.Fatal("ReplaceActual did not pass the v2 bitmap through")
	}

	merged := Merge(nil, []*roaring.Bitmap{replacement}, 0).(*merger)
	if merged.drops[0] != replacement {
		t.Fatal("Merge did not pass the v2 drop bitmap through")
	}
}

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

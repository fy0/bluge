// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package zap

import (
	"testing"

	bleveindex "github.com/blevesearch/bleve_index_api"

	"github.com/fy0/bluge/analysis"
	"github.com/fy0/bluge/internal/blugeidx"
	"github.com/fy0/bluge/numeric"
)

func TestCanonicalNumericDocValues(t *testing.T) {
	shift0 := numeric.MustNewPrefixCodedInt64(42, 0)
	shift4 := numeric.MustNewPrefixCodedInt64(42, 4)
	tokens := analysis.TokenStream{
		{Term: shift0, PositionIncr: 1, Type: analysis.Numeric},
		{Term: shift4, Type: analysis.Numeric},
	}
	freqs, _ := analysis.TokenFrequency(tokens, false, 0)

	idFreqs, _ := analysis.TokenFrequency(analysis.TokenStream{
		{Term: []byte("1"), PositionIncr: 1},
	}, false, 0)
	id := blugeidx.NewField("_id", []byte("1"),
		bleveindex.IndexField|bleveindex.StoreField, 1, idFreqs, 1)
	canonical := blugeidx.NewField("price", shift0,
		bleveindex.IndexField|bleveindex.DocValues, 2, freqs, 8)
	canonical.SetCanonicalDocValues()
	raw := blugeidx.NewField("raw", shift0,
		bleveindex.IndexField|bleveindex.DocValues, 2, freqs, 8)
	doc, err := blugeidx.NewDocument([]*blugeidx.Field{id, canonical, raw})
	if err != nil {
		t.Fatal(err)
	}

	segment, _, err := New([]*blugeidx.Document{doc})
	if err != nil {
		t.Fatal(err)
	}
	sb := segment.(*SegmentBase)
	counts := map[string]int{}
	_, err = sb.VisitDocValues(0, []string{"price", "raw"}, func(field string, _ []byte) {
		counts[field]++
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if counts["price"] != 1 {
		t.Fatalf("expected one canonical price docvalue, got %d", counts["price"])
	}
	if counts["raw"] != 2 {
		t.Fatalf("expected both raw docvalues, got %d", counts["raw"])
	}
}

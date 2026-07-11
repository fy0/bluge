// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"testing"

	"github.com/fy0/bluge/internal/blugeidx"
)

func TestNumericFieldsMarkCanonicalDocValues(t *testing.T) {
	doc := NewDocument("1").
		AddField(NewNumericField("price", 42)).
		AddField(NewKeywordField("raw", "value").Sortable())
	doc.Analyze()

	indexDoc, err := doc.ToBlugeIndexDocument()
	if err != nil {
		t.Fatal(err)
	}
	canonical := map[string]bool{}
	indexDoc.VisitFields(func(field *blugeidx.Field) {
		canonical[field.Name()] = field.CanonicalDocValues()
	})
	if !canonical["price"] {
		t.Fatal("expected numeric field to use canonical docvalues")
	}
	if canonical["raw"] {
		t.Fatal("keyword field must retain all docvalues")
	}
}

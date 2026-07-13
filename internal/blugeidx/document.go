// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package blugeidx

import (
	"fmt"

	bleveindex "github.com/blevesearch/bleve_index_api"
	blugeseg "github.com/fy0/bluge/segment"

	"github.com/fy0/bluge/analysis"
)

const (
	TextFieldType byte = 't'
	IDFieldName        = "_id"
)

// Document is the build representation consumed by zapxtext after Bluge has
// analyzed the source document.
type Document struct {
	fields             []*Field
	numPlainTextBytes  uint64
	storedFieldsBytes  uint64
	indexedFieldExists bool
}

// FromSegmentDocument preserves native Bluge token frequencies when the field
// exposes them and falls back to the segment API for custom fields.
func FromSegmentDocument(doc blugeseg.Document) (*Document, error) {
	if doc == nil {
		return nil, fmt.Errorf("nil document")
	}

	var fields []*Field
	var conversionErr error
	doc.EachField(func(field blugeseg.Field) {
		if conversionErr != nil {
			return
		}
		if field == nil {
			conversionErr = fmt.Errorf("nil field")
			return
		}

		fields = append(fields, newField(field))
	})
	if conversionErr != nil {
		return nil, conversionErr
	}
	return NewDocument(fields)
}

// NewDocument creates a native build document from already analyzed fields.
func NewDocument(fields []*Field) (*Document, error) {
	rv := &Document{fields: fields}
	var idStored bool
	for _, field := range fields {
		if field == nil {
			return nil, fmt.Errorf("nil field")
		}
		if field.name == IDFieldName {
			field.options |= bleveindex.IndexField | bleveindex.StoreField
			idStored = true
		}
		rv.numPlainTextBytes += field.numPlainTextBytes
		if field.options.IsStored() {
			rv.storedFieldsBytes += uint64(len(field.value))
		}
		if field.options.IsIndexed() {
			rv.indexedFieldExists = true
		}
	}
	if !idStored {
		return nil, fmt.Errorf("document missing _id field")
	}
	return rv, nil
}

func (d *Document) VisitFields(visitor func(*Field)) {
	for _, field := range d.fields {
		visitor(field)
	}
}

func (d *Document) NumPlainTextBytes() uint64 { return d.numPlainTextBytes }
func (d *Document) StoredFieldsBytes() uint64 { return d.storedFieldsBytes }
func (d *Document) Indexed() bool             { return d.indexedFieldExists }

type analyzedField interface {
	AnalyzedTokenFrequencies() analysis.TokenFrequencies
}

type locationAwareField interface {
	IncludeLocations() bool
}

type plainTextSizedField interface {
	NumPlainTextBytes() int
}

// Field retains the source field for compatibility while providing direct
// access to Bluge's native token map on the common path.
type Field struct {
	name               string
	value              []byte
	options            bleveindex.FieldIndexingOptions
	analyzedLength     int
	analyzedTokenFreqs analysis.TokenFrequencies
	hasNativeTokenFreq bool
	numPlainTextBytes  uint64
	canonicalDocValues bool
	source             blugeseg.Field
}

// NewField creates a native field using an existing analyzed token map.
func NewField(name string, value []byte, options bleveindex.FieldIndexingOptions,
	analyzedLength int, tokenFreqs analysis.TokenFrequencies, numPlainTextBytes uint64) *Field {
	return &Field{
		name:               name,
		value:              value,
		options:            options,
		analyzedLength:     analyzedLength,
		analyzedTokenFreqs: tokenFreqs,
		hasNativeTokenFreq: true,
		numPlainTextBytes:  numPlainTextBytes,
	}
}

func newField(field blugeseg.Field) *Field {
	rv := &Field{
		name:              field.Name(),
		value:             field.Value(),
		analyzedLength:    field.Length(),
		numPlainTextBytes: uint64(len(field.Value())),
		source:            field,
	}
	if field.Index() {
		rv.options |= bleveindex.IndexField
	}
	if field.Store() {
		rv.options |= bleveindex.StoreField
	}
	if field.IndexDocValues() {
		rv.options |= bleveindex.DocValues
	}
	if native, ok := field.(analyzedField); ok {
		rv.analyzedTokenFreqs = native.AnalyzedTokenFrequencies()
		rv.hasNativeTokenFreq = true
	}
	if sized, ok := field.(plainTextSizedField); ok {
		rv.numPlainTextBytes = uint64(sized.NumPlainTextBytes())
	}
	if withLocations, ok := field.(locationAwareField); ok {
		if withLocations.IncludeLocations() {
			rv.options |= bleveindex.IncludeTermVectors
		}
	} else if fieldHasLocations(field) {
		rv.options |= bleveindex.IncludeTermVectors
	}
	return rv
}

func fieldHasLocations(field blugeseg.Field) bool {
	var found bool
	field.EachTerm(func(term blugeseg.FieldTerm) {
		if found {
			return
		}
		term.EachLocation(func(blugeseg.Location) {
			found = true
		})
	})
	return found
}

func (f *Field) Name() string                             { return f.name }
func (f *Field) Value() []byte                            { return f.value }
func (f *Field) ArrayPositions() []uint64                 { return nil }
func (f *Field) EncodedFieldType() byte                   { return TextFieldType }
func (f *Field) Options() bleveindex.FieldIndexingOptions { return f.options }
func (f *Field) AnalyzedLength() int                      { return f.analyzedLength }
func (f *Field) NumPlainTextBytes() uint64                { return f.numPlainTextBytes }
func (f *Field) NativeTokenFrequencies() (analysis.TokenFrequencies, bool) {
	return f.analyzedTokenFreqs, f.hasNativeTokenFreq
}

func (f *Field) SetCanonicalDocValues() { f.canonicalDocValues = true }
func (f *Field) CanonicalDocValues() bool {
	return f.canonicalDocValues
}

func (f *Field) EachTerm(visitor blugeseg.VisitTerm) {
	if f.hasNativeTokenFreq {
		for _, term := range f.analyzedTokenFreqs {
			visitor(term)
		}
		return
	}
	f.source.EachTerm(visitor)
}

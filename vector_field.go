// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"github.com/fy0/bluge/analysis"
	segment "github.com/fy0/bluge/segment"
)

// VectorField stores an embedding on a document without adding it to the
// text segment. A configured VectorBackend receives it from Writer.
type VectorField struct {
	name       string
	vector     []float32
	similarity VectorSimilarity
}

// NewVectorField creates a cosine-similarity vector field.
func NewVectorField(name string, vector []float32) *VectorField {
	return NewVectorFieldWithSimilarity(name, vector, VectorCosine)
}

// NewVectorFieldWithSimilarity creates a vector field using the requested
// scoring function. The vector is copied so later caller mutations cannot
// change a pending document.
func NewVectorFieldWithSimilarity(name string, vector []float32,
	similarity VectorSimilarity) *VectorField {
	return &VectorField{
		name:       name,
		vector:     append([]float32(nil), vector...),
		similarity: similarity,
	}
}

func (f *VectorField) Name() string               { return f.name }
func (f *VectorField) Length() int                { return 0 }
func (f *VectorField) Value() []byte              { return nil }
func (f *VectorField) Index() bool                { return false }
func (f *VectorField) Store() bool                { return false }
func (f *VectorField) IndexDocValues() bool       { return false }
func (f *VectorField) EachTerm(segment.VisitTerm) {}
func (f *VectorField) Analyze(int) int            { return 0 }
func (f *VectorField) AnalyzedTokenFrequencies() analysis.TokenFrequencies {
	return nil
}
func (f *VectorField) PositionIncrementGap() int { return 0 }
func (f *VectorField) Size() int {
	return sizeOfString + len(f.name) + sizeOfSlice + len(f.vector)*4
}

// VectorValue returns a copy of the embedding associated with the field.
func (f *VectorField) VectorValue() []float32 {
	return append([]float32(nil), f.vector...)
}

// VectorSimilarity returns the score function associated with the field.
func (f *VectorField) VectorSimilarity() VectorSimilarity { return f.similarity }

// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	flatVectorMagic   = "BLUGEVEC"
	flatVectorVersion = uint32(1)
	maxVectorBlobSize = 64 << 20
)

func checkedVectorUint32(value int, label string) (uint32, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s cannot be negative: %d", label, value)
	}
	unsigned := uint64(value)
	if unsigned > math.MaxUint32 {
		return 0, fmt.Errorf("%s exceeds uint32: %d", label, value)
	}
	return uint32(unsigned), nil
}

func checkedVectorUint32ToInt(value uint32, label string) (int, error) {
	if uint64(value) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%s exceeds platform limits: %d", label, value)
	}
	return int(value), nil
}

// FlatVectorBackend is the cgo-free exact-search backend. A non-empty path
// persists the vector sidecar; an empty path keeps the index in memory.
//
// It is intentionally exact. That makes it useful as the correctness and
// recall baseline for a future FAISS, Rust, or WASM backend.
type FlatVectorBackend struct {
	path   string
	mu     sync.Mutex
	memory *flatVectorIndex
}

// NewFlatVectorBackend creates an exact vector backend. The sidecar is
// independent from the Bluge text snapshot and should normally live next to
// the configured index directory.
func NewFlatVectorBackend(path string) *FlatVectorBackend {
	return &FlatVectorBackend{path: path}
}

func (b *FlatVectorBackend) Name() string { return "flat" }

func (b *FlatVectorBackend) Open(_ Config) (VectorIndex, error) {
	if b.path == "" {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.memory == nil {
			b.memory = newFlatVectorIndex("")
		}
		return b.memory, nil
	}
	return loadFlatVectorIndex(b.path)
}

type flatVectorRecord struct {
	vector     []float32
	similarity VectorSimilarity
}

type flatVectorIndex struct {
	mu      sync.RWMutex
	path    string
	vectors map[string]map[Identifier]flatVectorRecord
	specs   map[string]VectorFieldSpec
}

func newFlatVectorIndex(path string) *flatVectorIndex {
	return &flatVectorIndex{
		path:    path,
		vectors: make(map[string]map[Identifier]flatVectorRecord),
		specs:   make(map[string]VectorFieldSpec),
	}
}

func loadFlatVectorIndex(path string) (*flatVectorIndex, error) {
	index := newFlatVectorIndex(path)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return index, nil
	}
	if err != nil {
		return nil, err
	}
	if err := index.decode(data); err != nil {
		return nil, fmt.Errorf("decode vector sidecar %q: %w", path, err)
	}
	return index, nil
}

func (f *flatVectorIndex) Close() error { return nil }

func (f *flatVectorIndex) SnapshotVectorIndex() VectorIndex {
	f.mu.RLock()
	defer f.mu.RUnlock()
	vectors, specs := cloneFlatVectorData(f.vectors, f.specs)
	return &flatVectorIndex{
		vectors: vectors,
		specs:   specs,
	}
}

func (f *flatVectorIndex) ValidateVectorChanges(changes []VectorChange) error {
	if len(changes) == 0 {
		return nil
	}
	f.mu.RLock()
	nextVectors, nextSpecs := cloneFlatVectorData(f.vectors, f.specs)
	f.mu.RUnlock()
	return applyFlatVectorChanges(nextVectors, nextSpecs, changes)
}

func (f *flatVectorIndex) ApplyVectorChanges(changes []VectorChange) error {
	if len(changes) == 0 {
		return nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	nextVectors, nextSpecs := cloneFlatVectorData(f.vectors, f.specs)
	if err := applyFlatVectorChanges(nextVectors, nextSpecs, changes); err != nil {
		return err
	}

	if err := f.persist(nextVectors, nextSpecs); err != nil {
		return err
	}
	f.vectors = nextVectors
	f.specs = nextSpecs
	return nil
}

func applyFlatVectorChanges(nextVectors map[string]map[Identifier]flatVectorRecord,
	nextSpecs map[string]VectorFieldSpec, changes []VectorChange) error {
	for _, change := range changes {
		if change.Delete {
			if change.Field == "" {
				for field, records := range nextVectors {
					delete(records, change.ID)
					if len(records) == 0 {
						delete(nextVectors, field)
					}
				}
			} else if records := nextVectors[change.Field]; records != nil {
				delete(records, change.ID)
				if len(records) == 0 {
					delete(nextVectors, change.Field)
				}
			}
			continue
		}

		if err := validateVectorChange(change); err != nil {
			return err
		}
		spec, ok := nextSpecs[change.Field]
		if ok {
			if spec.Dims != len(change.Vector) {
				return fmt.Errorf("%w: field %q has %d dimensions, got %d",
					ErrVectorInvalidDimension, change.Field, spec.Dims, len(change.Vector))
			}
			if spec.Similarity != change.Similarity {
				return fmt.Errorf("field %q changes similarity from %q to %q",
					change.Field, spec.Similarity, change.Similarity)
			}
		} else {
			nextSpecs[change.Field] = VectorFieldSpec{
				Name:       change.Field,
				Dims:       len(change.Vector),
				Similarity: change.Similarity,
			}
		}

		records := nextVectors[change.Field]
		if records == nil {
			records = make(map[Identifier]flatVectorRecord)
			nextVectors[change.Field] = records
		}
		records[change.ID] = flatVectorRecord{
			vector:     append([]float32(nil), change.Vector...),
			similarity: change.Similarity,
		}
	}
	return nil
}

func validateVectorChange(change VectorChange) error {
	if change.Field == "" {
		return ErrVectorInvalidField
	}
	if len(change.Vector) == 0 {
		return ErrVectorInvalidDimension
	}
	if !validVectorSimilarity(change.Similarity) {
		return fmt.Errorf("%w: unsupported similarity %q", ErrVectorInvalidField, change.Similarity)
	}
	for _, value := range change.Vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return ErrVectorInvalidValue
		}
	}
	return nil
}

func validVectorSimilarity(similarity VectorSimilarity) bool {
	return similarity == VectorL2 || similarity == VectorDot || similarity == VectorCosine
}

func (f *flatVectorIndex) Search(field string, query []float32, k int, filter Query) ([]VectorHit, error) {
	if filter != nil {
		return nil, ErrVectorFilterUnsupported
	}
	return f.search(field, query, k, nil)
}

func (f *flatVectorIndex) SearchCandidates(field string, query []float32, k int,
	allowed map[Identifier]struct{}) ([]VectorHit, error) {
	return f.search(field, query, k, allowed)
}

func (f *flatVectorIndex) search(field string, query []float32, k int,
	allowed map[Identifier]struct{}) ([]VectorHit, error) {
	if k <= 0 {
		return nil, ErrVectorInvalidK
	}
	if len(query) == 0 {
		return nil, ErrVectorInvalidDimension
	}
	for _, value := range query {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, ErrVectorInvalidValue
		}
	}

	f.mu.RLock()
	spec, ok := f.specs[field]
	records := f.vectors[field]
	if !ok || len(records) == 0 {
		f.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrVectorFieldNotFound, field)
	}
	if spec.Dims != len(query) {
		f.mu.RUnlock()
		return nil, fmt.Errorf("%w: field %q has %d dimensions, got %d",
			ErrVectorInvalidDimension, field, spec.Dims, len(query))
	}

	hits := make([]VectorHit, 0, len(records))
	for id, record := range records {
		if allowed != nil {
			if _, ok := allowed[id]; !ok {
				continue
			}
		}
		hits = append(hits, VectorHit{
			ID:    id,
			Score: vectorScore(spec.Similarity, query, record.vector),
		})
	}
	f.mu.RUnlock()

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

func vectorScore(similarity VectorSimilarity, query, vector []float32) float64 {
	switch similarity {
	case VectorL2:
		var distance float64
		for i, value := range query {
			delta := float64(value) - float64(vector[i])
			distance += delta * delta
		}
		// Higher scores are better in Bluge; FAISS's lower L2 distance is
		// represented as a negative squared distance.
		return -distance
	case VectorDot:
		var score float64
		for i, value := range query {
			score += float64(value) * float64(vector[i])
		}
		return score
	case VectorCosine:
		var dot, queryNorm, vectorNorm float64
		for i, value := range query {
			other := float64(vector[i])
			x := float64(value)
			dot += x * other
			queryNorm += x * x
			vectorNorm += other * other
		}
		if queryNorm == 0 || vectorNorm == 0 {
			return 0
		}
		return dot / math.Sqrt(queryNorm*vectorNorm)
	default:
		return math.Inf(-1)
	}
}

func cloneFlatVectorData(vectors map[string]map[Identifier]flatVectorRecord,
	specs map[string]VectorFieldSpec) (map[string]map[Identifier]flatVectorRecord,
	map[string]VectorFieldSpec) {
	nextVectors := make(map[string]map[Identifier]flatVectorRecord, len(vectors))
	for field, records := range vectors {
		nextRecords := make(map[Identifier]flatVectorRecord, len(records))
		for id, record := range records {
			nextRecords[id] = flatVectorRecord{
				vector:     append([]float32(nil), record.vector...),
				similarity: record.similarity,
			}
		}
		nextVectors[field] = nextRecords
	}
	nextSpecs := make(map[string]VectorFieldSpec, len(specs))
	for field, spec := range specs {
		nextSpecs[field] = spec
	}
	return nextVectors, nextSpecs
}

func (f *flatVectorIndex) persist(vectors map[string]map[Identifier]flatVectorRecord,
	specs map[string]VectorFieldSpec) error {
	if f.path == "" {
		return nil
	}
	data, err := encodeFlatVectorData(vectors, specs)
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bluge-vectors-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, f.path); err != nil {
		// os.Rename cannot replace an existing file on Windows.
		if removeErr := os.Remove(f.path); removeErr != nil && !os.IsNotExist(removeErr) {
			return err
		}
		if err = os.Rename(tmpName, f.path); err != nil {
			return err
		}
	}
	return nil
}

func encodeFlatVectorData(vectors map[string]map[Identifier]flatVectorRecord,
	specs map[string]VectorFieldSpec) ([]byte, error) {
	fields := make([]string, 0, len(vectors))
	for field := range vectors {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	var buf bytes.Buffer
	buf.WriteString(flatVectorMagic)
	if err := binary.Write(&buf, binary.LittleEndian, flatVectorVersion); err != nil {
		return nil, err
	}
	fieldCount, err := checkedVectorUint32(len(fields), "vector field count")
	if err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, fieldCount); err != nil {
		return nil, err
	}
	for _, field := range fields {
		spec := specs[field]
		if err := writeVectorString(&buf, field); err != nil {
			return nil, err
		}
		dimensions, err := checkedVectorUint32(spec.Dims, "vector dimensions")
		if err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.LittleEndian, dimensions); err != nil {
			return nil, err
		}
		if err := writeVectorString(&buf, string(spec.Similarity)); err != nil {
			return nil, err
		}

		ids := make([]string, 0, len(vectors[field]))
		for id := range vectors[field] {
			ids = append(ids, string(id))
		}
		sort.Strings(ids)
		if err := binary.Write(&buf, binary.LittleEndian, uint64(len(ids))); err != nil {
			return nil, err
		}
		for _, rawID := range ids {
			if err := writeVectorString(&buf, rawID); err != nil {
				return nil, err
			}
			for _, value := range vectors[field][Identifier(rawID)].vector {
				if err := binary.Write(&buf, binary.LittleEndian, math.Float32bits(value)); err != nil {
					return nil, err
				}
			}
		}
	}
	return buf.Bytes(), nil
}

func writeVectorString(w io.Writer, value string) error {
	length, err := checkedVectorUint32(len(value), "vector string length")
	if err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, length); err != nil {
		return err
	}
	_, err = io.WriteString(w, value)
	return err
}

func (f *flatVectorIndex) decode(data []byte) error {
	reader := bytes.NewReader(data)
	magic := make([]byte, len(flatVectorMagic))
	if _, err := io.ReadFull(reader, magic); err != nil {
		return err
	}
	if string(magic) != flatVectorMagic {
		return fmt.Errorf("invalid magic")
	}
	var version, fieldCount uint32
	if err := binary.Read(reader, binary.LittleEndian, &version); err != nil {
		return err
	}
	if version != flatVectorVersion {
		return fmt.Errorf("unsupported version %d", version)
	}
	if err := binary.Read(reader, binary.LittleEndian, &fieldCount); err != nil {
		return err
	}
	fieldCountValue, err := checkedVectorUint32ToInt(fieldCount, "vector field count")
	if err != nil {
		return err
	}
	if fieldCountValue > len(data) {
		return fmt.Errorf("invalid field count %d", fieldCount)
	}
	for i := uint32(0); i < fieldCount; i++ {
		field, err := readVectorString(reader)
		if err != nil {
			return err
		}
		var dims uint32
		if err := binary.Read(reader, binary.LittleEndian, &dims); err != nil {
			return err
		}
		rawSimilarity, err := readVectorString(reader)
		if err != nil {
			return err
		}
		similarity := VectorSimilarity(rawSimilarity)
		if dims == 0 || !validVectorSimilarity(similarity) {
			return fmt.Errorf("invalid vector spec for field %q", field)
		}
		if uint64(dims) > uint64(maxVectorBlobSize/4) ||
			uint64(dims) > uint64(^uint(0)>>1) {
			return fmt.Errorf("invalid dimensions %d for field %q", dims, field)
		}
		var recordCount uint64
		if err := binary.Read(reader, binary.LittleEndian, &recordCount); err != nil {
			return err
		}
		minRecordSize := uint64(4) + uint64(dims)*4
		remaining := uint64(reader.Len()) //nolint:gosec // bytes.Reader.Len is always non-negative.
		if minRecordSize == 0 || recordCount > remaining/minRecordSize {
			return fmt.Errorf("invalid record count for field %q", field)
		}
		records := make(map[Identifier]flatVectorRecord, int(recordCount))
		for j := uint64(0); j < recordCount; j++ {
			id, err := readVectorString(reader)
			if err != nil {
				return err
			}
			vector := make([]float32, int(dims))
			for k := range vector {
				var bits uint32
				if err := binary.Read(reader, binary.LittleEndian, &bits); err != nil {
					return err
				}
				vector[k] = math.Float32frombits(bits)
				if math.IsNaN(float64(vector[k])) || math.IsInf(float64(vector[k]), 0) {
					return fmt.Errorf("invalid vector value for field %q", field)
				}
			}
			records[Identifier(id)] = flatVectorRecord{
				vector:     vector,
				similarity: similarity,
			}
		}
		f.specs[field] = VectorFieldSpec{
			Name:       field,
			Dims:       int(dims),
			Similarity: similarity,
		}
		f.vectors[field] = records
	}
	if reader.Len() != 0 {
		return fmt.Errorf("trailing bytes in vector sidecar")
	}
	return nil
}

func readVectorString(reader *bytes.Reader) (string, error) {
	var length uint32
	if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
		return "", err
	}
	if length > maxVectorBlobSize {
		return "", fmt.Errorf("invalid vector string length %d", length)
	}
	size := int(length)
	if size > reader.Len() {
		return "", fmt.Errorf("invalid vector string length %d", length)
	}
	value := make([]byte, size)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return string(value), nil
}

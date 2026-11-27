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
	"maps"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/fy0/bluge/internal/fsatomic"
)

const (
	vectorBackendFlatName = "flat"
	flatVectorMagic       = "BLUGEVEC"
	flatVectorVersion     = uint32(1)
	maxVectorBlobSize     = 64 << 20
)

type FlatVectorBackend struct {
	path   string
	mu     sync.Mutex
	memory *flatVectorIndex
}

func NewFlatVectorBackend(path string) *FlatVectorBackend {
	return &FlatVectorBackend{path: path}
}

func (b *FlatVectorBackend) Name() string { return vectorBackendFlatName }

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
	return &flatVectorIndex{vectors: f.vectors, specs: f.specs}
}

func (f *flatVectorIndex) vectorFieldSpecs() map[string]VectorFieldSpec {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return maps.Clone(f.specs)
}

func (f *flatVectorIndex) ApplyVectorChanges(changes []VectorChange) error {
	if err := validatePublicVectorChanges(changes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := mergeVectorFieldSpecs(changes, f.specs); err != nil {
		return err
	}
	return f.applyVectorBatchLocked(changes, func() error { return nil })
}

func (f *flatVectorIndex) applyVectorBatch(changes []VectorChange, commit func() error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyVectorBatchLocked(changes, commit)
}

func (f *flatVectorIndex) applyVectorBatchLocked(changes []VectorChange, commit func() error) error {
	nextSpecs, nextVectors := maps.Clone(f.specs), maps.Clone(f.vectors)
	applyFlatVectorChanges(nextVectors, nextSpecs, changes)
	if err := commit(); err != nil {
		return err
	}
	if err := f.persist(nextVectors, nextSpecs); err != nil {
		return err
	}
	f.vectors, f.specs = nextVectors, nextSpecs
	return nil
}

func applyFlatVectorChanges(vectors map[string]map[Identifier]flatVectorRecord,
	specs map[string]VectorFieldSpec, changes []VectorChange) {
	copied := make(map[string]bool)
	for _, change := range changes {
		if change.Delete {
			for field := range vectors {
				if change.Field == "" || change.Field == field {
					records := writableFlatRecords(vectors, copied, field)
					delete(records, change.ID)
					if len(records) == 0 {
						delete(vectors, field)
					}
				}
			}
			continue
		}
		specs[change.Field] = VectorFieldSpec{
			Name: change.Field, Dims: len(change.Vector), Similarity: change.Similarity,
		}
		records := writableFlatRecords(vectors, copied, change.Field)
		records[change.ID] = flatVectorRecord{
			vector: append([]float32(nil), change.Vector...), similarity: change.Similarity,
		}
	}
}

func writableFlatRecords(vectors map[string]map[Identifier]flatVectorRecord,
	copied map[string]bool, field string) map[Identifier]flatVectorRecord {
	records := vectors[field]
	if !copied[field] {
		records = maps.Clone(records)
		copied[field] = true
	}
	if records == nil {
		records = make(map[Identifier]flatVectorRecord)
	}
	vectors[field] = records
	return records
}

func validatePublicVectorChanges(changes []VectorChange) error {
	for _, change := range changes {
		if change.ID == "" {
			return fmt.Errorf("empty vector ID")
		}
		if !change.Delete {
			if err := validateVectorChange(change); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateVectorChange(change VectorChange) error {
	if change.Field == "" {
		return ErrVectorInvalidField
	}
	if len(change.Vector) == 0 || uint64(len(change.Vector)) > math.MaxUint32 {
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
	return f.SearchCandidates(field, query, k, nil)
}

func (f *flatVectorIndex) SearchCandidates(field string, query []float32, k int,
	allowed map[Identifier]struct{}) ([]VectorHit, error) {
	if err := validateVectorQuery(query, k); err != nil {
		return nil, err
	}
	return f.searchVectorCandidates(field, query, k, allowed)
}

func (f *flatVectorIndex) searchVectorCandidates(field string, query []float32, k int,
	allowed map[Identifier]struct{}) ([]VectorHit, error) {
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
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return fsatomic.Replace(tmpName, f.path)
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
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(fields))); err != nil {
		return nil, err
	}
	for _, field := range fields {
		spec := specs[field]
		if err := writeVectorString(&buf, field); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.LittleEndian, uint32(spec.Dims)); err != nil {
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
			if err := binary.Write(&buf, binary.LittleEndian, vectors[field][Identifier(rawID)].vector); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}

func writeVectorString(w io.Writer, value string) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(value))); err != nil {
		return err
	}
	_, err := io.WriteString(w, value)
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
	if int(fieldCount) > len(data) {
		return fmt.Errorf("invalid field count %d", fieldCount)
	}
	for i := uint32(0); i < fieldCount; i++ {
		if err := f.decodeField(reader); err != nil {
			return err
		}
	}
	if reader.Len() != 0 {
		return fmt.Errorf("trailing bytes in vector sidecar")
	}
	return nil
}

func (f *flatVectorIndex) decodeField(reader *bytes.Reader) error {
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
	if dims > maxVectorBlobSize/4 {
		return fmt.Errorf("invalid dimensions %d for field %q", dims, field)
	}
	var recordCount uint64
	if err := binary.Read(reader, binary.LittleEndian, &recordCount); err != nil {
		return err
	}
	minRecordSize := uint64(4) + uint64(dims)*4
	remaining := uint64(reader.Len())
	if recordCount > remaining/minRecordSize {
		return fmt.Errorf("invalid record count for field %q", field)
	}
	records := make(map[Identifier]flatVectorRecord, recordCount)
	for j := uint64(0); j < recordCount; j++ {
		id, err := readVectorString(reader)
		if err != nil {
			return err
		}
		vector := make([]float32, int(dims))
		if err := binary.Read(reader, binary.LittleEndian, vector); err != nil {
			return err
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

func validateVectorQuery(query []float32, k int) error {
	if k <= 0 {
		return ErrVectorInvalidK
	}
	return validateVectorValues(query)
}

func validateVectorValues(query []float32) error {
	for _, value := range query {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return ErrVectorInvalidValue
		}
	}
	return nil
}

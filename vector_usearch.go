// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/fy0/bluge/internal/fsatomic"
)

const usearchManifestVersion = uint32(1)

// USearchVectorOptions controls the HNSW index created for new vector fields.
// Zero values use the USearch defaults selected by this adapter.
type USearchVectorOptions struct {
	Connectivity    int
	ExpansionAdd    int
	ExpansionSearch int
}

type USearchHardwareInfo struct {
	Compiled  string
	Available string
}

func defaultUSearchVectorOptions() USearchVectorOptions {
	return USearchVectorOptions{
		Connectivity:    16,
		ExpansionAdd:    128,
		ExpansionSearch: 64,
	}
}

func (o USearchVectorOptions) normalized() (USearchVectorOptions, error) {
	defaults := defaultUSearchVectorOptions()
	if o.Connectivity == 0 {
		o.Connectivity = defaults.Connectivity
	}
	if o.ExpansionAdd == 0 {
		o.ExpansionAdd = defaults.ExpansionAdd
	}
	if o.ExpansionSearch == 0 {
		o.ExpansionSearch = defaults.ExpansionSearch
	}
	if o.Connectivity < 2 || o.ExpansionAdd < 1 || o.ExpansionSearch < 1 {
		return USearchVectorOptions{}, fmt.Errorf("usearch HNSW options must be positive")
	}
	return o, nil
}

type USearchVectorBackend struct {
	path        string
	libraryPath string
	options     USearchVectorOptions
}

func NewUSearchVectorBackend(path string) *USearchVectorBackend {
	return &USearchVectorBackend{
		path:    path,
		options: defaultUSearchVectorOptions(),
	}
}

func NewUSearchVectorBackendWithLibrary(path, libraryPath string) *USearchVectorBackend {
	backend := NewUSearchVectorBackend(path)
	backend.libraryPath = libraryPath
	return backend
}

func NewUSearchVectorBackendWithOptions(path string, options USearchVectorOptions) *USearchVectorBackend {
	return &USearchVectorBackend{path: path, options: options}
}

// WithLibraryPath returns a copy configured to load libraryPath.
func (b *USearchVectorBackend) WithLibraryPath(libraryPath string) *USearchVectorBackend {
	copy := *b
	copy.libraryPath = libraryPath
	return &copy
}

func (b *USearchVectorBackend) Name() string { return vectorBackendUSearchName }

func (b *USearchVectorBackend) Open(_ Config) (VectorIndex, error) {
	if b.path == "" {
		return nil, errors.New("usearch vector backend requires a sidecar directory")
	}
	options, err := b.options.normalized()
	if err != nil {
		return nil, err
	}
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return nil, err
	}
	return openUSearchVectorIndex(api, b.path, options)
}

func (b *USearchVectorBackend) HardwareAcceleration() (USearchHardwareInfo, error) {
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return USearchHardwareInfo{}, err
	}
	probe := api.(usearchHardwareAPI)
	return USearchHardwareInfo{
		Compiled:  probe.hardwareAccelerationCompiled(),
		Available: probe.hardwareAccelerationAvailable(),
	}, nil
}

type usearchHardwareAPI interface {
	hardwareAccelerationCompiled() string
	hardwareAccelerationAvailable() string
}

const (
	usearchBatchTargetFloats = 1 << 20
	usearchBatchMaxRows      = 4096
	usearchNativeFailed      = "native operation failed"
)

type usearchBatchAdder struct {
	api        usearchNativeAPI
	handle     unsafe.Pointer
	dimensions int
	keys       []uint64
	vectors    []float32
}

func newUSearchBatchAdder(api usearchNativeAPI, handle unsafe.Pointer,
	dimensions int) *usearchBatchAdder {
	rows := max(1, min(usearchBatchTargetFloats/dimensions, usearchBatchMaxRows))
	return &usearchBatchAdder{
		api:        api,
		handle:     handle,
		dimensions: dimensions,
		keys:       make([]uint64, 0, rows),
		vectors:    make([]float32, 0, rows*dimensions),
	}
}

func (b *usearchBatchAdder) Add(key uint64, vector []float32) error {
	if len(b.keys) == cap(b.keys) {
		if err := b.Flush(); err != nil {
			return err
		}
	}
	b.keys = append(b.keys, key)
	b.vectors = append(b.vectors, vector...)
	return nil
}

func (b *usearchBatchAdder) Flush() error {
	if len(b.keys) == 0 {
		return nil
	}
	status := b.api.addBatch(b.handle, b.keys, b.vectors, b.dimensions)
	if err := usearchNativeStatus(b.api, b.handle, status, "add batch"); err != nil {
		return err
	}
	b.keys = b.keys[:0]
	b.vectors = b.vectors[:0]
	return nil
}

type usearchNativeAPI interface {
	create(dimensions uintptr, metric uint32, connectivity, expansionAdd, expansionSearch uintptr) unsafe.Pointer
	open(path string) unsafe.Pointer
	openBuffer(data []byte) unsafe.Pointer
	destroy(handle unsafe.Pointer)
	dimensions(handle unsafe.Pointer) int
	size(handle unsafe.Pointer) int
	serializedLength(handle unsafe.Pointer) int
	saveBuffer(handle unsafe.Pointer, output []byte) int32
	reserve(handle unsafe.Pointer, capacity int) int32
	add(handle unsafe.Pointer, key uint64, vector []float32) int32
	addBatch(handle unsafe.Pointer, keys []uint64, vectors []float32, dimensions int) int32
	get(handle unsafe.Pointer, key uint64, vector []float32) int32
	remove(handle unsafe.Pointer, key uint64) int32
	removeBatch(handle unsafe.Pointer, keys []uint64) int32
	compact(handle unsafe.Pointer) int32
	save(handle unsafe.Pointer, path string) int32
	search(handle unsafe.Pointer, query []float32, count int, keys []uint64, distances []float32) (int32, int)
	searchFiltered(handle unsafe.Pointer, query []float32, count int, allowedKeys, keys []uint64,
		distances []float32) (int32, int)
	errorMessage(handle unsafe.Pointer) string
}

type usearchManifest struct {
	Version uint32                     `json:"version"`
	NextKey uint64                     `json:"next_key"`
	IDs     map[string]uint64          `json:"ids"`
	Fields  map[string]VectorFieldSpec `json:"fields"`
}

type usearchVectorIndex struct {
	api     usearchNativeAPI
	root    string
	options USearchVectorOptions

	mu       sync.RWMutex
	manifest usearchManifest
	reverse  map[uint64]Identifier
	indices  map[string]unsafe.Pointer
	closed   bool
}

func newUsearchManifest() usearchManifest {
	return usearchManifest{
		Version: usearchManifestVersion,
		NextKey: 1,
		IDs:     make(map[string]uint64),
		Fields:  make(map[string]VectorFieldSpec),
	}
}

func openUSearchVectorIndex(api usearchNativeAPI, root string,
	options USearchVectorOptions) (*usearchVectorIndex, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create usearch sidecar directory: %w", err)
	}
	manifest, err := readUsearchManifest(root)
	if err != nil {
		return nil, err
	}
	index := &usearchVectorIndex{
		api:      api,
		root:     root,
		options:  options,
		manifest: manifest,
		reverse:  make(map[uint64]Identifier, len(manifest.IDs)),
		indices:  make(map[string]unsafe.Pointer, len(manifest.Fields)),
	}
	for rawID, key := range manifest.IDs {
		index.reverse[key] = Identifier(rawID)
	}
	for field, spec := range manifest.Fields {
		handle := api.open(usearchFieldPath(root, field))
		if handle == nil {
			index.destroy()
			return nil, fmt.Errorf("open usearch field %q: native index could not be opened", field)
		}
		if dimensions := api.dimensions(handle); dimensions != spec.Dims {
			api.destroy(handle)
			index.destroy()
			return nil, fmt.Errorf("usearch field %q has %d dimensions, manifest says %d",
				field, dimensions, spec.Dims)
		}
		index.indices[field] = handle
	}
	return index, nil
}

func (u *usearchVectorIndex) Name() string { return vectorBackendUSearchName }

func (u *usearchVectorIndex) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	u.closed = true
	u.destroyLocked()
	return nil
}

func (u *usearchVectorIndex) destroy() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.destroyLocked()
}

func (u *usearchVectorIndex) destroyLocked() {
	for field, handle := range u.indices {
		if handle != nil {
			u.api.destroy(handle)
		}
		delete(u.indices, field)
	}
}

func (u *usearchVectorIndex) vectorFieldSpecs() map[string]VectorFieldSpec {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return maps.Clone(u.manifest.Fields)
}

func (u *usearchVectorIndex) ApplyVectorChanges(changes []VectorChange) error {
	if err := validatePublicVectorChanges(changes); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, err := mergeVectorFieldSpecs(changes, u.manifest.Fields); err != nil {
		return err
	}
	return u.applyVectorBatchLocked(changes, func() error { return nil })
}

func (u *usearchVectorIndex) applyVectorBatch(changes []VectorChange, commit func() error) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.applyVectorBatchLocked(changes, commit)
}

func (u *usearchVectorIndex) applyVectorBatchLocked(changes []VectorChange, commit func() error) error {
	if u.closed {
		return errors.New("usearch vector index is closed")
	}
	if err := commit(); err != nil {
		return err
	}
	batches := u.coalesceChanges(changes)
	for _, field := range sortedMapKeys(batches) {
		if err := u.applyFieldChanges(field, batches[field]); err != nil {
			return err
		}
	}
	return writeUsearchManifest(u.root, u.manifest)
}

type usearchFieldChanges struct {
	removals  map[uint64]struct{}
	additions map[uint64][]float32
	compact   bool
}

func usearchChangesFor(batches map[string]*usearchFieldChanges, field string) *usearchFieldChanges {
	batch := batches[field]
	if batch == nil {
		batch = &usearchFieldChanges{
			removals: make(map[uint64]struct{}), additions: make(map[uint64][]float32),
		}
		batches[field] = batch
	}
	return batch
}

func (u *usearchVectorIndex) coalesceChanges(changes []VectorChange) map[string]*usearchFieldChanges {
	batches := make(map[string]*usearchFieldChanges)
	firstNewKey := u.manifest.NextKey
	fieldData := make(map[string]bool)
	hasStoredVectors := func(field string) bool {
		if present, checked := fieldData[field]; checked {
			return present
		}
		handle := u.indices[field]
		present := handle != nil && u.api.size(handle) > 0
		fieldData[field] = present
		return present
	}
	for _, change := range changes {
		key, known := u.manifest.IDs[string(change.ID)]
		if change.Delete {
			if known {
				for field := range u.manifest.Fields {
					if change.Field == "" || change.Field == field {
						batch := usearchChangesFor(batches, field)
						delete(batch.additions, key)
						if key < firstNewKey && hasStoredVectors(field) {
							batch.removals[key] = struct{}{}
							batch.compact = true
						}
					}
				}
			}
			continue
		}
		if !known {
			key = u.manifest.NextKey
			u.manifest.NextKey++
			u.manifest.IDs[string(change.ID)] = key
			u.reverse[key] = change.ID
		}
		u.manifest.Fields[change.Field] = VectorFieldSpec{
			Name: change.Field, Dims: len(change.Vector), Similarity: change.Similarity,
		}
		batch := usearchChangesFor(batches, change.Field)
		if known && key < firstNewKey && hasStoredVectors(change.Field) {
			batch.removals[key] = struct{}{}
		}
		batch.additions[key] = change.Vector
	}
	return batches
}

func (u *usearchVectorIndex) applyFieldChanges(field string, batch *usearchFieldChanges) error {
	handle := u.indices[field]
	spec := u.manifest.Fields[field]
	if handle == nil {
		handle = u.api.create(uintptr(spec.Dims), usearchMetric(spec.Similarity),
			uintptr(u.options.Connectivity), uintptr(u.options.ExpansionAdd), uintptr(u.options.ExpansionSearch))
		if handle == nil {
			return fmt.Errorf("create usearch field %q failed", field)
		}
		u.indices[field] = handle
	}
	if len(batch.removals) > 0 {
		if err := u.nativeStatus(handle, u.api.removeBatch(handle, sortedMapKeys(batch.removals)), "remove batch"); err != nil {
			return err
		}
		if batch.compact {
			if err := u.nativeStatus(handle, u.api.compact(handle), "compact"); err != nil {
				return err
			}
		}
	}
	if len(batch.additions) > 0 {
		if err := u.nativeStatus(handle, u.api.reserve(handle, u.api.size(handle)+len(batch.additions)), "reserve batch"); err != nil {
			return err
		}
		adder := newUSearchBatchAdder(u.api, handle, spec.Dims)
		for _, key := range sortedMapKeys(batch.additions) {
			if err := adder.Add(key, batch.additions[key]); err != nil {
				return err
			}
		}
		if err := adder.Flush(); err != nil {
			return err
		}
	}
	return u.persistField(field, handle)
}

func sortedMapKeys[K interface{ ~string | ~uint64 }, V any](values map[K]V) []K {
	keys := make([]K, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func (u *usearchVectorIndex) nativeStatus(handle unsafe.Pointer, status int32, operation string) error {
	return usearchNativeStatus(u.api, handle, status, operation)
}

func usearchNativeStatus(api usearchNativeAPI, handle unsafe.Pointer,
	status int32, operation string) error {
	if status == 0 {
		return nil
	}
	message := api.errorMessage(handle)
	if message == "" {
		message = usearchNativeFailed
	}
	return fmt.Errorf("usearch %s: %s", operation, message)
}

func usearchMetric(similarity VectorSimilarity) uint32 {
	switch similarity {
	case VectorL2:
		return 1
	case VectorDot:
		return 2
	case VectorCosine:
		return 3
	default:
		return 0
	}
}

func (u *usearchVectorIndex) Search(field string, query []float32, k int, filter Query) ([]VectorHit, error) {
	if filter != nil {
		return nil, ErrVectorFilterUnsupported
	}
	return u.SearchCandidates(field, query, k, nil)
}

func (u *usearchVectorIndex) SearchCandidates(field string, query []float32, k int,
	allowed map[Identifier]struct{}) ([]VectorHit, error) {
	if err := validateVectorQuery(query, k); err != nil {
		return nil, err
	}
	return u.searchVectorCandidates(field, query, k, allowed)
}

func (u *usearchVectorIndex) searchVectorCandidates(field string, query []float32, k int,
	allowed map[Identifier]struct{}) ([]VectorHit, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.closed {
		return nil, errors.New("usearch vector index is closed")
	}
	spec, ok := u.manifest.Fields[field]
	handle := u.indices[field]
	if !ok || handle == nil || u.api.size(handle) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrVectorFieldNotFound, field)
	}
	if spec.Dims != len(query) {
		return nil, fmt.Errorf("%w: field %q has %d dimensions, got %d",
			ErrVectorInvalidDimension, field, spec.Dims, len(query))
	}
	if allowed != nil && len(allowed) == 0 {
		return []VectorHit{}, nil
	}

	var allowedKeys []uint64
	if allowed != nil {
		allowedKeys = make([]uint64, 0, len(allowed))
		for id := range allowed {
			if key, exists := u.manifest.IDs[string(id)]; exists {
				allowedKeys = append(allowedKeys, key)
			}
		}
		if len(allowedKeys) == 0 {
			return []VectorHit{}, nil
		}
		slices.Sort(allowedKeys)
	}

	keys, distances, err := searchUSearch(u.api, handle, query, k, allowedKeys)
	if err != nil {
		return nil, err
	}
	hits := make([]VectorHit, 0, len(keys))
	for i := range keys {
		id := u.reverse[keys[i]]
		hits = append(hits, VectorHit{
			ID:    id,
			Score: usearchScore(spec.Similarity, float64(distances[i])),
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	return hits, nil
}

func searchUSearch(api usearchNativeAPI, handle unsafe.Pointer, query []float32, count int,
	allowedKeys []uint64) ([]uint64, []float32, error) {
	keys, distances := make([]uint64, count), make([]float32, count)
	var status int32
	var found int
	if allowedKeys != nil {
		status, found = api.searchFiltered(handle, query, count, allowedKeys, keys, distances)
	} else {
		status, found = api.search(handle, query, count, keys, distances)
	}
	if err := usearchNativeStatus(api, handle, status, "search"); err != nil {
		return nil, nil, err
	}
	return keys[:found], distances[:found], nil
}

func usearchScore(similarity VectorSimilarity, distance float64) float64 {
	switch similarity {
	case VectorL2:
		return -distance
	case VectorDot, VectorCosine:
		// USearch represents these metrics as 1 - similarity.
		return 1 - distance
	default:
		return math.Inf(-1)
	}
}

func (u *usearchVectorIndex) persistField(field string, handle unsafe.Pointer) error {
	tmp, err := os.CreateTemp(u.root, ".bluge-usearch-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	defer os.Remove(tmpName)
	if err := u.nativeStatus(handle, u.api.save(handle, tmpName), "save"); err != nil {
		return err
	}
	if err := syncCollectionFile(tmpName); err != nil {
		return err
	}
	return fsatomic.Replace(tmpName, usearchFieldPath(u.root, field))
}

func usearchManifestPath(root string) string {
	return filepath.Join(root, "metadata.json")
}

func usearchFieldPath(root, field string) string {
	var name strings.Builder
	const hex = "0123456789abcdef"
	for i := 0; i < len(field); i++ {
		c := field[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			name.WriteByte(c)
		} else {
			name.WriteByte('%')
			name.WriteByte(hex[c>>4])
			name.WriteByte(hex[c&15])
		}
	}
	return filepath.Join(root, "field-"+name.String()+".usearch")
}

func readUsearchManifest(root string) (usearchManifest, error) {
	data, err := os.ReadFile(usearchManifestPath(root))
	if os.IsNotExist(err) {
		return newUsearchManifest(), nil
	}
	if err != nil {
		return usearchManifest{}, err
	}
	var manifest usearchManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return usearchManifest{}, fmt.Errorf("decode usearch manifest: %w", err)
	}
	if manifest.Version != usearchManifestVersion {
		return usearchManifest{}, fmt.Errorf("unsupported usearch manifest version %d", manifest.Version)
	}
	return manifest, nil
}

func writeUsearchManifest(root string, manifest usearchManifest) error {
	return replaceCollectionJSON(usearchManifestPath(root), manifest)
}

// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"unsafe"
)

const usearchManifestVersion = uint32(1)

// USearchVectorOptions controls the HNSW index created for new vector fields.
// Zero values use the USearch defaults selected by this adapter.
type USearchVectorOptions struct {
	Connectivity    int
	ExpansionAdd    int
	ExpansionSearch int
}

// USearchHardwareInfo reports the ISA families compiled into the native DLL
// and the subset available on the current CPU. It is useful for diagnosing a
// serial fallback or a mismatched release artifact.
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

// USearchVectorBackend stores one USearch HNSW index per vector field. path is
// a directory containing the manifest and native index files. The native DLL
// is loaded through a small C ABI and can be supplied explicitly with
// NewUSearchVectorBackendWithLibrary or BLUGE_USEARCH_LIBRARY_PATH.
//
// The algorithm, graph construction, distance kernels, deletion and native
// serialization are provided by USearch. This type owns only the Bluge ID
// mapping, field metadata, and the sidecar lifecycle.
type USearchVectorBackend struct {
	path        string
	libraryPath string
	options     USearchVectorOptions
}

// NewUSearchVectorBackend creates a persistent USearch backend. The native
// library is resolved from BLUGE_USEARCH_LIBRARY_PATH, the executable
// directory, or the platform library search path.
func NewUSearchVectorBackend(path string) *USearchVectorBackend {
	return &USearchVectorBackend{
		path:    path,
		options: defaultUSearchVectorOptions(),
	}
}

// NewUSearchVectorBackendWithLibrary creates a persistent backend with an
// explicit path to the separately-built native library.
func NewUSearchVectorBackendWithLibrary(path, libraryPath string) *USearchVectorBackend {
	backend := NewUSearchVectorBackend(path)
	backend.libraryPath = libraryPath
	return backend
}

// NewUSearchVectorBackendWithOptions creates a backend with explicit HNSW
// construction and search parameters.
func NewUSearchVectorBackendWithOptions(path string, options USearchVectorOptions) *USearchVectorBackend {
	return &USearchVectorBackend{path: path, options: options}
}

// WithLibraryPath returns a copy configured to load libraryPath.
func (b *USearchVectorBackend) WithLibraryPath(libraryPath string) *USearchVectorBackend {
	if b == nil {
		return nil
	}
	copy := *b
	copy.libraryPath = libraryPath
	return &copy
}

func (b *USearchVectorBackend) Name() string { return "usearch" }

func (b *USearchVectorBackend) Open(_ Config) (VectorIndex, error) {
	if b == nil || b.path == "" {
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

// HardwareAcceleration inspects the separately-built native library without
// opening a vector sidecar. Older ABI-compatible libraries may not expose the
// optional probe symbols; those return empty strings.
func (b *USearchVectorBackend) HardwareAcceleration() (USearchHardwareInfo, error) {
	if b == nil {
		return USearchHardwareInfo{}, errors.New("nil usearch vector backend")
	}
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return USearchHardwareInfo{}, err
	}
	probe, ok := api.(usearchHardwareAPI)
	if !ok {
		return USearchHardwareInfo{}, nil
	}
	return USearchHardwareInfo{
		Compiled:  probe.hardwareAccelerationCompiled(),
		Available: probe.hardwareAccelerationAvailable(),
	}, nil
}

type usearchHardwareAPI interface {
	hardwareAccelerationCompiled() string
	hardwareAccelerationAvailable() string
}

type usearchNativeAPI interface {
	create(dimensions, metric, connectivity, expansionAdd, expansionSearch uintptr) unsafe.Pointer
	open(path string) unsafe.Pointer
	openBuffer(data []byte) unsafe.Pointer
	destroy(handle unsafe.Pointer)
	dimensions(handle unsafe.Pointer) int
	size(handle unsafe.Pointer) int
	serializedLength(handle unsafe.Pointer) int
	saveBuffer(handle unsafe.Pointer, output []byte) int32
	reserve(handle unsafe.Pointer, capacity int) int32
	add(handle unsafe.Pointer, key uint64, vector []float32) int32
	get(handle unsafe.Pointer, key uint64, vector []float32) int32
	remove(handle unsafe.Pointer, key uint64) int32
	compact(handle unsafe.Pointer) int32
	save(handle unsafe.Pointer, path string) int32
	search(handle unsafe.Pointer, query []float32, count int, keys []uint64, distances []float32) (int32, int)
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

func (u *usearchVectorIndex) Name() string { return "usearch" }

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

func (u *usearchVectorIndex) ValidateVectorChanges(changes []VectorChange) error {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.closed {
		return errors.New("usearch vector index is closed")
	}
	return validateUsearchVectorChanges(changes, u.manifest.Fields)
}

func validateUsearchVectorChanges(changes []VectorChange,
	fields map[string]VectorFieldSpec) error {
	nextFields := make(map[string]VectorFieldSpec, len(fields))
	for field, spec := range fields {
		nextFields[field] = spec
	}
	for _, change := range changes {
		if change.Delete {
			continue
		}
		if err := validateVectorChange(change); err != nil {
			return err
		}
		spec, ok := nextFields[change.Field]
		if !ok {
			nextFields[change.Field] = VectorFieldSpec{
				Name:       change.Field,
				Dims:       len(change.Vector),
				Similarity: change.Similarity,
			}
			continue
		}
		if spec.Dims != len(change.Vector) {
			return fmt.Errorf("%w: field %q has %d dimensions, got %d",
				ErrVectorInvalidDimension, change.Field, spec.Dims, len(change.Vector))
		}
		if spec.Similarity != change.Similarity {
			return fmt.Errorf("field %q changes similarity from %q to %q",
				change.Field, spec.Similarity, change.Similarity)
		}
	}
	return nil
}

func (u *usearchVectorIndex) ApplyVectorChanges(changes []VectorChange) error {
	if len(changes) == 0 {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return errors.New("usearch vector index is closed")
	}
	if err := validateUsearchVectorChanges(changes, u.manifest.Fields); err != nil {
		return err
	}

	dirty := make(map[string]bool)
	removed := make(map[string]bool)
	for _, change := range changes {
		key, known := u.manifest.IDs[string(change.ID)]
		if change.Delete {
			if !known {
				continue
			}
			if change.Field == "" {
				for field, handle := range u.indices {
					if err := u.nativeStatus(handle, u.api.remove(handle, key), "remove"); err != nil {
						return err
					}
					dirty[field] = true
					removed[field] = true
				}
			} else if handle := u.indices[change.Field]; handle != nil {
				if err := u.nativeStatus(handle, u.api.remove(handle, key), "remove"); err != nil {
					return err
				}
				dirty[change.Field] = true
				removed[change.Field] = true
			}
			continue
		}

		if !known {
			key = u.manifest.NextKey
			if key == 0 {
				key = 1
			}
			u.manifest.NextKey = key + 1
			u.manifest.IDs[string(change.ID)] = key
			u.reverse[key] = change.ID
		}

		spec := u.manifest.Fields[change.Field]
		handle := u.indices[change.Field]
		if handle == nil {
			var err error
			spec = VectorFieldSpec{
				Name:       change.Field,
				Dims:       len(change.Vector),
				Similarity: change.Similarity,
			}
			handle = u.api.create(uintptr(spec.Dims), usearchMetric(spec.Similarity),
				uintptr(u.options.Connectivity), uintptr(u.options.ExpansionAdd),
				uintptr(u.options.ExpansionSearch))
			if handle == nil {
				return fmt.Errorf("create usearch field %q: native index could not be created", change.Field)
			}
			if u.options.Connectivity > 0 {
				if err = u.nativeStatus(handle, u.api.reserve(handle, 1), "reserve"); err != nil {
					u.api.destroy(handle)
					return err
				}
			}
			u.indices[change.Field] = handle
			u.manifest.Fields[change.Field] = spec
		}

		// Replacement semantics match the flat backend and make direct
		// VectorBatcher callers idempotent even without an explicit delete.
		if err := u.nativeStatus(handle, u.api.remove(handle, key), "replace"); err != nil {
			return err
		}
		if err := u.nativeStatus(handle, u.api.add(handle, key, change.Vector), "add"); err != nil {
			return err
		}
		dirty[change.Field] = true
	}

	for field := range dirty {
		handle := u.indices[field]
		if handle == nil {
			continue
		}
		if removed[field] {
			// USearch keeps deleted slots until compaction. Compact only
			// batches containing deletes, where reclaiming those slots also
			// bounds long-running update/delete workloads.
			if err := u.nativeStatus(handle, u.api.compact(handle), "compact"); err != nil {
				return err
			}
		}
		if err := u.persistField(field, handle); err != nil {
			return err
		}
	}
	return writeUsearchManifest(u.root, u.manifest)
}

func (u *usearchVectorIndex) nativeStatus(handle unsafe.Pointer, status int32, operation string) error {
	if status == 0 {
		return nil
	}
	message := u.api.errorMessage(handle)
	if message == "" {
		message = "native operation failed"
	}
	return fmt.Errorf("usearch %s: %s", operation, message)
}

func usearchMetric(similarity VectorSimilarity) uintptr {
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

	count := k
	if allowed != nil {
		count = u.api.size(handle)
	}
	if count < 1 {
		return []VectorHit{}, nil
	}
	keys := make([]uint64, count)
	distances := make([]float32, count)
	status, resultCount := u.api.search(handle, query, count, keys, distances)
	if err := u.nativeStatus(handle, status, "search"); err != nil {
		return nil, err
	}
	if resultCount < 0 || resultCount > len(keys) || resultCount > len(distances) {
		return nil, fmt.Errorf("usearch search returned invalid result count %d", resultCount)
	}
	hits := make([]VectorHit, 0, minInt(k, resultCount))
	for i := 0; i < resultCount; i++ {
		id, exists := u.reverse[keys[i]]
		if !exists {
			continue
		}
		if allowed != nil {
			if _, exists = allowed[id]; !exists {
				continue
			}
		}
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
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
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
	return replaceUsearchFile(tmpName, usearchFieldPath(u.root, field))
}

func usearchManifestPath(root string) string {
	return filepath.Join(root, "manifest.json")
}

func usearchFieldPath(root, field string) string {
	digest := sha256.Sum256([]byte(field))
	return filepath.Join(root, "field-"+hex.EncodeToString(digest[:])+".usearch")
}

func readUsearchManifest(root string) (usearchManifest, error) {
	data, err := os.ReadFile(usearchManifestPath(root))
	if os.IsNotExist(err) {
		return newUsearchManifest(), nil
	}
	if err != nil {
		return usearchManifest{}, err
	}
	manifest := newUsearchManifest()
	if err := json.Unmarshal(data, &manifest); err != nil {
		return usearchManifest{}, fmt.Errorf("decode usearch manifest: %w", err)
	}
	if manifest.Version != usearchManifestVersion {
		return usearchManifest{}, fmt.Errorf("unsupported usearch manifest version %d", manifest.Version)
	}
	if manifest.NextKey == 0 {
		manifest.NextKey = 1
	}
	if manifest.IDs == nil {
		manifest.IDs = make(map[string]uint64)
	}
	if manifest.Fields == nil {
		manifest.Fields = make(map[string]VectorFieldSpec)
	}
	return manifest, nil
}

func writeUsearchManifest(root string, manifest usearchManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".bluge-usearch-manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceUsearchFile(tmpName, usearchManifestPath(root))
}

func replaceUsearchFile(source, target string) error {
	if err := os.Rename(source, target); err == nil {
		return nil
	} else {
		if removeErr := os.Remove(target); removeErr != nil && !os.IsNotExist(removeErr) {
			return err
		}
	}
	return os.Rename(source, target)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

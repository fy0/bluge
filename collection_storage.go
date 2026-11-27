// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"github.com/fy0/bluge/internal/fsatomic"
)

type collectionManifest struct {
	Version  int    `json:"version"`
	CorpusID string `json:"corpus_id"`
	Storage  string `json:"storage"`
}

type collectionSpace struct {
	Version     int             `json:"version"`
	CorpusID    string          `json:"corpus_id"`
	Spec        VectorSpaceSpec `json:"spec"`
	Generation  uint64          `json:"-"`
	Generations []uint64        `json:"generations"`
}

type collectionVectorMetadata struct {
	Version    int                   `json:"version"`
	CorpusID   string                `json:"corpus_id"`
	Spec       VectorSpaceSpec       `json:"spec"`
	Generation uint64                `json:"generation"`
	Storage    string                `json:"storage"`
	IndexSize  int64                 `json:"index_size"`
	Revisions  map[Identifier]string `json:"revisions"`
	Native     *usearchManifest      `json:"native,omitempty"`
}

func loadCollectionManifest(config CollectionConfig, create bool) (collectionManifest, error) {
	path := filepath.Join(config.Path, "collection.json")
	var m collectionManifest
	err := readCollectionJSON(path, &m)
	if errors.Is(err, os.ErrNotExist) && create {
		// A lost manifest must not silently assign a new identity to old data.
		entries, readErr := os.ReadDir(config.Path)
		if readErr != nil {
			return m, readErr
		}
		for _, e := range entries {
			if e.Name() != "collection.lock" && e.Name() != "generations.lock" && !strings.HasPrefix(e.Name(), ".pending-") {
				return m, fmt.Errorf("%w: collection manifest missing in nonempty directory", ErrCollectionCorrupt)
			}
		}
		m.CorpusID, err = newCollectionToken()
		if err != nil {
			return m, err
		}
		m.Version, m.Storage = collectionFormatVersion, config.VectorStorage
		err = writeCollectionJSON(path, m)
	}
	if err != nil {
		return m, err
	}
	if m.Version != collectionFormatVersion || m.CorpusID == "" || m.Storage != config.VectorStorage {
		return m, fmt.Errorf("%w: collection version, identity or storage mismatch", ErrCollectionCorrupt)
	}
	return m, nil
}

func collectionSpacePath(root, id string) string {
	// Prefixing also avoids Windows reserved device names such as "con".
	return filepath.Join(root, "vectors", "space-"+id)
}

func selectCollectionSpaces(spaces map[string]collectionSpace, spaceIDs []string) (map[string]collectionSpace, error) {
	if len(spaceIDs) == 0 {
		return spaces, nil
	}
	selected := make(map[string]collectionSpace, len(spaceIDs))
	for _, id := range spaceIDs {
		space, ok := spaces[id]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrVectorSpaceNotFound, id)
		}
		selected[id] = space
	}
	return selected, nil
}

func loadCollectionSpaces(root, corpus string, spaceIDs ...string) (map[string]collectionSpace, error) {
	spaces := make(map[string]collectionSpace)
	entries, err := os.ReadDir(filepath.Join(root, "vectors"))
	if errors.Is(err, os.ErrNotExist) {
		return selectCollectionSpaces(spaces, spaceIDs)
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "space-") {
			continue
		}
		if len(spaceIDs) != 0 && !slices.Contains(spaceIDs, strings.TrimPrefix(e.Name(), "space-")) {
			continue
		}
		dir := filepath.Join(root, "vectors", e.Name())
		var space collectionSpace
		if err := readCollectionJSON(filepath.Join(dir, "spec.json"), &space); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if err = checkCollectionMissingSpec(dir); err == nil {
					continue
				}
			}
			return nil, err
		}
		if space.Version != collectionFormatVersion || space.CorpusID != corpus ||
			"space-"+space.Spec.ID != e.Name() {
			return nil, fmt.Errorf("%w: invalid vector space %q", ErrCollectionCorrupt, e.Name())
		}
		space.Generations, err = collectionCatalogGenerations(dir, space)
		if err != nil {
			return nil, err
		}
		if len(space.Generations) != 0 {
			space.Generation = space.Generations[len(space.Generations)-1]
		}
		spaces[space.Spec.ID] = space
	}
	return selectCollectionSpaces(spaces, spaceIDs)
}

func checkCollectionMissingSpec(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "index-") || strings.HasPrefix(name, "metadata-") {
			return fmt.Errorf("%w: missing vector space specification", ErrCollectionCorrupt)
		}
	}
	return nil
}

func collectionGeneration(dir string) (uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var max uint64
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "lease-") {
			continue
		}
		n, recognized, err := collectionFileGeneration(name)
		if err != nil {
			return 0, err
		}
		if !recognized {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max, nil
}

func collectionGenerationFiles(dir string, generation uint64) (string, string) {
	return filepath.Join(dir, fmt.Sprintf("index-%020d", generation)),
		filepath.Join(dir, fmt.Sprintf("metadata-%020d.json", generation))
}

func readCollectionJSON(path string, value interface{}) error {
	// #nosec G703 -- Paths come from the trusted collection configuration or local benchmark files.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrCollectionCorrupt, path, err)
	}
	return nil
}

func writeCollectionJSON(path string, value interface{}) error {
	return writeCollectionJSONFile(path, value, false)
}

func replaceCollectionJSON(path string, value interface{}) error {
	return writeCollectionJSONFile(path, value, true)
}

func writeCollectionJSONFile(path string, value interface{}, replace bool) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if replace {
		return fsatomic.Replace(f.Name(), path)
	}
	return fsatomic.Publish(f.Name(), path)
}

func syncCollectionFile(path string) error {
	// #nosec G703 -- Collection file paths come from the configured directory.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func loadCollectionVectorMetadata(config CollectionConfig, space collectionSpace) (*collectionVectorMetadata, error) {
	if space.Generation == 0 {
		return &collectionVectorMetadata{Version: collectionFormatVersion, CorpusID: space.CorpusID,
			Spec: space.Spec, Storage: config.VectorStorage, Revisions: make(map[Identifier]string)}, nil
	}
	dir := collectionSpacePath(config.Path, space.Spec.ID)
	indexPath, metadataPath := collectionGenerationFiles(dir, space.Generation)
	var m collectionVectorMetadata
	if err := readCollectionJSON(metadataPath, &m); err != nil {
		return nil, err
	}
	if m.Version != collectionFormatVersion || m.CorpusID != space.CorpusID || m.Spec != space.Spec ||
		m.Generation != space.Generation || m.Storage != config.VectorStorage || m.Revisions == nil {
		return nil, fmt.Errorf("%w: vector metadata mismatch", ErrCollectionCorrupt)
	}
	info, err := os.Stat(indexPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCollectionCorrupt, err)
	}
	if info.Size() != m.IndexSize {
		return nil, fmt.Errorf("%w: index size mismatch", ErrCollectionCorrupt)
	}
	for id, revision := range m.Revisions {
		if id == "" || revision == "" {
			return nil, fmt.Errorf("%w: empty vector identity", ErrCollectionCorrupt)
		}
	}
	if m.Storage == vectorBackendUSearchName {
		if err := validateCollectionNativeMetadata(&m); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

func validateCollectionNativeMetadata(m *collectionVectorMetadata) error {
	field := VectorFieldSpec{Name: m.Spec.Field, Dims: m.Spec.Dims, Similarity: m.Spec.Similarity}
	if m.Native == nil || m.Native.Version != usearchManifestVersion || m.Native.NextKey == 0 ||
		len(m.Native.Fields) != 1 || m.Native.Fields[m.Spec.Field] != field {
		return fmt.Errorf("%w: invalid native manifest", ErrCollectionCorrupt)
	}
	keys := make(map[uint64]bool, len(m.Native.IDs))
	for id, key := range m.Native.IDs {
		if id == "" || key == 0 || keys[key] || key >= m.Native.NextKey {
			return fmt.Errorf("%w: invalid native key mapping", ErrCollectionCorrupt)
		}
		keys[key] = true
	}
	for id := range m.Revisions {
		if m.Native.IDs[string(id)] == 0 {
			return fmt.Errorf("%w: missing native key", ErrCollectionCorrupt)
		}
	}
	return nil
}

// openCollectionVectors loads a private native handle, never the handle of a
// live reader. A nonempty staging directory permits writes to this copy only.
func openCollectionVectors(config CollectionConfig, space collectionSpace,
	m *collectionVectorMetadata, staging string) (VectorIndex, error) {
	indexPath, _ := collectionGenerationFiles(collectionSpacePath(config.Path, space.Spec.ID), space.Generation)
	if config.VectorStorage == vectorBackendFlatName {
		f := newFlatVectorIndex("")
		if space.Generation != 0 {
			var err error
			f, err = loadFlatVectorIndex(indexPath)
			if err != nil {
				return nil, err
			}
			if len(f.vectors[space.Spec.Field]) != len(m.Revisions) {
				return nil, fmt.Errorf("%w: vector count mismatch", ErrCollectionCorrupt)
			}
		}
		f.path = ""
		if staging != "" {
			f.path = filepath.Join(staging, "index.flat")
		}
		return f, nil
	}
	api, err := loadUSearchAPI(config.LibraryPath)
	if err != nil {
		return nil, err
	}
	native := newUsearchManifest()
	if m.Native != nil {
		native = *m.Native
	}
	u := &usearchVectorIndex{api: api, root: staging, options: config.USearchOptions, manifest: native,
		reverse: make(map[uint64]Identifier, len(native.IDs)), indices: make(map[string]unsafe.Pointer)}
	for id, key := range native.IDs {
		u.reverse[key] = Identifier(id)
	}
	if space.Generation != 0 {
		handle := api.open(indexPath)
		if handle == nil {
			return nil, fmt.Errorf("%w: cannot open native vector index", ErrCollectionCorrupt)
		}
		u.indices[space.Spec.Field] = handle
		dims, count := api.dimensions(handle), api.size(handle)
		if dims != space.Spec.Dims || count != len(m.Revisions) {
			_ = u.Close()
			return nil, fmt.Errorf("%w: native generation %d has dimensions=%d count=%d, expected dimensions=%d count=%d", ErrCollectionCorrupt,
				space.Generation, dims, count, space.Spec.Dims, len(m.Revisions))
		}
	}
	return u, nil
}

func (c *Collection) UpsertVectors(spaceID string, records []VectorRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return err
	}
	space, ok := c.spaces[spaceID]
	if !ok {
		return ErrVectorSpaceNotFound
	}
	if len(records) == 0 {
		return nil
	}
	if err := c.loadRevisions(); err != nil {
		return err
	}
	seen, err := validateCollectionVectorRecords(space.Spec, records, c.revisions)
	if err != nil {
		return err
	}
	m, err := loadCollectionVectorMetadata(c.config, space)
	if err != nil {
		return err
	}
	dir := collectionSpacePath(c.config.Path, spaceID)
	last, err := collectionGeneration(dir)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp(dir, ".pending-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	source, err := c.stageVectors(space, m, staging, records, seen)
	if err != nil {
		return err
	}
	m.Generation = last + 1
	return c.publishVectors(space, m, source)
}

func (c *Collection) vectorChanges(m *collectionVectorMetadata, records []VectorRecord,
	seen map[Identifier]bool) []VectorChange {
	changes := make([]VectorChange, 0, len(records))
	for id, revision := range m.Revisions {
		if c.revisions[id] != revision && !seen[id] {
			changes = append(changes, VectorChange{ID: id, Delete: true})
			delete(m.Revisions, id)
		}
	}
	for _, r := range records {
		changes = append(changes, VectorChange{ID: r.ID, Field: m.Spec.Field, Vector: r.Vector, Similarity: m.Spec.Similarity})
		m.Revisions[r.ID] = r.Revision
	}
	return changes
}

func (c *Collection) stageVectors(space collectionSpace, m *collectionVectorMetadata,
	staging string, records []VectorRecord, seen map[Identifier]bool) (source string, err error) {
	vectors, err := openCollectionVectors(c.config, space, m, staging)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, vectors.Close()) }()
	changes := c.vectorChanges(m, records, seen)
	if err := vectors.(vectorBatchTransaction).applyVectorBatch(changes, func() error { return nil }); err != nil {
		return "", err
	}
	if u, ok := vectors.(*usearchVectorIndex); ok {
		for id := range u.manifest.IDs {
			if _, live := m.Revisions[Identifier(id)]; !live {
				delete(u.manifest.IDs, id)
			}
		}
		m.Native = &u.manifest
		return usearchFieldPath(staging, space.Spec.Field), nil
	}
	return filepath.Join(staging, "index.flat"), nil
}

func validateCollectionVectorRecords(spec VectorSpaceSpec, records []VectorRecord,
	revisions map[Identifier]string) (map[Identifier]bool, error) {
	seen := make(map[Identifier]bool, len(records))
	var conflicts []Identifier
	for _, r := range records {
		if r.ID == "" || r.Revision == "" || seen[r.ID] {
			return nil, errors.New("invalid vector identity")
		}
		seen[r.ID] = true
		if revisions[r.ID] != r.Revision {
			conflicts = append(conflicts, r.ID)
		}
		if len(r.Vector) != spec.Dims {
			return nil, ErrVectorInvalidDimension
		}
		for _, value := range r.Vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, ErrVectorInvalidValue
			}
		}
	}
	if len(conflicts) != 0 {
		return nil, &VectorRevisionConflictError{IDs: conflicts}
	}
	return seen, nil
}

func (c *Collection) publishVectors(space collectionSpace, m *collectionVectorMetadata, source string) error {
	dir := collectionSpacePath(c.config.Path, space.Spec.ID)
	indexPath, metadataPath := collectionGenerationFiles(dir, m.Generation)
	if err := syncCollectionFile(source); err != nil {
		return err
	}
	if err := fsatomic.Publish(source, indexPath); err != nil {
		return err
	}
	info, err := os.Stat(indexPath)
	if err != nil {
		return err
	}
	m.IndexSize = info.Size()
	if err := writeCollectionJSON(metadataPath, m); err != nil {
		return err
	}
	if err := c.commitVectorGeneration(space, m.Generation); err != nil {
		return err
	}
	_, c.maintenanceErr = maintainCollection(context.Background(), c.config, c.corpus, space.Spec.ID)
	return nil
}

func (c *Collection) commitVectorGeneration(space collectionSpace, generation uint64) error {
	gate, err := lockCollectionCoordinator(context.Background(), c.config.Path)
	if err != nil {
		return err
	}
	defer gate.Close()
	// Maintenance may have removed older generations since this space was cached.
	spaces, err := loadCollectionSpaces(c.config.Path, c.corpus, space.Spec.ID)
	if err != nil {
		return err
	}
	current := spaces[space.Spec.ID]
	current.Generation = generation
	current.Generations = append(current.Generations, generation)
	dir := collectionSpacePath(c.config.Path, space.Spec.ID)
	if err := writeCollectionSpace(dir, &current); err != nil {
		c.writeErr = err
		return err
	}
	c.spaces[space.Spec.ID] = current
	return nil
}

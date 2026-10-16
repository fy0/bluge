// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"
)

type collectionManifest struct {
	Version  int    `json:"version"`
	CorpusID string `json:"corpus_id"`
	Storage  string `json:"storage"`
}

type collectionSpace struct {
	Version       int             `json:"version"`
	CorpusID      string          `json:"corpus_id"`
	Spec          VectorSpaceSpec `json:"spec"`
	Generation    uint64          `json:"-"`
	Generations   []uint64        `json:"generations"`
	CatalogSHA256 string          `json:"catalog_sha256,omitempty"`
}

type collectionVectorMetadata struct {
	Version    int                   `json:"version"`
	CorpusID   string                `json:"corpus_id"`
	Spec       VectorSpaceSpec       `json:"spec"`
	Generation uint64                `json:"generation"`
	Storage    string                `json:"storage"`
	Revisions  map[Identifier]string `json:"revisions"`
	Native     *usearchManifest      `json:"native,omitempty"`
}

type collectionCommit struct {
	Version        int    `json:"version"`
	CorpusID       string `json:"corpus_id"`
	SpaceID        string `json:"space_id"`
	Generation     uint64 `json:"generation"`
	IndexSHA256    string `json:"index_sha256"`
	MetadataSHA256 string `json:"metadata_sha256"`
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
				generation, scanErr := collectionGeneration(dir, true)
				if scanErr != nil {
					return nil, scanErr
				}
				if generation != 0 {
					return nil, fmt.Errorf("%w: missing space specification for %s", ErrCollectionCorrupt, dir)
				}
				continue
			}
			return nil, err
		}
		if space.Version != collectionFormatVersion || space.CorpusID != corpus ||
			space.Spec.validate() != nil || "space-"+space.Spec.ID != e.Name() {
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

func collectionGeneration(dir string, committedOnly bool) (uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var max uint64
	for _, e := range entries {
		name := e.Name()
		if committedOnly && !strings.HasPrefix(name, "commit-") || strings.HasPrefix(name, "lease-") {
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

func collectionGenerationFiles(dir, storage string, generation uint64) (string, string, string) {
	return filepath.Join(dir, fmt.Sprintf("index-%020d.%s", generation, storage)),
		filepath.Join(dir, fmt.Sprintf("metadata-%020d.json", generation)),
		filepath.Join(dir, fmt.Sprintf("commit-%020d.json", generation))
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
		return replaceCollectionFile(f.Name(), path)
	}
	return publishCollectionFile(f.Name(), path)
}

func collectionFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func syncCollectionFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// readCollectionCommit checks identity and file presence without reading payloads.
func readCollectionCommit(config CollectionConfig, space collectionSpace) (collectionCommit, error) {
	dir := collectionSpacePath(config.Path, space.Spec.ID)
	indexPath, metadataPath, commitPath := collectionGenerationFiles(dir, config.VectorStorage, space.Generation)
	var commit collectionCommit
	if err := readCollectionJSON(commitPath, &commit); err != nil {
		return commit, fmt.Errorf("%w: commit: %w", ErrCollectionCorrupt, err)
	}
	if commit.Version != collectionFormatVersion || commit.CorpusID != space.CorpusID ||
		commit.SpaceID != space.Spec.ID || commit.Generation != space.Generation {
		return commit, fmt.Errorf("%w: commit identity mismatch", ErrCollectionCorrupt)
	}
	for _, path := range []string{indexPath, metadataPath} {
		info, err := os.Stat(path)
		if err != nil {
			return commit, fmt.Errorf("%w: payload: %w", ErrCollectionCorrupt, err)
		}
		if !info.Mode().IsRegular() {
			return commit, fmt.Errorf("%w: non-file payload %s", ErrCollectionCorrupt, path)
		}
	}
	return commit, nil
}

func loadCollectionVectorMetadata(config CollectionConfig, space collectionSpace) (*collectionVectorMetadata, error) {
	if space.Generation == 0 {
		return &collectionVectorMetadata{Version: collectionFormatVersion, CorpusID: space.CorpusID,
			Spec: space.Spec, Storage: config.VectorStorage, Revisions: make(map[Identifier]string)}, nil
	}
	commit, err := readCollectionCommit(config, space)
	if err != nil {
		return nil, err
	}
	dir := collectionSpacePath(config.Path, space.Spec.ID)
	indexPath, metadataPath, _ := collectionGenerationFiles(dir, config.VectorStorage, space.Generation)
	for path, expected := range map[string]string{indexPath: commit.IndexSHA256, metadataPath: commit.MetadataSHA256} {
		actual, err := collectionFileHash(path)
		if err != nil {
			return nil, err
		}
		if actual != expected {
			return nil, fmt.Errorf("%w: checksum mismatch for %s", ErrCollectionCorrupt, path)
		}
	}
	var m collectionVectorMetadata
	if err := readCollectionJSON(metadataPath, &m); err != nil {
		return nil, err
	}
	if m.Version != collectionFormatVersion || m.CorpusID != space.CorpusID || m.Spec != space.Spec ||
		m.Generation != space.Generation || m.Storage != config.VectorStorage || m.Revisions == nil {
		return nil, fmt.Errorf("%w: vector metadata mismatch", ErrCollectionCorrupt)
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
	indexPath, _, _ := collectionGenerationFiles(collectionSpacePath(config.Path, space.Spec.ID), config.VectorStorage, space.Generation)
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

// UpsertVectors publishes one complete generation for one model without writing
// the text index. Batch embeddings to amortize full-index serialization costs.
// After publication, maintenance retains the current and reader-held generations.
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
	seen, err := validateCollectionVectorRecords(space.Spec, records, c.revisions)
	if err != nil {
		return err
	}
	m, err := loadCollectionVectorMetadata(c.config, space)
	if err != nil {
		return err
	}
	dir := collectionSpacePath(c.config.Path, spaceID)
	last, err := collectionGeneration(dir, false)
	if err != nil {
		return err
	}
	if last == ^uint64(0) {
		return errors.New("vector generation overflow")
	}
	staging, err := os.MkdirTemp(dir, ".pending-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	vectors, err := openCollectionVectors(c.config, space, m, staging)
	if err != nil {
		return err
	}
	defer vectors.Close()
	changes := make([]VectorChange, 0, len(records))
	for id, revision := range m.Revisions {
		// Upserts already replace existing vectors. Only explicit removals
		// need compaction; replacing a stale vector can reuse its native slot.
		if c.revisions[id] != revision && !seen[id] {
			changes = append(changes, VectorChange{ID: id, Delete: true})
			delete(m.Revisions, id)
		}
	}
	for _, r := range records {
		changes = append(changes, VectorChange{ID: r.ID, Field: space.Spec.Field, Vector: r.Vector, Similarity: space.Spec.Similarity})
		m.Revisions[r.ID] = r.Revision
	}
	if err := vectors.(VectorBatcher).ApplyVectorChanges(changes); err != nil {
		return err
	}
	source := filepath.Join(staging, "index.flat")
	if u, ok := vectors.(*usearchVectorIndex); ok {
		// A collection generation has one field. Removed IDs cannot be needed
		// by another field, and must not accumulate in new generations.
		for id := range u.manifest.IDs {
			if _, live := m.Revisions[Identifier(id)]; !live {
				delete(u.manifest.IDs, id)
			}
		}
		m.Native = &u.manifest
		source = usearchFieldPath(staging, space.Spec.Field)
	}
	if err := vectors.Close(); err != nil {
		return err
	}
	m.Generation = last + 1
	return c.publishVectors(space, m, source)
}

func validateCollectionVectorRecords(spec VectorSpaceSpec, records []VectorRecord,
	revisions map[Identifier]string) (map[Identifier]bool, error) {
	seen := make(map[Identifier]bool, len(records))
	var conflicts []Identifier
	for _, r := range records {
		if r.ID == "" || r.Revision == "" || seen[r.ID] {
			return nil, errors.New("vector batch requires unique nonempty IDs and revisions")
		}
		seen[r.ID] = true
		if revisions[r.ID] != r.Revision {
			conflicts = append(conflicts, r.ID)
		}
		if len(r.Vector) != spec.Dims {
			return nil, ErrVectorInvalidDimension
		}
		if err := validateVectorChange(VectorChange{ID: r.ID, Field: spec.Field, Vector: r.Vector, Similarity: spec.Similarity}); err != nil {
			return nil, err
		}
	}
	if len(conflicts) != 0 {
		return nil, &VectorRevisionConflictError{IDs: conflicts}
	}
	return seen, nil
}

// publishVectors makes staged payloads durable before publishing their catalog.
// The caller holds c.mu and keeps the staging directory alive through cleanup.
func (c *Collection) publishVectors(space collectionSpace, m *collectionVectorMetadata, source string) error {
	spaceID := space.Spec.ID
	dir := collectionSpacePath(c.config.Path, spaceID)
	indexPath, metadataPath, commitPath := collectionGenerationFiles(dir, c.config.VectorStorage, m.Generation)
	if err := syncCollectionFile(source); err != nil {
		return err
	}
	if err := publishCollectionFile(source, indexPath); err != nil {
		return err
	}
	if err := writeCollectionJSON(metadataPath, m); err != nil {
		return err
	}
	indexHash, err := collectionFileHash(indexPath)
	if err != nil {
		return err
	}
	metadataHash, err := collectionFileHash(metadataPath)
	if err != nil {
		return err
	}
	commit := collectionCommit{Version: collectionFormatVersion, CorpusID: c.corpus, SpaceID: spaceID,
		Generation: m.Generation, IndexSHA256: indexHash, MetadataSHA256: metadataHash}
	gate, err := lockCollectionCoordinator(context.Background(), c.config.Path)
	if err != nil {
		return err
	}
	// GC can have shortened the on-disk catalog since this writer cached the
	// space. Reload it under the coordinator before appending a publication.
	spaces, err := loadCollectionSpaces(c.config.Path, c.corpus, spaceID)
	if err != nil {
		_ = gate.Close()
		return err
	}
	current, ok := spaces[spaceID]
	if !ok || current.Spec != space.Spec || current.Generation != space.Generation {
		_ = gate.Close()
		return fmt.Errorf("%w: vector space changed while writing", ErrCollectionCorrupt)
	}
	if err := writeCollectionJSON(commitPath, commit); err != nil {
		_ = gate.Close()
		return err
	}
	current.Generation = m.Generation
	current.Generations = append(current.Generations, m.Generation)
	if err := writeCollectionSpace(dir, &current); err != nil {
		_ = gate.Close()
		// Catalog replacement may be visible even if its directory sync failed.
		c.writeErr = err
		return err
	}
	c.spaces[spaceID] = current
	if err := gate.Close(); err != nil {
		c.maintenanceErr = err
		return nil
	}
	// Staging is no longer in use. Cleanup errors do not change commit success.
	_, c.maintenanceErr = maintainCollection(context.Background(), c.config, c.corpus, spaceID)
	return nil
}

// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"
)

type collectionManifest struct {
	Version  int    `json:"version"`
	CorpusID string `json:"corpus_id"`
	Storage  string `json:"storage"`
}

type collectionSpace struct {
	Version    int             `json:"version"`
	CorpusID   string          `json:"corpus_id"`
	Spec       VectorSpaceSpec `json:"spec"`
	Generation uint64          `json:"-"`
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
			if e.Name() != "collection.lock" && !strings.HasPrefix(e.Name(), ".pending-") {
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

func loadCollectionSpaces(root, corpus string) (map[string]collectionSpace, error) {
	spaces := make(map[string]collectionSpace)
	entries, err := os.ReadDir(filepath.Join(root, "vectors"))
	if errors.Is(err, os.ErrNotExist) {
		return spaces, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "space-") {
			continue
		}
		dir := filepath.Join(root, "vectors", e.Name())
		var space collectionSpace
		if err := readCollectionJSON(filepath.Join(dir, "spec.json"), &space); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if space.Version != collectionFormatVersion || space.CorpusID != corpus ||
			space.Spec.validate() != nil || "space-"+space.Spec.ID != e.Name() {
			return nil, fmt.Errorf("%w: invalid vector space %q", ErrCollectionCorrupt, e.Name())
		}
		space.Generation, err = collectionGeneration(dir, true)
		if err != nil {
			return nil, err
		}
		spaces[space.Spec.ID] = space
	}
	return spaces, nil
}

func collectionGeneration(dir string, committedOnly bool) (uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var max uint64
	for _, e := range entries {
		name := e.Name()
		prefix := "commit-"
		if !strings.HasPrefix(name, prefix) {
			if committedOnly {
				continue
			}
			if strings.HasPrefix(name, "index-") {
				prefix = "index-"
			} else if strings.HasPrefix(name, "metadata-") {
				prefix = "metadata-"
			} else {
				continue
			}
		}
		ext := filepath.Ext(name)
		if ext != ".json" && ext != ".usearch" && ext != ".flat" {
			continue
		}
		if prefix == "commit-" && ext != ".json" {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext)
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || len(raw) != 20 || n == 0 {
			return 0, fmt.Errorf("%w: invalid generation filename %q", ErrCollectionCorrupt, name)
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

func loadCollectionVectorMetadata(config CollectionConfig, space collectionSpace) (*collectionVectorMetadata, error) {
	if space.Generation == 0 {
		return &collectionVectorMetadata{Version: collectionFormatVersion, CorpusID: space.CorpusID,
			Spec: space.Spec, Storage: config.VectorStorage, Revisions: make(map[Identifier]string)}, nil
	}
	dir := collectionSpacePath(config.Path, space.Spec.ID)
	indexPath, metadataPath, commitPath := collectionGenerationFiles(dir, config.VectorStorage, space.Generation)
	var commit collectionCommit
	if err := readCollectionJSON(commitPath, &commit); err != nil {
		return nil, err
	}
	if commit.Version != collectionFormatVersion || commit.CorpusID != space.CorpusID ||
		commit.SpaceID != space.Spec.ID || commit.Generation != space.Generation {
		return nil, fmt.Errorf("%w: commit identity mismatch", ErrCollectionCorrupt)
	}
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
	if m.Storage == "usearch" {
		if m.Native == nil || m.Native.Version != usearchManifestVersion || m.Native.NextKey == 0 ||
			len(m.Native.Fields) != 1 || m.Native.Fields[m.Spec.Field] != (VectorFieldSpec{Name: m.Spec.Field, Dims: m.Spec.Dims, Similarity: m.Spec.Similarity}) {
			return nil, fmt.Errorf("%w: invalid native manifest", ErrCollectionCorrupt)
		}
		keys := make(map[uint64]bool, len(m.Native.IDs))
		for id, key := range m.Native.IDs {
			if id == "" || key == 0 || keys[key] || key >= m.Native.NextKey {
				return nil, fmt.Errorf("%w: invalid native key mapping", ErrCollectionCorrupt)
			}
			keys[key] = true
		}
		for id := range m.Revisions {
			if m.Native.IDs[string(id)] == 0 {
				return nil, fmt.Errorf("%w: missing native key", ErrCollectionCorrupt)
			}
		}
	}
	return &m, nil
}

// openCollectionVectors loads a private native handle, never the handle of a
// live reader. A nonempty staging directory permits writes to this copy only.
func openCollectionVectors(config CollectionConfig, space collectionSpace,
	m *collectionVectorMetadata, staging string) (VectorIndex, error) {
	indexPath, _, _ := collectionGenerationFiles(collectionSpacePath(config.Path, space.Spec.ID), config.VectorStorage, space.Generation)
	if config.VectorStorage == "flat" {
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
		if api.dimensions(handle) != space.Spec.Dims || api.size(handle) != len(m.Revisions) {
			_ = u.Close()
			return nil, fmt.Errorf("%w: native dimensions or count mismatch", ErrCollectionCorrupt)
		}
	}
	return u, nil
}

// UpsertVectors publishes one complete generation for one model without writing
// the text index. Batch embeddings to amortize full-index serialization costs.
// Old generations are retained in this experimental implementation.
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
	seen := make(map[Identifier]bool, len(records))
	var conflicts []Identifier
	for _, r := range records {
		if r.ID == "" || r.Revision == "" || seen[r.ID] {
			return errors.New("vector batch requires unique nonempty IDs and revisions")
		}
		seen[r.ID] = true
		if c.revisions[r.ID] != r.Revision {
			conflicts = append(conflicts, r.ID)
		}
		if len(r.Vector) != space.Spec.Dims {
			return ErrVectorInvalidDimension
		}
		if err := validateVectorChange(VectorChange{ID: r.ID, Field: space.Spec.Field, Vector: r.Vector, Similarity: space.Spec.Similarity}); err != nil {
			return err
		}
	}
	if len(conflicts) != 0 {
		return &VectorRevisionConflictError{IDs: conflicts}
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
		if c.revisions[id] != revision {
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
		m.Native = &u.manifest
		source = usearchFieldPath(staging, space.Spec.Field)
	}
	if err := vectors.Close(); err != nil {
		return err
	}
	m.Generation = last + 1
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
	if err := writeCollectionJSON(commitPath, commit); err != nil {
		// Publication may have succeeded even if the directory sync failed.
		c.writeErr = err
		return err
	}
	space.Generation = m.Generation
	c.spaces[spaceID] = space
	return nil
}

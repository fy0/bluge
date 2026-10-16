// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/index/lock"
)

var (
	ErrCollectionClosed       = errors.New("collection is closed")
	ErrVectorSpaceNotFound    = errors.New("vector space was not found")
	ErrVectorRevisionConflict = errors.New("vector document revision is stale")
	ErrCollectionCorrupt      = errors.New("collection data is corrupt")
)

const (
	collectionFormatVersion = 2
	collectionRevisionField = "_collection_revision"
	collectionCorpusField   = "_collection_corpus"
	collectionVectorField   = "embedding"
)

// CollectionConfig configures the experimental, independently persisted vector
// mode. TextConfig controls text analysis and scoring; its directory is replaced
// with Path/text. VectorStorage is "usearch" (default) or "flat" (exact search).
type CollectionConfig struct {
	Path           string
	TextConfig     Config
	VectorStorage  string
	LibraryPath    string
	USearchOptions USearchVectorOptions
}

func DefaultCollectionConfig(path string) CollectionConfig {
	return CollectionConfig{
		Path:          path,
		TextConfig:    DefaultConfig(filepath.Join(path, "text")),
		VectorStorage: vectorBackendUSearchName,
	}
}

func (c CollectionConfig) normalized() (CollectionConfig, error) {
	if c.Path == "" {
		return c, errors.New("collection path is empty")
	}
	var err error
	c.Path, err = filepath.Abs(c.Path)
	if err != nil {
		return c, err
	}
	if c.VectorStorage == "" {
		c.VectorStorage = vectorBackendUSearchName
	}
	if c.VectorStorage != vectorBackendFlatName && c.VectorStorage != vectorBackendUSearchName {
		return c, fmt.Errorf("unsupported collection vector storage %q", c.VectorStorage)
	}
	c.USearchOptions, err = c.USearchOptions.normalized()
	if err != nil {
		return c, err
	}
	if c.TextConfig.indexConfig.DirectoryFunc == nil {
		c.TextConfig = DefaultConfig(filepath.Join(c.Path, "text"))
	}
	if c.TextConfig.VectorBackend != nil && c.TextConfig.VectorBackend.Name() != vectorBackendUnsupportedName {
		return c, errors.New("collection text config must not configure a vector backend")
	}
	if c.TextConfig.indexConfig.UnsafeBatch {
		return c, errors.New("collection requires durable text batches")
	}
	c.TextConfig = c.TextConfig.WithVectorBackend(unsupportedVectorBackend{})
	path := filepath.Join(c.Path, "text")
	c.TextConfig.indexConfig.DirectoryFunc = func() index.Directory { return index.NewFileSystemDirectory(path) }
	return c, nil
}

// VectorSpaceSpec is immutable once registered. ID names the embedding space;
// Model, ModelRevision and Preprocessing are optional descriptions. Field defaults
// to "embedding" and Similarity to cosine. Callers supply compatible vectors.
type VectorSpaceSpec struct {
	ID            string           `json:"id"`
	Field         string           `json:"field"`
	Model         string           `json:"model"`
	ModelRevision string           `json:"model_revision"`
	Preprocessing string           `json:"preprocessing"`
	Dims          int              `json:"dims"`
	Similarity    VectorSimilarity `json:"similarity"`
}

func (s VectorSpaceSpec) validate() error {
	if len(s.ID) == 0 || len(s.ID) > 64 {
		return errors.New("space ID must contain 1 to 64 lowercase letters, digits, hyphens or underscores")
	}
	for _, ch := range s.ID {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return errors.New("invalid vector space ID")
		}
	}
	if s.Field == "" {
		return errors.New("vector space requires a field")
	}
	if s.Dims <= 0 {
		return ErrVectorInvalidDimension
	}
	if !validVectorSimilarity(s.Similarity) {
		return ErrVectorInvalidField
	}
	return nil
}

type DocumentRevision struct {
	ID       Identifier `json:"id"`
	Revision string     `json:"revision"`
}

type VectorRecord struct {
	ID       Identifier
	Revision string
	Vector   []float32
}

// VectorRevisionConflictError rejects an entire vector batch before publication.
type VectorRevisionConflictError struct{ IDs []Identifier }

func (e *VectorRevisionConflictError) Error() string {
	return fmt.Sprintf("%v: %v", ErrVectorRevisionConflict, e.IDs)
}
func (e *VectorRevisionConflictError) Unwrap() error { return ErrVectorRevisionConflict }

// Collection has one writer and any number of snapshot readers. All text changes
// must go through this object so revisions and chunk IDs remain authoritative.
// Mutations are serialized; existing readers can continue searching concurrently.
type Collection struct {
	mu             sync.Mutex
	config         CollectionConfig
	corpus         string
	writer         *Writer
	lock           lock.LockedFile
	spaces         map[string]collectionSpace
	revisions      map[Identifier]string
	closed         bool
	writeErr       error
	maintenanceErr error
}

func OpenCollection(config CollectionConfig) (*Collection, error) {
	config, err := config.normalized()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(config.Path, 0o755); err != nil {
		return nil, err
	}
	l, err := lock.OpenExclusive(filepath.Join(config.Path, "collection.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = l.Close()
		}
	}()
	manifest, err := loadCollectionManifest(config, true)
	if err != nil {
		return nil, err
	}
	spaces, err := loadCollectionSpaces(config.Path, manifest.CorpusID)
	if err != nil {
		return nil, err
	}
	w, err := OpenWriter(config.TextConfig)
	if err != nil {
		return nil, err
	}
	r, err := w.Reader()
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	revisions, scanErr := collectionRevisions(context.Background(), r, manifest.CorpusID)
	err = errors.Join(scanErr, r.Close())
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	c := &Collection{config: config, corpus: manifest.CorpusID, writer: w,
		lock: l, spaces: spaces, revisions: revisions}
	_, c.maintenanceErr = maintainCollection(context.Background(), config, manifest.CorpusID)
	if errors.Is(c.maintenanceErr, ErrCollectionCorrupt) {
		_ = w.Close()
		return nil, c.maintenanceErr
	}
	failed = false
	return c, nil
}

func (c *Collection) check() error {
	if c.closed {
		return ErrCollectionClosed
	}
	if c.writeErr != nil {
		return fmt.Errorf("reopen collection after write failure: %w", c.writeErr)
	}
	return nil
}

// UpsertDocuments assigns fresh revisions unless inputRevisions supplies one
// nonempty embedding-input version per document. Reuse a version for metadata-only
// changes to preserve vectors; change it whenever embedding inputs change.
// Duplicate IDs, reserved fields and invalid revisions reject the whole batch.
func (c *Collection) UpsertDocuments(documents []*Document, inputRevisions ...string) ([]DocumentRevision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return nil, err
	}
	if len(inputRevisions) != 0 && len(inputRevisions) != len(documents) {
		return nil, errors.New("input revisions must match the document count")
	}
	for _, revision := range inputRevisions {
		if revision == "" {
			return nil, errors.New("empty embedding-input revision")
		}
	}
	batch := NewBatch()
	revisions := make([]DocumentRevision, 0, len(documents))
	seen := make(map[Identifier]struct{}, len(documents))
	for i, doc := range documents {
		id, err := validateCollectionDocument(doc)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate chunk ID %q", id)
		}
		seen[id] = struct{}{}
		var revision string
		if len(inputRevisions) != 0 {
			revision = inputRevisions[i]
		} else {
			revision, err = newCollectionToken()
			if err != nil {
				return nil, err
			}
		}
		copyDoc := append(Document(nil), (*doc)...)
		copyDoc.AddField(NewStoredOnlyField(collectionRevisionField, []byte(revision)))
		copyDoc.AddField(NewStoredOnlyField(collectionCorpusField, []byte(c.corpus)))
		batch.Update(id, &copyDoc)
		revisions = append(revisions, DocumentRevision{ID: id, Revision: revision})
	}
	if len(revisions) == 0 {
		return revisions, nil
	}
	if err := c.writer.Batch(batch); err != nil {
		c.writeErr = err
		return nil, err
	}
	for _, r := range revisions {
		c.revisions[r.ID] = r.Revision
	}
	return revisions, nil
}

func validateCollectionDocument(doc *Document) (Identifier, error) {
	if doc == nil {
		return "", errors.New("nil collection document")
	}
	id, found, err := documentIdentifier(doc)
	if err != nil {
		return "", err
	}
	if !found || id == "" {
		return "", errors.New("collection document requires a nonempty _id")
	}
	idFields := 0
	for _, f := range *doc {
		if f.Name() == _idField {
			idFields++
			if !f.Index() || !f.Store() {
				return "", errors.New("collection _id must be indexed and stored")
			}
		}
		if strings.HasPrefix(f.Name(), "_collection_") {
			return "", fmt.Errorf("reserved field %q", f.Name())
		}
		if _, ok := f.(interface{ VectorValue() []float32 }); ok {
			return "", errors.New("write collection vectors with UpsertVectors")
		}
	}
	if idFields != 1 {
		return "", errors.New("collection document requires exactly one _id field")
	}
	return id, nil
}

func (c *Collection) DeleteDocuments(ids ...Identifier) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	batch := NewBatch()
	for _, id := range ids {
		if id == "" {
			return errors.New("empty chunk ID")
		}
		batch.Delete(id)
	}
	if err := c.writer.Batch(batch); err != nil {
		c.writeErr = err
		return err
	}
	for _, id := range ids {
		delete(c.revisions, id)
	}
	return nil
}

func (c *Collection) CreateVectorSpace(spec VectorSpaceSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return err
	}
	if spec.Field == "" {
		spec.Field = collectionVectorField
	}
	if spec.Similarity == "" {
		spec.Similarity = VectorCosine
	}
	if err := spec.validate(); err != nil {
		return err
	}
	if existing, ok := c.spaces[spec.ID]; ok {
		if existing.Spec == spec {
			return nil
		}
		return errors.New("vector space specification is immutable; choose a new ID")
	}
	dir := collectionSpacePath(c.config.Path, spec.ID)
	gate, err := lockCollectionCoordinator(context.Background(), c.config.Path)
	if err != nil {
		return err
	}
	defer gate.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	space := collectionSpace{Version: collectionFormatVersion, CorpusID: c.corpus, Spec: spec, Generations: make([]uint64, 0)}
	if err := writeCollectionSpace(dir, &space); err != nil {
		return err
	}
	c.spaces[spec.ID] = space
	return nil
}

func (c *Collection) VectorSpaces() ([]VectorSpaceSpec, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return nil, err
	}
	return sortedCollectionSpecs(c.spaces), nil
}

func sortedCollectionSpecs(spaces map[string]collectionSpace) []VectorSpaceSpec {
	rv := make([]VectorSpaceSpec, 0, len(spaces))
	for _, s := range spaces {
		rv = append(rv, s.Spec)
	}
	sort.Slice(rv, func(i, j int) bool { return rv[i].ID < rv[j].ID })
	return rv
}

// Reader pins the selected spaces and a text snapshot. No names selects all spaces.
func (c *Collection) Reader(spaceIDs ...string) (*CollectionReader, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return nil, err
	}
	spaces, err := selectCollectionSpaces(c.spaces, spaceIDs)
	if err != nil {
		return nil, err
	}
	gate, err := lockCollectionCoordinator(context.Background(), c.config.Path)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	r, err := c.writer.Reader()
	if err != nil {
		return nil, err
	}
	return newCollectionReader(c.config, c.corpus, r, spaces)
}

func (c *Collection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return errors.Join(c.writer.Close(), c.lock.Close())
}

func newCollectionToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func collectionCatalogHash(space collectionSpace) (string, error) {
	space.CatalogSHA256 = ""
	b, err := json.Marshal(space)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func writeCollectionSpace(dir string, space *collectionSpace) error {
	if space.Generations == nil {
		return fmt.Errorf("%w: cannot publish a nil generation catalog", ErrCollectionCorrupt)
	}
	var err error
	space.CatalogSHA256, err = collectionCatalogHash(*space)
	if err != nil {
		return err
	}
	return replaceCollectionJSON(filepath.Join(dir, "spec.json"), space)
}

func collectionCatalogGenerations(dir string, space collectionSpace) ([]uint64, error) {
	gens := space.Generations
	if gens == nil {
		return nil, fmt.Errorf("%w: missing generation catalog for %s", ErrCollectionCorrupt, space.Spec.ID)
	}
	hash, err := collectionCatalogHash(space)
	if err != nil {
		return nil, err
	}
	if hash != space.CatalogSHA256 {
		return nil, fmt.Errorf("%w: generation catalog checksum for %s", ErrCollectionCorrupt, space.Spec.ID)
	}
	var previous uint64
	for _, n := range gens {
		if n <= previous {
			return nil, fmt.Errorf("%w: unordered generation catalog for %s", ErrCollectionCorrupt, space.Spec.ID)
		}
		previous = n
		_, _, commit := collectionGenerationFiles(dir, "", n)
		info, err := os.Stat(commit)
		if err != nil {
			return nil, fmt.Errorf("%w: catalog references missing commit %s: %w", ErrCollectionCorrupt, commit, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: non-file commit %s", ErrCollectionCorrupt, commit)
		}
	}
	return gens, nil
}

// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"fmt"
	"os"
	"path/filepath"
)

func writeCollectionSpace(dir string, space *collectionSpace) error {
	return replaceCollectionJSON(filepath.Join(dir, "spec.json"), space)
}

func collectionCatalogGenerations(dir string, space collectionSpace) ([]uint64, error) {
	if space.Generations == nil {
		return nil, fmt.Errorf("%w: missing generations", ErrCollectionCorrupt)
	}
	var previous uint64
	for _, generation := range space.Generations {
		if generation <= previous {
			return nil, fmt.Errorf("%w: unordered generations", ErrCollectionCorrupt)
		}
		previous = generation
		indexPath, metadataPath := collectionGenerationFiles(dir, generation)
		for _, path := range []string{indexPath, metadataPath} {
			info, err := os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrCollectionCorrupt, err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%w: non-file payload", ErrCollectionCorrupt)
			}
		}
	}
	return space.Generations, nil
}

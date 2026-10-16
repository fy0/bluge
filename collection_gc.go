// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fy0/bluge/index/lock"
)

// CollectionMaintenance reports completed removals, including partial progress
// when an error is returned. Bytes are logical file sizes, not allocated blocks.
type CollectionMaintenance struct {
	// RemovedGenerations counts retired commits. On error, payload deletion may
	// still be pending; RemovedBytes only counts completed file removals.
	RemovedGenerations int
	// PinnedGenerations counts reader-held generations older than the current one.
	PinnedGenerations int
	RemovedFiles      int
	RemovedBytes      int64
}

// Maintain keeps the current generation and generations held by readers, and
// removes abandoned publication files. No names selects all spaces.
// It checks catalogs and file presence; use VerifyCollection to check contents.
// A failed pass is safe to retry. It never repairs corrupt commits.
func (c *Collection) Maintain(ctx context.Context, spaceIDs ...string) (CollectionMaintenance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return CollectionMaintenance{}, err
	}
	result, err := maintainCollection(ctx, c.config, c.corpus, spaceIDs...)
	c.maintenanceErr = err
	return result, err
}

// LastMaintenanceError reports the last automatic or explicit maintenance error.
// UpsertVectors returns success once committed, even if subsequent cleanup fails.
func (c *Collection) LastMaintenanceError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maintenanceErr
}

// MaintainCollection is the offline maintenance entry point. It requires the
// collection writer lock; use Collection.Maintain when a writer is already open.
func MaintainCollection(ctx context.Context, config CollectionConfig, spaceIDs ...string) (CollectionMaintenance, error) {
	config, err := config.normalized()
	if err != nil {
		return CollectionMaintenance{}, err
	}
	l, err := lock.OpenExclusive(filepath.Join(config.Path, "collection.lock"), os.O_RDWR, 0)
	if err != nil {
		return CollectionMaintenance{}, err
	}
	defer l.Close()
	m, err := loadCollectionManifest(config, false)
	if err != nil {
		return CollectionMaintenance{}, err
	}
	return maintainCollection(ctx, config, m.CorpusID, spaceIDs...)
}

// VerifyCollection checks checksums and metadata for every retained generation in
// the named spaces, or all spaces if none are named. It does not modify data.
// The coordinator prevents publication and reclamation while verification runs.
func VerifyCollection(ctx context.Context, config CollectionConfig, spaceIDs ...string) error {
	config, err := config.normalized()
	if err != nil {
		return err
	}
	gate, err := lockCollectionCoordinator(ctx, config.Path)
	if err != nil {
		return err
	}
	defer gate.Close()
	m, err := loadCollectionManifest(config, false)
	if err != nil {
		return err
	}
	spaces, err := loadCollectionSpaces(config.Path, m.CorpusID, spaceIDs...)
	if err != nil {
		return err
	}
	for _, space := range spaces {
		for _, generation := range space.Generations {
			if err := collectionContextError(ctx); err != nil {
				return err
			}
			space.Generation = generation
			if _, err := loadCollectionVectorMetadata(config, space); err != nil {
				return fmt.Errorf("%w: verify %s generation %d: %w", ErrCollectionCorrupt, space.Spec.ID, generation, err)
			}
		}
	}
	return nil
}

// The coordinator inode is permanent. Hold it from discovery until all reader
// leases are acquired, and throughout retirement. A lease inode may only be
// removed under this coordinator after an exclusive lease was acquired.
func lockCollectionCoordinator(ctx context.Context, root string) (lock.LockedFile, error) {
	for {
		if err := collectionContextError(ctx); err != nil {
			return nil, err
		}
		l, err := lock.OpenExclusive(filepath.Join(root, "generations.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err == nil || !collectionLockBusy(err) {
			return l, err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func collectionLeasePath(dir string, generation uint64) string {
	return filepath.Join(dir, fmt.Sprintf("lease-%020d.lock", generation))
}

func pinCollectionSpaces(config CollectionConfig, spaces map[string]collectionSpace) ([]lock.LockedFile, error) {
	var leases []lock.LockedFile
	for _, space := range spaces {
		if space.Generation == 0 {
			continue
		}
		path := collectionLeasePath(collectionSpacePath(config.Path, space.Spec.ID), space.Generation)
		l, err := lock.OpenShared(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, errors.Join(err, closeCollectionLeases(leases))
		}
		leases = append(leases, l)
	}
	return leases, nil
}

func closeCollectionLeases(leases []lock.LockedFile) error {
	var err error
	for _, l := range leases {
		err = errors.Join(err, l.Close())
	}
	return err
}

func collectionFileGeneration(name string) (uint64, bool, error) {
	var raw string
	switch {
	case strings.HasPrefix(name, "commit-") && strings.HasSuffix(name, ".json"):
		raw = strings.TrimSuffix(strings.TrimPrefix(name, "commit-"), ".json")
	case strings.HasPrefix(name, "metadata-") && strings.HasSuffix(name, ".json"):
		raw = strings.TrimSuffix(strings.TrimPrefix(name, "metadata-"), ".json")
	case strings.HasPrefix(name, "index-") && (strings.HasSuffix(name, ".flat") || strings.HasSuffix(name, ".usearch")):
		raw = strings.TrimSuffix(strings.TrimPrefix(name, "index-"), filepath.Ext(name))
	case strings.HasPrefix(name, "lease-") && strings.HasSuffix(name, ".lock"):
		raw = strings.TrimSuffix(strings.TrimPrefix(name, "lease-"), ".lock")
	default:
		return 0, false, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || len(raw) != 20 || n == 0 {
		return 0, true, fmt.Errorf("%w: invalid generation filename %q", ErrCollectionCorrupt, name)
	}
	return n, true, nil
}

func maintainCollection(ctx context.Context, config CollectionConfig, corpus string, spaceIDs ...string) (result CollectionMaintenance, retErr error) {
	gate, err := lockCollectionCoordinator(ctx, config.Path)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, gate.Close()) }()
	manifest, err := loadCollectionManifest(config, false)
	if err != nil {
		return result, fmt.Errorf("%w: collection manifest: %w", ErrCollectionCorrupt, err)
	}
	if manifest.CorpusID != corpus {
		return result, fmt.Errorf("%w: collection manifest changed", ErrCollectionCorrupt)
	}
	spaces, err := loadCollectionSpaces(config.Path, corpus, spaceIDs...)
	if err != nil {
		return result, err
	}
	// Check publication records before changing any selected space. Hashing large
	// payloads belongs to explicit verification and lazy loading, not reclamation.
	for _, space := range spaces {
		for _, n := range space.Generations {
			if err := collectionContextError(ctx); err != nil {
				return result, err
			}
			space.Generation = n
			if _, err := readCollectionCommit(config, space); err != nil {
				return result, err
			}
		}
	}
	for _, spec := range sortedCollectionSpecs(spaces) {
		space := spaces[spec.ID]
		dir := collectionSpacePath(config.Path, spec.ID)
		retire := make(map[uint64]bool)
		var leases []lock.LockedFile
		for _, n := range space.Generations {
			if n == space.Generation {
				continue
			}
			if err := collectionContextError(ctx); err != nil {
				return result, errors.Join(err, closeCollectionLeases(leases))
			}
			lease, err := lock.OpenExclusive(collectionLeasePath(dir, n), os.O_CREATE|os.O_RDWR, 0o600)
			if collectionLockBusy(err) {
				result.PinnedGenerations++
				continue
			}
			if err != nil {
				return result, errors.Join(err, closeCollectionLeases(leases))
			}
			leases = append(leases, lease)
			retire[n] = true
		}
		if len(retire) == 0 {
			continue
		}
		generations := space.Generations
		space.Generations = make([]uint64, 0, len(generations)-len(retire))
		for _, n := range generations {
			if !retire[n] {
				space.Generations = append(space.Generations, n)
			}
		}
		// This durable catalog replacement is the retirement point. Any files
		// left behind afterwards are unreferenced, including old commit files.
		if err := writeCollectionSpace(dir, &space); err != nil {
			return result, errors.Join(err, closeCollectionLeases(leases))
		}
		spaces[space.Spec.ID] = space
		result.RemovedGenerations += len(retire)
		for _, n := range generations {
			if !retire[n] {
				continue
			}
			_, _, commit := collectionGenerationFiles(dir, config.VectorStorage, n)
			retired := filepath.Join(dir, ".retired-"+filepath.Base(commit))
			err = publishCollectionFile(commit, retired)
			if err != nil {
				return result, errors.Join(err, closeCollectionLeases(leases))
			}
		}
		if err := closeCollectionLeases(leases); err != nil {
			return result, err
		}
	}
	// The writer lock excludes active staging writes. The coordinator excludes new
	// readers. Retirements are durable before deleting now-uncommitted payloads.
	var dirs []string
	if len(spaceIDs) != 0 {
		for id := range spaces {
			dirs = append(dirs, collectionSpacePath(config.Path, id))
		}
	} else {
		dirs = append(dirs, config.Path)
		entries, err := os.ReadDir(filepath.Join(config.Path, "vectors"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "space-") {
				dirs = append(dirs, filepath.Join(config.Path, "vectors", e.Name()))
			}
		}
	}
	for _, dir := range dirs {
		committed := make(map[uint64]bool)
		if dir != config.Path {
			for _, n := range spaces[strings.TrimPrefix(filepath.Base(dir), "space-")].Generations {
				committed[n] = true
			}
		}
		if err := sweepCollectionDirectory(ctx, dir, committed, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func sweepCollectionDirectory(ctx context.Context, dir string, committed map[uint64]bool, result *CollectionMaintenance) error {
	if err := syncCollectionDirectory(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	checked := make(map[uint64]bool)
	for _, e := range entries {
		n, recognized, err := collectionFileGeneration(e.Name())
		if err != nil {
			return err
		}
		if recognized && !committed[n] && !checked[n] {
			checked[n] = true
			lease, err := lock.OpenExclusive(collectionLeasePath(dir, n), os.O_RDWR, 0)
			if collectionLockBusy(err) {
				return fmt.Errorf("%w: reader holds unreferenced generation %d", ErrCollectionCorrupt, n)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if lease != nil {
				if err := lease.Close(); err != nil {
					return err
				}
			}
		}
	}
	for _, e := range entries {
		if err := collectionContextError(ctx); err != nil {
			return err
		}
		n, recognized, _ := collectionFileGeneration(e.Name())
		if !(recognized && !committed[n]) && !strings.HasPrefix(e.Name(), ".pending-") && !strings.HasPrefix(e.Name(), ".retired-") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		var count int
		var size int64
		err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				count++
				size += info.Size()
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		result.RemovedFiles += count
		result.RemovedBytes += size
	}
	return syncCollectionDirectory(dir)
}

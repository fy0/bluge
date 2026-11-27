// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

//go:build !windows

package fsatomic

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func Publish(source, target string) error {
	// #nosec G703 -- Source and target belong to the configured index directory.
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(target))
}

func SyncDir(path string) error {
	// #nosec G703 -- Directory paths come from the configured index.
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func Replace(source, target string) error {
	return Publish(source, target)
}

func LockBusy(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

func ReadConflict(err error) bool {
	return LockBusy(err)
}

// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

//go:build !windows

package bluge

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func publishCollectionFile(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return syncCollectionDirectory(filepath.Dir(target))
}

func syncCollectionDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func replaceCollectionFile(source, target string) error {
	return publishCollectionFile(source, target)
}

func collectionLockBusy(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

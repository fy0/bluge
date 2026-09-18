// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

//go:build !windows

package bluge

import (
	"errors"
	"os"
	"path/filepath"
)

func publishCollectionFile(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

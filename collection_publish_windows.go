// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import "golang.org/x/sys/windows"

func publishCollectionFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}

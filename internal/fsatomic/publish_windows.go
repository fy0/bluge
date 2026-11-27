// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package fsatomic

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func Publish(source, target string) error {
	return moveFile(source, target, windows.MOVEFILE_WRITE_THROUGH)
}

func Replace(source, target string) error {
	return moveFile(source, target, windows.MOVEFILE_WRITE_THROUGH|windows.MOVEFILE_REPLACE_EXISTING)
}

func moveFile(source, target string, flags uint32) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	// NTFS may briefly reject replacement despite FILE_SHARE_DELETE.
	// A failed MoveFileEx leaves both names unchanged.
	for attempt := 0; ; attempt++ {
		err := windows.MoveFileEx(from, to, flags)
		if err == nil {
			return nil
		}
		if attempt == 8 || (!errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION)) {
			return &os.LinkError{Op: "MoveFileEx", Old: source, New: target, Err: err}
		}
		time.Sleep(time.Millisecond << min(attempt, 5))
	}
}

// Windows does not support fsync on directory handles through os.File.Sync.
func SyncDir(string) error { return nil }

func LockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func ReadConflict(err error) bool {
	// CreateFile can report access denied while a file is pending deletion.
	return LockBusy(err) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

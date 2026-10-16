// Copyright (c) 2026 The Bluge Authors.
// Licensed under the Apache License, Version 2.0.

package bluge

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func publishCollectionFile(source, target string) error {
	return moveCollectionFile(source, target, windows.MOVEFILE_WRITE_THROUGH)
}

func replaceCollectionFile(source, target string) error {
	return moveCollectionFile(source, target, windows.MOVEFILE_WRITE_THROUGH|windows.MOVEFILE_REPLACE_EXISTING)
}

func moveCollectionFile(source, target string, flags uint32) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	// A scanner or a handle to the just-replaced catalog can temporarily keep
	// NTFS from replacing the name again, even with FILE_SHARE_DELETE. Failed
	// MoveFileEx calls leave both names unchanged, so bounded retries are safe.
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

// MoveFileEx with WRITE_THROUGH durably orders retirement before deletion.
// Windows does not support fsync on directory handles through os.File.Sync.
func syncCollectionDirectory(string) error { return nil }

func collectionLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

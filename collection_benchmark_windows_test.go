//go:build windows

package bluge

import (
	"errors"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type storageBenchInstant int64

const storageBenchClock = "Windows QueryPerformanceCounter"

var storageBenchCounter = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
var storageBenchFrequency = func() int64 {
	var value int64
	frequency := windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	if ok, _, err := frequency.Call(uintptr(unsafe.Pointer(&value))); ok == 0 || value <= 0 {
		panic(err)
	}
	return value
}()

func storageBenchNow() storageBenchInstant {
	var value int64
	if ok, _, err := storageBenchCounter.Call(uintptr(unsafe.Pointer(&value))); ok == 0 {
		panic(err)
	}
	return storageBenchInstant(value)
}

func storageBenchElapsedMS(start storageBenchInstant) float64 {
	return float64(storageBenchNow()-start) * 1000 / float64(storageBenchFrequency)
}

// Observation must not block the atomic spec replacement being measured.
func storageBenchReadCatalog(path string) ([]byte, error) {
	file, err := storageBenchOpenShared(path, windows.FILE_ATTRIBUTE_NORMAL)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(file)
	return data, errors.Join(readErr, file.Close())
}

func storageBenchReadDir(path string) ([]os.DirEntry, error) {
	file, err := storageBenchOpenShared(path, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return nil, err
	}
	entries, readErr := file.ReadDir(-1)
	return entries, errors.Join(readErr, file.Close())
}

func storageBenchOpenShared(path string, flags uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

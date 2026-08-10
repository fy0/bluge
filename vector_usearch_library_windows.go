//go:build windows && (amd64 || arm64)

package bluge

import "syscall"

func usearchLibraryName() string {
	return "vector_engine.dll"
}

func openUSearchLibrary(path string) (uintptr, error) {
	handle, err := syscall.LoadLibrary(path)
	return uintptr(handle), err
}

func closeUSearchLibrary(handle uintptr) error {
	return syscall.FreeLibrary(syscall.Handle(handle))
}

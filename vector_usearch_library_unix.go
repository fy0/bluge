//go:build (linux || darwin) && (amd64 || arm64)

package bluge

import (
	"runtime"

	"github.com/ebitengine/purego"
)

func usearchLibraryName() string {
	if runtime.GOOS == "darwin" {
		return "libvector_engine.dylib"
	}
	return "libvector_engine.so"
}

func openUSearchLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

func closeUSearchLibrary(handle uintptr) error {
	return purego.Dlclose(handle)
}

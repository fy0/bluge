//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

const usearchLibraryPathEnv = "BLUGE_USEARCH_LIBRARY_PATH"
const usearchABIVersion = 1

var (
	usearchLibraryMu sync.Mutex
	usearchLibraries = make(map[string]*puregoUSearchAPI)
)

// puregoUSearchAPI mirrors the narrow C ABI exported by vector_engine. The
// platform-specific files only open and close the native library; purego owns
// symbol resolution and C calling-convention adaptation on every platform.
type puregoUSearchAPI struct {
	handle uintptr

	abiVersion                      func() uint32
	hardwareAccelerationCompiledFn  func() string
	hardwareAccelerationAvailableFn func() string
	indexCreate                     func(uintptr, uint32, uintptr, uintptr, uintptr) unsafe.Pointer
	indexOpen                       func(string) unsafe.Pointer
	indexOpenBuffer                 func(unsafe.Pointer, uintptr) unsafe.Pointer
	indexDestroy                    func(unsafe.Pointer)
	indexDimensions                 func(unsafe.Pointer) uintptr
	indexSize                       func(unsafe.Pointer) uintptr
	indexSerializedLength           func(unsafe.Pointer) uintptr
	indexSaveBuffer                 func(unsafe.Pointer, unsafe.Pointer, uintptr) int32
	indexReserve                    func(unsafe.Pointer, uintptr) int32
	indexAdd                        func(unsafe.Pointer, uint64, unsafe.Pointer, uintptr) int32
	indexGet                        func(unsafe.Pointer, uint64, unsafe.Pointer, uintptr, *uintptr) int32
	indexRemove                     func(unsafe.Pointer, uint64) int32
	indexCompact                    func(unsafe.Pointer) int32
	indexSave                       func(unsafe.Pointer, string) int32
	indexSearch                     func(unsafe.Pointer, unsafe.Pointer, uintptr, uintptr, unsafe.Pointer, unsafe.Pointer, *uintptr) int32
	indexLastError                  func(unsafe.Pointer, unsafe.Pointer, uintptr) uintptr
}

func loadUSearchAPI(path string) (usearchNativeAPI, error) {
	candidates := usearchLibraryCandidates(path)
	usearchLibraryMu.Lock()
	defer usearchLibraryMu.Unlock()

	var attempts []string
	for _, candidate := range candidates {
		key := candidate
		if absolute, err := filepath.Abs(candidate); err == nil {
			key = absolute
		}
		if api := usearchLibraries[key]; api != nil {
			return api, nil
		}

		handle, err := openUSearchLibrary(candidate)
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		api := &puregoUSearchAPI{handle: handle}
		if err = api.register(); err != nil {
			_ = closeUSearchLibrary(handle)
			attempts = append(attempts, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		if version := api.abiVersion(); int(version) != usearchABIVersion {
			_ = closeUSearchLibrary(handle)
			attempts = append(attempts, fmt.Sprintf("%s: unsupported ABI version %d", candidate, version))
			continue
		}
		usearchLibraries[key] = api
		return api, nil
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("usearch native backend has no library candidates for %s/%s",
			runtime.GOOS, runtime.GOARCH)
	}
	return nil, fmt.Errorf("failed to load usearch C ABI library; set %s; attempts: %s",
		usearchLibraryPathEnv, strings.Join(attempts, "; "))
}

func (l *puregoUSearchAPI) register() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("register symbols: %v", recovered)
		}
	}()

	register := func(function any, name string) {
		purego.RegisterLibFunc(function, l.handle, name)
	}
	register(&l.abiVersion, "bluge_usearch_abi_version")
	register(&l.indexCreate, "bluge_usearch_index_create")
	register(&l.indexOpen, "bluge_usearch_index_open")
	register(&l.indexOpenBuffer, "bluge_usearch_index_open_buffer")
	register(&l.indexDestroy, "bluge_usearch_index_destroy")
	register(&l.indexDimensions, "bluge_usearch_index_dimensions")
	register(&l.indexSize, "bluge_usearch_index_size")
	register(&l.indexSerializedLength, "bluge_usearch_index_serialized_length")
	register(&l.indexSaveBuffer, "bluge_usearch_index_save_buffer")
	register(&l.indexReserve, "bluge_usearch_index_reserve")
	register(&l.indexAdd, "bluge_usearch_index_add")
	register(&l.indexGet, "bluge_usearch_index_get")
	register(&l.indexRemove, "bluge_usearch_index_remove")
	register(&l.indexCompact, "bluge_usearch_index_compact")
	register(&l.indexSave, "bluge_usearch_index_save")
	register(&l.indexSearch, "bluge_usearch_index_search")
	register(&l.indexLastError, "bluge_usearch_index_last_error")

	registerOptionalUSearchFunc(&l.hardwareAccelerationCompiledFn, l.handle,
		"bluge_usearch_hardware_acceleration_compiled")
	registerOptionalUSearchFunc(&l.hardwareAccelerationAvailableFn, l.handle,
		"bluge_usearch_hardware_acceleration_available")
	return nil
}

func registerOptionalUSearchFunc(function any, handle uintptr, name string) (registered bool) {
	defer func() {
		if recover() != nil {
			registered = false
		}
	}()
	purego.RegisterLibFunc(function, handle, name)
	return true
}

func usearchLibraryCandidates(explicit string) []string {
	seen := make(map[string]struct{})
	var candidates []string
	appendCandidate := func(candidate string) {
		if candidate == "" {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}
	appendCandidate(explicit)
	if explicit != "" {
		return candidates
	}
	appendCandidate(os.Getenv(usearchLibraryPathEnv))
	libraryName := usearchLibraryName()
	if executable, err := os.Executable(); err == nil {
		appendCandidate(filepath.Join(filepath.Dir(executable), libraryName))
	}
	appendCandidate(libraryName)
	return candidates
}

func vectorPointer(vector []float32) unsafe.Pointer {
	if len(vector) == 0 {
		return nil
	}
	return unsafe.Pointer(&vector[0])
}

func bytePointer(buffer []byte) unsafe.Pointer {
	if len(buffer) == 0 {
		return nil
	}
	return unsafe.Pointer(&buffer[0])
}

func (l *puregoUSearchAPI) create(dimensions, metric, connectivity,
	expansionAdd, expansionSearch uintptr) unsafe.Pointer {
	return l.indexCreate(dimensions, uint32(metric), connectivity, expansionAdd, expansionSearch)
}

func (l *puregoUSearchAPI) open(path string) unsafe.Pointer {
	return l.indexOpen(path)
}

func (l *puregoUSearchAPI) openBuffer(data []byte) unsafe.Pointer {
	if len(data) == 0 {
		return nil
	}
	result := l.indexOpenBuffer(bytePointer(data), uintptr(len(data)))
	runtime.KeepAlive(data)
	return result
}

func (l *puregoUSearchAPI) destroy(handle unsafe.Pointer) {
	if handle != nil {
		l.indexDestroy(handle)
	}
}

func (l *puregoUSearchAPI) dimensions(handle unsafe.Pointer) int {
	return int(l.indexDimensions(handle))
}

func (l *puregoUSearchAPI) size(handle unsafe.Pointer) int {
	return int(l.indexSize(handle))
}

func (l *puregoUSearchAPI) serializedLength(handle unsafe.Pointer) int {
	return int(l.indexSerializedLength(handle))
}

func (l *puregoUSearchAPI) saveBuffer(handle unsafe.Pointer, output []byte) int32 {
	status := l.indexSaveBuffer(handle, bytePointer(output), uintptr(len(output)))
	runtime.KeepAlive(output)
	return status
}

func (l *puregoUSearchAPI) reserve(handle unsafe.Pointer, capacity int) int32 {
	return l.indexReserve(handle, uintptr(capacity))
}

func (l *puregoUSearchAPI) add(handle unsafe.Pointer, key uint64, vector []float32) int32 {
	status := l.indexAdd(handle, key, vectorPointer(vector), uintptr(len(vector)))
	runtime.KeepAlive(vector)
	return status
}

func (l *puregoUSearchAPI) get(handle unsafe.Pointer, key uint64, vector []float32) int32 {
	var resultCount uintptr
	status := l.indexGet(handle, key, vectorPointer(vector), uintptr(len(vector)), &resultCount)
	runtime.KeepAlive(vector)
	if status != 0 {
		return status
	}
	// USearch returns the number of vectors written, not scalar values. This
	// call requests exactly one vector regardless of its dimensions.
	if resultCount != 1 {
		return -1
	}
	return 0
}

func (l *puregoUSearchAPI) remove(handle unsafe.Pointer, key uint64) int32 {
	return l.indexRemove(handle, key)
}

func (l *puregoUSearchAPI) compact(handle unsafe.Pointer) int32 {
	return l.indexCompact(handle)
}

func (l *puregoUSearchAPI) save(handle unsafe.Pointer, path string) int32 {
	return l.indexSave(handle, path)
}

func (l *puregoUSearchAPI) search(handle unsafe.Pointer, query []float32, count int,
	keys []uint64, distances []float32) (int32, int) {
	var resultCount uintptr
	status := l.indexSearch(handle, vectorPointer(query), uintptr(len(query)), uintptr(count),
		unsafe.Pointer(&keys[0]), unsafe.Pointer(&distances[0]), &resultCount)
	runtime.KeepAlive(query)
	runtime.KeepAlive(keys)
	runtime.KeepAlive(distances)
	return status, int(resultCount)
}

func (l *puregoUSearchAPI) errorMessage(handle unsafe.Pointer) string {
	buffer := make([]byte, 4096)
	length := l.errorMessageInto(handle, buffer)
	if length > len(buffer) {
		buffer = make([]byte, length)
		length = l.errorMessageInto(handle, buffer)
	}
	if length > len(buffer) {
		length = len(buffer)
	}
	return string(buffer[:length])
}

func (l *puregoUSearchAPI) errorMessageInto(handle unsafe.Pointer, buffer []byte) int {
	length := l.indexLastError(handle, bytePointer(buffer), uintptr(len(buffer)))
	runtime.KeepAlive(buffer)
	return int(length)
}

func (l *puregoUSearchAPI) hardwareAccelerationCompiled() string {
	if l.hardwareAccelerationCompiledFn == nil {
		return ""
	}
	return l.hardwareAccelerationCompiledFn()
}

func (l *puregoUSearchAPI) hardwareAccelerationAvailable() string {
	if l.hardwareAccelerationAvailableFn == nil {
		return ""
	}
	return l.hardwareAccelerationAvailableFn()
}

var _ usearchNativeAPI = (*puregoUSearchAPI)(nil)
var _ usearchHardwareAPI = (*puregoUSearchAPI)(nil)

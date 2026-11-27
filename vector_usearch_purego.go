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

const usearchABIVersion = 4

// USearch keeps one search context per concurrent call.
const usearchSearchThreads = 64

var (
	usearchLibraryMu sync.Mutex
	usearchLibraries = make(map[string]*puregoUSearchAPI)
)

type puregoUSearchAPI struct {
	handle uintptr

	handleGates sync.Map // unsafe.Pointer -> *usearchHandleGate

	abiVersionFunc                  func() uint32
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
	indexAddBatch                   func(unsafe.Pointer, unsafe.Pointer, unsafe.Pointer, uintptr, uintptr) int32
	indexGet                        func(unsafe.Pointer, uint64, unsafe.Pointer, uintptr, *uintptr) int32
	indexRemove                     func(unsafe.Pointer, uint64) int32
	indexRemoveBatch                func(unsafe.Pointer, unsafe.Pointer, uintptr) int32
	indexCompact                    func(unsafe.Pointer) int32
	indexSave                       func(unsafe.Pointer, string) int32
	indexSearch                     func(unsafe.Pointer, unsafe.Pointer, uintptr, uintptr,
		unsafe.Pointer, unsafe.Pointer, *uintptr) int32
	indexSearchFiltered func(unsafe.Pointer, unsafe.Pointer, uintptr, uintptr,
		unsafe.Pointer, uintptr, unsafe.Pointer, unsafe.Pointer, *uintptr) int32
	indexLastError func(unsafe.Pointer, unsafe.Pointer, uintptr) uintptr

	indexReserveThreads func(unsafe.Pointer, uintptr, uintptr) bool
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
		version := api.abiVersionFunc()
		if version != usearchABIVersion {
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
	register(&l.abiVersionFunc, "bluge_usearch_abi_version")
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
	register(&l.indexAddBatch, "bluge_usearch_index_add_batch")
	register(&l.indexGet, "bluge_usearch_index_get")
	register(&l.indexRemove, "bluge_usearch_index_remove")
	register(&l.indexRemoveBatch, "bluge_usearch_index_remove_batch")
	register(&l.indexCompact, "bluge_usearch_index_compact")
	register(&l.indexSave, "bluge_usearch_index_save")
	register(&l.indexSearch, "bluge_usearch_index_search")
	register(&l.indexSearchFiltered, "bluge_usearch_index_search_filtered")
	register(&l.indexLastError, "bluge_usearch_index_last_error")

	register(&l.indexReserveThreads, "bluge_usearch_index_reserve_threads")
	register(&l.hardwareAccelerationCompiledFn, "bluge_usearch_hardware_acceleration_compiled")
	register(&l.hardwareAccelerationAvailableFn, "bluge_usearch_hardware_acceleration_available")
	return nil
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

func uint64Pointer(values []uint64) unsafe.Pointer {
	if len(values) == 0 {
		return nil
	}
	return unsafe.Pointer(&values[0])
}

func (l *puregoUSearchAPI) create(dimensions uintptr, metric uint32, connectivity,
	expansionAdd, expansionSearch uintptr) unsafe.Pointer {
	return l.prepareHandle(l.indexCreate(dimensions, metric, connectivity, expansionAdd, expansionSearch))
}

func (l *puregoUSearchAPI) open(path string) unsafe.Pointer {
	return l.prepareHandle(l.indexOpen(path))
}

func (l *puregoUSearchAPI) openBuffer(data []byte) unsafe.Pointer {
	if len(data) == 0 {
		return nil
	}
	handle := l.indexOpenBuffer(bytePointer(data), uintptr(len(data)))
	runtime.KeepAlive(data)
	return l.prepareHandle(handle)
}

func (l *puregoUSearchAPI) prepareHandle(handle unsafe.Pointer) unsafe.Pointer {
	if handle == nil {
		return nil
	}
	if !l.indexReserveThreads(handle, l.indexSize(handle), usearchSearchThreads) {
		l.indexDestroy(handle)
		return nil
	}
	l.handleGates.Store(handle, newUsearchHandleGate(usearchSearchThreads))
	return handle
}

// Exclusive operations acquire every permit; the mutex prevents partial-drain deadlocks.
type usearchHandleGate struct {
	slots     chan struct{}
	exclusive sync.Mutex
}

func newUsearchHandleGate(limit int) *usearchHandleGate {
	return &usearchHandleGate{slots: make(chan struct{}, limit)}
}

func (l *puregoUSearchAPI) gateFor(handle unsafe.Pointer) *usearchHandleGate {
	gate, _ := l.handleGates.Load(handle)
	return gate.(*usearchHandleGate)
}

func (l *puregoUSearchAPI) acquire(handle unsafe.Pointer) func() {
	if handle == nil {
		return func() {}
	}
	gate := l.gateFor(handle)
	gate.slots <- struct{}{}
	return func() {
		<-gate.slots
	}
}

func (l *puregoUSearchAPI) acquireExclusive(handle unsafe.Pointer) func() {
	if handle == nil {
		return func() {}
	}
	gate := l.gateFor(handle)
	gate.exclusive.Lock()
	for i := 0; i < cap(gate.slots); i++ {
		gate.slots <- struct{}{}
	}
	return func() {
		for i := 0; i < cap(gate.slots); i++ {
			<-gate.slots
		}
		gate.exclusive.Unlock()
	}
}

func (l *puregoUSearchAPI) destroy(handle unsafe.Pointer) {
	if handle != nil {
		release := l.acquireExclusive(handle)
		l.indexDestroy(handle)
		l.handleGates.Delete(handle)
		release()
	}
}

func (l *puregoUSearchAPI) dimensions(handle unsafe.Pointer) int {
	release := l.acquire(handle)
	defer release()
	return int(l.indexDimensions(handle))
}

func (l *puregoUSearchAPI) size(handle unsafe.Pointer) int {
	release := l.acquire(handle)
	defer release()
	return int(l.indexSize(handle))
}

func (l *puregoUSearchAPI) serializedLength(handle unsafe.Pointer) int {
	release := l.acquire(handle)
	defer release()
	return int(l.indexSerializedLength(handle))
}

func (l *puregoUSearchAPI) saveBuffer(handle unsafe.Pointer, output []byte) int32 {
	release := l.acquireExclusive(handle)
	defer release()
	status := l.indexSaveBuffer(handle, bytePointer(output), uintptr(len(output)))
	runtime.KeepAlive(output)
	return status
}

func (l *puregoUSearchAPI) reserve(handle unsafe.Pointer, capacity int) int32 {
	release := l.acquireExclusive(handle)
	defer release()
	if l.indexReserveThreads(handle, uintptr(capacity), usearchSearchThreads) {
		return 0
	}
	return -1
}

func (l *puregoUSearchAPI) add(handle unsafe.Pointer, key uint64, vector []float32) int32 {
	release := l.acquire(handle)
	defer release()
	status := l.indexAdd(handle, key, vectorPointer(vector), uintptr(len(vector)))
	runtime.KeepAlive(vector)
	return status
}

func (l *puregoUSearchAPI) addBatch(handle unsafe.Pointer, keys []uint64,
	vectors []float32, dimensions int) int32 {
	release := l.acquire(handle)
	defer release()
	status := l.indexAddBatch(handle, uint64Pointer(keys), vectorPointer(vectors),
		uintptr(len(keys)), uintptr(dimensions))
	runtime.KeepAlive(keys)
	runtime.KeepAlive(vectors)
	return status
}

func (l *puregoUSearchAPI) get(handle unsafe.Pointer, key uint64, vector []float32) int32 {
	release := l.acquire(handle)
	defer release()
	var resultCount uintptr
	status := l.indexGet(handle, key, vectorPointer(vector), uintptr(len(vector)), &resultCount)
	runtime.KeepAlive(vector)
	if status == 0 && resultCount != 1 {
		return -1
	}
	return status
}

func (l *puregoUSearchAPI) remove(handle unsafe.Pointer, key uint64) int32 {
	release := l.acquire(handle)
	defer release()
	return l.indexRemove(handle, key)
}

func (l *puregoUSearchAPI) removeBatch(handle unsafe.Pointer, keys []uint64) int32 {
	release := l.acquire(handle)
	defer release()
	status := l.indexRemoveBatch(handle, uint64Pointer(keys), uintptr(len(keys)))
	runtime.KeepAlive(keys)
	return status
}

func (l *puregoUSearchAPI) compact(handle unsafe.Pointer) int32 {
	release := l.acquireExclusive(handle)
	defer release()
	if status := l.indexCompact(handle); status != 0 {
		return status
	}
	if l.indexReserveThreads(handle, l.indexSize(handle), usearchSearchThreads) {
		return 0
	}
	return -1
}

func (l *puregoUSearchAPI) save(handle unsafe.Pointer, path string) int32 {
	release := l.acquireExclusive(handle)
	defer release()
	return l.indexSave(handle, path)
}

func (l *puregoUSearchAPI) search(handle unsafe.Pointer, query []float32, count int,
	keys []uint64, distances []float32) (int32, int) {
	release := l.acquire(handle)
	defer release()
	var resultCount uintptr
	status := l.indexSearch(handle, vectorPointer(query), uintptr(len(query)), uintptr(count),
		uint64Pointer(keys), vectorPointer(distances), &resultCount)
	runtime.KeepAlive(query)
	runtime.KeepAlive(keys)
	runtime.KeepAlive(distances)
	return status, int(resultCount)
}

func (l *puregoUSearchAPI) searchFiltered(handle unsafe.Pointer, query []float32, count int,
	allowedKeys, keys []uint64, distances []float32) (int32, int) {
	release := l.acquire(handle)
	defer release()
	var resultCount uintptr
	status := l.indexSearchFiltered(handle, vectorPointer(query), uintptr(len(query)), uintptr(count),
		uint64Pointer(allowedKeys), uintptr(len(allowedKeys)), uint64Pointer(keys),
		vectorPointer(distances), &resultCount)
	runtime.KeepAlive(query)
	runtime.KeepAlive(allowedKeys)
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
	release := l.acquire(handle)
	defer release()
	length := l.indexLastError(handle, bytePointer(buffer), uintptr(len(buffer)))
	runtime.KeepAlive(buffer)
	return int(length)
}

func (l *puregoUSearchAPI) hardwareAccelerationCompiled() string {
	return l.hardwareAccelerationCompiledFn()
}

func (l *puregoUSearchAPI) hardwareAccelerationAvailable() string {
	return l.hardwareAccelerationAvailableFn()
}

var _ usearchNativeAPI = (*puregoUSearchAPI)(nil)
var _ usearchHardwareAPI = (*puregoUSearchAPI)(nil)

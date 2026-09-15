//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

const usearchLibraryPathEnv = "BLUGE_USEARCH_LIBRARY_PATH"

// The adapter negotiates an ABI version range so newer Go builds keep working
// with older native libraries. ABI 3 is the baseline contract; ABI 4 adds
// bluge_usearch_index_reserve_threads.
const (
	usearchMinABIVersion            = 3
	usearchABIVersionReserveThreads = 4
	usearchMaxABIVersion            = usearchABIVersionReserveThreads
)

// usearchSearchThreads bounds in-flight native calls per index handle on
// ABI >= 4 libraries, matching the size the search context pool is grown to.
// Every USearch context carries a visited set proportional to the segment
// size plus a per-thread cast buffer, so the value trades scratch memory for
// parallelism: 64 keeps one segment searchable from every core on wide
// machines while bounding scratch to a few megabytes per million-vector
// segment.
const usearchSearchThreads = 64

var (
	usearchLibraryMu sync.Mutex
	usearchLibraries = make(map[string]*puregoUSearchAPI)
)

// puregoUSearchAPI mirrors the narrow C ABI exported by vector_engine. The
// platform-specific files only open and close the native library; purego owns
// symbol resolution and C calling-convention adaptation on every platform.
type puregoUSearchAPI struct {
	handle uintptr

	// abiVersion is the version reported by bluge_usearch_abi_version at load
	// time. It gates entry points introduced after ABI 3.
	abiVersion uint32

	// USearch allocates a pool of search contexts per index. Concurrent calls
	// beyond the pool size fail with "Reserve capacity ahead of searches!".
	// ABI 3 libraries size the pool to hardware_concurrency at create/load
	// time and cannot grow it; ABI 4 libraries grow it to
	// usearchSearchThreads when the handle is prepared. Either way every
	// handle gets a semaphore sized to its pool so excess callers wait
	// instead of failing.
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

	// indexReserveThreads exists only on ABI >= 4 libraries. Nil on older
	// artifacts, where the context pool keeps its default size.
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
		if version < usearchMinABIVersion || version > usearchMaxABIVersion {
			_ = closeUSearchLibrary(handle)
			attempts = append(attempts, fmt.Sprintf("%s: unsupported ABI version %d", candidate, version))
			continue
		}
		api.abiVersion = version
		if version >= usearchABIVersionReserveThreads {
			// ABI 4 libraries always export reserve_threads; a missing symbol
			// degrades to the default pool size rather than a load failure.
			registerOptionalUSearchFunc(&api.indexReserveThreads, handle,
				"bluge_usearch_index_reserve_threads")
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

	registerOptionalUSearchFunc(&l.hardwareAccelerationCompiledFn, l.handle,
		"bluge_usearch_hardware_acceleration_compiled")
	registerOptionalUSearchFunc(&l.hardwareAccelerationAvailableFn, l.handle,
		"bluge_usearch_hardware_acceleration_available")
	return nil
}

func registerOptionalUSearchFunc(function any, handle uintptr, name string) {
	defer func() {
		_ = recover()
	}()
	purego.RegisterLibFunc(function, handle, name)
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
	handle := l.indexCreate(dimensions, metric, connectivity, expansionAdd, expansionSearch)
	if handle != nil {
		l.prepareHandle(handle)
	}
	return handle
}

func (l *puregoUSearchAPI) open(path string) unsafe.Pointer {
	handle := l.indexOpen(path)
	if handle != nil {
		l.prepareHandle(handle)
	}
	return handle
}

func (l *puregoUSearchAPI) openBuffer(data []byte) unsafe.Pointer {
	if len(data) == 0 {
		return nil
	}
	handle := l.indexOpenBuffer(bytePointer(data), uintptr(len(data)))
	runtime.KeepAlive(data)
	if handle != nil {
		l.prepareHandle(handle)
	}
	return handle
}

// prepareHandle grows the native context pool on ABI >= 4 libraries and
// installs the handle's concurrency gate. The handle is not shared yet, so
// the reservation needs no permit. When the library cannot grow the pool the
// gate keeps the default pool size, so the bound is never looser than the
// pool itself.
func (l *puregoUSearchAPI) prepareHandle(handle unsafe.Pointer) {
	limit := fallbackUsearchConcurrency()
	if l.indexReserveThreads != nil &&
		l.indexReserveThreads(handle, l.indexSize(handle), usearchSearchThreads) {
		limit = usearchSearchThreads
	}
	l.handleGates.Store(handle, newUsearchHandleGate(limit))
}

// fallbackUsearchConcurrency mirrors the default USearch context pool, which
// is sized to std::thread::hardware_concurrency when an index is created or
// loaded without an explicit thread count.
func fallbackUsearchConcurrency() int {
	if cpus := runtime.NumCPU(); cpus > 0 {
		return cpus
	}
	return 1
}

// usearchHandleGate bounds in-flight native calls on one index handle to the
// size of its USearch context pool. Ordinary operations take a single permit;
// operations that reallocate the pool or the stored vectors (reserve,
// compact, save, destroy) take every permit so they never overlap an
// in-flight call. exclusive serializes all-permit acquisitions against each
// other: two concurrent drains could otherwise each hold a subset of the
// permits and deadlock. inFlight and peak count in-flight calls for tests.
type usearchHandleGate struct {
	slots     chan struct{}
	exclusive sync.Mutex
	inFlight  atomic.Int32
	peak      atomic.Int32
}

func newUsearchHandleGate(limit int) *usearchHandleGate {
	if limit < 1 {
		limit = 1
	}
	return &usearchHandleGate{slots: make(chan struct{}, limit)}
}

func (g *usearchHandleGate) trackAcquire() {
	inFlight := g.inFlight.Add(1)
	for {
		peak := g.peak.Load()
		if inFlight <= peak || g.peak.CompareAndSwap(peak, inFlight) {
			return
		}
	}
}

// gateFor returns the concurrency gate for handle, installing a
// default-sized one when the handle predates gate tracking. The fallback is
// capped by the smallest pool the handle could have: hardware_concurrency on
// ABI 3, or a successfully grown pool on ABI 4.
func (l *puregoUSearchAPI) gateFor(handle unsafe.Pointer) *usearchHandleGate {
	if gate, ok := l.handleGates.Load(handle); ok {
		return gate.(*usearchHandleGate)
	}
	limit := fallbackUsearchConcurrency()
	if l.indexReserveThreads != nil && limit > usearchSearchThreads {
		limit = usearchSearchThreads
	}
	gate, _ := l.handleGates.LoadOrStore(handle, newUsearchHandleGate(limit))
	return gate.(*usearchHandleGate)
}

// acquire takes one context permit for the next native call on handle and
// returns the release function.
func (l *puregoUSearchAPI) acquire(handle unsafe.Pointer) func() {
	if handle == nil {
		return func() {}
	}
	gate := l.gateFor(handle)
	gate.slots <- struct{}{}
	gate.trackAcquire()
	return func() {
		gate.inFlight.Add(-1)
		<-gate.slots
	}
}

// acquireExclusive takes every context permit so the next native call on
// handle cannot overlap any in-flight call. Reserve, compaction,
// serialization, and destruction use it because they reallocate the
// structures the other calls read.
func (l *puregoUSearchAPI) acquireExclusive(handle unsafe.Pointer) func() {
	if handle == nil {
		return func() {}
	}
	gate := l.gateFor(handle)
	gate.exclusive.Lock()
	for i := 0; i < cap(gate.slots); i++ {
		gate.slots <- struct{}{}
	}
	gate.trackAcquire()
	return func() {
		gate.inFlight.Add(-1)
		for i := 0; i < cap(gate.slots); i++ {
			<-gate.slots
		}
		gate.exclusive.Unlock()
	}
}

// handlePeakInFlight reports the largest observed number of concurrent native
// calls on the handle. It exists so tests can prove the bounded-parallelism
// contract.
func (l *puregoUSearchAPI) handlePeakInFlight(handle unsafe.Pointer) int32 {
	if gate, ok := l.handleGates.Load(handle); ok {
		return gate.(*usearchHandleGate).peak.Load()
	}
	return 0
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
	if l.indexReserveThreads != nil {
		// A members-only reserve would reset the context pool to the
		// hardware default, so growth goes through reserve_threads to keep
		// the negotiated pool size.
		if l.indexReserveThreads(handle, uintptr(capacity), usearchSearchThreads) {
			return 0
		}
		return -1
	}
	return l.indexReserve(handle, uintptr(capacity))
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
	return l.indexCompact(handle)
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

//go:build windows

package bluge

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

const usearchLibraryPathEnv = "BLUGE_USEARCH_LIBRARY_PATH"
const usearchABIVersion = 1

var (
	usearchLibraryMu sync.Mutex
	usearchLibraries = make(map[string]*windowsUSearchAPI)
)

type windowsUSearchAPI struct {
	handle  uintptr
	symbols map[string]uintptr
}

var usearchSymbols = []string{
	"bluge_usearch_abi_version",
	"bluge_usearch_index_create",
	"bluge_usearch_index_open",
	"bluge_usearch_index_open_buffer",
	"bluge_usearch_index_destroy",
	"bluge_usearch_index_dimensions",
	"bluge_usearch_index_size",
	"bluge_usearch_index_serialized_length",
	"bluge_usearch_index_save_buffer",
	"bluge_usearch_index_reserve",
	"bluge_usearch_index_add",
	"bluge_usearch_index_get",
	"bluge_usearch_index_remove",
	"bluge_usearch_index_compact",
	"bluge_usearch_index_save",
	"bluge_usearch_index_search",
	"bluge_usearch_index_last_error",
}

var optionalUSearchSymbols = []string{
	"bluge_usearch_hardware_acceleration_compiled",
	"bluge_usearch_hardware_acceleration_available",
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

		handle, err := syscall.LoadLibrary(candidate)
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %v", candidate, err))
			continue
		}
		api := &windowsUSearchAPI{
			handle:  uintptr(handle),
			symbols: make(map[string]uintptr, len(usearchSymbols)),
		}
		missing := ""
		for _, symbol := range usearchSymbols {
			address, lookupErr := syscall.GetProcAddress(syscall.Handle(api.handle), symbol)
			if lookupErr != nil {
				missing = fmt.Sprintf("%s: %v", symbol, lookupErr)
				break
			}
			api.symbols[symbol] = address
		}
		if missing != "" {
			_ = syscall.FreeLibrary(syscall.Handle(api.handle))
			attempts = append(attempts, fmt.Sprintf("%s: missing symbol %s", candidate, missing))
			continue
		}
		for _, symbol := range optionalUSearchSymbols {
			if address, lookupErr := syscall.GetProcAddress(syscall.Handle(api.handle), symbol); lookupErr == nil {
				api.symbols[symbol] = address
			}
		}
		version, _, _ := purego.SyscallN(api.symbols["bluge_usearch_abi_version"])
		if int(version) != usearchABIVersion {
			_ = syscall.FreeLibrary(syscall.Handle(api.handle))
			attempts = append(attempts, fmt.Sprintf("%s: unsupported ABI version %d", candidate, version))
			continue
		}
		usearchLibraries[key] = api
		return api, nil
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("usearch native backend has no library candidates")
	}
	return nil, fmt.Errorf("failed to load usearch C ABI library; set %s; attempts: %s",
		usearchLibraryPathEnv, strings.Join(attempts, "; "))
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
	if executable, err := os.Executable(); err == nil {
		appendCandidate(filepath.Join(filepath.Dir(executable), "vector_engine.dll"))
	}
	appendCandidate("vector_engine.dll")
	return candidates
}

func (l *windowsUSearchAPI) call(name string, args ...uintptr) uintptr {
	result, _, _ := purego.SyscallN(l.symbols[name], args...)
	return result
}

func cString(value string) []byte {
	result := make([]byte, len(value)+1)
	copy(result, value)
	return result
}

func pointerFromResult(value uintptr) unsafe.Pointer {
	if value == 0 {
		return nil
	}
	return *(*unsafe.Pointer)(unsafe.Pointer(&value))
}

func pointerValue(value unsafe.Pointer) uintptr {
	return uintptr(value)
}

func vectorPointer(vector []float32) unsafe.Pointer {
	if len(vector) == 0 {
		return nil
	}
	return unsafe.Pointer(&vector[0])
}

func (l *windowsUSearchAPI) create(dimensions, metric, connectivity,
	expansionAdd, expansionSearch uintptr) unsafe.Pointer {
	return pointerFromResult(l.call("bluge_usearch_index_create", dimensions, metric,
		connectivity, expansionAdd, expansionSearch))
}

func (l *windowsUSearchAPI) open(path string) unsafe.Pointer {
	cPath := cString(path)
	result := pointerFromResult(l.call("bluge_usearch_index_open",
		uintptr(unsafe.Pointer(&cPath[0]))))
	runtime.KeepAlive(cPath)
	return result
}

func (l *windowsUSearchAPI) openBuffer(data []byte) unsafe.Pointer {
	if len(data) == 0 {
		return nil
	}
	result := pointerFromResult(l.call("bluge_usearch_index_open_buffer",
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data))))
	runtime.KeepAlive(data)
	return result
}

func (l *windowsUSearchAPI) destroy(handle unsafe.Pointer) {
	if handle != nil {
		l.call("bluge_usearch_index_destroy", pointerValue(handle))
	}
}

func (l *windowsUSearchAPI) dimensions(handle unsafe.Pointer) int {
	return int(l.call("bluge_usearch_index_dimensions", pointerValue(handle)))
}

func (l *windowsUSearchAPI) size(handle unsafe.Pointer) int {
	return int(l.call("bluge_usearch_index_size", pointerValue(handle)))
}

func (l *windowsUSearchAPI) serializedLength(handle unsafe.Pointer) int {
	return int(l.call("bluge_usearch_index_serialized_length", pointerValue(handle)))
}

func (l *windowsUSearchAPI) saveBuffer(handle unsafe.Pointer, output []byte) int32 {
	var outputPointer unsafe.Pointer
	if len(output) > 0 {
		outputPointer = unsafe.Pointer(&output[0])
	}
	status := int32(l.call("bluge_usearch_index_save_buffer", pointerValue(handle),
		uintptr(outputPointer), uintptr(len(output))))
	runtime.KeepAlive(output)
	return status
}

func (l *windowsUSearchAPI) reserve(handle unsafe.Pointer, capacity int) int32 {
	return int32(l.call("bluge_usearch_index_reserve", pointerValue(handle), uintptr(capacity)))
}

func (l *windowsUSearchAPI) add(handle unsafe.Pointer, key uint64, vector []float32) int32 {
	var status uintptr
	if unsafe.Sizeof(uintptr(0)) == 4 {
		status = l.call("bluge_usearch_index_add", pointerValue(handle), uintptr(key),
			uintptr(key>>32), uintptr(vectorPointer(vector)), uintptr(len(vector)))
	} else {
		status = l.call("bluge_usearch_index_add", pointerValue(handle), uintptr(key),
			uintptr(vectorPointer(vector)), uintptr(len(vector)))
	}
	runtime.KeepAlive(vector)
	return int32(status)
}

func (l *windowsUSearchAPI) get(handle unsafe.Pointer, key uint64, vector []float32) int32 {
	var resultCount uintptr
	var status uintptr
	if unsafe.Sizeof(uintptr(0)) == 4 {
		status = l.call("bluge_usearch_index_get", pointerValue(handle), uintptr(key),
			uintptr(key>>32), uintptr(vectorPointer(vector)), uintptr(len(vector)),
			uintptr(unsafe.Pointer(&resultCount)))
	} else {
		status = l.call("bluge_usearch_index_get", pointerValue(handle), uintptr(key),
			uintptr(vectorPointer(vector)), uintptr(len(vector)),
			uintptr(unsafe.Pointer(&resultCount)))
	}
	runtime.KeepAlive(vector)
	if status != 0 {
		return int32(status)
	}
	// USearch returns the number of vectors written, not the number of scalar
	// values. This call requests one vector, regardless of its dimension.
	if resultCount != 1 {
		return -1
	}
	return 0
}

func (l *windowsUSearchAPI) remove(handle unsafe.Pointer, key uint64) int32 {
	if unsafe.Sizeof(uintptr(0)) == 4 {
		return int32(l.call("bluge_usearch_index_remove", pointerValue(handle),
			uintptr(key), uintptr(key>>32)))
	}
	return int32(l.call("bluge_usearch_index_remove", pointerValue(handle), uintptr(key)))
}

func (l *windowsUSearchAPI) compact(handle unsafe.Pointer) int32 {
	return int32(l.call("bluge_usearch_index_compact", pointerValue(handle)))
}

func (l *windowsUSearchAPI) save(handle unsafe.Pointer, path string) int32 {
	cPath := cString(path)
	status := int32(l.call("bluge_usearch_index_save", pointerValue(handle),
		uintptr(unsafe.Pointer(&cPath[0]))))
	runtime.KeepAlive(cPath)
	return status
}

func (l *windowsUSearchAPI) search(handle unsafe.Pointer, query []float32, count int,
	keys []uint64, distances []float32) (int32, int) {
	var resultCount uintptr
	status := int32(l.call("bluge_usearch_index_search", pointerValue(handle),
		uintptr(vectorPointer(query)), uintptr(len(query)), uintptr(count),
		uintptr(unsafe.Pointer(&keys[0])), uintptr(unsafe.Pointer(&distances[0])),
		uintptr(unsafe.Pointer(&resultCount))))
	runtime.KeepAlive(query)
	runtime.KeepAlive(keys)
	runtime.KeepAlive(distances)
	return status, int(resultCount)
}

func (l *windowsUSearchAPI) errorMessage(handle unsafe.Pointer) string {
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

func (l *windowsUSearchAPI) errorMessageInto(handle unsafe.Pointer, buffer []byte) int {
	var output unsafe.Pointer
	if len(buffer) > 0 {
		output = unsafe.Pointer(&buffer[0])
	}
	length := l.call("bluge_usearch_index_last_error", pointerValue(handle),
		uintptr(output), uintptr(len(buffer)))
	runtime.KeepAlive(buffer)
	return int(length)
}

func (l *windowsUSearchAPI) hardwareAccelerationCompiled() string {
	return l.hardwareAcceleration("bluge_usearch_hardware_acceleration_compiled")
}

func (l *windowsUSearchAPI) hardwareAccelerationAvailable() string {
	return l.hardwareAcceleration("bluge_usearch_hardware_acceleration_available")
}

func (l *windowsUSearchAPI) hardwareAcceleration(symbol string) string {
	address := l.symbols[symbol]
	if address == 0 {
		return ""
	}
	value, _, _ := purego.SyscallN(address)
	if value == 0 {
		return ""
	}
	data := unsafe.Slice((*byte)(pointerFromResult(value)), 4096)
	if end := bytes.IndexByte(data, 0); end >= 0 {
		data = data[:end]
	}
	return string(data)
}

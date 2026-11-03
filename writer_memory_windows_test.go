//go:build windows

package bluge

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

func writerMemoryPeakRSS() (uint64, error) {
	var counters struct {
		Size, PageFaults                                       uint32
		PeakWorkingSet, WorkingSet, PeakPagedPool, PagedPool   uintptr
		PeakNonPagedPool, NonPagedPool, Pagefile, PeakPagefile uintptr
	}
	counters.Size = uint32(unsafe.Sizeof(counters))
	proc := windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
	ok, _, err := proc.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
	if ok == 0 {
		return 0, err
	}
	return uint64(counters.PeakWorkingSet), nil
}

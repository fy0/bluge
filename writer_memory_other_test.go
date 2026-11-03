//go:build !windows && !linux

package bluge

// Zero indicates unavailable; use an external process monitor on other systems.
func writerMemoryPeakRSS() (uint64, error) { return 0, nil }

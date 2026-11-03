//go:build linux

package bluge

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func writerMemoryPeakRSS() (uint64, error) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0, err
	}
	if usage.Maxrss < 0 {
		return 0, fmt.Errorf("invalid peak RSS: %d", usage.Maxrss)
	}
	return uint64(usage.Maxrss) * 1024, nil
}

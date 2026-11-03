//go:build linux

package bluge

import "golang.org/x/sys/unix"

func writerMemoryPeakRSS() (uint64, error) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0, err
	}
	return uint64(usage.Maxrss) * 1024, nil
}

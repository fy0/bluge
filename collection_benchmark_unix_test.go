//go:build !windows

package bluge

import (
	"os"
	"time"
)

type storageBenchInstant = time.Time

const storageBenchClock = "Go monotonic clock"

func storageBenchNow() storageBenchInstant { return time.Now() }

func storageBenchElapsedMS(start storageBenchInstant) float64 {
	return float64(time.Since(start).Nanoseconds()) / 1e6
}

func storageBenchReadCatalog(path string) ([]byte, error) { return os.ReadFile(path) }

func storageBenchReadDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }

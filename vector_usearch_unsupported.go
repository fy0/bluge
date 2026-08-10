//go:build (!windows && !linux && !darwin) || (!amd64 && !arm64)

package bluge

import (
	"fmt"
	"runtime"
)

func loadUSearchAPI(_ string) (usearchNativeAPI, error) {
	return nil, fmt.Errorf("usearch native backend does not support %s/%s; supported targets are windows, linux, and darwin on amd64 or arm64",
		runtime.GOOS, runtime.GOARCH)
}

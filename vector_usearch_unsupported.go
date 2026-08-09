//go:build !windows

package bluge

import "fmt"

func loadUSearchAPI(_ string) (usearchNativeAPI, error) {
	return nil, fmt.Errorf("usearch purego loader is currently implemented for Windows only")
}

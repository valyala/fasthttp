//go:build go1.21
// +build go1.21

package fasthttp

import "maps"

// Copy global map for fs.go
func fsMapCopy(FSCompressedFileSuffixes map[string]string) map[string]string {
	compressedFileSuffixes := make(map[string]string, len(FSCompressedFileSuffixes))
	maps.Copy(compressedFileSuffixes, FSCompressedFileSuffixes)
	return compressedFileSuffixes
}

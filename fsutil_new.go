//go:build go1.21
// +build go1.21

package fasthttp

import "maps"

// Copy global map for fs.go
func fsMapCopy(fsCompressedFileSuffixes map[string]string) map[string]string {
	compressedFileSuffixes := make(map[string]string, len(fsCompressedFileSuffixes))
	maps.Copy(compressedFileSuffixes, fsCompressedFileSuffixes)
	return compressedFileSuffixes
}

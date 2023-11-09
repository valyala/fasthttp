//go:build go1.21

package fasthttp

import "maps"

// fsMapCopy copies the given fsCompressedFileSuffixes map.
func fsMapCopy(fsCompressedFileSuffixes map[string]string) map[string]string {
	compressedFileSuffixes := make(map[string]string, len(fsCompressedFileSuffixes))
	maps.Copy(compressedFileSuffixes, fsCompressedFileSuffixes)
	return compressedFileSuffixes
}

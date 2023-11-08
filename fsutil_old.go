//go:build !go1.21
// +build !go1.21

package fasthttp

// Copy global map for fs.go
func fsMapCopy(fsCompressedFileSuffixes map[string]string) map[string]string {
	compressedFileSuffixes := make(map[string]string, len(fsCompressedFileSuffixes))
	for k, v := range fsCompressedFileSuffixes {
		compressedFileSuffixes[k] = v
	}
	return compressedFileSuffixes
}

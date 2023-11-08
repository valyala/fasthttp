//go:build !go1.21
// +build !go1.21

package fasthttp

// Copy global map for fs.go
func fsMapCopy(FSCompressedFileSuffixes map[string]string) map[string]string {
	compressedFileSuffixes := make(map[string]string, len(FSCompressedFileSuffixes))
	for k, v := range FSCompressedFileSuffixes {
		compressedFileSuffixes[k] = v
	}
	return compressedFileSuffixes
}

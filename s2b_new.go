//go:build go1.20 || go1.21

package fasthttp

import "unsafe"

// s2b converts string to a byte slice without memory allocation.
func s2b(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

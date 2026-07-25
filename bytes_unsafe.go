package fasthttp

import "unsafe"

// byteAtUnchecked returns b[i] without a bounds check.
// The caller must prove that i is non-negative and less than len(b).
func byteAtUnchecked(b []byte, i int) byte {
	return *(*byte)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), i))
}

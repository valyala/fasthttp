//go:build windows
// +build windows

package fasthttp

func addLeadingSlash(dst, src []byte) []byte {
	// zero length 、"C:/" and "a" case
	isDesk := len(src) > 2 && src[1] == ':'
	if len(src) == 0 || (!isDesk && src[0] != '/') {
		dst = append(dst, '/')
	}
	return dst
}

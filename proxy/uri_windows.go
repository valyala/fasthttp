// +build windows

package proxy

func addLeadingSlash(dst, src []byte) []byte {
	// zero length and "C:/" case
	if len(src) == 0 || (len(src) > 2 && src[1] != ':') {
		dst = append(dst, '/')
	}

	return dst
}

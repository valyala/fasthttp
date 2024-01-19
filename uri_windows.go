package fasthttp

import "path/filepath"

func addLeadingSlash(dst, src []byte) []byte {
	// zero length 、"C:/" and "a" case
	isDisk := len(src) > 2 && src[1] == ':'
	if len(src) == 0 || (!isDisk && src[0] != '/') {
		dst = append(dst, '/')
	}

	return dst
}

func replaceSlashes(dst []byte) []byte {
	// fix: Path Traversal Attacks on Windows
	if filepath.Separator == '\\' {
		for i := range dst {
			if dst[i] == '\\' {
				dst[i] = '/'
			}
		}
	}
	return dst
}

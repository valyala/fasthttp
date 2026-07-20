package fasthttp

import "bytes"

// hasWindowsReservedPathColon reports whether path contains a ':' byte
// that cannot be a legitimate Windows drive letter.
func hasWindowsReservedPathColon(path []byte, rootIsEmpty bool) bool {
	rest := path
	if rootIsEmpty && len(path) >= 2 && isASCIILetter(path[0]) && path[1] == ':' {
		rest = path[2:]
	}
	return bytes.IndexByte(rest, ':') >= 0
}

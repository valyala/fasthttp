package fasthttp

import "bytes"

// ServeFile and ServeFileLiteral pass absolute filenames through a URI.
// Remove the URI's leading slash before opening a Windows drive path.
func serveFilePathRewrite(ctx *RequestCtx) []byte {
	path := ctx.Path()
	if len(path) >= 4 && path[0] == '/' && isASCIILetter(path[1]) && path[2] == ':' && path[3] == '/' {
		return path[1:]
	}
	return path
}

// hasWindowsReservedPathColon reports whether path contains a ':' byte
// that cannot be a legitimate Windows drive letter.
func hasWindowsReservedPathColon(path []byte, rootIsEmpty bool) bool {
	rest := path
	if rootIsEmpty && len(path) >= 2 && isASCIILetter(path[0]) && path[1] == ':' {
		rest = path[2:]
	}
	return bytes.IndexByte(rest, ':') >= 0
}

func isASCIILetter(b byte) bool {
	b |= 0x20 // Make lower case.
	return 'a' <= b && b <= 'z'
}

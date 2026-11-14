package fasthttp

// ServeFile and ServeFileLiteral pass absolute filenames through a URI.
// Remove the URI's leading slash before opening a Windows drive path.
func serveFilePathRewrite(ctx *RequestCtx) []byte {
	path := ctx.Path()
	if len(path) >= 4 && path[0] == '/' && isASCIILetter(path[1]) && path[2] == ':' && path[3] == '/' {
		return path[1:]
	}
	return path
}

// hasWindowsReservedPath rejects alternate data streams and names that
// Windows can alias by trimming trailing spaces or periods.
func hasWindowsReservedPath(path []byte, rootIsEmpty bool) bool {
	if rootIsEmpty && len(path) >= 2 && isASCIILetter(path[0]) && path[1] == ':' {
		path = path[2:]
	}
	start := 0
	for i, c := range path {
		switch c {
		case ':':
			return true
		case '/', '\\':
			if hasWindowsTrimmedName(path[start:i]) {
				return true
			}
			start = i + 1
		}
	}
	return hasWindowsTrimmedName(path[start:])
}

func hasWindowsTrimmedName(name []byte) bool {
	return len(name) > 0 && (name[len(name)-1] == ' ' ||
		(name[len(name)-1] == '.' && string(name) != "." && string(name) != ".."))
}

func isASCIILetter(b byte) bool {
	b |= 0x20 // Make lower case.
	return 'a' <= b && b <= 'z'
}

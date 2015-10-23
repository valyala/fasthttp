package fasthttp

import (
	"bytes"
)

// URI represents URI :) .
//
// It is forbidden copying URI instances. Create new instances instead.
type URI struct {
	// Full uri like {Scheme}://{Host}{Path}?{QueryString}#{Hash}
	URI []byte

	// Original path passed to URI.Parse()
	PathOriginal []byte

	// Scheme part, i.e. http of http://aaa.com/foo/bar?baz=123#qwe .
	//
	// Scheme is always lowercased.
	Scheme []byte

	// Host part, i.e. aaa.com of http://aaa.com/foo/bar?baz=123#qwe .
	//
	// Host is always lowercased.
	Host []byte

	// Path part, i.e. /foo/bar of http://aaa.com/foo/bar?baz=123#qwe .
	//
	// Path is always urldecoded and normalized,
	// i.e. '//f%20obar/baz/../zzz' becomes '/f obar/zzz'.
	Path []byte

	// Query string part, i.e. baz=123 of http://aaa.com/foo/bar?baz=123#qwe .
	QueryString []byte

	// Hash part, i.e. qwe of http://aaa.com/foo/bar?baz=123#qwe .
	Hash []byte

	// Parsed query string arguments.
	//
	// Becomes available after URI.ParseQueryArgs() call.
	QueryArgs       Args
	parsedQueryArgs bool
}

// Clear clears uri.
func (x *URI) Clear() {
	x.URI = x.URI[:0]
	x.PathOriginal = x.PathOriginal[:0]
	x.Scheme = x.Scheme[:0]
	x.Host = x.Host[:0]
	x.Path = x.Path[:0]
	x.QueryString = x.QueryString[:0]
	x.Hash = x.Hash[:0]
	x.QueryArgs.Clear()
	x.parsedQueryArgs = false
}

// Parse initializes URI from the given host and uri.
//
// It is safe modifying host and uri buffers after the Parse call.
func (x *URI) Parse(host, uri []byte) {
	x.Clear()

	scheme, host, uri := splitHostUri(host, uri)
	x.Scheme = append(x.Scheme, scheme...)
	lowercaseBytes(x.Scheme)
	x.Host = append(x.Host, host...)
	lowercaseBytes(x.Host)

	x.URI = append(x.URI, x.Scheme...)
	lowercaseBytes(x.URI)
	x.URI = append(x.URI, strColonSlashSlash...)
	x.URI = append(x.URI, x.Host...)
	if len(uri) > 0 && uri[0] != '/' {
		x.URI = append(x.URI, '/')
	}
	x.URI = append(x.URI, uri...)

	b := uri
	n := bytes.IndexByte(b, '?')
	if n < 0 {
		x.PathOriginal = append(x.PathOriginal, b...)
		x.Path = normalizePath(x.Path, b)
		return
	}
	x.PathOriginal = append(x.PathOriginal, b[:n]...)
	x.Path = normalizePath(x.Path, x.PathOriginal)
	b = b[n+1:]

	n = bytes.IndexByte(b, '#')
	if n >= 0 {
		x.Hash = append(x.Hash, b[n+1:]...)
		b = b[:n]
	}

	x.QueryString = append(x.QueryString, b...)
}

func normalizePath(dst, src []byte) []byte {
	dst = decodeArg(dst, src, false)

	// remove duplicate slashes
	b := dst
	bSize := len(b)
	for {
		n := bytes.Index(b, strSlashSlash)
		if n < 0 {
			break
		}
		b = b[n:]
		copy(b, b[1:])
		b = b[:len(b)-1]
		bSize--
	}
	dst = dst[:bSize]

	// remove /foo/../ parts
	b = dst
	for {
		n := bytes.Index(b, strSlashDotDotSlash)
		if n < 0 {
			break
		}
		nn := bytes.LastIndexByte(b[:n], '/')
		if nn < 0 {
			nn = 0
		}
		n += len(strSlashDotDotSlash) - 1
		copy(b[nn:], b[n:])
		b = b[:len(b)-n+nn]
	}

	// remove trailing /foo/..
	n := bytes.LastIndex(b, strSlashDotDot)
	if n >= 0 && n+len(strSlashDotDot) == len(b) {
		nn := bytes.LastIndexByte(b[:n], '/')
		if nn < 0 {
			return strSlash
		}
		b = b[:nn+1]
	}

	if len(b) == 0 {
		return strSlash
	}
	return b
}

// AppendRequestURI appends RequestURI to dst and returns dst
// (which may be newly allocated).
//
// Appended RequestURI doesn't contain Scheme and Host.
func (x *URI) AppendRequestURI(dst []byte) []byte {
	path := x.Path
	if len(path) == 0 {
		path = strSlash
	}
	dst = appendQuotedArg(dst, path)
	if x.QueryArgs.Len() > 0 {
		dst = append(dst, '?')
		dst = x.QueryArgs.AppendBytes(dst)
	}
	if len(x.Hash) > 0 {
		dst = append(dst, '#')
		dst = append(dst, x.Hash...)
	}
	return dst
}

// AppendBytes appends URI to dst and returns dst (with may be newly allocated).
func (x *URI) AppendBytes(dst []byte) []byte {
	startPos := len(dst)
	scheme := x.Scheme
	if len(scheme) == 0 {
		scheme = strHTTP
	}
	dst = append(dst, scheme...)
	dst = append(dst, strColonSlashSlash...)
	dst = append(dst, x.Host...)
	lowercaseBytes(dst[startPos:])
	return x.AppendRequestURI(dst)
}

func splitHostUri(host, uri []byte) ([]byte, []byte, []byte) {
	n := bytes.Index(uri, strColonSlashSlash)
	if n < 0 {
		return strHTTP, host, uri
	}
	scheme := uri[:n]
	if bytes.IndexByte(scheme, '/') >= 0 {
		return strHTTP, host, uri
	}
	n += len(strColonSlashSlash)
	uri = uri[n:]
	n = bytes.IndexByte(uri, '/')
	if n < 0 {
		return scheme, uri, strSlash
	}
	return scheme, uri[:n], uri[n:]
}

// ParseQueryArgs initializes QueryArgs by parsing QueryString.
func (x *URI) ParseQueryArgs() {
	if x.parsedQueryArgs {
		return
	}
	x.QueryArgs.ParseBytes(x.QueryString)
	x.parsedQueryArgs = true
}

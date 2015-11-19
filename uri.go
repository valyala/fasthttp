package fasthttp

import (
	"bytes"
)

// URI represents URI :) .
//
// It is forbidden copying URI instances. Create new instances instead.
type URI struct {
	// Original path passed to URI.Parse()
	PathOriginal []byte

	// Scheme part, i.e. http of http://aaa.com/foo/bar?baz=123#qwe .
	//
	// Scheme is always lowercased.
	Scheme []byte

	// Path part, i.e. /foo/bar of http://aaa.com/foo/bar?baz=123#qwe .
	//
	// Path is always urldecoded and normalized,
	// i.e. '//f%20obar/baz/../zzz' becomes '/f obar/zzz'.
	Path []byte

	// Query string part, i.e. baz=123 of http://aaa.com/foo/bar?baz=123#qwe .
	QueryString []byte

	// Hash part, i.e. qwe of http://aaa.com/foo/bar?baz=123#qwe .
	Hash []byte

	host []byte

	queryArgs       Args
	parsedQueryArgs bool

	fullURI    []byte
	requestURI []byte

	h *RequestHeader
}

// Reset clears uri.
func (x *URI) Reset() {
	x.PathOriginal = x.PathOriginal[:0]
	x.Scheme = x.Scheme[:0]
	x.Path = x.Path[:0]
	x.QueryString = x.QueryString[:0]
	x.Hash = x.Hash[:0]

	x.host = x.host[:0]
	x.queryArgs.Reset()
	x.parsedQueryArgs = false

	x.fullURI = x.fullURI[:0]
	x.requestURI = x.requestURI[:0]
	x.h = nil
}

// Host returns host part, i.e. aaa.com of http://aaa.com/foo/bar?baz=123#qwe .
//
// Host is always lowercased.
func (x *URI) Host() []byte {
	if len(x.host) == 0 && x.h != nil {
		x.host = append(x.host[:0], x.h.Host()...)
		lowercaseBytes(x.host)
		x.h = nil
	}
	return x.host
}

// Parse initializes URI from the given host and uri.
//
// It is safe modifying host and uri buffers after the Parse call.
func (x *URI) Parse(host, uri []byte) {
	x.parse(host, uri, nil)
}

func (x *URI) parseQuick(uri []byte, h *RequestHeader) {
	x.parse(nil, uri, h)
}

func (x *URI) parse(host, uri []byte, h *RequestHeader) {
	x.Reset()
	x.h = h

	scheme, host, uri := splitHostUri(host, uri)
	x.Scheme = append(x.Scheme, scheme...)
	lowercaseBytes(x.Scheme)
	x.host = append(x.host, host...)
	lowercaseBytes(x.host)

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
	dst = dst[:0]

	// add leading slash
	if len(src) == 0 || src[0] != '/' {
		dst = append(dst, '/')
	}

	dst = decodeArgAppend(dst, src, false)

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

	return b
}

// RequestURI returns RequestURI - i.e. URI without Scheme and Host.
func (x *URI) RequestURI() []byte {
	path := x.Path
	if len(path) == 0 {
		path = strSlash
	}

	dst := appendQuotedArg(x.requestURI[:0], path)
	if x.queryArgs.Len() > 0 {
		dst = append(dst, '?')
		dst = x.queryArgs.AppendBytes(dst)
	} else if len(x.QueryString) > 0 {
		dst = append(dst, '?')
		dst = append(dst, x.QueryString...)
	}
	if len(x.Hash) > 0 {
		dst = append(dst, '#')
		dst = append(dst, x.Hash...)
	}
	x.requestURI = dst
	return x.requestURI
}

// FullURI returns full uri in the form {Scheme}://{Host}{RequestURI}#{Hash}.
func (x *URI) FullURI() []byte {
	scheme := x.Scheme
	if len(scheme) == 0 {
		scheme = strHTTP
	}
	dst := append(x.fullURI[:0], scheme...)
	dst = append(dst, strColonSlashSlash...)
	dst = append(dst, x.Host()...)
	lowercaseBytes(dst)
	x.fullURI = append(dst, x.RequestURI()...)
	return x.fullURI
}

// String returns full uri.
func (x *URI) String() string {
	return string(x.FullURI())
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

// Returns query args.
func (x *URI) QueryArgs() *Args {
	x.parseQueryArgs()
	return &x.queryArgs
}

func (x *URI) parseQueryArgs() {
	if x.parsedQueryArgs {
		return
	}
	x.queryArgs.ParseBytes(x.QueryString)
	x.parsedQueryArgs = true
}

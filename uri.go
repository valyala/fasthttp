package fasthttp

import (
	"bytes"
)

type URI struct {
	// Full uri like {Scheme}://{Host}{Path}?{QueryString}#{Hash}
	URI []byte

	// Original Path passed to URI.Parse()
	PathOriginal []byte

	Scheme      []byte
	Host        []byte
	Path        []byte
	QueryString []byte
	Hash        []byte

	// Becomes available after URI.ParseQueryArgs() call
	QueryArgs       Args
	parsedQueryArgs bool
}

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
		x.Path = decodeArg(x.Path, b, false)
		return
	}
	x.PathOriginal = append(x.PathOriginal, b[:n]...)
	x.Path = decodeArg(x.Path, x.PathOriginal, false)
	b = b[n+1:]

	n = bytes.IndexByte(b, '#')
	if n >= 0 {
		x.Hash = append(x.Hash, b[n+1:]...)
		b = b[:n]
	}

	x.QueryString = append(x.QueryString, b...)
}

// Appends RequestURI to dst. RequestURI doesn't contain Scheme and Host.
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

// Appends URI to dst.
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

func (x *URI) ParseQueryArgs() {
	if x.parsedQueryArgs {
		return
	}
	x.QueryArgs.ParseBytes(x.QueryString)
	x.parsedQueryArgs = true
}

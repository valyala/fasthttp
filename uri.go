package fasthttp

import (
	"bytes"
	"io"
)

// URI represents URI :) .
//
// It is forbidden copying URI instances. Create new instance and use CopyTo
// instead.
type URI struct {
	pathOriginal []byte
	scheme       []byte
	path         []byte
	queryString  []byte
	hash         []byte
	host         []byte

	queryArgs       Args
	parsedQueryArgs bool

	fullURI    []byte
	requestURI []byte

	h *RequestHeader
}

// CopyTo copies uri contents to dst.
func (x *URI) CopyTo(dst *URI) {
	dst.Reset()
	dst.pathOriginal = append(dst.pathOriginal[:0], x.pathOriginal...)
	dst.scheme = append(dst.scheme[:0], x.scheme...)
	dst.path = append(dst.path[:0], x.path...)
	dst.queryString = append(dst.queryString[:0], x.queryString...)
	dst.hash = append(dst.hash[:0], x.hash...)
	dst.host = append(dst.host[:0], x.host...)

	dst.parsedQueryArgs = false

	// fullURI and requestURI shouldn't be copied, since they are created
	// from scratch on each FullURI() and RequestURI() call.
	dst.h = x.h
}

// Hash returns URI hash, i.e. qwe of http://aaa.com/foo/bar?baz=123#qwe .
//
// The returned value is valid until the next URI method call.
func (x *URI) Hash() []byte {
	return x.hash
}

// SetHash sets URI hash.
func (x *URI) SetHash(hash string) {
	x.hash = append(x.hash[:0], hash...)
}

// SetHashBytes sets URI hash.
func (x *URI) SetHashBytes(hash []byte) {
	x.hash = append(x.hash[:0], hash...)
}

// QueryString returns URI query string,
// i.e. baz=123 of http://aaa.com/foo/bar?baz=123#qwe .
//
// The returned value is valid until the next URI method call.
func (x *URI) QueryString() []byte {
	return x.queryString
}

// SetQueryString sets URI query string.
func (x *URI) SetQueryString(queryString string) {
	x.queryString = append(x.queryString[:0], queryString...)
	x.parsedQueryArgs = false
}

// SetQueryStringBytes sets URI query string.
func (x *URI) SetQueryStringBytes(queryString []byte) {
	x.queryString = append(x.queryString[:0], queryString...)
	x.parsedQueryArgs = false
}

// Path returns URI path, i.e. /foo/bar of http://aaa.com/foo/bar?baz=123#qwe .
//
// The returned path is always urldecoded and normalized,
// i.e. '//f%20obar/baz/../zzz' becomes '/f obar/zzz'.
//
// The returned value is valid until the next URI method call.
func (x *URI) Path() []byte {
	path := x.path
	if len(path) == 0 {
		path = strSlash
	}
	return path
}

// SetPath sets URI path.
func (x *URI) SetPath(path string) {
	x.pathOriginal = append(x.pathOriginal, path...)
	x.path = normalizePath(x.path, x.pathOriginal)
}

// SetPathBytes sets URI path.
func (x *URI) SetPathBytes(path []byte) {
	x.pathOriginal = append(x.pathOriginal[:0], path...)
	x.path = normalizePath(x.path, x.pathOriginal)
}

// PathOriginal returns the original path from requestURI passed to URI.Parse().
//
// The returned value is valid until the next URI method call.
func (x *URI) PathOriginal() []byte {
	return x.pathOriginal
}

// Scheme returns URI scheme, i.e. http of http://aaa.com/foo/bar?baz=123#qwe .
//
// Returned scheme is always lowercased.
//
// The returned value is valid until the next URI method call.
func (x *URI) Scheme() []byte {
	scheme := x.scheme
	if len(scheme) == 0 {
		scheme = strHTTP
	}
	return scheme
}

// SetScheme sets URI scheme, i.e. http, https, ftp, etc.
func (x *URI) SetScheme(scheme string) {
	x.scheme = append(x.scheme[:0], scheme...)
	lowercaseBytes(x.scheme)
}

// SetSchemeBytes sets URI scheme, i.e. http, https, ftp, etc.
func (x *URI) SetSchemeBytes(scheme []byte) {
	x.scheme = append(x.scheme[:0], scheme...)
	lowercaseBytes(x.scheme)
}

// Reset clears uri.
func (x *URI) Reset() {
	x.pathOriginal = x.pathOriginal[:0]
	x.scheme = x.scheme[:0]
	x.path = x.path[:0]
	x.queryString = x.queryString[:0]
	x.hash = x.hash[:0]

	x.host = x.host[:0]
	x.queryArgs.Reset()
	x.parsedQueryArgs = false

	// There is no need in x.fullURI = x.fullURI[:0], since full uri
	// is calucalted on each call to FullURI().

	// There is no need in x.requestURI = x.requestURI[:0], since requestURI
	// is calculated on each call to RequestURI().

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

// SetHost sets host for the uri.
func (x *URI) SetHost(host string) {
	x.host = append(x.host[:0], host...)
	lowercaseBytes(x.host)
}

// SetHostBytes sets host for the uri.
func (x *URI) SetHostBytes(host []byte) {
	x.host = append(x.host[:0], host...)
	lowercaseBytes(x.host)
}

// Parse initializes URI from the given host and uri.
func (x *URI) Parse(host, uri []byte) {
	x.parse(host, uri, nil)
}

func (x *URI) parseQuick(uri []byte, h *RequestHeader) {
	x.parse(nil, uri, h)
}

func (x *URI) parse(host, uri []byte, h *RequestHeader) {
	x.Reset()
	x.h = h

	scheme, host, uri := splitHostURI(host, uri)
	x.scheme = append(x.scheme, scheme...)
	lowercaseBytes(x.scheme)
	x.host = append(x.host, host...)
	lowercaseBytes(x.host)

	b := uri
	n := bytes.IndexByte(b, '?')
	if n < 0 {
		x.pathOriginal = append(x.pathOriginal, b...)
		x.path = normalizePath(x.path, b)
		return
	}
	x.pathOriginal = append(x.pathOriginal, b[:n]...)
	x.path = normalizePath(x.path, x.pathOriginal)
	b = b[n+1:]

	n = bytes.IndexByte(b, '#')
	if n >= 0 {
		x.hash = append(x.hash, b[n+1:]...)
		b = b[:n]
	}

	x.queryString = append(x.queryString, b...)
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

	// remove /./ parts
	for {
		n := bytes.Index(b, strSlashDotSlash)
		if n < 0 {
			break
		}
		nn := n + len(strSlashDotSlash) - 1
		copy(b[n:], b[nn:])
		b = b[:len(b)-nn+n]
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
	dst := appendQuotedPath(x.requestURI[:0], x.Path())
	if x.queryArgs.Len() > 0 {
		dst = append(dst, '?')
		dst = x.queryArgs.AppendBytes(dst)
	} else if len(x.queryString) > 0 {
		dst = append(dst, '?')
		dst = append(dst, x.queryString...)
	}
	if len(x.hash) > 0 {
		dst = append(dst, '#')
		dst = append(dst, x.hash...)
	}
	x.requestURI = dst
	return x.requestURI
}

// LastPathSegment returns the last part of uri path after '/'.
//
// Examples:
//
//    * For /foo/bar/baz.html path returns baz.html.
//    * For /foo/bar/ returns empty byte slice.
//    * For /foobar.js returns foobar.js.
func (x *URI) LastPathSegment() []byte {
	path := x.Path()
	n := bytes.LastIndexByte(path, '/')
	if n < 0 {
		return path
	}
	return path[n+1:]
}

// Update updates uri.
//
// The following newURI types are accepted:
//
//     * Absolute, i.e. http://foobar.com/aaa/bb?cc . In this case the original
//       uri is replaced by newURI.
//     * Missing host, i.e. /aaa/bb?cc . In this case only RequestURI part
//       of the original uri is replaced.
//     * Relative path, i.e.  xx?yy=abc . In this case the original RequestURI
//       is updated according to the new relative path.
func (x *URI) Update(newURI string) {
	x.fullURI = append(x.fullURI[:0], newURI...)
	x.UpdateBytes(x.fullURI)
}

// UpdateBytes updates uri.
//
// The following newURI types are accepted:
//
//     * Absolute, i.e. http://foobar.com/aaa/bb?cc . In this case the original
//       uri is replaced by newURI.
//     * Missing host, i.e. /aaa/bb?cc . In this case only RequestURI part
//       of the original uri is replaced.
//     * Relative path, i.e.  xx?yy=abc . In this case the original RequestURI
//       is updated according to the new relative path.
func (x *URI) UpdateBytes(newURI []byte) {
	x.requestURI = x.updateBytes(newURI, x.requestURI)
}

func (x *URI) updateBytes(newURI, buf []byte) []byte {
	if len(newURI) == 0 {
		return buf
	}
	if newURI[0] == '/' {
		// uri without host
		buf = x.appendSchemeHost(buf[:0])
		buf = append(buf, newURI...)
		x.Parse(nil, buf)
		return buf
	}

	n := bytes.Index(newURI, strColonSlashSlash)
	if n >= 0 {
		// absolute uri
		x.Parse(nil, newURI)
		return buf
	}

	// relative path
	if newURI[0] == '?' {
		// query string only update
		x.SetQueryStringBytes(newURI[1:])
		return buf
	}

	path := x.Path()
	n = bytes.LastIndexByte(path, '/')
	if n < 0 {
		panic("BUG: path must contain at least one slash")
	}
	buf = x.appendSchemeHost(buf[:0])
	buf = appendQuotedPath(buf, path[:n+1])
	buf = append(buf, newURI...)
	x.Parse(nil, buf)
	return buf
}

// FullURI returns full uri in the form {Scheme}://{Host}{RequestURI}#{Hash}.
func (x *URI) FullURI() []byte {
	x.fullURI = x.AppendBytes(x.fullURI[:0])
	return x.fullURI
}

// AppendBytes appends full uri to dst and returns the extended dst.
func (x *URI) AppendBytes(dst []byte) []byte {
	dst = x.appendSchemeHost(dst)
	return append(dst, x.RequestURI()...)
}

func (x *URI) appendSchemeHost(dst []byte) []byte {
	dst = append(dst, x.Scheme()...)
	dst = append(dst, strColonSlashSlash...)
	return append(dst, x.Host()...)
}

// WriteTo writes full uri to w.
//
// WriteTo implements io.WriterTo interface.
func (x *URI) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(x.FullURI())
	return int64(n), err
}

// String returns full uri.
func (x *URI) String() string {
	return string(x.FullURI())
}

func splitHostURI(host, uri []byte) ([]byte, []byte, []byte) {
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

// QueryArgs returns query args.
func (x *URI) QueryArgs() *Args {
	x.parseQueryArgs()
	return &x.queryArgs
}

func (x *URI) parseQueryArgs() {
	if x.parsedQueryArgs {
		return
	}
	x.queryArgs.ParseBytes(x.queryString)
	x.parsedQueryArgs = true
}

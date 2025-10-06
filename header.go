package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"iter"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rChar = byte('\r')
	nChar = byte('\n')
)

type header struct {
	h       []argsKV
	cookies []argsKV

	bufK               []byte
	bufV               []byte
	contentLengthBytes []byte
	contentType        []byte
	protocol           []byte

	mulHeader [][]byte
	trailer   [][]byte

	contentLength int

	disableNormalizing    bool
	secureErrorLogMessage bool
	noHTTP11              bool
	connectionClose       bool
	noDefaultContentType  bool
}

// ResponseHeader represents HTTP response header.
//
// It is forbidden copying ResponseHeader instances.
// Create new instances instead and use CopyTo.
//
// ResponseHeader instance MUST NOT be used from concurrently running
// goroutines.
type ResponseHeader struct {
	header

	noCopy noCopy

	statusMessage   []byte
	contentEncoding []byte
	server          []byte

	statusCode int

	noDefaultDate bool
}

// RequestHeader represents HTTP request header.
//
// It is forbidden copying RequestHeader instances.
// Create new instances instead and use CopyTo.
//
// RequestHeader instance MUST NOT be used from concurrently running
// goroutines.
type RequestHeader struct {
	header

	noCopy noCopy

	method     []byte
	requestURI []byte
	host       []byte
	userAgent  []byte

	// stores an immutable copy of headers as they were received from the
	// wire.
	rawHeaders []byte

	disableSpecialHeader bool
	cookiesCollected     bool
}

// SetContentRange sets 'Content-Range: bytes startPos-endPos/contentLength'
// header.
func (h *ResponseHeader) SetContentRange(startPos, endPos, contentLength int) {
	b := h.bufV[:0]
	b = append(b, strBytes...)
	b = append(b, ' ')
	b = AppendUint(b, startPos)
	b = append(b, '-')
	b = AppendUint(b, endPos)
	b = append(b, '/')
	b = AppendUint(b, contentLength)
	h.bufV = b

	h.setNonSpecial(strContentRange, h.bufV)
}

// SetByteRange sets 'Range: bytes=startPos-endPos' header.
//
//   - If startPos is negative, then 'bytes=-startPos' value is set.
//   - If endPos is negative, then 'bytes=startPos-' value is set.
func (h *RequestHeader) SetByteRange(startPos, endPos int) {
	b := h.bufV[:0]
	b = append(b, strBytes...)
	b = append(b, '=')
	if startPos >= 0 {
		b = AppendUint(b, startPos)
	} else {
		endPos = -startPos
	}
	b = append(b, '-')
	if endPos >= 0 {
		b = AppendUint(b, endPos)
	}
	h.bufV = b

	h.setNonSpecial(strRange, h.bufV)
}

// StatusCode returns response status code.
func (h *ResponseHeader) StatusCode() int {
	if h.statusCode == 0 {
		return StatusOK
	}
	return h.statusCode
}

// SetStatusCode sets response status code.
func (h *ResponseHeader) SetStatusCode(statusCode int) {
	h.statusCode = statusCode
}

// StatusMessage returns response status message.
func (h *ResponseHeader) StatusMessage() []byte {
	return h.statusMessage
}

// SetStatusMessage sets response status message bytes.
func (h *ResponseHeader) SetStatusMessage(statusMessage []byte) {
	h.statusMessage = append(h.statusMessage[:0], statusMessage...)
}

// SetProtocol sets response protocol bytes.
func (h *ResponseHeader) SetProtocol(protocol []byte) {
	h.protocol = append(h.protocol[:0], protocol...)
}

// SetLastModified sets 'Last-Modified' header to the given value.
func (h *ResponseHeader) SetLastModified(t time.Time) {
	h.bufV = AppendHTTPDate(h.bufV[:0], t)
	h.setNonSpecial(strLastModified, h.bufV)
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (h *header) ConnectionClose() bool {
	return h.connectionClose
}

// SetConnectionClose sets 'Connection: close' header.
func (h *header) SetConnectionClose() {
	h.connectionClose = true
}

// ResetConnectionClose clears 'Connection: close' header if it exists.
func (h *header) ResetConnectionClose() {
	if h.connectionClose {
		h.connectionClose = false
		h.h = delAllArgs(h.h, HeaderConnection)
	}
}

// ConnectionUpgrade returns true if 'Connection: Upgrade' header is set.
func (h *ResponseHeader) ConnectionUpgrade() bool {
	return hasHeaderValue(h.Peek(HeaderConnection), strUpgrade)
}

// ConnectionUpgrade returns true if 'Connection: Upgrade' header is set.
func (h *RequestHeader) ConnectionUpgrade() bool {
	return hasHeaderValue(h.Peek(HeaderConnection), strUpgrade)
}

// PeekCookie is able to returns cookie by a given key from response.
func (h *ResponseHeader) PeekCookie(key string) []byte {
	return peekArgStr(h.cookies, key)
}

// ContentLength returns Content-Length header value.
//
// It may be negative:
// -1 means Transfer-Encoding: chunked.
// -2 means Transfer-Encoding: identity.
func (h *ResponseHeader) ContentLength() int {
	return h.contentLength
}

// ContentLength returns Content-Length header value.
//
// It may be negative:
// -1 means Transfer-Encoding: chunked.
// -2 means Transfer-Encoding: identity.
func (h *RequestHeader) ContentLength() int {
	if h.disableSpecialHeader {
		// Parse Content-Length from raw headers when special headers are disabled
		v := peekArgBytes(h.h, strContentLength)
		if len(v) == 0 {
			// Check for Transfer-Encoding: chunked
			te := peekArgBytes(h.h, strTransferEncoding)
			if bytes.Equal(te, strChunked) {
				return -1 // chunked
			}
			return -2 // identity
		}
		n, err := parseContentLength(v)
		if err != nil {
			return -2 // identity on parse error
		}
		return n
	}
	return h.contentLength
}

// SetContentLength sets Content-Length header value.
//
// Content-Length may be negative:
// -1 means Transfer-Encoding: chunked.
// -2 means Transfer-Encoding: identity.
func (h *ResponseHeader) SetContentLength(contentLength int) {
	if h.mustSkipContentLength() {
		return
	}
	h.contentLength = contentLength
	if contentLength >= 0 {
		h.contentLengthBytes = AppendUint(h.contentLengthBytes[:0], contentLength)
		h.h = delAllArgs(h.h, HeaderTransferEncoding)
		return
	} else if contentLength == -1 {
		h.contentLengthBytes = h.contentLengthBytes[:0]
		h.h = setArgBytes(h.h, strTransferEncoding, strChunked, argsHasValue)
		return
	}
	h.SetConnectionClose()
}

func (h *ResponseHeader) mustSkipContentLength() bool {
	// From http/1.1 specs:
	// All 1xx (informational), 204 (no content), and 304 (not modified) responses MUST NOT include a message-body
	statusCode := h.StatusCode()

	// Fast path.
	if statusCode < 100 || statusCode == StatusOK {
		return false
	}

	// Slow path.
	return statusCode == StatusNotModified || statusCode == StatusNoContent || statusCode < 200
}

// SetContentLength sets Content-Length header value.
//
// Negative content-length sets 'Transfer-Encoding: chunked' header.
func (h *RequestHeader) SetContentLength(contentLength int) {
	h.contentLength = contentLength
	if contentLength >= 0 {
		h.contentLengthBytes = AppendUint(h.contentLengthBytes[:0], contentLength)
		h.h = delAllArgs(h.h, HeaderTransferEncoding)
	} else {
		h.contentLengthBytes = h.contentLengthBytes[:0]
		h.h = setArgBytes(h.h, strTransferEncoding, strChunked, argsHasValue)
	}
}

func (h *ResponseHeader) isCompressibleContentType() bool {
	contentType := h.ContentType()
	return bytes.HasPrefix(contentType, strTextSlash) ||
		bytes.HasPrefix(contentType, strApplicationSlash) ||
		bytes.HasPrefix(contentType, strImageSVG) ||
		bytes.HasPrefix(contentType, strImageIcon) ||
		bytes.HasPrefix(contentType, strFontSlash) ||
		bytes.HasPrefix(contentType, strMultipartSlash)
}

// ContentType returns Content-Type header value.
func (h *ResponseHeader) ContentType() []byte {
	contentType := h.contentType
	if !h.noDefaultContentType && len(h.contentType) == 0 {
		contentType = defaultContentType
	}
	return contentType
}

// SetContentType sets Content-Type header value.
func (h *header) SetContentType(contentType string) {
	h.contentType = append(h.contentType[:0], contentType...)
}

// SetContentTypeBytes sets Content-Type header value.
func (h *header) SetContentTypeBytes(contentType []byte) {
	h.contentType = append(h.contentType[:0], contentType...)
}

// ContentEncoding returns Content-Encoding header value.
func (h *ResponseHeader) ContentEncoding() []byte {
	return h.contentEncoding
}

// SetContentEncoding sets Content-Encoding header value.
func (h *ResponseHeader) SetContentEncoding(contentEncoding string) {
	h.contentEncoding = append(h.contentEncoding[:0], contentEncoding...)
}

// SetContentEncodingBytes sets Content-Encoding header value.
func (h *ResponseHeader) SetContentEncodingBytes(contentEncoding []byte) {
	h.contentEncoding = append(h.contentEncoding[:0], contentEncoding...)
}

// addVaryBytes add value to the 'Vary' header if it's not included.
func (h *ResponseHeader) addVaryBytes(value []byte) {
	v := h.peek(strVary)
	if len(v) == 0 {
		// 'Vary' is not set
		h.SetBytesV(HeaderVary, value)
	} else if !bytes.Contains(v, value) {
		// 'Vary' is set and not contains target value
		h.SetBytesV(HeaderVary, append(append(v, ','), value...))
	} // else: 'Vary' is set and contains target value
}

// Server returns Server header value.
func (h *ResponseHeader) Server() []byte {
	return h.server
}

// SetServer sets Server header value.
func (h *ResponseHeader) SetServer(server string) {
	h.server = append(h.server[:0], server...)
}

// SetServerBytes sets Server header value.
func (h *ResponseHeader) SetServerBytes(server []byte) {
	h.server = append(h.server[:0], server...)
}

// ContentType returns Content-Type header value.
func (h *RequestHeader) ContentType() []byte {
	if h.disableSpecialHeader {
		return peekArgBytes(h.h, []byte(HeaderContentType))
	}
	return h.contentType
}

// ContentEncoding returns Content-Encoding header value.
func (h *RequestHeader) ContentEncoding() []byte {
	return peekArgBytes(h.h, strContentEncoding)
}

// SetContentEncoding sets Content-Encoding header value.
func (h *RequestHeader) SetContentEncoding(contentEncoding string) {
	h.SetBytesK(strContentEncoding, contentEncoding)
}

// SetContentEncodingBytes sets Content-Encoding header value.
func (h *RequestHeader) SetContentEncodingBytes(contentEncoding []byte) {
	h.setNonSpecial(strContentEncoding, contentEncoding)
}

// SetMultipartFormBoundary sets the following Content-Type:
// 'multipart/form-data; boundary=...'
// where ... is substituted by the given boundary.
func (h *RequestHeader) SetMultipartFormBoundary(boundary string) {
	b := h.bufV[:0]
	b = append(b, strMultipartFormData...)
	b = append(b, ';', ' ')
	b = append(b, strBoundary...)
	b = append(b, '=')
	b = append(b, boundary...)
	h.bufV = b

	h.SetContentTypeBytes(h.bufV)
}

// SetMultipartFormBoundaryBytes sets the following Content-Type:
// 'multipart/form-data; boundary=...'
// where ... is substituted by the given boundary.
func (h *RequestHeader) SetMultipartFormBoundaryBytes(boundary []byte) {
	b := h.bufV[:0]
	b = append(b, strMultipartFormData...)
	b = append(b, ';', ' ')
	b = append(b, strBoundary...)
	b = append(b, '=')
	b = append(b, boundary...)
	h.bufV = b

	h.SetContentTypeBytes(h.bufV)
}

// SetTrailer sets header Trailer value for chunked response
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *header) SetTrailer(trailer string) error {
	return h.SetTrailerBytes(s2b(trailer))
}

// SetTrailerBytes sets Trailer header value for chunked response
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *header) SetTrailerBytes(trailer []byte) error {
	h.trailer = h.trailer[:0]
	return h.AddTrailerBytes(trailer)
}

// AddTrailer add Trailer header value for chunked response
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *header) AddTrailer(trailer string) error {
	return h.AddTrailerBytes(s2b(trailer))
}

var ErrBadTrailer = errors.New("contain forbidden trailer")

// AddTrailerBytes add Trailer header value for chunked response
// to indicate which headers will be sent after the body.
//
// Use Set to set the trailer header later.
//
// Trailers are only supported with chunked transfer.
// Trailers allow the sender to include additional headers at the end of chunked messages.
//
// The following trailers are forbidden:
// 1. necessary for message framing (e.g., Transfer-Encoding and Content-Length),
// 2. routing (e.g., Host),
// 3. request modifiers (e.g., controls and conditionals in Section 5 of [RFC7231]),
// 4. authentication (e.g., see [RFC7235] and [RFC6265]),
// 5. response control data (e.g., see Section 7.1 of [RFC7231]),
// 6. determining how to process the payload (e.g., Content-Encoding, Content-Type, Content-Range, and Trailer)
//
// Return ErrBadTrailer if contain any forbidden trailers.
func (h *header) AddTrailerBytes(trailer []byte) (err error) {
	for i := -1; i+1 < len(trailer); {
		trailer = trailer[i+1:]
		i = bytes.IndexByte(trailer, ',')
		if i < 0 {
			i = len(trailer)
		}
		key := trailer[:i]
		for len(key) > 0 && key[0] == ' ' {
			key = key[1:]
		}
		for len(key) > 0 && key[len(key)-1] == ' ' {
			key = key[:len(key)-1]
		}
		// Forbidden by RFC 7230, section 4.1.2
		if isBadTrailer(key) {
			err = ErrBadTrailer
			continue
		}
		h.bufK = append(h.bufK[:0], key...)
		normalizeHeaderKey(h.bufK, h.disableNormalizing || bytes.IndexByte(h.bufK, ' ') != -1)
		if cap(h.trailer) > len(h.trailer) {
			h.trailer = h.trailer[:len(h.trailer)+1]
			h.trailer[len(h.trailer)-1] = append(h.trailer[len(h.trailer)-1][:0], h.bufK...)
		} else {
			key = make([]byte, len(h.bufK))
			copy(key, h.bufK)
			h.trailer = append(h.trailer, key)
		}
	}

	return err
}

// validHeaderFieldByte returns true if c valid header field byte
// as defined by RFC 7230.
func validHeaderFieldByte(c byte) bool {
	return c < 128 && validHeaderFieldByteTable[c] == 1
}

// validHeaderValueByte returns true if c valid header value byte
// as defined by RFC 7230.
func validHeaderValueByte(c byte) bool {
	return validHeaderValueByteTable[c] == 1
}

// isValidHeaderKey returns true if a is a valid header key.
func isValidHeaderKey(a []byte) bool {
	if len(a) == 0 {
		return false
	}

	// See if a looks like a header key. If not, return it unchanged.
	noCanon := false
	for _, c := range a {
		if validHeaderFieldByte(c) {
			continue
		}
		// Don't canonicalize.
		if c == ' ' {
			// We accept invalid headers with a space before the
			// colon, but must not canonicalize them.
			// See https://go.dev/issue/34540.
			noCanon = true
			continue
		}
		return false
	}
	if noCanon {
		return true
	}

	return true
}

// VisitHeaderParams calls f for each parameter in the given header bytes.
// It stops processing when f returns false or an invalid parameter is found.
// Parameter values may be quoted, in which case \ is treated as an escape
// character, and the value is unquoted before being passed to value.
// See: https://www.rfc-editor.org/rfc/rfc9110#section-5.6.6
//
// f must not retain references to key and/or value after returning.
// Copy key and/or value contents before returning if you need retaining them.
func VisitHeaderParams(b []byte, f func(key, value []byte) bool) {
	for len(b) > 0 {
		idxSemi := 0
		for idxSemi < len(b) && b[idxSemi] != ';' {
			idxSemi++
		}
		if idxSemi >= len(b) {
			return
		}
		b = b[idxSemi+1:]
		for len(b) > 0 && b[0] == ' ' {
			b = b[1:]
		}

		n := 0
		if len(b) == 0 || !validHeaderFieldByte(b[n]) {
			return
		}
		n++
		for n < len(b) && validHeaderFieldByte(b[n]) {
			n++
		}

		if n >= len(b)-1 || b[n] != '=' {
			return
		}
		param := b[:n]
		n++

		switch {
		case validHeaderFieldByte(b[n]):
			m := n
			n++
			for n < len(b) && validHeaderFieldByte(b[n]) {
				n++
			}
			if !f(param, b[m:n]) {
				return
			}
		case b[n] == '"':
			foundEndQuote := false
			escaping := false
			n++
			m := n
			for ; n < len(b); n++ {
				if b[n] == '"' && !escaping {
					foundEndQuote = true
					break
				}
				escaping = (b[n] == '\\' && !escaping)
			}
			if !foundEndQuote {
				return
			}
			if !f(param, b[m:n]) {
				return
			}
			n++
		default:
			return
		}
		b = b[n:]
	}
}

// MultipartFormBoundary returns boundary part
// from 'multipart/form-data; boundary=...' Content-Type.
func (h *RequestHeader) MultipartFormBoundary() []byte {
	b := h.ContentType()
	if !bytes.HasPrefix(b, strMultipartFormData) {
		return nil
	}
	b = b[len(strMultipartFormData):]
	if len(b) == 0 || b[0] != ';' {
		return nil
	}

	var n int
	for len(b) > 0 {
		n++
		for len(b) > n && b[n] == ' ' {
			n++
		}
		b = b[n:]
		if !bytes.HasPrefix(b, strBoundary) {
			if n = bytes.IndexByte(b, ';'); n < 0 {
				return nil
			}
			continue
		}

		b = b[len(strBoundary):]
		if len(b) == 0 || b[0] != '=' {
			return nil
		}
		b = b[1:]
		if n = bytes.IndexByte(b, ';'); n >= 0 {
			b = b[:n]
		}
		if len(b) > 1 && b[0] == '"' && b[len(b)-1] == '"' {
			b = b[1 : len(b)-1]
		}
		return b
	}
	return nil
}

// Host returns Host header value.
func (h *RequestHeader) Host() []byte {
	if h.disableSpecialHeader {
		return peekArgBytes(h.h, []byte(HeaderHost))
	}
	return h.host
}

// SetHost sets Host header value.
func (h *RequestHeader) SetHost(host string) {
	h.host = append(h.host[:0], host...)
}

// SetHostBytes sets Host header value.
func (h *RequestHeader) SetHostBytes(host []byte) {
	h.host = append(h.host[:0], host...)
}

// UserAgent returns User-Agent header value.
func (h *RequestHeader) UserAgent() []byte {
	if h.disableSpecialHeader {
		return peekArgBytes(h.h, []byte(HeaderUserAgent))
	}
	return h.userAgent
}

// SetUserAgent sets User-Agent header value.
func (h *RequestHeader) SetUserAgent(userAgent string) {
	h.userAgent = append(h.userAgent[:0], userAgent...)
}

// SetUserAgentBytes sets User-Agent header value.
func (h *RequestHeader) SetUserAgentBytes(userAgent []byte) {
	h.userAgent = append(h.userAgent[:0], userAgent...)
}

// Referer returns Referer header value.
func (h *RequestHeader) Referer() []byte {
	return peekArgBytes(h.h, strReferer)
}

// SetReferer sets Referer header value.
func (h *RequestHeader) SetReferer(referer string) {
	h.SetBytesK(strReferer, referer)
}

// SetRefererBytes sets Referer header value.
func (h *RequestHeader) SetRefererBytes(referer []byte) {
	h.setNonSpecial(strReferer, referer)
}

// Method returns HTTP request method.
func (h *RequestHeader) Method() []byte {
	if len(h.method) == 0 {
		return []byte(MethodGet)
	}
	return h.method
}

// SetMethod sets HTTP request method.
func (h *RequestHeader) SetMethod(method string) {
	h.method = append(h.method[:0], method...)
}

// SetMethodBytes sets HTTP request method.
func (h *RequestHeader) SetMethodBytes(method []byte) {
	h.method = append(h.method[:0], method...)
}

// Protocol returns HTTP protocol.
func (h *header) Protocol() []byte {
	if len(h.protocol) == 0 {
		return strHTTP11
	}
	return h.protocol
}

// SetProtocol sets HTTP request protocol.
func (h *RequestHeader) SetProtocol(protocol string) {
	h.protocol = append(h.protocol[:0], protocol...)
	h.noHTTP11 = !bytes.Equal(h.protocol, strHTTP11)
}

// SetProtocolBytes sets HTTP request protocol.
func (h *RequestHeader) SetProtocolBytes(protocol []byte) {
	h.protocol = append(h.protocol[:0], protocol...)
	h.noHTTP11 = !bytes.Equal(h.protocol, strHTTP11)
}

// RequestURI returns RequestURI from the first HTTP request line.
func (h *RequestHeader) RequestURI() []byte {
	requestURI := h.requestURI
	if len(requestURI) == 0 {
		requestURI = strSlash
	}
	return requestURI
}

// SetRequestURI sets RequestURI for the first HTTP request line.
// RequestURI must be properly encoded.
// Use URI.RequestURI for constructing proper RequestURI if unsure.
func (h *RequestHeader) SetRequestURI(requestURI string) {
	h.requestURI = append(h.requestURI[:0], requestURI...)
}

// SetRequestURIBytes sets RequestURI for the first HTTP request line.
// RequestURI must be properly encoded.
// Use URI.RequestURI for constructing proper RequestURI if unsure.
func (h *RequestHeader) SetRequestURIBytes(requestURI []byte) {
	h.requestURI = append(h.requestURI[:0], requestURI...)
}

// IsGet returns true if request method is GET.
func (h *RequestHeader) IsGet() bool {
	return string(h.Method()) == MethodGet
}

// IsPost returns true if request method is POST.
func (h *RequestHeader) IsPost() bool {
	return string(h.Method()) == MethodPost
}

// IsPut returns true if request method is PUT.
func (h *RequestHeader) IsPut() bool {
	return string(h.Method()) == MethodPut
}

// IsHead returns true if request method is HEAD.
func (h *RequestHeader) IsHead() bool {
	return string(h.Method()) == MethodHead
}

// IsDelete returns true if request method is DELETE.
func (h *RequestHeader) IsDelete() bool {
	return string(h.Method()) == MethodDelete
}

// IsConnect returns true if request method is CONNECT.
func (h *RequestHeader) IsConnect() bool {
	return string(h.Method()) == MethodConnect
}

// IsOptions returns true if request method is OPTIONS.
func (h *RequestHeader) IsOptions() bool {
	return string(h.Method()) == MethodOptions
}

// IsTrace returns true if request method is TRACE.
func (h *RequestHeader) IsTrace() bool {
	return string(h.Method()) == MethodTrace
}

// IsPatch returns true if request method is PATCH.
func (h *RequestHeader) IsPatch() bool {
	return string(h.Method()) == MethodPatch
}

// IsHTTP11 returns true if the header is HTTP/1.1.
func (h *header) IsHTTP11() bool {
	return !h.noHTTP11
}

// HasAcceptEncoding returns true if the header contains
// the given Accept-Encoding value.
func (h *RequestHeader) HasAcceptEncoding(acceptEncoding string) bool {
	h.bufV = append(h.bufV[:0], acceptEncoding...)
	return h.HasAcceptEncodingBytes(h.bufV)
}

// HasAcceptEncodingBytes returns true if the header contains
// the given Accept-Encoding value.
func (h *RequestHeader) HasAcceptEncodingBytes(acceptEncoding []byte) bool {
	ae := h.peek(strAcceptEncoding)
	n := bytes.Index(ae, acceptEncoding)
	if n < 0 {
		return false
	}
	b := ae[n+len(acceptEncoding):]
	if len(b) > 0 && b[0] != ',' {
		return false
	}
	if n == 0 {
		return true
	}
	return ae[n-1] == ' '
}

// Len returns the number of headers set,
// i.e. the number of times f is called in VisitAll.
func (h *ResponseHeader) Len() int {
	n := 0
	for range h.All() {
		n++
	}
	return n
}

// Len returns the number of headers set,
// i.e. the number of times f is called in VisitAll.
func (h *RequestHeader) Len() int {
	n := 0
	for range h.All() {
		n++
	}
	return n
}

// DisableSpecialHeader disables special header processing.
// fasthttp will not set any special headers for you, such as Host, Content-Type, User-Agent, etc.
// You must set everything yourself.
// If RequestHeader.Read() is called, special headers will be ignored.
// This can be used to control case and order of special headers.
// This is generally not recommended.
func (h *RequestHeader) DisableSpecialHeader() {
	h.disableSpecialHeader = true
}

// EnableSpecialHeader enables special header processing.
// fasthttp will send Host, Content-Type, User-Agent, etc headers for you.
// This is suggested and enabled by default.
func (h *RequestHeader) EnableSpecialHeader() {
	h.disableSpecialHeader = false
}

// DisableNormalizing disables header names' normalization.
//
// By default all the header names are normalized by uppercasing
// the first letter and all the first letters following dashes,
// while lowercasing all the other letters.
// Examples:
//
//   - CONNECTION -> Connection
//   - conteNT-tYPE -> Content-Type
//   - foo-bar-baz -> Foo-Bar-Baz
//
// Disable header names' normalization only if know what are you doing.
func (h *header) DisableNormalizing() {
	h.disableNormalizing = true
}

// EnableNormalizing enables header names' normalization.
//
// Header names are normalized by uppercasing the first letter and
// all the first letters following dashes, while lowercasing all
// the other letters.
// Examples:
//
//   - CONNECTION -> Connection
//   - conteNT-tYPE -> Content-Type
//   - foo-bar-baz -> Foo-Bar-Baz
//
// This is enabled by default unless disabled using DisableNormalizing().
func (h *header) EnableNormalizing() {
	h.disableNormalizing = false
}

// SetNoDefaultContentType allows you to control if a default Content-Type header will be set (false) or not (true).
func (h *header) SetNoDefaultContentType(noDefaultContentType bool) {
	h.noDefaultContentType = noDefaultContentType
}

// Reset clears response header.
func (h *ResponseHeader) Reset() {
	h.disableNormalizing = false
	h.SetNoDefaultContentType(false)
	h.noDefaultDate = false
	h.resetSkipNormalize()
}

func (h *ResponseHeader) resetSkipNormalize() {
	h.noHTTP11 = false
	h.connectionClose = false

	h.statusCode = 0
	h.statusMessage = h.statusMessage[:0]
	h.protocol = h.protocol[:0]
	h.contentLength = 0
	h.contentLengthBytes = h.contentLengthBytes[:0]

	h.contentType = h.contentType[:0]
	h.contentEncoding = h.contentEncoding[:0]
	h.server = h.server[:0]

	h.h = h.h[:0]
	h.cookies = h.cookies[:0]
	h.trailer = h.trailer[:0]
	h.mulHeader = h.mulHeader[:0]
}

// Reset clears request header.
func (h *RequestHeader) Reset() {
	h.disableSpecialHeader = false
	h.disableNormalizing = false
	h.SetNoDefaultContentType(false)
	h.resetSkipNormalize()
}

func (h *RequestHeader) resetSkipNormalize() {
	h.noHTTP11 = false
	h.connectionClose = false

	h.contentLength = 0
	h.contentLengthBytes = h.contentLengthBytes[:0]

	h.method = h.method[:0]
	h.protocol = h.protocol[:0]
	h.requestURI = h.requestURI[:0]
	h.host = h.host[:0]
	h.contentType = h.contentType[:0]
	h.userAgent = h.userAgent[:0]
	h.trailer = h.trailer[:0]
	h.mulHeader = h.mulHeader[:0]

	h.h = h.h[:0]
	h.cookies = h.cookies[:0]
	h.cookiesCollected = false

	h.rawHeaders = h.rawHeaders[:0]
}

func (h *header) copyTo(dst *header) {
	dst.disableNormalizing = h.disableNormalizing
	dst.noHTTP11 = h.noHTTP11
	dst.connectionClose = h.connectionClose
	dst.noDefaultContentType = h.noDefaultContentType
	dst.contentLength = h.contentLength
	dst.contentLengthBytes = append(dst.contentLengthBytes, h.contentLengthBytes...)

	dst.protocol = append(dst.protocol, h.protocol...)
	dst.contentType = append(dst.contentType, h.contentType...)
	dst.trailer = copyTrailer(dst.trailer, h.trailer)
	dst.cookies = copyArgs(dst.cookies, h.cookies)
	dst.h = copyArgs(dst.h, h.h)
}

// CopyTo copies all the headers to dst.
func (h *ResponseHeader) CopyTo(dst *ResponseHeader) {
	dst.Reset()

	h.copyTo(&dst.header)

	dst.noDefaultDate = h.noDefaultDate
	dst.statusCode = h.statusCode
	dst.statusMessage = append(dst.statusMessage, h.statusMessage...)
	dst.contentEncoding = append(dst.contentEncoding, h.contentEncoding...)
	dst.server = append(dst.server, h.server...)
}

// CopyTo copies all the headers to dst.
func (h *RequestHeader) CopyTo(dst *RequestHeader) {
	dst.Reset()

	h.copyTo(&dst.header)

	dst.method = append(dst.method, h.method...)
	dst.requestURI = append(dst.requestURI, h.requestURI...)
	dst.host = append(dst.host, h.host...)
	dst.userAgent = append(dst.userAgent, h.userAgent...)
	dst.cookiesCollected = h.cookiesCollected
	dst.rawHeaders = append(dst.rawHeaders, h.rawHeaders...)
}

// All returns an iterator over key-value pairs in h.
//
// The key and value may invalid outside the iteration loop.
// Copy key and/or value contents for each iteration if you need retaining
// them.
func (h *ResponseHeader) All() iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		if len(h.contentLengthBytes) > 0 && !yield(strContentLength, h.contentLengthBytes) {
			return
		}
		if contentType := h.ContentType(); len(contentType) > 0 && !yield(strContentType, contentType) {
			return
		}

		if contentEncoding := h.ContentEncoding(); len(contentEncoding) > 0 && !yield(strContentEncoding, contentEncoding) {
			return
		}

		if server := h.Server(); len(server) > 0 && !yield(strServer, server) {
			return
		}

		for i := range h.cookies {
			if !yield(strSetCookie, h.cookies[i].value) {
				return
			}
		}

		if len(h.trailer) > 0 && !yield(strTrailer, appendTrailerBytes(nil, h.trailer, strCommaSpace)) {
			return
		}

		for i := range h.h {
			if !yield(h.h[i].key, h.h[i].value) {
				return
			}
		}

		if h.ConnectionClose() && !yield(strConnection, strClose) {
			return
		}
	}
}

// VisitAll calls f for each header.
//
// f must not retain references to key and/or value after returning.
// Copy key and/or value contents before returning if you need retaining them.
//
// Deprecated: Use All instead.
func (h *ResponseHeader) VisitAll(f func(key, value []byte)) {
	h.All()(func(key, value []byte) bool {
		f(key, value)
		return true
	})
}

// Trailers returns an iterator over trailers in h.
//
// The value of trailer may invalid outside the iteration loop.
func (h *header) Trailers() iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for i := range h.trailer {
			if !yield(h.trailer[i]) {
				break
			}
		}
	}
}

// VisitAllTrailer calls f for each response Trailer.
//
// f must not retain references to value after returning.
//
// Deprecated: Use Trailers instead.
func (h *header) VisitAllTrailer(f func(value []byte)) {
	h.Trailers()(func(v []byte) bool {
		f(v)
		return true
	})
}

// Cookies returns an iterator over key-value paired response cookie in h.
//
// Cookie name is passed in key and the whole Set-Cookie header value
// is passed in value for each iteration. Value may be parsed with
// Cookie.ParseBytes().
//
// The key and value may invalid outside the iteration loop.
// Copy key and/or value contents for each iteration if you need retaining
// them.
func (h *ResponseHeader) Cookies() iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		for i := range h.cookies {
			if !yield(h.cookies[i].key, h.cookies[i].value) {
				break
			}
		}
	}
}

// VisitAllCookie calls f for each response cookie.
//
// Cookie name is passed in key and the whole Set-Cookie header value
// is passed in value on each f invocation. Value may be parsed
// with Cookie.ParseBytes().
//
// f must not retain references to key and/or value after returning.
//
// Deprecated: Use Cookies instead.
func (h *ResponseHeader) VisitAllCookie(f func(key, value []byte)) {
	h.Cookies()(func(key, value []byte) bool {
		f(key, value)
		return true
	})
}

// Cookies returns an iterator over key-value pairs request cookie in h.
//
// The key and value may invalid outside the iteration loop.
// Copy key and/or value contents for each iteration if you need retaining
// them.
func (h *RequestHeader) Cookies() iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		h.collectCookies()
		for i := range h.cookies {
			if !yield(h.cookies[i].key, h.cookies[i].value) {
				break
			}
		}
	}
}

// VisitAllCookie calls f for each request cookie.
//
// f must not retain references to key and/or value after returning.
//
// Deprecated: Use Cookies instead.
func (h *RequestHeader) VisitAllCookie(f func(key, value []byte)) {
	h.Cookies()(func(key, value []byte) bool {
		f(key, value)
		return true
	})
}

// All returns an iterator over key-value pairs in h.
//
// The key and value may invalid outside the iteration loop.
// Copy key and/or value contents for each iteration if you need retaining
// them.
//
// To get the headers in order they were received use AllInOrder.
func (h *RequestHeader) All() iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		if host := h.Host(); len(host) > 0 && !yield(strHost, host) {
			return
		}
		if len(h.contentLengthBytes) > 0 && !yield(strContentLength, h.contentLengthBytes) {
			return
		}

		if contentType := h.ContentType(); len(contentType) > 0 && !yield(strContentType, contentType) {
			return
		}

		if userAgent := h.UserAgent(); len(userAgent) > 0 && !yield(strUserAgent, userAgent) {
			return
		}

		if len(h.trailer) > 0 && !yield(strTrailer, appendTrailerBytes(nil, h.trailer, strCommaSpace)) {
			return
		}

		h.collectCookies()

		if len(h.cookies) > 0 {
			h.bufV = appendRequestCookieBytes(h.bufV[:0], h.cookies)
			if !yield(strCookie, h.bufV) {
				return
			}
		}

		for i := range h.h {
			if !yield(h.h[i].key, h.h[i].value) {
				return
			}
		}
		if h.ConnectionClose() && !yield(strConnection, strClose) {
			return
		}
	}
}

// VisitAll calls f for each header.
//
// f must not retain references to key and/or value after returning.
// Copy key and/or value contents before returning if you need retaining them.
//
// To get the headers in order they were received use VisitAllInOrder.
//
// Deprecated: Use All instead.
func (h *RequestHeader) VisitAll(f func(key, value []byte)) {
	h.All()(func(key, value []byte) bool {
		f(key, value)
		return true
	})
}

// AllInOrder returns an iterator over key-value pairs in h in the order they
// were received.
//
// The key and value may invalid outside the iteration loop.
// Copy key and/or value contents for each iteration if you need retaining
// them.
//
// The returned iterator is slightly slower than All because it has to reparse
// the raw headers to get the order.
func (h *RequestHeader) AllInOrder() iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		var s headerScanner
		s.b = h.rawHeaders
		for s.next() {
			normalizeHeaderKey(s.key, h.disableNormalizing || bytes.IndexByte(s.key, ' ') != -1)
			if len(s.key) > 0 {
				if !yield(s.key, s.value) {
					break
				}
			}
		}
	}
}

// VisitAllInOrder calls f for each header in the order they were received.
//
// f must not retain references to key and/or value after returning.
// Copy key and/or value contents before returning if you need retaining them.
//
// This function is slightly slower than VisitAll because it has to reparse the
// raw headers to get the order.
//
// Deprecated: Use AllInOrder instead.
func (h *RequestHeader) VisitAllInOrder(f func(key, value []byte)) {
	h.AllInOrder()(func(key, value []byte) bool {
		f(key, value)
		return true
	})
}

// Del deletes header with the given key.
func (h *ResponseHeader) Del(key string) {
	h.bufK = getHeaderKeyBytes(h.bufK, key, h.disableNormalizing)
	h.del(h.bufK)
}

// DelBytes deletes header with the given key.
func (h *ResponseHeader) DelBytes(key []byte) {
	h.bufK = append(h.bufK[:0], key...)
	normalizeHeaderKey(h.bufK, h.disableNormalizing || bytes.IndexByte(key, ' ') != -1)
	h.del(h.bufK)
}

func (h *ResponseHeader) del(key []byte) {
	switch string(key) {
	case HeaderContentType:
		h.contentType = h.contentType[:0]
	case HeaderContentEncoding:
		h.contentEncoding = h.contentEncoding[:0]
	case HeaderServer:
		h.server = h.server[:0]
	case HeaderSetCookie:
		h.cookies = h.cookies[:0]
	case HeaderContentLength:
		h.contentLength = 0
		h.contentLengthBytes = h.contentLengthBytes[:0]
	case HeaderConnection:
		h.connectionClose = false
	case HeaderTrailer:
		h.trailer = h.trailer[:0]
	}
	h.h = delAllArgs(h.h, b2s(key))
}

// Del deletes header with the given key.
func (h *RequestHeader) Del(key string) {
	h.bufK = getHeaderKeyBytes(h.bufK, key, h.disableNormalizing)
	h.del(h.bufK)
}

// DelBytes deletes header with the given key.
func (h *RequestHeader) DelBytes(key []byte) {
	h.bufK = append(h.bufK[:0], key...)
	normalizeHeaderKey(h.bufK, h.disableNormalizing || bytes.IndexByte(key, ' ') != -1)
	h.del(h.bufK)
}

func (h *RequestHeader) del(key []byte) {
	switch string(key) {
	case HeaderHost:
		h.host = h.host[:0]
	case HeaderContentType:
		h.contentType = h.contentType[:0]
	case HeaderUserAgent:
		h.userAgent = h.userAgent[:0]
	case HeaderCookie:
		h.cookies = h.cookies[:0]
	case HeaderContentLength:
		h.contentLength = 0
		h.contentLengthBytes = h.contentLengthBytes[:0]
	case HeaderConnection:
		h.connectionClose = false
	case HeaderTrailer:
		h.trailer = h.trailer[:0]
	}
	h.h = delAllArgs(h.h, b2s(key))
}

// setSpecialHeader handles special headers and return true when a header is processed.
func (h *ResponseHeader) setSpecialHeader(key, value []byte) bool {
	if len(key) == 0 {
		return false
	}

	switch key[0] | 0x20 {
	case 'c':
		switch {
		case caseInsensitiveCompare(strContentType, key):
			h.SetContentTypeBytes(value)
			return true
		case caseInsensitiveCompare(strContentLength, key):
			if contentLength, err := parseContentLength(value); err == nil {
				h.contentLength = contentLength
				h.contentLengthBytes = append(h.contentLengthBytes[:0], value...)
			}
			return true
		case caseInsensitiveCompare(strContentEncoding, key):
			h.SetContentEncodingBytes(value)
			return true
		case caseInsensitiveCompare(strConnection, key):
			if bytes.Equal(strClose, value) {
				h.SetConnectionClose()
			} else {
				h.ResetConnectionClose()
				h.setNonSpecial(key, value)
			}
			return true
		}
	case 's':
		if caseInsensitiveCompare(strServer, key) {
			h.SetServerBytes(value)
			return true
		} else if caseInsensitiveCompare(strSetCookie, key) {
			var kv *argsKV
			h.cookies, kv = allocArg(h.cookies)
			kv.key = getCookieKey(kv.key, value)
			kv.value = append(kv.value[:0], value...)
			return true
		}
	case 't':
		if caseInsensitiveCompare(strTransferEncoding, key) {
			// Transfer-Encoding is managed automatically.
			return true
		} else if caseInsensitiveCompare(strTrailer, key) {
			_ = h.SetTrailerBytes(value)
			return true
		}
	case 'd':
		if caseInsensitiveCompare(strDate, key) {
			// Date is managed automatically.
			return true
		}
	}

	return false
}

// setNonSpecial directly put into map i.e. not a basic header.
func (h *header) setNonSpecial(key, value []byte) {
	h.h = setArgBytes(h.h, key, value, argsHasValue)
}

// setSpecialHeader handles special headers and return true when a header is processed.
func (h *RequestHeader) setSpecialHeader(key, value []byte) bool {
	if len(key) == 0 || h.disableSpecialHeader {
		return false
	}

	switch key[0] | 0x20 {
	case 'c':
		switch {
		case caseInsensitiveCompare(strContentType, key):
			h.SetContentTypeBytes(value)
			return true
		case caseInsensitiveCompare(strContentLength, key):
			if contentLength, err := parseContentLength(value); err == nil {
				h.contentLength = contentLength
				h.contentLengthBytes = append(h.contentLengthBytes[:0], value...)
			}
			return true
		case caseInsensitiveCompare(strConnection, key):
			if bytes.Equal(strClose, value) {
				h.SetConnectionClose()
			} else {
				h.ResetConnectionClose()
				h.setNonSpecial(key, value)
			}
			return true
		case caseInsensitiveCompare(strCookie, key):
			h.collectCookies()
			h.cookies = parseRequestCookies(h.cookies, value)
			return true
		}
	case 't':
		if caseInsensitiveCompare(strTransferEncoding, key) {
			// Transfer-Encoding is managed automatically.
			return true
		} else if caseInsensitiveCompare(strTrailer, key) {
			_ = h.SetTrailerBytes(value)
			return true
		}
	case 'h':
		if caseInsensitiveCompare(strHost, key) {
			h.SetHostBytes(value)
			return true
		}
	case 'u':
		if caseInsensitiveCompare(strUserAgent, key) {
			h.SetUserAgentBytes(value)
			return true
		}
	}

	return false
}

// Add adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use Set for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Server, Transfer-Encoding
// and Date headers can only be set once and will overwrite the previous value,
// while Set-Cookie will not clear previous cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) Add(key, value string) {
	h.AddBytesKV(s2b(key), s2b(value))
}

// AddBytesK adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesK for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Server, Transfer-Encoding
// and Date headers can only be set once and will overwrite the previous value,
// while Set-Cookie will not clear previous cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) AddBytesK(key []byte, value string) {
	h.AddBytesKV(key, s2b(value))
}

// AddBytesV adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesV for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Server, Transfer-Encoding
// and Date headers can only be set once and will overwrite the previous value,
// while Set-Cookie will not clear previous cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) AddBytesV(key string, value []byte) {
	h.AddBytesKV(s2b(key), value)
}

// AddBytesKV adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesKV for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Server, Transfer-Encoding
// and Date headers can only be set once and will overwrite the previous value,
// while the Set-Cookie header will not clear previous cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) AddBytesKV(key, value []byte) {
	if h.setSpecialHeader(key, value) {
		return
	}

	h.bufK = getHeaderKeyBytes(h.bufK, b2s(key), h.disableNormalizing)
	h.h = appendArgBytes(h.h, h.bufK, value, argsHasValue)
}

// Set sets the given 'key: value' header.
//
// Please note that the Set-Cookie header will not clear previous cookies,
// use SetCookie instead to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
//
// Use Add for setting multiple header values under the same key.
func (h *ResponseHeader) Set(key, value string) {
	h.bufK, h.bufV = initHeaderKV(h.bufK, h.bufV, key, value, h.disableNormalizing)
	h.SetCanonical(h.bufK, h.bufV)
}

// SetBytesK sets the given 'key: value' header.
//
// Please note that the Set-Cookie header will not clear previous cookies,
// use SetCookie instead to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
//
// Use AddBytesK for setting multiple header values under the same key.
func (h *ResponseHeader) SetBytesK(key []byte, value string) {
	h.bufV = append(h.bufV[:0], value...)
	h.SetBytesKV(key, h.bufV)
}

// SetBytesV sets the given 'key: value' header.
//
// Please note that the Set-Cookie header will not clear previous cookies,
// use SetCookie instead to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
//
// Use AddBytesV for setting multiple header values under the same key.
func (h *ResponseHeader) SetBytesV(key string, value []byte) {
	h.bufK = getHeaderKeyBytes(h.bufK, key, h.disableNormalizing)
	h.SetCanonical(h.bufK, value)
}

// SetBytesKV sets the given 'key: value' header.
//
// Please note that the Set-Cookie header will not clear previous cookies,
// use SetCookie instead to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
//
// Use AddBytesKV for setting multiple header values under the same key.
func (h *ResponseHeader) SetBytesKV(key, value []byte) {
	h.bufK = append(h.bufK[:0], key...)
	normalizeHeaderKey(h.bufK, h.disableNormalizing || bytes.IndexByte(key, ' ') != -1)
	h.SetCanonical(h.bufK, value)
}

// SetCanonical sets the given 'key: value' header assuming that
// key is in canonical form.
//
// Please note that the Set-Cookie header will not clear previous cookies,
// use SetCookie instead to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked response body.
func (h *ResponseHeader) SetCanonical(key, value []byte) {
	if h.setSpecialHeader(key, value) {
		return
	}
	h.setNonSpecial(key, value)
}

// SetCookie sets the given response cookie.
//
// It is safe re-using the cookie after the function returns.
func (h *ResponseHeader) SetCookie(cookie *Cookie) {
	h.cookies = setArgBytes(h.cookies, cookie.Key(), cookie.Cookie(), argsHasValue)
}

// SetCookie sets 'key: value' cookies.
func (h *RequestHeader) SetCookie(key, value string) {
	h.collectCookies()
	h.cookies = setArg(h.cookies, key, value, argsHasValue)
}

// SetCookieBytesK sets 'key: value' cookies.
func (h *RequestHeader) SetCookieBytesK(key []byte, value string) {
	h.SetCookie(b2s(key), value)
}

// SetCookieBytesKV sets 'key: value' cookies.
func (h *RequestHeader) SetCookieBytesKV(key, value []byte) {
	h.SetCookie(b2s(key), b2s(value))
}

// DelClientCookie instructs the client to remove the given cookie.
// This doesn't work for a cookie with specific domain or path,
// you should delete it manually like:
//
//	c := AcquireCookie()
//	c.SetKey(key)
//	c.SetDomain("example.com")
//	c.SetPath("/path")
//	c.SetExpire(CookieExpireDelete)
//	h.SetCookie(c)
//	ReleaseCookie(c)
//
// Use DelCookie if you want just removing the cookie from response header.
func (h *ResponseHeader) DelClientCookie(key string) {
	h.DelCookie(key)

	c := AcquireCookie()
	c.SetKey(key)
	c.SetExpire(CookieExpireDelete)
	h.SetCookie(c)
	ReleaseCookie(c)
}

// DelClientCookieBytes instructs the client to remove the given cookie.
// This doesn't work for a cookie with specific domain or path,
// you should delete it manually like:
//
//	c := AcquireCookie()
//	c.SetKey(key)
//	c.SetDomain("example.com")
//	c.SetPath("/path")
//	c.SetExpire(CookieExpireDelete)
//	h.SetCookie(c)
//	ReleaseCookie(c)
//
// Use DelCookieBytes if you want just removing the cookie from response header.
func (h *ResponseHeader) DelClientCookieBytes(key []byte) {
	h.DelClientCookie(b2s(key))
}

// DelCookie removes cookie under the given key from response header.
//
// Note that DelCookie doesn't remove the cookie from the client.
// Use DelClientCookie instead.
func (h *ResponseHeader) DelCookie(key string) {
	h.cookies = delAllArgs(h.cookies, key)
}

// DelCookieBytes removes cookie under the given key from response header.
//
// Note that DelCookieBytes doesn't remove the cookie from the client.
// Use DelClientCookieBytes instead.
func (h *ResponseHeader) DelCookieBytes(key []byte) {
	h.DelCookie(b2s(key))
}

// DelCookie removes cookie under the given key.
func (h *RequestHeader) DelCookie(key string) {
	h.collectCookies()
	h.cookies = delAllArgs(h.cookies, key)
}

// DelCookieBytes removes cookie under the given key.
func (h *RequestHeader) DelCookieBytes(key []byte) {
	h.DelCookie(b2s(key))
}

// DelAllCookies removes all the cookies from response headers.
func (h *ResponseHeader) DelAllCookies() {
	h.cookies = h.cookies[:0]
}

// DelAllCookies removes all the cookies from request headers.
func (h *RequestHeader) DelAllCookies() {
	h.collectCookies()
	h.cookies = h.cookies[:0]
}

// Add adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use Set for setting a single header for the given key.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) Add(key, value string) {
	h.AddBytesKV(s2b(key), s2b(value))
}

// AddBytesK adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesK for setting a single header for the given key.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) AddBytesK(key []byte, value string) {
	h.AddBytesKV(key, s2b(value))
}

// AddBytesV adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesV for setting a single header for the given key.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) AddBytesV(key string, value []byte) {
	h.AddBytesKV(s2b(key), value)
}

// AddBytesKV adds the given 'key: value' header.
//
// Multiple headers with the same key may be added with this function.
// Use SetBytesKV for setting a single header for the given key.
//
// the Content-Type, Content-Length, Connection, Transfer-Encoding,
// Host and User-Agent headers can only be set once and will overwrite
// the previous value, while the Cookie header will not clear previous cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see AddTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) AddBytesKV(key, value []byte) {
	if h.setSpecialHeader(key, value) {
		return
	}

	h.bufK = getHeaderKeyBytes(h.bufK, b2s(key), h.disableNormalizing)
	h.h = appendArgBytes(h.h, h.bufK, value, argsHasValue)
}

// Set sets the given 'key: value' header.
//
// Please note that the Cookie header will not clear previous cookies,
// delete cookies before calling in order to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
//
// Use Add for setting multiple header values under the same key.
func (h *RequestHeader) Set(key, value string) {
	h.bufK, h.bufV = initHeaderKV(h.bufK, h.bufV, key, value, h.disableNormalizing)
	h.SetCanonical(h.bufK, h.bufV)
}

// SetBytesK sets the given 'key: value' header.
//
// Please note that the Cookie header will not clear previous cookies,
// delete cookies before calling in order to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
//
// Use AddBytesK for setting multiple header values under the same key.
func (h *RequestHeader) SetBytesK(key []byte, value string) {
	h.bufV = append(h.bufV[:0], value...)
	h.SetBytesKV(key, h.bufV)
}

// SetBytesV sets the given 'key: value' header.
//
// Please note that the Cookie header will not clear previous cookies,
// delete cookies before calling in order to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
//
// Use AddBytesV for setting multiple header values under the same key.
func (h *RequestHeader) SetBytesV(key string, value []byte) {
	h.bufK = getHeaderKeyBytes(h.bufK, key, h.disableNormalizing)
	h.SetCanonical(h.bufK, value)
}

// SetBytesKV sets the given 'key: value' header.
//
// Please note that the Cookie header will not clear previous cookies,
// delete cookies before calling in order to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
//
// Use AddBytesKV for setting multiple header values under the same key.
func (h *RequestHeader) SetBytesKV(key, value []byte) {
	h.bufK = append(h.bufK[:0], key...)
	normalizeHeaderKey(h.bufK, h.disableNormalizing || bytes.IndexByte(key, ' ') != -1)
	h.SetCanonical(h.bufK, value)
}

// SetCanonical sets the given 'key: value' header assuming that
// key is in canonical form.
//
// Please note that the Cookie header will not clear previous cookies,
// delete cookies before calling in order to reset cookies.
//
// If the header is set as a Trailer (forbidden trailers will not be set, see SetTrailer for more details),
// it will be sent after the chunked request body.
func (h *RequestHeader) SetCanonical(key, value []byte) {
	if h.setSpecialHeader(key, value) {
		return
	}
	h.setNonSpecial(key, value)
}

// Peek returns header value for the given key.
//
// The returned value is valid until the response is released,
// either though ReleaseResponse or your request handler returning.
// Do not store references to the returned value. Make copies instead.
func (h *ResponseHeader) Peek(key string) []byte {
	h.bufK = getHeaderKeyBytes(h.bufK, key, h.disableNormalizing)
	return h.peek(h.bufK)
}

// PeekBytes returns header value for the given key.
//
// The returned value is valid until the response is released,
// either though ReleaseResponse or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) PeekBytes(key []byte) []byte {
	h.bufK = append(h.bufK[:0], key...)
	normalizeHeaderKey(h.bufK, h.disableNormalizing || bytes.IndexByte(key, ' ') != -1)
	return h.peek(h.bufK)
}

// Peek returns header value for the given key.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) Peek(key string) []byte {
	h.bufK = getHeaderKeyBytes(h.bufK, key, h.disableNormalizing)
	return h.peek(h.bufK)
}

// PeekBytes returns header value for the given key.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) PeekBytes(key []byte) []byte {
	h.bufK = append(h.bufK[:0], key...)
	normalizeHeaderKey(h.bufK, h.disableNormalizing || bytes.IndexByte(key, ' ') != -1)
	return h.peek(h.bufK)
}

func (h *ResponseHeader) peek(key []byte) []byte {
	switch string(key) {
	case HeaderContentType:
		return h.ContentType()
	case HeaderContentEncoding:
		return h.ContentEncoding()
	case HeaderServer:
		return h.Server()
	case HeaderConnection:
		if h.ConnectionClose() {
			return strClose
		}
		return peekArgBytes(h.h, key)
	case HeaderContentLength:
		return h.contentLengthBytes
	case HeaderSetCookie:
		return appendResponseCookieBytes(nil, h.cookies)
	case HeaderTrailer:
		return appendTrailerBytes(nil, h.trailer, strCommaSpace)
	default:
		return peekArgBytes(h.h, key)
	}
}

func (h *RequestHeader) peek(key []byte) []byte {
	switch string(key) {
	case HeaderHost:
		return h.Host()
	case HeaderContentType:
		return h.ContentType()
	case HeaderUserAgent:
		return h.UserAgent()
	case HeaderConnection:
		if h.ConnectionClose() {
			return strClose
		}
		return peekArgBytes(h.h, key)
	case HeaderContentLength:
		return h.contentLengthBytes
	case HeaderCookie:
		if h.cookiesCollected {
			return appendRequestCookieBytes(nil, h.cookies)
		}
		return peekArgBytes(h.h, key)
	case HeaderTrailer:
		return appendTrailerBytes(nil, h.trailer, strCommaSpace)
	default:
		return peekArgBytes(h.h, key)
	}
}

// PeekAll returns all header value for the given key.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) PeekAll(key string) [][]byte {
	h.bufK = getHeaderKeyBytes(h.bufK, key, h.disableNormalizing)
	return h.peekAll(h.bufK)
}

func (h *RequestHeader) peekAll(key []byte) [][]byte {
	h.mulHeader = h.mulHeader[:0]
	switch string(key) {
	case HeaderHost:
		if host := h.Host(); len(host) > 0 {
			h.mulHeader = append(h.mulHeader, host)
		}
	case HeaderContentType:
		if contentType := h.ContentType(); len(contentType) > 0 {
			h.mulHeader = append(h.mulHeader, contentType)
		}
	case HeaderUserAgent:
		if ua := h.UserAgent(); len(ua) > 0 {
			h.mulHeader = append(h.mulHeader, ua)
		}
	case HeaderConnection:
		if h.ConnectionClose() {
			h.mulHeader = append(h.mulHeader, strClose)
		} else {
			h.mulHeader = peekAllArgBytesToDst(h.mulHeader, h.h, key)
		}
	case HeaderContentLength:
		h.mulHeader = append(h.mulHeader, h.contentLengthBytes)
	case HeaderCookie:
		if h.cookiesCollected {
			h.mulHeader = append(h.mulHeader, appendRequestCookieBytes(nil, h.cookies))
		} else {
			h.mulHeader = peekAllArgBytesToDst(h.mulHeader, h.h, key)
		}
	case HeaderTrailer:
		h.mulHeader = append(h.mulHeader, appendTrailerBytes(nil, h.trailer, strCommaSpace))
	default:
		h.mulHeader = peekAllArgBytesToDst(h.mulHeader, h.h, key)
	}
	return h.mulHeader
}

// PeekAll returns all header value for the given key.
//
// The returned value is valid until the request is released,
// either though ReleaseResponse or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) PeekAll(key string) [][]byte {
	h.bufK = getHeaderKeyBytes(h.bufK, key, h.disableNormalizing)
	return h.peekAll(h.bufK)
}

func (h *ResponseHeader) peekAll(key []byte) [][]byte {
	h.mulHeader = h.mulHeader[:0]
	switch string(key) {
	case HeaderContentType:
		if contentType := h.ContentType(); len(contentType) > 0 {
			h.mulHeader = append(h.mulHeader, contentType)
		}
	case HeaderContentEncoding:
		if contentEncoding := h.ContentEncoding(); len(contentEncoding) > 0 {
			h.mulHeader = append(h.mulHeader, contentEncoding)
		}
	case HeaderServer:
		if server := h.Server(); len(server) > 0 {
			h.mulHeader = append(h.mulHeader, server)
		}
	case HeaderConnection:
		if h.ConnectionClose() {
			h.mulHeader = append(h.mulHeader, strClose)
		} else {
			h.mulHeader = peekAllArgBytesToDst(h.mulHeader, h.h, key)
		}
	case HeaderContentLength:
		h.mulHeader = append(h.mulHeader, h.contentLengthBytes)
	case HeaderSetCookie:
		h.mulHeader = append(h.mulHeader, appendResponseCookieBytes(nil, h.cookies))
	case HeaderTrailer:
		h.mulHeader = append(h.mulHeader, appendTrailerBytes(nil, h.trailer, strCommaSpace))
	default:
		h.mulHeader = peekAllArgBytesToDst(h.mulHeader, h.h, key)
	}
	return h.mulHeader
}

// PeekKeys return all header keys.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) PeekKeys() [][]byte {
	h.mulHeader = h.mulHeader[:0]
	for key := range h.All() {
		h.mulHeader = append(h.mulHeader, key)
	}
	return h.mulHeader
}

// PeekKeys return all header keys.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) PeekKeys() [][]byte {
	h.mulHeader = h.mulHeader[:0]
	for key := range h.All() {
		h.mulHeader = append(h.mulHeader, key)
	}
	return h.mulHeader
}

// PeekTrailerKeys return all trailer keys.
//
// The returned value is valid until the request is released,
// either though ReleaseResponse or your request handler returning.
// Any future calls to the Peek* will modify the returned value.
// Do not store references to returned value. Make copies instead.
func (h *header) PeekTrailerKeys() [][]byte {
	return h.trailer
}

// Cookie returns cookie for the given key.
func (h *RequestHeader) Cookie(key string) []byte {
	h.collectCookies()
	return peekArgStr(h.cookies, key)
}

// CookieBytes returns cookie for the given key.
func (h *RequestHeader) CookieBytes(key []byte) []byte {
	h.collectCookies()
	return peekArgBytes(h.cookies, key)
}

// Cookie fills cookie for the given cookie.Key.
//
// Returns false if cookie with the given cookie.Key is missing.
func (h *ResponseHeader) Cookie(cookie *Cookie) bool {
	v := peekArgBytes(h.cookies, cookie.Key())
	if v == nil {
		return false
	}
	cookie.ParseBytes(v) //nolint:errcheck
	return true
}

// Read reads response header from r.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (h *ResponseHeader) Read(r *bufio.Reader) error {
	n := 1
	for {
		err := h.tryRead(r, n)
		if err == nil {
			return nil
		}
		if err != errNeedMore {
			h.resetSkipNormalize()
			return err
		}
		n = r.Buffered() + 1
	}
}

func (h *ResponseHeader) tryRead(r *bufio.Reader, n int) error {
	h.resetSkipNormalize()
	b, err := r.Peek(n)
	if len(b) == 0 {
		// Return ErrTimeout on any timeout.
		if x, ok := err.(interface{ Timeout() bool }); ok && x.Timeout() {
			return ErrTimeout
		}
		// treat all other errors on the first byte read as EOF
		if n == 1 || err == io.EOF {
			return io.EOF
		}

		// This is for go 1.6 bug. See https://github.com/golang/go/issues/14121 .
		if err == bufio.ErrBufferFull {
			if h.secureErrorLogMessage {
				return &ErrSmallBuffer{
					error: errors.New("error when reading response headers"),
				}
			}
			return &ErrSmallBuffer{
				error: fmt.Errorf("error when reading response headers: %w", errSmallBuffer),
			}
		}

		return fmt.Errorf("error when reading response headers: %w", err)
	}
	b = mustPeekBuffered(r)
	headersLen, errParse := h.parse(b)
	if errParse != nil {
		return headerError("response", err, errParse, b, h.secureErrorLogMessage)
	}
	mustDiscard(r, headersLen)
	return nil
}

// ReadTrailer reads response trailer header from r.
//
// io.EOF is returned if r is closed before reading the first byte.
func (h *header) ReadTrailer(r *bufio.Reader) error {
	n := 1
	for {
		err := h.tryReadTrailer(r, n)
		if err == nil {
			return nil
		}
		if err != errNeedMore {
			return err
		}
		n = r.Buffered() + 1
	}
}

func (h *header) tryReadTrailer(r *bufio.Reader, n int) error {
	b, err := r.Peek(n)
	if len(b) == 0 {
		// Return ErrTimeout on any timeout.
		if x, ok := err.(interface{ Timeout() bool }); ok && x.Timeout() {
			return ErrTimeout
		}

		if n == 1 || err == io.EOF {
			return io.EOF
		}

		// This is for go 1.6 bug. See https://github.com/golang/go/issues/14121 .
		if err == bufio.ErrBufferFull {
			if h.secureErrorLogMessage {
				return &ErrSmallBuffer{
					error: errors.New("error when reading response trailer"),
				}
			}
			return &ErrSmallBuffer{
				error: fmt.Errorf("error when reading response trailer: %w", errSmallBuffer),
			}
		}

		return fmt.Errorf("error when reading response trailer: %w", err)
	}
	b = mustPeekBuffered(r)
	hh, headersLen, errParse := parseTrailer(b, h.h, h.disableNormalizing)
	h.h = hh
	if errParse != nil {
		if err == io.EOF {
			return err
		}
		return headerError("response", err, errParse, b, h.secureErrorLogMessage)
	}
	mustDiscard(r, headersLen)
	return nil
}

func headerError(typ string, err, errParse error, b []byte, secureErrorLogMessage bool) error {
	if errParse != errNeedMore {
		return headerErrorMsg(typ, errParse, b, secureErrorLogMessage)
	}
	if err == nil {
		return errNeedMore
	}

	// Buggy servers may leave trailing CRLFs after http body.
	// Treat this case as EOF.
	if isOnlyCRLF(b) {
		return io.EOF
	}

	if err != bufio.ErrBufferFull {
		return headerErrorMsg(typ, err, b, secureErrorLogMessage)
	}
	return &ErrSmallBuffer{
		error: headerErrorMsg(typ, errSmallBuffer, b, secureErrorLogMessage),
	}
}

func headerErrorMsg(typ string, err error, b []byte, secureErrorLogMessage bool) error {
	if secureErrorLogMessage {
		return fmt.Errorf("error when reading %s headers: %w. Buffer size=%d", typ, err, len(b))
	}
	return fmt.Errorf("error when reading %s headers: %w. Buffer size=%d, contents: %s", typ, err, len(b), bufferSnippet(b))
}

// Read reads request header from r.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (h *RequestHeader) Read(r *bufio.Reader) error {
	return h.readLoop(r, true)
}

// readLoop reads request header from r optionally loops until it has enough data.
//
// io.EOF is returned if r is closed before reading the first header byte.
func (h *RequestHeader) readLoop(r *bufio.Reader, waitForMore bool) error {
	n := 1
	for {
		err := h.tryRead(r, n)
		if err == nil {
			return nil
		}
		if !waitForMore || err != errNeedMore {
			h.resetSkipNormalize()
			return err
		}
		n = r.Buffered() + 1
	}
}

func (h *RequestHeader) tryRead(r *bufio.Reader, n int) error {
	h.resetSkipNormalize()
	b, err := r.Peek(n)
	if len(b) == 0 {
		if err == io.EOF {
			return err
		}

		if err == nil {
			panic("bufio.Reader.Peek() returned nil, nil")
		}

		// This is for go 1.6 bug. See https://github.com/golang/go/issues/14121 .
		if err == bufio.ErrBufferFull {
			return &ErrSmallBuffer{
				error: fmt.Errorf("error when reading request headers: %w (n=%d, r.Buffered()=%d)", errSmallBuffer, n, r.Buffered()),
			}
		}

		// n == 1 on the first read for the request.
		if n == 1 {
			// We didn't read a single byte.
			return ErrNothingRead{error: err}
		}

		return fmt.Errorf("error when reading request headers: %w", err)
	}
	b = mustPeekBuffered(r)
	headersLen, errParse := h.parse(b)
	if errParse != nil {
		return headerError("request", err, errParse, b, h.secureErrorLogMessage)
	}
	mustDiscard(r, headersLen)
	return nil
}

func bufferSnippet(b []byte) string {
	n := len(b)
	start := 200
	end := n - start
	if start >= end {
		start = n
		end = n
	}
	bStart, bEnd := b[:start], b[end:]
	if len(bEnd) == 0 {
		return fmt.Sprintf("%q", b)
	}
	return fmt.Sprintf("%q...%q", bStart, bEnd)
}

func isOnlyCRLF(b []byte) bool {
	for _, ch := range b {
		if ch != rChar && ch != nChar {
			return false
		}
	}
	return true
}

func updateServerDate() {
	refreshServerDate()
	go func() {
		for {
			time.Sleep(time.Second)
			refreshServerDate()
		}
	}()
}

var (
	serverDate     atomic.Value
	serverDateOnce sync.Once // serverDateOnce.Do(updateServerDate)
)

func refreshServerDate() {
	b := AppendHTTPDate(nil, time.Now())
	serverDate.Store(b)
}

// Write writes response header to w.
func (h *ResponseHeader) Write(w *bufio.Writer) error {
	_, err := w.Write(h.Header())
	return err
}

// WriteTo writes response header to w.
//
// WriteTo implements io.WriterTo interface.
func (h *ResponseHeader) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(h.Header())
	return int64(n), err
}

// Header returns response header representation.
//
// Headers that set as Trailer will not represent. Use TrailerHeader for trailers.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) Header() []byte {
	h.bufV = h.AppendBytes(h.bufV[:0])
	return h.bufV
}

// writeTrailer writes response trailer to w.
func (h *ResponseHeader) writeTrailer(w *bufio.Writer) error {
	_, err := w.Write(h.TrailerHeader())
	return err
}

// TrailerHeader returns response trailer header representation.
//
// Trailers will only be received with chunked transfer.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) TrailerHeader() []byte {
	h.bufV = h.bufV[:0]
	for _, t := range h.trailer {
		value := h.peek(t)
		h.bufV = appendHeaderLine(h.bufV, t, value)
	}
	h.bufV = append(h.bufV, strCRLF...)
	return h.bufV
}

// String returns response header representation.
func (h *ResponseHeader) String() string {
	return string(h.Header())
}

// appendStatusLine appends the response status line to dst and returns
// the extended dst.
func (h *ResponseHeader) appendStatusLine(dst []byte) []byte {
	statusCode := h.StatusCode()
	if statusCode < 0 {
		statusCode = StatusOK
	}
	return formatStatusLine(dst, h.Protocol(), statusCode, h.StatusMessage())
}

// AppendBytes appends response header representation to dst and returns
// the extended dst.
func (h *ResponseHeader) AppendBytes(dst []byte) []byte {
	dst = h.appendStatusLine(dst[:0])

	server := h.Server()
	if len(server) != 0 {
		dst = appendHeaderLine(dst, strServer, server)
	}

	if !h.noDefaultDate {
		serverDateOnce.Do(updateServerDate)
		dst = appendHeaderLine(dst, strDate, serverDate.Load().([]byte))
	}

	// Append Content-Type only for non-zero responses
	// or if it is explicitly set.
	// See https://github.com/valyala/fasthttp/issues/28 .
	if h.ContentLength() != 0 || len(h.contentType) > 0 {
		contentType := h.ContentType()
		if len(contentType) > 0 {
			dst = appendHeaderLine(dst, strContentType, contentType)
		}
	}
	contentEncoding := h.ContentEncoding()
	if len(contentEncoding) > 0 {
		dst = appendHeaderLine(dst, strContentEncoding, contentEncoding)
	}

	if len(h.contentLengthBytes) > 0 {
		dst = appendHeaderLine(dst, strContentLength, h.contentLengthBytes)
	}

	for i, n := 0, len(h.h); i < n; i++ {
		kv := &h.h[i]

		// Exclude trailer from header
		exclude := false
		for _, t := range h.trailer {
			if bytes.Equal(kv.key, t) {
				exclude = true
				break
			}
		}
		if !exclude && (h.noDefaultDate || !bytes.Equal(kv.key, strDate)) {
			dst = appendHeaderLine(dst, kv.key, kv.value)
		}
	}

	if len(h.trailer) > 0 {
		dst = appendHeaderLine(dst, strTrailer, appendTrailerBytes(nil, h.trailer, strCommaSpace))
	}

	n := len(h.cookies)
	if n > 0 {
		for i := 0; i < n; i++ {
			kv := &h.cookies[i]
			dst = appendHeaderLine(dst, strSetCookie, kv.value)
		}
	}

	if h.ConnectionClose() {
		dst = appendHeaderLine(dst, strConnection, strClose)
	}

	return append(dst, strCRLF...)
}

// Write writes request header to w.
func (h *RequestHeader) Write(w *bufio.Writer) error {
	_, err := w.Write(h.Header())
	return err
}

// WriteTo writes request header to w.
//
// WriteTo implements io.WriterTo interface.
func (h *RequestHeader) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(h.Header())
	return int64(n), err
}

// Header returns request header representation.
//
// Headers that set as Trailer will not represent. Use TrailerHeader for trailers.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) Header() []byte {
	h.bufV = h.AppendBytes(h.bufV[:0])
	return h.bufV
}

// writeTrailer writes request trailer to w.
func (h *RequestHeader) writeTrailer(w *bufio.Writer) error {
	_, err := w.Write(h.TrailerHeader())
	return err
}

// TrailerHeader returns request trailer header representation.
//
// Trailers will only be received with chunked transfer.
//
// The returned value is valid until the request is released,
// either though ReleaseRequest or your request handler returning.
// Do not store references to returned value. Make copies instead.
func (h *RequestHeader) TrailerHeader() []byte {
	h.bufV = h.bufV[:0]
	for _, t := range h.trailer {
		value := h.peek(t)
		h.bufV = appendHeaderLine(h.bufV, t, value)
	}
	h.bufV = append(h.bufV, strCRLF...)
	return h.bufV
}

// RawHeaders returns raw header key/value bytes.
//
// Depending on server configuration, header keys may be normalized to
// capital-case in place.
//
// This copy is set aside during parsing, so empty slice is returned for all
// cases where parsing did not happen. Similarly, request line is not stored
// during parsing and can not be returned.
//
// The slice is not safe to use after the handler returns.
func (h *RequestHeader) RawHeaders() []byte {
	return h.rawHeaders
}

// String returns request header representation.
func (h *RequestHeader) String() string {
	return string(h.Header())
}

// AppendBytes appends request header representation to dst and returns
// the extended dst.
func (h *RequestHeader) AppendBytes(dst []byte) []byte {
	dst = append(dst, h.Method()...)
	dst = append(dst, ' ')
	dst = append(dst, h.RequestURI()...)
	dst = append(dst, ' ')
	dst = append(dst, h.Protocol()...)
	dst = append(dst, strCRLF...)

	userAgent := h.UserAgent()
	if len(userAgent) > 0 && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strUserAgent, userAgent)
	}

	host := h.Host()
	if len(host) > 0 && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strHost, host)
	}

	contentType := h.ContentType()
	if !h.noDefaultContentType && len(contentType) == 0 && !h.ignoreBody() {
		contentType = strDefaultContentType
	}
	if len(contentType) > 0 && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strContentType, contentType)
	}
	if len(h.contentLengthBytes) > 0 && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strContentLength, h.contentLengthBytes)
	}

	for i, n := 0, len(h.h); i < n; i++ {
		kv := &h.h[i]
		// Exclude trailer from header
		exclude := false
		for _, t := range h.trailer {
			if bytes.Equal(kv.key, t) {
				exclude = true
				break
			}
		}
		if !exclude {
			dst = appendHeaderLine(dst, kv.key, kv.value)
		}
	}

	if len(h.trailer) > 0 {
		dst = appendHeaderLine(dst, strTrailer, appendTrailerBytes(nil, h.trailer, strCommaSpace))
	}

	// there is no need in h.collectCookies() here, since if cookies aren't collected yet,
	// they all are located in h.h.
	n := len(h.cookies)
	if n > 0 && !h.disableSpecialHeader {
		dst = append(dst, strCookie...)
		dst = append(dst, strColonSpace...)
		dst = appendRequestCookieBytes(dst, h.cookies)
		dst = append(dst, strCRLF...)
	}

	if h.ConnectionClose() && !h.disableSpecialHeader {
		dst = appendHeaderLine(dst, strConnection, strClose)
	}

	return append(dst, strCRLF...)
}

func appendHeaderLine(dst, key, value []byte) []byte {
	dst = append(dst, key...)
	dst = append(dst, strColonSpace...)
	dst = append(dst, value...)
	return append(dst, strCRLF...)
}

func (h *ResponseHeader) parse(buf []byte) (int, error) {
	m, err := h.parseFirstLine(buf)
	if err != nil {
		return 0, err
	}
	n, err := h.parseHeaders(buf[m:])
	if err != nil {
		return 0, err
	}
	return m + n, nil
}

func (h *RequestHeader) ignoreBody() bool {
	return h.IsGet() || h.IsHead()
}

func (h *RequestHeader) parse(buf []byte) (int, error) {
	m, err := h.parseFirstLine(buf)
	if err != nil {
		return 0, err
	}

	h.rawHeaders, _, err = readRawHeaders(h.rawHeaders[:0], buf[m:])
	if err != nil {
		return 0, err
	}
	var n int
	n, err = h.parseHeaders(buf[m:])
	if err != nil {
		return 0, err
	}
	return m + n, nil
}

func parseTrailer(src []byte, dest []argsKV, disableNormalizing bool) ([]argsKV, int, error) {
	// Skip any 0 length chunk.
	if src[0] == '0' {
		skip := len(strCRLF) + 1
		if len(src) < skip {
			return dest, 0, io.EOF
		}
		src = src[skip:]
	}

	var s headerScanner
	s.b = src

	for s.next() {
		if len(s.key) == 0 {
			continue
		}
		disable := disableNormalizing
		for _, ch := range s.key {
			if !validHeaderFieldByte(ch) {
				// We accept invalid headers with a space before the
				// colon, but must not canonicalize them.
				// See: https://github.com/valyala/fasthttp/issues/1917
				if ch == ' ' {
					disable = true
					continue
				}
				return dest, 0, fmt.Errorf("invalid trailer key %q", s.key)
			}
		}
		// Forbidden by RFC 7230, section 4.1.2
		if isBadTrailer(s.key) {
			return dest, 0, fmt.Errorf("forbidden trailer key %q", s.key)
		}
		normalizeHeaderKey(s.key, disable)
		dest = appendArgBytes(dest, s.key, s.value, argsHasValue)
	}
	if s.err != nil {
		return dest, 0, s.err
	}
	return dest, s.r, nil
}

func isBadTrailer(key []byte) bool {
	if len(key) == 0 {
		return true
	}

	switch key[0] | 0x20 {
	case 'a':
		return caseInsensitiveCompare(key, strAuthorization)
	case 'c':
		// Security fix: Changed > to >= to properly block Content-Type header in trailers
		if len(key) >= len(HeaderContentType) && caseInsensitiveCompare(key[:8], strContentType[:8]) {
			// skip compare prefix 'Content-'
			return caseInsensitiveCompare(key[8:], strContentEncoding[8:]) ||
				caseInsensitiveCompare(key[8:], strContentLength[8:]) ||
				caseInsensitiveCompare(key[8:], strContentType[8:]) ||
				caseInsensitiveCompare(key[8:], strContentRange[8:])
		}
		return caseInsensitiveCompare(key, strConnection) ||
			// Security: Block Cookie header in trailers to prevent session hijacking
			caseInsensitiveCompare(key, strCookie)
	case 'e':
		return caseInsensitiveCompare(key, strExpect)
	case 'h':
		return caseInsensitiveCompare(key, strHost)
	case 'k':
		return caseInsensitiveCompare(key, strKeepAlive)
	case 'l':
		// Security: Block Location header in trailers to prevent redirect attacks
		return caseInsensitiveCompare(key, strLocation)
	case 'm':
		return caseInsensitiveCompare(key, strMaxForwards)
	case 'p':
		if len(key) >= len(HeaderProxyConnection) && caseInsensitiveCompare(key[:6], strProxyConnection[:6]) {
			// skip compare prefix 'Proxy-'
			return caseInsensitiveCompare(key[6:], strProxyConnection[6:]) ||
				caseInsensitiveCompare(key[6:], strProxyAuthenticate[6:]) ||
				caseInsensitiveCompare(key[6:], strProxyAuthorization[6:])
		}
	case 'r':
		return caseInsensitiveCompare(key, strRange)
	case 's':
		// Security: Block Set-Cookie header in trailers
		return caseInsensitiveCompare(key, strSetCookie)
	case 't':
		return caseInsensitiveCompare(key, strTE) ||
			caseInsensitiveCompare(key, strTrailer) ||
			caseInsensitiveCompare(key, strTransferEncoding)
	case 'w':
		return caseInsensitiveCompare(key, strWWWAuthenticate)
	case 'x':
		// Security: Block X-Forwarded-* and X-Real-IP headers to prevent IP spoofing
		return (len(key) >= 11 && caseInsensitiveCompare(key[:11], []byte("x-forwarded"))) ||
			(len(key) >= 9 && caseInsensitiveCompare(key[:9], []byte("x-real-ip")))
	}
	return false
}

func (h *ResponseHeader) parseFirstLine(buf []byte) (int, error) {
	bNext := buf
	var b []byte
	var err error
	for len(b) == 0 {
		if b, bNext, err = nextLine(bNext); err != nil {
			return 0, err
		}
	}

	// parse protocol
	n := bytes.IndexByte(b, ' ')
	if n < 0 {
		if h.secureErrorLogMessage {
			return 0, errors.New("cannot find whitespace in the first line of response")
		}
		return 0, fmt.Errorf("cannot find whitespace in the first line of response %q", buf)
	}
	h.noHTTP11 = !bytes.Equal(b[:n], strHTTP11)
	b = b[n+1:]

	// parse status code
	h.statusCode, n, err = parseUintBuf(b)
	if err != nil {
		if h.secureErrorLogMessage {
			return 0, fmt.Errorf("cannot parse response status code: %w", err)
		}
		return 0, fmt.Errorf("cannot parse response status code: %w. Response %q", err, buf)
	}
	if len(b) > n && b[n] != ' ' {
		if h.secureErrorLogMessage {
			return 0, errors.New("unexpected char at the end of status code")
		}
		return 0, fmt.Errorf("unexpected char at the end of status code. Response %q", buf)
	}
	if len(b) > n+1 {
		h.SetStatusMessage(b[n+1:])
	}

	return len(buf) - len(bNext), nil
}

func isValidMethod(method []byte) bool {
	for _, ch := range method {
		if validMethodValueByteTable[ch] == 0 {
			return false
		}
	}
	return true
}

func (h *RequestHeader) parseFirstLine(buf []byte) (int, error) {
	bNext := buf
	var b []byte
	var err error
	for len(b) == 0 {
		if b, bNext, err = nextLine(bNext); err != nil {
			return 0, err
		}
	}

	// parse method
	n := bytes.IndexByte(b, ' ')
	if n <= 0 {
		if h.secureErrorLogMessage {
			return 0, errors.New("cannot find http request method")
		}
		return 0, fmt.Errorf("cannot find http request method in %q", buf)
	}
	h.method = append(h.method[:0], b[:n]...)

	if !isValidMethod(h.method) {
		if h.secureErrorLogMessage {
			return 0, errors.New("unsupported http request method")
		}
		return 0, fmt.Errorf("unsupported http request method %q in %q", h.method, buf)
	}

	b = b[n+1:]

	// Check for extra whitespace after method - only one space should separate method from URI
	if len(b) > 0 && b[0] == ' ' {
		if h.secureErrorLogMessage {
			return 0, errors.New("extra whitespace in request line")
		}
		return 0, fmt.Errorf("extra whitespace in request line %q", buf)
	}

	// parse requestURI - RFC 9112 requires exactly one space between components
	n = bytes.IndexByte(b, ' ')
	if n < 0 {
		return 0, fmt.Errorf("cannot find whitespace in the first line of request %q", buf)
	} else if n == 0 {
		if h.secureErrorLogMessage {
			return 0, errors.New("requestURI cannot be empty")
		}
		return 0, fmt.Errorf("requestURI cannot be empty in %q", buf)
	}

	// Check for extra whitespace - only one space should separate URI from HTTP version
	if n+1 < len(b) && b[n+1] == ' ' {
		if h.secureErrorLogMessage {
			return 0, errors.New("extra whitespace in request line")
		}
		return 0, fmt.Errorf("extra whitespace in request line %q", buf)
	}

	protoStr := b[n+1:]

	// Follow RFCs 7230 and 9112 and require that HTTP versions match the following pattern: HTTP/[0-9]\.[0-9]
	if len(protoStr) != len(strHTTP11) {
		if h.secureErrorLogMessage {
			return 0, fmt.Errorf("unsupported HTTP version %q", protoStr)
		}
		return 0, fmt.Errorf("unsupported HTTP version %q in %q", protoStr, buf)
	}
	if !bytes.HasPrefix(protoStr, strHTTP11[:5]) {
		if h.secureErrorLogMessage {
			return 0, fmt.Errorf("unsupported HTTP version %q", protoStr)
		}
		return 0, fmt.Errorf("unsupported HTTP version %q in %q", protoStr, buf)
	}
	if protoStr[5] < '0' || protoStr[5] > '9' || protoStr[7] < '0' || protoStr[7] > '9' {
		if h.secureErrorLogMessage {
			return 0, fmt.Errorf("unsupported HTTP version %q", protoStr)
		}
		return 0, fmt.Errorf("unsupported HTTP version %q in %q", protoStr, buf)
	}

	h.noHTTP11 = !bytes.Equal(protoStr, strHTTP11)
	h.protocol = append(h.protocol[:0], protoStr...)
	h.requestURI = append(h.requestURI[:0], b[:n]...)

	return len(buf) - len(bNext), nil
}

func readRawHeaders(dst, buf []byte) ([]byte, int, error) {
	n := bytes.IndexByte(buf, nChar)
	if n < 0 {
		return dst[:0], 0, errNeedMore
	}
	if (n == 1 && buf[0] == rChar) || n == 0 {
		// empty headers
		return dst, n + 1, nil
	}

	n++
	b := buf
	m := n
	for {
		b = b[m:]
		m = bytes.IndexByte(b, nChar)
		if m < 0 {
			return dst, 0, errNeedMore
		}
		m++
		n += m
		if (m == 2 && b[0] == rChar) || m == 1 {
			dst = append(dst, buf[:n]...)
			return dst, n, nil
		}
	}
}

func (h *ResponseHeader) parseHeaders(buf []byte) (int, error) {
	// 'identity' content-length by default
	h.contentLength = -2

	var s headerScanner
	s.b = buf
	var kv *argsKV

	for s.next() {
		if len(s.key) == 0 {
			h.connectionClose = true
			return 0, fmt.Errorf("invalid header key %q", s.key)
		}

		disableNormalizing := h.disableNormalizing
		for _, ch := range s.key {
			if !validHeaderFieldByte(ch) {
				h.connectionClose = true
				// We accept invalid headers with a space before the
				// colon, but must not canonicalize them.
				// See: https://github.com/valyala/fasthttp/issues/1917
				if ch == ' ' {
					disableNormalizing = true
					continue
				}
				return 0, fmt.Errorf("invalid header key %q", s.key)
			}
		}
		normalizeHeaderKey(s.key, disableNormalizing)

		for _, ch := range s.value {
			if !validHeaderValueByte(ch) {
				h.connectionClose = true
				return 0, fmt.Errorf("invalid header value %q", s.value)
			}
		}

		switch s.key[0] | 0x20 {
		case 'c':
			if caseInsensitiveCompare(s.key, strContentType) {
				h.contentType = append(h.contentType[:0], s.value...)
				continue
			}
			if caseInsensitiveCompare(s.key, strContentEncoding) {
				h.contentEncoding = append(h.contentEncoding[:0], s.value...)
				continue
			}
			if caseInsensitiveCompare(s.key, strContentLength) {
				if h.contentLength != -1 {
					var err error
					h.contentLength, err = parseContentLength(s.value)
					if err != nil {
						h.contentLength = -2
						h.connectionClose = true
						return 0, err
					}
					h.contentLengthBytes = append(h.contentLengthBytes[:0], s.value...)
				}
				continue
			}
			if caseInsensitiveCompare(s.key, strConnection) {
				if bytes.Equal(s.value, strClose) {
					h.connectionClose = true
				} else {
					h.connectionClose = false
					h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
				}
				continue
			}
		case 's':
			if caseInsensitiveCompare(s.key, strServer) {
				h.server = append(h.server[:0], s.value...)
				continue
			}
			if caseInsensitiveCompare(s.key, strSetCookie) {
				h.cookies, kv = allocArg(h.cookies)
				kv.key = getCookieKey(kv.key, s.value)
				kv.value = append(kv.value[:0], s.value...)
				continue
			}
		case 't':
			if caseInsensitiveCompare(s.key, strTransferEncoding) {
				if len(s.value) > 0 && !bytes.Equal(s.value, strIdentity) {
					h.contentLength = -1
					h.h = setArgBytes(h.h, strTransferEncoding, strChunked, argsHasValue)
				}
				continue
			}
			if caseInsensitiveCompare(s.key, strTrailer) {
				err := h.SetTrailerBytes(s.value)
				if err != nil {
					h.connectionClose = true
					return 0, err
				}
				continue
			}
		}
		h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
	}

	if s.err != nil {
		h.connectionClose = true
		return 0, s.err
	}

	if h.contentLength < 0 {
		h.contentLengthBytes = h.contentLengthBytes[:0]
	}
	if h.contentLength == -2 && !h.ConnectionUpgrade() && !h.mustSkipContentLength() {
		// According to modern HTTP/1.1 specifications (RFC 7230):
		// `identity` as a value for `Transfer-Encoding` was removed
		// in the errata to RFC 2616.
		// Therefore, we do not include `Transfer-Encoding: identity` in the header.
		// See: https://github.com/valyala/fasthttp/issues/1909
		h.connectionClose = true
	}
	if h.noHTTP11 && !h.connectionClose {
		// close connection for non-http/1.1 response unless 'Connection: keep-alive' is set.
		v := peekArgBytes(h.h, strConnection)
		h.connectionClose = !hasHeaderValue(v, strKeepAlive)
	}

	return s.r, nil
}

func (h *RequestHeader) parseHeaders(buf []byte) (int, error) {
	h.contentLength = -2

	contentLengthSeen := false

	var s headerScanner
	s.b = buf

	for s.next() {
		if len(s.key) == 0 {
			h.connectionClose = true
			return 0, fmt.Errorf("invalid header key %q", s.key)
		}

		disableNormalizing := h.disableNormalizing
		for _, ch := range s.key {
			if !validHeaderFieldByte(ch) {
				if ch == ' ' {
					disableNormalizing = true
					continue
				}
				h.connectionClose = true
				return 0, fmt.Errorf("invalid header key %q", s.key)
			}
		}
		normalizeHeaderKey(s.key, disableNormalizing)

		for _, ch := range s.value {
			if !validHeaderValueByte(ch) {
				h.connectionClose = true
				return 0, fmt.Errorf("invalid header value %q", s.value)
			}
		}

		if h.disableSpecialHeader {
			h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
			continue
		}

		switch s.key[0] | 0x20 {
		case 'h':
			if caseInsensitiveCompare(s.key, strHost) {
				h.host = append(h.host[:0], s.value...)
				continue
			}
		case 'u':
			if caseInsensitiveCompare(s.key, strUserAgent) {
				h.userAgent = append(h.userAgent[:0], s.value...)
				continue
			}
		case 'c':
			if caseInsensitiveCompare(s.key, strContentType) {
				h.contentType = append(h.contentType[:0], s.value...)
				continue
			}
			if caseInsensitiveCompare(s.key, strContentLength) {
				if contentLengthSeen {
					h.connectionClose = true
					return 0, errors.New("duplicate Content-Length header")
				}
				contentLengthSeen = true

				if h.contentLength != -1 {
					var err error
					h.contentLength, err = parseContentLength(s.value)
					if err != nil {
						h.contentLength = -2
						h.connectionClose = true
						return 0, err
					}
					h.contentLengthBytes = append(h.contentLengthBytes[:0], s.value...)
				}
				continue
			}
			if caseInsensitiveCompare(s.key, strConnection) {
				if bytes.Equal(s.value, strClose) {
					h.connectionClose = true
				} else {
					h.connectionClose = false
					h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
				}
				continue
			}
		case 't':
			if caseInsensitiveCompare(s.key, strTransferEncoding) {
				isIdentity := caseInsensitiveCompare(s.value, strIdentity)
				isChunked := caseInsensitiveCompare(s.value, strChunked)

				if !isIdentity && !isChunked {
					h.connectionClose = true
					if h.secureErrorLogMessage {
						return 0, errors.New("unsupported Transfer-Encoding")
					}
					return 0, fmt.Errorf("unsupported Transfer-Encoding: %q", s.value)
				}

				if isChunked {
					h.contentLength = -1
					h.h = setArgBytes(h.h, strTransferEncoding, strChunked, argsHasValue)
				}
				continue
			}
			if caseInsensitiveCompare(s.key, strTrailer) {
				err := h.SetTrailerBytes(s.value)
				if err != nil {
					h.connectionClose = true
					return 0, err
				}
				continue
			}
		}
		h.h = appendArgBytes(h.h, s.key, s.value, argsHasValue)
	}

	if s.err != nil {
		h.connectionClose = true
		return 0, s.err
	}

	if h.contentLength < 0 {
		h.contentLengthBytes = h.contentLengthBytes[:0]
	}
	if h.noHTTP11 && !h.connectionClose {
		// close connection for non-http/1.1 request unless 'Connection: keep-alive' is set.
		v := peekArgBytes(h.h, strConnection)
		h.connectionClose = !hasHeaderValue(v, strKeepAlive)
	}
	return s.r, nil
}

func (h *RequestHeader) collectCookies() {
	if h.cookiesCollected {
		return
	}

	for i, n := 0, len(h.h); i < n; i++ {
		kv := &h.h[i]
		if caseInsensitiveCompare(kv.key, strCookie) {
			h.cookies = parseRequestCookies(h.cookies, kv.value)
			tmp := *kv
			copy(h.h[i:], h.h[i+1:])
			n--
			i--
			h.h[n] = tmp
			h.h = h.h[:n]
		}
	}
	h.cookiesCollected = true
}

var errNonNumericChars = errors.New("non-numeric chars found")

func parseContentLength(b []byte) (int, error) {
	v, n, err := parseUintBuf(b)
	if err != nil {
		return -1, fmt.Errorf("cannot parse Content-Length: %w", err)
	}
	if n != len(b) {
		return -1, fmt.Errorf("cannot parse Content-Length: %w", errNonNumericChars)
	}
	return v, nil
}

type headerValueScanner struct {
	b     []byte
	value []byte
}

func (s *headerValueScanner) next() bool {
	b := s.b
	if len(b) == 0 {
		return false
	}
	n := bytes.IndexByte(b, ',')
	if n < 0 {
		s.value = stripSpace(b)
		s.b = b[len(b):]
		return true
	}
	s.value = stripSpace(b[:n])
	s.b = b[n+1:]
	return true
}

func stripSpace(b []byte) []byte {
	for len(b) > 0 && b[0] == ' ' {
		b = b[1:]
	}
	for len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	return b
}

func hasHeaderValue(s, value []byte) bool {
	var vs headerValueScanner
	vs.b = s
	for vs.next() {
		if caseInsensitiveCompare(vs.value, value) {
			return true
		}
	}
	return false
}

func nextLine(b []byte) ([]byte, []byte, error) {
	nNext := bytes.IndexByte(b, nChar)
	if nNext < 0 {
		return nil, nil, errNeedMore
	}
	n := nNext
	if n > 0 && b[n-1] == rChar {
		n--
	}
	return b[:n], b[nNext+1:], nil
}

func initHeaderKV(bufK, bufV []byte, key, value string, disableNormalizing bool) ([]byte, []byte) {
	bufK = getHeaderKeyBytes(bufK, key, disableNormalizing)
	// https://tools.ietf.org/html/rfc7230#section-3.2.4
	bufV = append(bufV[:0], value...)
	bufV = removeNewLines(bufV)
	return bufK, bufV
}

func getHeaderKeyBytes(bufK []byte, key string, disableNormalizing bool) []byte {
	bufK = append(bufK[:0], key...)
	normalizeHeaderKey(bufK, disableNormalizing || bytes.IndexByte(bufK, ' ') != -1)
	return bufK
}

func normalizeHeaderKey(b []byte, disableNormalizing bool) {
	if disableNormalizing {
		return
	}

	n := len(b)
	if n == 0 {
		return
	}

	// If the header isn't valid, we don't normalize it.
	for _, c := range b {
		if !validHeaderFieldByte(c) {
			return
		}
	}

	upper := true
	for i, c := range b {
		if upper {
			c = toUpperTable[c]
		} else {
			c = toLowerTable[c]
		}
		upper = c == '-'
		b[i] = c
	}
}

// removeNewLines will replace `\r` and `\n` with an empty space.
func removeNewLines(raw []byte) []byte {
	// check if a `\r` is present and save the position.
	// if no `\r` is found, check if a `\n` is present.
	foundR := bytes.IndexByte(raw, rChar)
	foundN := bytes.IndexByte(raw, nChar)
	start := 0

	switch {
	case foundN != -1:
		if foundR > foundN {
			start = foundN
		} else if foundR != -1 {
			start = foundR
		}
	case foundR != -1:
		start = foundR
	default:
		return raw
	}

	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case rChar, nChar:
			raw[i] = ' '
		default:
			continue
		}
	}
	return raw
}

// AppendNormalizedHeaderKey appends normalized header key (name) to dst
// and returns the resulting dst.
//
// Normalized header key starts with uppercase letter. The first letters
// after dashes are also uppercased. All the other letters are lowercased.
// Examples:
//
//   - coNTENT-TYPe -> Content-Type
//   - HOST -> Host
//   - foo-bar-baz -> Foo-Bar-Baz
func AppendNormalizedHeaderKey(dst []byte, key string) []byte {
	dst = append(dst, key...)
	normalizeHeaderKey(dst[len(dst)-len(key):], false)
	return dst
}

// AppendNormalizedHeaderKeyBytes appends normalized header key (name) to dst
// and returns the resulting dst.
//
// Normalized header key starts with uppercase letter. The first letters
// after dashes are also uppercased. All the other letters are lowercased.
// Examples:
//
//   - coNTENT-TYPe -> Content-Type
//   - HOST -> Host
//   - foo-bar-baz -> Foo-Bar-Baz
func AppendNormalizedHeaderKeyBytes(dst, key []byte) []byte {
	return AppendNormalizedHeaderKey(dst, b2s(key))
}

func appendTrailerBytes(dst []byte, trailer [][]byte, sep []byte) []byte {
	for i, n := 0, len(trailer); i < n; i++ {
		dst = append(dst, trailer[i]...)
		if i+1 < n {
			dst = append(dst, sep...)
		}
	}
	return dst
}

func copyTrailer(dst, src [][]byte) [][]byte {
	if cap(dst) >= len(src) {
		dst = dst[:len(src)]
	} else {
		dst = append(dst[:0], src...)
	}

	for i := range dst {
		l := len(src[i])
		if cap(dst[i]) >= l {
			dst[i] = dst[i][:l]
		} else {
			dst[i] = make([]byte, l)
		}
		copy(dst[i], src[i])
	}
	return dst
}

var (
	errNeedMore    = errors.New("need more data: cannot find trailing lf")
	errSmallBuffer = errors.New("small read buffer. Increase ReadBufferSize")
)

// ErrNothingRead is returned when a keep-alive connection is closed,
// either because the remote closed it or because of a read timeout.
type ErrNothingRead struct {
	error
}

// ErrSmallBuffer is returned when the provided buffer size is too small
// for reading request and/or response headers.
//
// ReadBufferSize value from Server or clients should reduce the number
// of such errors.
type ErrSmallBuffer struct {
	error
}

func mustPeekBuffered(r *bufio.Reader) []byte {
	buf, err := r.Peek(r.Buffered())
	if len(buf) == 0 || err != nil {
		panic(fmt.Sprintf("bufio.Reader.Peek() returned unexpected data (%q, %v)", buf, err))
	}
	return buf
}

func mustDiscard(r *bufio.Reader, n int) {
	if _, err := r.Discard(n); err != nil {
		panic(fmt.Sprintf("bufio.Reader.Discard(%d) failed: %v", n, err))
	}
}

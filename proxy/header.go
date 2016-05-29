package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// ResponseHeader represents HTTP response header.
//
// It is forbidden copying ResponseHeader instances.
// Create new instances instead and use CopyTo.
//
// ResponseHeader instance MUST NOT be used from concurrently running
// goroutines.
type ResponseHeader struct {
	noCopy noCopy

	disableNormalizing bool
	noHTTP11           bool
	connectionClose    bool

	statusCode         int
	contentLength      int
	contentLengthBytes []byte

	contentType []byte
	server      []byte

	h     []argsKV
	bufKV argsKV

	cookies []argsKV
}

// StatusCode returns response status code.
func (h *ResponseHeader) StatusCode() int {
	return h.statusCode
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (h *ResponseHeader) ConnectionClose() bool {
	return h.connectionClose
}

// SetConnectionClose sets 'Connection: close' header.
func (h *ResponseHeader) SetConnectionClose() {
	h.connectionClose = true
}

// ConnectionUpgrade returns true if 'Connection: Upgrade' header is set.
func (h *ResponseHeader) ConnectionUpgrade() bool {
	return hasHeaderValue(h.Peek("Connection"), strUpgrade)
}

// ContentLength returns Content-Length header value.
//
// It may be negative:
// -1 means Transfer-Encoding: chunked.
// -2 means Transfer-Encoding: identity.
func (h *ResponseHeader) ContentLength() int {
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
		h.h = delAllArgsBytes(h.h, strTransferEncoding)
	} else {
		h.contentLengthBytes = h.contentLengthBytes[:0]
		value := strChunked
		if contentLength == -2 {
			h.SetConnectionClose()
			value = strIdentity
		}
		h.h = setArgBytes(h.h, strTransferEncoding, value)
	}
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

// ContentType returns Content-Type header value.
func (h *ResponseHeader) ContentType() []byte {
	contentType := h.contentType
	if len(h.contentType) == 0 {
		contentType = defaultContentType
	}
	return contentType
}

// Server returns Server header value.
func (h *ResponseHeader) Server() []byte {
	return h.server
}

// IsHTTP11 returns true if the response is HTTP/1.1.
func (h *ResponseHeader) IsHTTP11() bool {
	return !h.noHTTP11
}

// DisableNormalizing disables header names' normalization.
//
// By default all the header names are normalized by uppercasing
// the first letter and all the first letters following dashes,
// while lowercasing all the other letters.
// Examples:
//
//     * CONNECTION -> Connection
//     * conteNT-tYPE -> Content-Type
//     * foo-bar-baz -> Foo-Bar-Baz
//
// Disable header names' normalization only if know what are you doing.
func (h *ResponseHeader) DisableNormalizing() {
	h.disableNormalizing = true
}

// Reset clears response header.
func (h *ResponseHeader) Reset() {
	h.disableNormalizing = false
	h.resetSkipNormalize()
}

func (h *ResponseHeader) resetSkipNormalize() {
	h.noHTTP11 = false
	h.connectionClose = false

	h.statusCode = 0
	h.contentLength = 0
	h.contentLengthBytes = h.contentLengthBytes[:0]

	h.contentType = h.contentType[:0]
	h.server = h.server[:0]

	h.h = h.h[:0]
	h.cookies = h.cookies[:0]
}

// Peek returns header value for the given key.
//
// Returned value is valid until the next call to ResponseHeader.
// Do not store references to returned value. Make copies instead.
func (h *ResponseHeader) Peek(key string) []byte {
	k := getHeaderKeyBytes(&h.bufKV, key, h.disableNormalizing)
	return h.peek(k)
}

func (h *ResponseHeader) peek(key []byte) []byte {
	switch string(key) {
	case "Content-Type":
		return h.ContentType()
	case "Server":
		return h.Server()
	case "Connection":
		if h.ConnectionClose() {
			return strClose
		}
		return peekArgBytes(h.h, key)
	case "Content-Length":
		return h.contentLengthBytes
	default:
		return peekArgBytes(h.h, key)
	}
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
		// treat all errors on the first byte read as EOF
		if n == 1 || err == io.EOF {
			return io.EOF
		}
		if err == bufio.ErrBufferFull {
			err = bufferFullError(r)
		}
		return fmt.Errorf("error when reading response headers: %s", err)
	}
	isEOF := (err != nil)
	b = mustPeekBuffered(r)
	var headersLen int
	if headersLen, err = h.parse(b); err != nil {
		if err == errNeedMore {
			if !isEOF {
				return err
			}

			// Buggy servers may leave trailing CRLFs after response body.
			// Treat this case as EOF.
			if isOnlyCRLF(b) {
				return io.EOF
			}
		}
		bStart, bEnd := bufferStartEnd(b)
		return fmt.Errorf("error when reading response headers: %s. buf=%q...%q", err, bStart, bEnd)
	}
	mustDiscard(r, headersLen)
	return nil
}

func bufferFullError(r *bufio.Reader) error {
	n := r.Buffered()
	b, err := r.Peek(n)
	if err != nil {
		panic(fmt.Sprintf("BUG: unexpected error returned from bufio.Reader.Peek(Buffered()): %s", err))
	}
	bStart, bEnd := bufferStartEnd(b)
	return fmt.Errorf("headers exceed %d bytes. Increase ReadBufferSize. buf=%q...%q", n, bStart, bEnd)
}

func bufferStartEnd(b []byte) ([]byte, []byte) {
	n := len(b)
	start := 200
	end := n - start
	if start >= end {
		start = n
		end = n
	}
	return b[:start], b[end:]
}

func isOnlyCRLF(b []byte) bool {
	for _, ch := range b {
		if ch != '\r' && ch != '\n' {
			return false
		}
	}
	return true
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
		return 0, fmt.Errorf("cannot find whitespace in the first line of response %q", buf)
	}
	h.noHTTP11 = !bytes.Equal(b[:n], strHTTP11)
	b = b[n+1:]

	// parse status code
	h.statusCode, n, err = parseUintBuf(b)
	if err != nil {
		return 0, fmt.Errorf("cannot parse response status code: %s. Response %q", err, buf)
	}
	if len(b) > n && b[n] != ' ' {
		return 0, fmt.Errorf("unexpected char at the end of status code. Response %q", buf)
	}

	return len(buf) - len(bNext), nil
}

func (h *ResponseHeader) parseHeaders(buf []byte) (int, error) {
	// 'identity' content-length by default
	h.contentLength = -2

	var s headerScanner
	s.b = buf
	s.disableNormalizing = h.disableNormalizing
	var err error
	var kv *argsKV
	for s.next() {
		switch string(s.key) {
		case "Content-Type":
			h.contentType = append(h.contentType[:0], s.value...)
		case "Server":
			h.server = append(h.server[:0], s.value...)
		case "Content-Length":
			if h.contentLength != -1 {
				if h.contentLength, err = parseContentLength(s.value); err != nil {
					h.contentLength = -2
				} else {
					h.contentLengthBytes = append(h.contentLengthBytes[:0], s.value...)
				}
			}
		case "Transfer-Encoding":
			if !bytes.Equal(s.value, strIdentity) {
				h.contentLength = -1
				h.h = setArgBytes(h.h, strTransferEncoding, strChunked)
			}
		case "Set-Cookie":
			h.cookies, kv = allocArg(h.cookies)
			kv.key = getCookieKey(kv.key, s.value)
			kv.value = append(kv.value[:0], s.value...)
		case "Connection":
			if bytes.Equal(s.value, strClose) {
				h.connectionClose = true
			} else {
				h.connectionClose = false
				h.h = appendArgBytes(h.h, s.key, s.value)
			}
		default:
			h.h = appendArgBytes(h.h, s.key, s.value)
		}
	}
	if s.err != nil {
		h.connectionClose = true
		return 0, s.err
	}

	if h.contentLength < 0 {
		h.contentLengthBytes = h.contentLengthBytes[:0]
	}
	if h.contentLength == -2 && !h.ConnectionUpgrade() && !h.mustSkipContentLength() {
		h.h = setArgBytes(h.h, strTransferEncoding, strIdentity)
		h.connectionClose = true
	}
	if h.noHTTP11 && !h.connectionClose {
		// close connection for non-http/1.1 response unless 'Connection: keep-alive' is set.
		v := peekArgBytes(h.h, strConnection)
		h.connectionClose = !hasHeaderValue(v, strKeepAlive) && !hasHeaderValue(v, strKeepAliveCamelCase)
	}

	return len(buf) - len(s.b), nil
}

func parseContentLength(b []byte) (int, error) {
	v, n, err := parseUintBuf(b)
	if err != nil {
		return -1, err
	}
	if n != len(b) {
		return -1, fmt.Errorf("non-numeric chars at the end of Content-Length")
	}
	return v, nil
}

type headerScanner struct {
	b     []byte
	key   []byte
	value []byte
	err   error

	disableNormalizing bool
}

func (s *headerScanner) next() bool {
	bLen := len(s.b)
	if bLen >= 2 && s.b[0] == '\r' && s.b[1] == '\n' {
		s.b = s.b[2:]
		return false
	}
	if bLen >= 1 && s.b[0] == '\n' {
		s.b = s.b[1:]
		return false
	}
	n := bytes.IndexByte(s.b, ':')
	if n < 0 {
		s.err = errNeedMore
		return false
	}
	s.key = s.b[:n]
	normalizeHeaderKey(s.key, s.disableNormalizing)
	n++
	for len(s.b) > n && s.b[n] == ' ' {
		n++
	}
	s.b = s.b[n:]
	n = bytes.IndexByte(s.b, '\n')
	if n < 0 {
		s.err = errNeedMore
		return false
	}
	s.value = s.b[:n]
	s.b = s.b[n+1:]

	if n > 0 && s.value[n-1] == '\r' {
		n--
	}
	for n > 0 && s.value[n-1] == ' ' {
		n--
	}
	s.value = s.value[:n]
	return true
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
		if bytes.Equal(vs.value, value) {
			return true
		}
	}
	return false
}

func nextLine(b []byte) ([]byte, []byte, error) {
	nNext := bytes.IndexByte(b, '\n')
	if nNext < 0 {
		return nil, nil, errNeedMore
	}
	n := nNext
	if n > 0 && b[n-1] == '\r' {
		n--
	}
	return b[:n], b[nNext+1:], nil
}

func getHeaderKeyBytes(kv *argsKV, key string, disableNormalizing bool) []byte {
	kv.key = append(kv.key[:0], key...)
	normalizeHeaderKey(kv.key, disableNormalizing)
	return kv.key
}

func normalizeHeaderKey(b []byte, disableNormalizing bool) {
	if disableNormalizing {
		return
	}

	n := len(b)
	up := true
	for i := 0; i < n; i++ {
		switch b[i] {
		case '-':
			up = true
		default:
			if up {
				up = false
				uppercaseByte(&b[i])
			} else {
				lowercaseByte(&b[i])
			}
		}
	}
}

var errNeedMore = errors.New("need more data: cannot find trailing lf")

func mustPeekBuffered(r *bufio.Reader) []byte {
	buf, err := r.Peek(r.Buffered())
	if len(buf) == 0 || err != nil {
		panic(fmt.Sprintf("bufio.Reader.Peek() returned unexpected data (%q, %v)", buf, err))
	}
	return buf
}

func mustDiscard(r *bufio.Reader, n int) {
	if _, err := r.Discard(n); err != nil {
		panic(fmt.Sprintf("bufio.Reader.Discard(%d) failed: %s", n, err))
	}
}

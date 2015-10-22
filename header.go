package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

var (
	defaultServerName  = []byte("fasthttp server")
	defaultContentType = []byte("text/plain; charset=utf-8")
)

var (
	strSlash           = []byte("/")
	strCRLF            = []byte("\r\n")
	strHTTP            = []byte("http")
	strHTTP11          = []byte("HTTP/1.1")
	strColonSlashSlash = []byte("://")
	strColonSpace      = []byte(": ")

	strGet  = []byte("GET")
	strHead = []byte("HEAD")
	strPost = []byte("POST")

	strConnection       = []byte("Connection")
	strContentLength    = []byte("Content-Length")
	strContentType      = []byte("Content-Type")
	strDate             = []byte("Date")
	strHost             = []byte("Host")
	strReferer          = []byte("Referer")
	strServer           = []byte("Server")
	strTransferEncoding = []byte("Transfer-Encoding")
	strUserAgent        = []byte("User-Agent")

	strClose               = []byte("close")
	strChunked             = []byte("chunked")
	strPostArgsContentType = []byte("application/x-www-form-urlencoded")
)

type ResponseHeader struct {
	StatusCode      int
	ContentLength   int
	ConnectionClose bool

	contentType []byte
	server      []byte

	h     []argsKV
	bufKV argsKV
}

type RequestHeader struct {
	Method        []byte
	RequestURI    []byte
	ContentLength int

	host        []byte
	contentType []byte

	h     []argsKV
	bufKV argsKV
}

func (h *RequestHeader) IsMethodGet() bool {
	return bytes.Equal(h.Method, strGet)
}

func (h *RequestHeader) IsMethodPost() bool {
	return bytes.Equal(h.Method, strPost)
}

func (h *RequestHeader) IsMethodHead() bool {
	return bytes.Equal(h.Method, strHead)
}

func (h *ResponseHeader) Clear() {
	h.StatusCode = 0
	h.ContentLength = 0
	h.ConnectionClose = false

	h.contentType = h.contentType[:0]
	h.server = h.server[:0]

	h.h = h.h[:0]
}

func (h *RequestHeader) Clear() {
	h.Method = h.Method[:0]
	h.RequestURI = h.RequestURI[:0]
	h.ContentLength = 0

	h.host = h.host[:0]
	h.contentType = h.contentType[:0]

	h.h = h.h[:0]
}

func (h *ResponseHeader) Set(key, value string) {
	initHeaderKV(&h.bufKV, key, value)
	h.set(h.bufKV.key, h.bufKV.value)
}

func (h *ResponseHeader) set(key, value []byte) {
	switch {
	case bytes.Equal(strContentType, key):
		h.contentType = append(h.contentType[:0], value...)
	case bytes.Equal(strServer, key):
		h.server = append(h.server[:0], value...)
	case bytes.Equal(strContentLength, key):
		// skip Conent-Length setting, since it will be set automatically.
	case bytes.Equal(strConnection, key):
		if bytes.Equal(strClose, value) {
			h.ConnectionClose = true
		}
		// skip other 'Connection' shit :)
	case bytes.Equal(strTransferEncoding, key):
		// Transfer-Encoding is managed automatically.
	case bytes.Equal(strDate, key):
		// Date is managed automatically.
	default:
		h.h = setKV(h.h, key, value)
	}
}

func (h *ResponseHeader) SetBytes(key string, value []byte) {
	k := getHeaderKeyBytes(&h.bufKV, key)
	h.set(k, value)
}

func (h *ResponseHeader) setStr(key []byte, value string) {
	h.bufKV.value = AppendBytesStr(h.bufKV.value[:0], value)
	h.set(key, h.bufKV.value)
}

func (h *RequestHeader) Set(key, value string) {
	initHeaderKV(&h.bufKV, key, value)
	h.set(h.bufKV.key, h.bufKV.value)
}

func (h *RequestHeader) set(key, value []byte) {
	switch {
	case bytes.Equal(strHost, key):
		h.host = append(h.host[:0], value...)
	case bytes.Equal(strContentType, key):
		h.contentType = append(h.contentType[:0], value...)
	case bytes.Equal(strContentLength, key):
		// Content-Length is managed automatically.
	case bytes.Equal(strTransferEncoding, key):
		// Transfer-Encoding is managed automatically.
	case bytes.Equal(strConnection, key):
		// Connection is managed automatically.
	default:
		h.h = setKV(h.h, key, value)
	}
}

func (h *RequestHeader) SetBytes(key string, value []byte) {
	k := getHeaderKeyBytes(&h.bufKV, key)
	h.set(k, value)
}

func (h *ResponseHeader) Peek(key string) []byte {
	k := getHeaderKeyBytes(&h.bufKV, key)
	return h.peek(k)
}

func (h *RequestHeader) Peek(key string) []byte {
	k := getHeaderKeyBytes(&h.bufKV, key)
	return h.peek(k)
}

func (h *ResponseHeader) peek(key []byte) []byte {
	switch {
	case bytes.Equal(strContentType, key):
		return h.contentType
	case bytes.Equal(strServer, key):
		return h.server
	case bytes.Equal(strConnection, key):
		if h.ConnectionClose {
			return strClose
		}
		return nil
	default:
		return peekKV(h.h, key)
	}
}

func (h *RequestHeader) peek(key []byte) []byte {
	switch {
	case bytes.Equal(strHost, key):
		return h.host
	case bytes.Equal(strContentType, key):
		return h.contentType
	default:
		return peekKV(h.h, key)
	}
}

func (h *ResponseHeader) Get(key string) string {
	return string(h.Peek(key))
}

func (h *RequestHeader) Get(key string) string {
	return string(h.Peek(key))
}

func (h *ResponseHeader) Read(r *bufio.Reader) error {
	n := 1
	for {
		err := h.tryRead(r, n)
		if err == nil {
			return nil
		}
		if err != errNeedMore {
			h.Clear()
			return err
		}
		n = r.Buffered() + 1
	}
}

func (h *ResponseHeader) tryRead(r *bufio.Reader, n int) error {
	h.Clear()
	b, err := r.Peek(n)
	if len(b) == 0 {
		if err == io.EOF {
			return err
		}
		if err == nil {
			panic("bufio.Reader.Peek() returned nil, nil")
		}
		return fmt.Errorf("error when reading response headers: %s", err)
	}
	isEOF := (err != nil)
	b = mustPeekBuffered(r)
	bLen := len(b)
	if b, err = h.parse(b); err != nil {
		if err == errNeedMore && !isEOF {
			return err
		}
		return fmt.Errorf("erorr when reading response headers: %s", err)
	}
	headersLen := bLen - len(b)
	mustDiscard(r, headersLen)
	return nil
}

func (h *RequestHeader) Read(r *bufio.Reader) error {
	n := 1
	for {
		err := h.tryRead(r, n)
		if err == nil {
			return nil
		}
		if err != errNeedMore {
			h.Clear()
			return err
		}
		n = r.Buffered() + 1
	}
}

func (h *RequestHeader) tryRead(r *bufio.Reader, n int) error {
	h.Clear()
	b, err := r.Peek(n)
	if len(b) == 0 {
		if err == io.EOF {
			return err
		}
		if err == nil {
			panic("bufio.Reader.Peek() returned nil, nil")
		}
		return fmt.Errorf("error when reading request headers: %s", err)
	}
	isEOF := (err != nil)
	b = mustPeekBuffered(r)
	bLen := len(b)
	if b, err = h.parse(b); err != nil {
		if err == errNeedMore && !isEOF {
			return err
		}
		return fmt.Errorf("error when reading request headers: %s", err)
	}
	headersLen := bLen - len(b)
	mustDiscard(r, headersLen)
	return nil
}

func init() {
	refreshServerDate()
	go func() {
		for {
			time.Sleep(time.Second)
			refreshServerDate()
		}
	}()
}

var (
	serverDate  atomic.Value
	gmtLocation = func() *time.Location {
		x, err := time.LoadLocation("GMT")
		if err != nil {
			panic(fmt.Sprintf("cannot load GMT location: %s", err))
		}
		return x
	}()
)

func refreshServerDate() {
	s := time.Now().In(gmtLocation).Format(time.RFC1123)
	serverDate.Store([]byte(s))
}

func (h *ResponseHeader) Write(w *bufio.Writer) error {
	statusCode := h.StatusCode
	if statusCode < 0 {
		return fmt.Errorf("response cannot have negative status code=%d", statusCode)
	}
	if statusCode == 0 {
		statusCode = StatusOK
	}
	w.Write(statusLine(statusCode))

	server := h.server
	if len(server) == 0 {
		server = defaultServerName
	}
	writeHeaderLine(w, strServer, server)
	writeHeaderLine(w, strDate, serverDate.Load().([]byte))

	contentType := h.contentType
	if len(contentType) == 0 {
		contentType = defaultContentType
	}
	writeHeaderLine(w, strContentType, contentType)

	if h.ContentLength < 0 {
		return fmt.Errorf("missing required Content-Length header")
	}
	writeContentLength(w, h.ContentLength)

	if h.ConnectionClose {
		writeHeaderLine(w, strConnection, strClose)
	}

	for i, n := 0, len(h.h); i < n; i++ {
		kv := &h.h[i]
		if !bytes.Equal(strServer, kv.key) && !bytes.Equal(strContentType, kv.key) {
			writeHeaderLine(w, kv.key, kv.value)
		}
	}

	_, err := w.Write(strCRLF)
	return err
}

func (h *RequestHeader) Write(w *bufio.Writer) error {
	method := h.Method
	if len(method) == 0 {
		method = strGet
	}
	w.Write(method)
	w.WriteByte(' ')
	if len(h.RequestURI) == 0 {
		return fmt.Errorf("missing required RequestURI")
	}
	w.Write(h.RequestURI)
	w.WriteByte(' ')
	w.Write(strHTTP11)
	w.Write(strCRLF)

	host := h.host
	if len(host) == 0 {
		return fmt.Errorf("missing required Host header")
	}
	writeHeaderLine(w, strHost, host)

	if h.IsMethodPost() {
		contentType := h.contentType
		if len(contentType) == 0 {
			return fmt.Errorf("missing required Content-Type header for POST request")
		}
		writeHeaderLine(w, strContentType, contentType)
		if h.ContentLength < 0 {
			return fmt.Errorf("missing required Content-Length header for POST request")
		}
		writeContentLength(w, h.ContentLength)
	}

	for i, n := 0, len(h.h); i < n; i++ {
		kv := &h.h[i]
		if !bytes.Equal(strHost, kv.key) && !bytes.Equal(strContentType, kv.key) {
			writeHeaderLine(w, kv.key, kv.value)
		}
	}

	_, err := w.Write(strCRLF)
	return err
}

func writeHeaderLine(w *bufio.Writer, key, value []byte) {
	w.Write(key)
	w.Write(strColonSpace)
	w.Write(value)
	w.Write(strCRLF)
}

func writeContentLength(w *bufio.Writer, contentLength int) {
	w.Write(strContentLength)
	w.Write(strColonSpace)
	writeInt(w, contentLength)
	w.Write(strCRLF)
}

func (h *ResponseHeader) parse(buf []byte) (b []byte, err error) {
	b, err = h.parseFirstLine(buf)
	if err != nil {
		return nil, err
	}
	return h.parseHeaders(b)
}

func (h *RequestHeader) parse(buf []byte) (b []byte, err error) {
	b, err = h.parseFirstLine(buf)
	if err != nil {
		return nil, err
	}
	return h.parseHeaders(b)
}

func (h *ResponseHeader) parseFirstLine(buf []byte) (b []byte, err error) {
	bNext := buf
	for len(b) == 0 {
		if b, bNext, err = nextLine(bNext); err != nil {
			return nil, err
		}
	}

	// skip protocol
	n := bytes.IndexByte(b, ' ')
	if n < 0 {
		return nil, fmt.Errorf("cannot find whitespace in the first line of response %q", buf)
	}
	b = b[n+1:]

	// parse status code
	h.StatusCode, n, err = parseUintBuf(b)
	if err != nil {
		return nil, fmt.Errorf("cannot parse response status code: %s. Response %q", err, buf)
	}
	if len(b) > n && b[n] != ' ' {
		return nil, fmt.Errorf("unexpected char at the end of status code. Response %q", buf)
	}

	return bNext, nil
}

func (h *RequestHeader) parseFirstLine(buf []byte) (b []byte, err error) {
	bNext := buf
	for len(b) == 0 {
		if b, bNext, err = nextLine(bNext); err != nil {
			return nil, err
		}
	}

	// parse method
	n := bytes.IndexByte(b, ' ')
	if n <= 0 {
		return nil, fmt.Errorf("cannot find http request method in %q", buf)
	}
	h.Method = append(h.Method[:0], b[:n]...)
	b = b[n+1:]

	// parse requestURI
	n = bytes.IndexByte(b, ' ')
	if n < 0 {
		n = len(b)
	} else if n == 0 {
		return nil, fmt.Errorf("RequestURI cannot be empty in %q", buf)
	}
	h.RequestURI = append(h.RequestURI[:0], b[:n]...)

	return bNext, nil
}

func (h *ResponseHeader) parseHeaders(buf []byte) ([]byte, error) {
	h.ContentLength = -2

	var p headerParser
	p.init(buf)
	var err error
	for p.next() {
		switch {
		case bytes.Equal(p.key, strContentType):
			h.contentType = append(h.contentType[:0], p.value...)
		case bytes.Equal(p.key, strServer):
			h.server = append(h.server[:0], p.value...)
		case bytes.Equal(p.key, strContentLength):
			if h.ContentLength != -1 {
				h.ContentLength, err = parseContentLength(p.value)
				if err != nil {
					if err == errNeedMore {
						return nil, err
					}
					return nil, fmt.Errorf("cannot parse Content-Length %q: %s at %q", p.value, err, buf)
				}
			}
		case bytes.Equal(p.key, strTransferEncoding):
			if bytes.Equal(p.value, strChunked) {
				h.ContentLength = -1
			}
		case bytes.Equal(p.key, strConnection):
			if bytes.Equal(p.value, strClose) {
				h.ConnectionClose = true
			}
		default:
			h.h = setKV(h.h, p.key, p.value)
		}
	}
	if p.err != nil {
		return nil, p.err
	}

	if len(h.contentType) == 0 {
		return nil, fmt.Errorf("missing required Content-Type header in %q", buf)
	}
	if h.ContentLength == -2 {
		return nil, fmt.Errorf("missing both Content-Length and Transfer-Encoding: chunked in %q", buf)
	}
	return p.b, nil
}

func (h *RequestHeader) parseHeaders(buf []byte) ([]byte, error) {
	h.ContentLength = -2

	var p headerParser
	p.init(buf)
	var err error
	for p.next() {
		switch {
		case bytes.Equal(p.key, strHost):
			h.host = append(h.host[:0], p.value...)
		case bytes.Equal(p.key, strContentType):
			h.contentType = append(h.contentType[:0], p.value...)
		case bytes.Equal(p.key, strContentLength):
			if h.ContentLength != -1 {
				h.ContentLength, err = parseContentLength(p.value)
				if err != nil {
					if err == errNeedMore {
						return nil, err
					}
					return nil, fmt.Errorf("cannot parse Content-Length %q: %s at %q", p.value, err, buf)
				}
			}
		case bytes.Equal(p.key, strTransferEncoding):
			if bytes.Equal(p.value, strChunked) {
				h.ContentLength = -1
			}
		default:
			h.h = setKV(h.h, p.key, p.value)
		}
	}
	if p.err != nil {
		return nil, p.err
	}

	if len(h.host) == 0 {
		return nil, fmt.Errorf("missing required Host header in %q", buf)
	}
	if h.IsMethodPost() {
		if len(h.contentType) == 0 {
			return nil, fmt.Errorf("missing Content-Type for POST header in %q", buf)
		}
		if h.ContentLength == -2 {
			return nil, fmt.Errorf("missing Content-Length for POST header in %q", buf)
		}
	} else {
		h.ContentLength = 0
	}
	return p.b, nil
}

func parseContentLength(b []byte) (int, error) {
	v, n, err := parseUintBuf(b)
	if err != nil {
		return -1, err
	}
	if n != len(b) {
		return -1, fmt.Errorf("Non-numeric chars at the end of Content-Length")
	}
	return v, nil
}

type headerParser struct {
	headers []byte
	b       []byte
	key     []byte
	value   []byte
	err     error
	lineNum int
}

func (p *headerParser) init(headers []byte) {
	p.headers = headers
	p.b = headers
	p.key = nil
	p.value = nil
	p.lineNum = 0
}

func (p *headerParser) next() bool {
	var b []byte
	b, p.b, p.err = nextLine(p.b)
	if p.err != nil {
		return false
	}
	if len(b) == 0 {
		return false
	}

	p.lineNum++
	n := bytes.IndexByte(b, ':')
	if n < 0 {
		p.err = fmt.Errorf("cannot find colon at line #%d in %q", p.lineNum, p.headers)
		return false
	}
	p.key = b[:n]
	n++
	normalizeHeaderKey(p.key)
	for len(b) > n && b[n] == ' ' {
		n++
	}
	p.value = b[n:]
	return true
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

func initHeaderKV(kv *argsKV, key, value string) {
	kv.key = getHeaderKeyBytes(kv, key)
	kv.value = AppendBytesStr(kv.value[:0], value)
}

func getHeaderKeyBytes(kv *argsKV, key string) []byte {
	kv.key = AppendBytesStr(kv.key[:0], key)
	normalizeHeaderKey(kv.key)
	return kv.key
}

func normalizeHeaderKey(b []byte) {
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

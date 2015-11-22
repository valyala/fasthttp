package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Request represents HTTP request.
//
// It is forbidden copying Request instances. Create new instances
// and use CopyTo() instead.
type Request struct {
	// Request header
	Header RequestHeader

	body []byte
	w    requestBodyWriter

	uri       URI
	parsedURI bool

	postArgs       Args
	parsedPostArgs bool
}

// Response represents HTTP response.
//
// It is forbidden copying Response instances. Create new instances
// and use CopyTo() instead.
type Response struct {
	// Response header
	Header ResponseHeader

	body []byte
	w    responseBodyWriter

	bodyStream io.Reader

	// If set to true, Response.Read() skips reading body.
	// Use it for HEAD requests.
	SkipBody bool
}

// SetRequestURI sets RequestURI.
func (req *Request) SetRequestURI(requestURI string) {
	req.Header.SetRequestURI(requestURI)
}

// SetRequestURIBytes sets RequestURI.
func (req *Request) SetRequestURIBytes(requestURI []byte) {
	req.Header.SetRequestURIBytes(requestURI)
}

// StatusCode returns response status code.
func (resp *Response) StatusCode() int {
	return resp.Header.StatusCode()
}

// SetStatusCode sets response status code.
func (resp *Response) SetStatusCode(statusCode int) {
	resp.Header.SetStatusCode(statusCode)
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (resp *Response) ConnectionClose() bool {
	return resp.Header.ConnectionClose()
}

// SetConnectionClose sets 'Connection: close' header.
func (resp *Response) SetConnectionClose() {
	resp.Header.SetConnectionClose()
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (req *Request) ConnectionClose() bool {
	return req.Header.ConnectionClose()
}

// SetConnectionClose sets 'Connection: close' header.
func (req *Request) SetConnectionClose() {
	req.Header.SetConnectionClose()
}

// SetBodyStream sets response body stream and, optionally body size.
//
// If bodySize is >= 0, then bodySize bytes are read from bodyStream
// and used as response body.
//
// If bodySize < 0, then bodyStream is read until io.EOF.
//
// bodyStream.Close() is called after finishing reading all body data
// if it implements io.Closer.
func (resp *Response) SetBodyStream(bodyStream io.Reader, bodySize int) {
	resp.bodyStream = bodyStream
	resp.Header.SetContentLength(bodySize)
}

// BodyWriter returns writer for populating response body.
func (resp *Response) BodyWriter() io.Writer {
	resp.w.r = resp
	return &resp.w
}

// BodyWriter returns writer for populating request body.
func (req *Request) BodyWriter() io.Writer {
	req.w.r = req
	return &req.w
}

type responseBodyWriter struct {
	r *Response
}

func (w *responseBodyWriter) Write(p []byte) (int, error) {
	w.r.body = append(w.r.body, p...)
	return len(p), nil
}

type requestBodyWriter struct {
	r *Request
}

func (w *requestBodyWriter) Write(p []byte) (int, error) {
	w.r.body = append(w.r.body, p...)
	return len(p), nil
}

// Body returns response body.
func (resp *Response) Body() []byte {
	return resp.body
}

// SetBody sets response body.
func (resp *Response) SetBody(body []byte) {
	resp.body = append(resp.body[:0], body...)
}

// Body returns request body.
func (req *Request) Body() []byte {
	return req.body
}

// SetBody sets request body.
func (req *Request) SetBody(body []byte) {
	req.body = append(req.body[:0], body...)
}

// CopyTo copies req contents to dst.
func (req *Request) CopyTo(dst *Request) {
	dst.Reset()
	req.Header.CopyTo(&dst.Header)
	dst.body = append(dst.body[:0], req.body...)
	if req.parsedURI {
		dst.parseURI()
	}
	if req.parsedPostArgs {
		dst.parsePostArgs()
	}
}

// CopyTo copies resp contents to dst except of BodyStream.
func (resp *Response) CopyTo(dst *Response) {
	dst.Reset()
	resp.Header.CopyTo(&dst.Header)
	dst.body = append(dst.body[:0], resp.body...)
	dst.SkipBody = resp.SkipBody
}

// URI returns request URI
func (req *Request) URI() *URI {
	req.parseURI()
	return &req.uri
}

func (req *Request) parseURI() {
	if req.parsedURI {
		return
	}
	req.parsedURI = true

	req.uri.parseQuick(req.Header.RequestURI(), &req.Header)
}

// PostArgs returns POST arguments.
func (req *Request) PostArgs() *Args {
	req.parsePostArgs()
	return &req.postArgs
}

func (req *Request) parsePostArgs() {
	if req.parsedPostArgs {
		return
	}
	req.parsedPostArgs = true

	if !req.Header.IsPost() {
		return
	}
	if !bytes.Equal(req.Header.ContentType(), strPostArgsContentType) {
		return
	}
	req.postArgs.ParseBytes(req.body)
	return
}

// Reset clears request contents.
func (req *Request) Reset() {
	req.Header.Reset()
	req.clearSkipHeader()
}

func (req *Request) clearSkipHeader() {
	req.body = req.body[:0]
	req.uri.Reset()
	req.parsedURI = false
	req.postArgs.Reset()
	req.parsedPostArgs = false
}

// Reset clears response contents.
func (resp *Response) Reset() {
	resp.Header.Reset()
	resp.clearSkipHeader()
}

func (resp *Response) clearSkipHeader() {
	resp.body = resp.body[:0]
	resp.bodyStream = nil
}

// Read reads request (including body) from the given r.
func (req *Request) Read(r *bufio.Reader) error {
	req.clearSkipHeader()
	err := req.Header.Read(r)
	if err != nil {
		return err
	}

	if req.Header.IsPost() {
		req.body, err = readBody(r, req.Header.ContentLength(), req.body)
		if err != nil {
			req.Reset()
			return err
		}
		req.Header.SetContentLength(len(req.body))
	}
	return nil
}

// Read reads response (including body) from the given r.
func (resp *Response) Read(r *bufio.Reader) error {
	resp.clearSkipHeader()
	err := resp.Header.Read(r)
	if err != nil {
		return err
	}

	if !isSkipResponseBody(resp.Header.StatusCode()) && !resp.SkipBody {
		resp.body, err = readBody(r, resp.Header.ContentLength(), resp.body)
		if err != nil {
			resp.Reset()
			return err
		}
		resp.Header.SetContentLength(len(resp.body))
	}
	return nil
}

func isSkipResponseBody(statusCode int) bool {
	// From http/1.1 specs:
	// All 1xx (informational), 204 (no content), and 304 (not modified) responses MUST NOT include a message-body
	if statusCode >= 100 && statusCode < 200 {
		return true
	}
	return statusCode == StatusNoContent || statusCode == StatusNotModified
}

var errRequestHostRequired = errors.New("Missing required Host header in request")

// Write writes request to w.
//
// Write doesn't flush request to w for performance reasons.
func (req *Request) Write(w *bufio.Writer) error {
	if len(req.Header.Host()) == 0 {
		uri := req.URI()
		host := uri.Host()
		if len(host) == 0 {
			return errRequestHostRequired
		}
		req.Header.SetHostBytes(host)
		req.Header.SetRequestURIBytes(uri.RequestURI())
	}
	req.Header.SetContentLength(len(req.body))
	err := req.Header.Write(w)
	if err != nil {
		return err
	}
	if req.Header.IsPost() {
		_, err = w.Write(req.body)
	} else if len(req.body) > 0 {
		return fmt.Errorf("Non-zero body for non-POST request. body=%q", req.body)
	}
	return err
}

// Write writes response to w.
//
// Write doesn't flush response to w for performance reasons.
func (resp *Response) Write(w *bufio.Writer) error {
	var err error
	if resp.bodyStream != nil {
		contentLength := resp.Header.ContentLength()
		if contentLength >= 0 {
			if err = resp.Header.Write(w); err != nil {
				return err
			}
			if err = writeBodyFixedSize(w, resp.bodyStream, contentLength); err != nil {
				return err
			}
		} else {
			resp.Header.SetContentLength(-1)
			if err = resp.Header.Write(w); err != nil {
				return err
			}
			if err = writeBodyChunked(w, resp.bodyStream); err != nil {
				return err
			}
		}
		if bsc, ok := resp.bodyStream.(io.Closer); ok {
			err = bsc.Close()
		}
		return err
	}

	resp.Header.SetContentLength(len(resp.body))
	if err = resp.Header.Write(w); err != nil {
		return err
	}
	_, err = w.Write(resp.body)
	return err
}

func writeBodyChunked(w *bufio.Writer, r io.Reader) error {
	vbuf := copyBufPool.Get()
	if vbuf == nil {
		vbuf = make([]byte, 4096)
	}
	buf := vbuf.([]byte)

	var err error
	var n int
	for {
		n, err = r.Read(buf)
		if n == 0 {
			if err == nil {
				panic("BUG: io.Reader returned 0, nil")
			}
			if err == io.EOF {
				if err = writeChunk(w, buf[:0]); err != nil {
					break
				}
				err = nil
			}
			break
		}
		if err = writeChunk(w, buf[:n]); err != nil {
			break
		}
	}

	copyBufPool.Put(vbuf)
	return err
}

var limitReaderPool sync.Pool

func writeBodyFixedSize(w *bufio.Writer, r io.Reader, size int) error {
	vbuf := copyBufPool.Get()
	if vbuf == nil {
		vbuf = make([]byte, 4096)
	}
	buf := vbuf.([]byte)

	vlr := limitReaderPool.Get()
	if vlr == nil {
		vlr = &io.LimitedReader{}
	}
	lr := vlr.(*io.LimitedReader)
	lr.R = r
	lr.N = int64(size)

	n, err := io.CopyBuffer(w, lr, buf)

	limitReaderPool.Put(vlr)
	copyBufPool.Put(vbuf)

	if n != int64(size) && err == nil {
		err = fmt.Errorf("read %d bytes from BodyStream instead of %d bytes", n, size)
	}
	return err
}

func writeChunk(w *bufio.Writer, b []byte) error {
	n := len(b)
	writeHexInt(w, n)
	w.Write(strCRLF)
	w.Write(b)
	_, err := w.Write(strCRLF)
	return err
}

var copyBufPool sync.Pool

func readBody(r *bufio.Reader, contentLength int, dst []byte) ([]byte, error) {
	dst = dst[:0]
	if contentLength >= 0 {
		return appendBodyFixedSize(r, dst, contentLength)
	}
	if contentLength == -1 {
		return readBodyChunked(r, dst)
	}
	return readBodyIdentity(r, dst)
}

func readBodyIdentity(r *bufio.Reader, dst []byte) ([]byte, error) {
	dst = dst[:cap(dst)]
	if len(dst) == 0 {
		dst = make([]byte, 1024)
	}
	offset := 0
	for {
		nn, err := r.Read(dst[offset:])
		if nn <= 0 {
			if err != nil {
				if err == io.EOF {
					return dst[:offset], nil
				}
				return dst[:offset], err
			}
			panic(fmt.Sprintf("BUG: bufio.Read() returned (%d, nil)", nn))
		}
		offset += nn
		if len(dst) == offset {
			b := make([]byte, round2(2*offset))
			copy(b, dst)
			dst = b
		}
	}
}

func appendBodyFixedSize(r *bufio.Reader, dst []byte, n int) ([]byte, error) {
	if n == 0 {
		return dst, nil
	}

	offset := len(dst)
	dstLen := offset + n
	if cap(dst) < dstLen {
		b := make([]byte, round2(dstLen))
		copy(b, dst)
		dst = b
	}
	dst = dst[:dstLen]

	for {
		nn, err := r.Read(dst[offset:])
		if nn <= 0 {
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return dst[:offset], err
			}
			panic(fmt.Sprintf("BUG: bufio.Read() returned (%d, nil)", nn))
		}
		offset += nn
		if offset == dstLen {
			return dst, nil
		}
	}
}

func readBodyChunked(r *bufio.Reader, dst []byte) ([]byte, error) {
	if len(dst) > 0 {
		panic("BUG: expected zero-length buffer")
	}

	strCRLFLen := len(strCRLF)
	for {
		chunkSize, err := parseChunkSize(r)
		if err != nil {
			return dst, err
		}
		dst, err = appendBodyFixedSize(r, dst, chunkSize+strCRLFLen)
		if err != nil {
			return dst, err
		}
		if !bytes.Equal(dst[len(dst)-strCRLFLen:], strCRLF) {
			return dst, fmt.Errorf("cannot find crlf at the end of chunk")
		}
		dst = dst[:len(dst)-strCRLFLen]
		if chunkSize == 0 {
			return dst, nil
		}
	}
}

func parseChunkSize(r *bufio.Reader) (int, error) {
	n, err := readHexInt(r)
	if err != nil {
		return -1, err
	}
	c, err := r.ReadByte()
	if err != nil {
		return -1, fmt.Errorf("cannot read '\r' char at the end of chunk size: %s", err)
	}
	if c != '\r' {
		return -1, fmt.Errorf("unexpected char %q at the end of chunk size. Expected %q", c, '\r')
	}
	c, err = r.ReadByte()
	if err != nil {
		return -1, fmt.Errorf("cannot read '\n' char at the end of chunk size: %s", err)
	}
	if c != '\n' {
		return -1, fmt.Errorf("unexpected char %q at the end of chunk size. Expected %q", c, '\n')
	}
	return n, nil
}

func round2(n int) int {
	if n <= 0 {
		return 0
	}
	n--
	x := uint(0)
	for n > 0 {
		n >>= 1
		x++
	}
	return 1 << x
}

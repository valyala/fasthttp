package fasthttp

import (
	"bufio"
	"bytes"
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

	// Request body.
	Body []byte

	// Request URI.
	// URI becomes available only after Request.ParseURI() call.
	URI       URI
	parsedURI bool

	// Arguments sent in POST.
	// PostArgs becomes available only after Request.ParsePostArgs() call.
	PostArgs       Args
	parsedPostArgs bool
}

// Response represents HTTP response.
//
// It is forbidden copying Response instances. Create new instances
// and use CopyTo() instead.
type Response struct {
	// Response header
	Header ResponseHeader

	// Response body.
	//
	// Either Body or BodyStream may be set, but not both.
	Body []byte

	// Response body stream.
	//
	// BodyStream may be set instead of Body for performance reasons only.
	//
	// BodyStream is read by Response.Write() until one of the following
	// events occur:
	// - Response.ContentLength bytes read if it is greater than 0.
	// - error or io.EOF is returned from BodyStream if Response.ContentLength
	// is 0 or negative.
	//
	// Response.Write() calls BodyStream.Close() (io.Closer) if such method
	// is present after finishing reading BodyStream.
	//
	// Either BodyStream or Body may be set, but not both.
	//
	// Client and Response.Read() never sets BodyStream - it sets only Body.
	BodyStream io.Reader

	// If set to true, Response.Read() skips reading body.
	// Use it for HEAD requests.
	SkipBody bool
}

// SetRequestURI sets RequestURI.
func (req *Request) SetRequestURI(requestURI string) {
	req.Header.SetRequestURI(requestURI)
}

// SetRequestURIBytes sets RequestURI.
//
// It is safe using requestURI buffer after the function return.
func (req *Request) SetRequestURIBytes(requestURI []byte) {
	req.Header.SetRequestURIBytes(requestURI)
}

// CopyTo copies req contents to dst.
func (req *Request) CopyTo(dst *Request) {
	dst.Clear()
	req.Header.CopyTo(&dst.Header)
	dst.Body = append(dst.Body[:0], req.Body...)
	if req.parsedURI {
		dst.ParseURI()
	}
	if req.parsedPostArgs {
		dst.ParsePostArgs()
	}
}

// CopyTo copies resp contents to dst except of BodyStream.
func (resp *Response) CopyTo(dst *Response) {
	dst.Clear()
	resp.Header.CopyTo(&dst.Header)
	dst.Body = append(dst.Body[:0], resp.Body...)
	dst.SkipBody = resp.SkipBody
}

// ParseURI parses request uri and fills Request.URI.
func (req *Request) ParseURI() {
	if req.parsedURI {
		return
	}
	req.URI.Parse(req.Header.host, req.Header.requestURI)
	req.parsedURI = true
}

// ParsePostArgs parses args sent in POST body and fills Request.PostArgs.
func (req *Request) ParsePostArgs() error {
	if req.parsedPostArgs {
		return nil
	}

	if !req.Header.IsPost() {
		return fmt.Errorf("Cannot parse POST args for %q request", req.Header.method)
	}
	if !bytes.Equal(req.Header.contentType, strPostArgsContentType) {
		return fmt.Errorf("Cannot parse POST args for %q Content-Type. Required %q Content-Type",
			req.Header.contentType, strPostArgsContentType)
	}
	req.PostArgs.ParseBytes(req.Body)
	req.parsedPostArgs = true
	return nil
}

// Clear clears request contents.
func (req *Request) Clear() {
	req.Header.Clear()
	req.clearSkipHeader()
}

func (req *Request) clearSkipHeader() {
	req.Body = req.Body[:0]
	req.URI.Clear()
	req.parsedURI = false
	req.PostArgs.Clear()
	req.parsedPostArgs = false
}

// Clear clears response contents.
func (resp *Response) Clear() {
	resp.Header.Clear()
	resp.clearSkipHeader()
}

func (resp *Response) clearSkipHeader() {
	resp.Body = resp.Body[:0]
	resp.BodyStream = nil
}

// Read reads request (including body) from the given r.
func (req *Request) Read(r *bufio.Reader) error {
	req.clearSkipHeader()
	err := req.Header.Read(r)
	if err != nil {
		return err
	}

	if req.Header.IsPost() {
		req.Body, err = readBody(r, req.Header.ContentLength, req.Body)
		if err != nil {
			req.Clear()
			return err
		}
		req.Header.ContentLength = len(req.Body)
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

	if !isSkipResponseBody(resp.Header.StatusCode) && !resp.SkipBody {
		resp.Body, err = readBody(r, resp.Header.ContentLength, resp.Body)
		if err != nil {
			resp.Clear()
			return err
		}
		resp.Header.ContentLength = len(resp.Body)
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

// Write write request to w.
//
// Write doesn't flush request to w for performance reasons.
func (req *Request) Write(w *bufio.Writer) error {
	if len(req.Header.host) == 0 {
		req.ParseURI()
		req.Header.SetHostBytes(req.URI.Host)
		req.Header.requestURI = req.URI.AppendRequestURI(req.Header.requestURI[:0])
	}
	req.Header.ContentLength = len(req.Body)
	err := req.Header.Write(w)
	if err != nil {
		return err
	}
	if req.Header.IsPost() {
		_, err = w.Write(req.Body)
	} else if len(req.Body) > 0 {
		return fmt.Errorf("Non-zero body for non-POST request. body=%q", req.Body)
	}
	return err
}

// Write writes response to w.
//
// Write doesn't flush response to w for performance reasons.
func (resp *Response) Write(w *bufio.Writer) error {
	var err error
	if resp.BodyStream != nil {
		if resp.Header.ContentLength > 0 {
			if err = resp.Header.Write(w); err != nil {
				return err
			}
			if err = writeBodyFixedSize(w, resp.BodyStream, resp.Header.ContentLength); err != nil {
				return err
			}
		} else {
			resp.Header.ContentLength = -1
			if err = resp.Header.Write(w); err != nil {
				return err
			}
			if err = writeBodyChunked(w, resp.BodyStream); err != nil {
				return err
			}
		}
		if bsc, ok := resp.BodyStream.(io.Closer); ok {
			err = bsc.Close()
		}
		return err
	}

	resp.Header.ContentLength = len(resp.Body)
	if err = resp.Header.Write(w); err != nil {
		return err
	}
	_, err = w.Write(resp.Body)
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

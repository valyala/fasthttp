package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"sync"
)

// Response represents HTTP response.
//
// It is forbidden copying Response instances. Create new instances
// and use CopyTo instead.
//
// Response instance MUST NOT be used from concurrently running goroutines.
type Response struct {
	noCopy noCopy

	// Response header
	//
	// Copying Header by value is forbidden. Use pointer to Header instead.
	Header ResponseHeader

	body []byte
	w    responseBodyWriter

	bodyStream io.ReadCloser

	// Response.Read() skips reading body if set to true.
	// Use it for reading HEAD responses.
	//
	// Response.Write() skips writing body if set to true.
	// Use it for writing HEAD responses.
	SkipBody bool

	// The following fields are used only for ProxyClient
	cc              *clientConn
	br              *bufio.Reader
	resetConnection bool
	sawEOF          bool
}

// StatusCode returns response status code.
func (resp *Response) StatusCode() int {
	return resp.Header.StatusCode()
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (resp *Response) ConnectionClose() bool {
	return resp.Header.ConnectionClose()
}

// SetBodyStream sets response body stream and, optionally body size.
//
// If bodySize is >= 0, then the bodyStream must provide exactly bodySize bytes
// before returning io.EOF.
//
// If bodySize < 0, then bodyStream is read until io.EOF.
//
// bodyStream.Close() is called after finishing reading all body data
// if it implements io.Closer.
//
// See also SetBodyStreamWriter.
func (resp *Response) SetBodyStream(bodyStream io.ReadCloser, bodySize int) {
	resp.ResetBody()
	resp.bodyStream = bodyStream
	resp.Header.SetContentLength(bodySize)
}

// BodyStream returns the body stream of the response.
func (resp *Response) BodyStream() io.ReadCloser {
	return resp.bodyStream
}

type responseBodyWriter struct {
	r *Response
}

// ResetBody resets response body.
func (resp *Response) ResetBody() {
	resp.closeBodyStream()
	resp.body = resp.body[:0]
}

func marshalMultipartForm(f *multipart.Form, boundary string) ([]byte, error) {
	var buf ByteBuffer
	if err := WriteMultipartForm(&buf, f, boundary); err != nil {
		return nil, err
	}
	return buf.B, nil
}

// WriteMultipartForm writes the given multipart form f with the given
// boundary to w.
func WriteMultipartForm(w io.Writer, f *multipart.Form, boundary string) error {
	// Do not care about memory allocations here, since multipart
	// form processing is slooow.
	if len(boundary) == 0 {
		panic("BUG: form boundary cannot be empty")
	}

	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(boundary); err != nil {
		return fmt.Errorf("cannot use form boundary %q: %s", boundary, err)
	}

	// marshal values
	for k, vv := range f.Value {
		for _, v := range vv {
			if err := mw.WriteField(k, v); err != nil {
				return fmt.Errorf("cannot write form field %q value %q: %s", k, v, err)
			}
		}
	}

	// marshal files
	for k, fvv := range f.File {
		for _, fv := range fvv {
			vw, err := mw.CreateFormFile(k, fv.Filename)
			if err != nil {
				return fmt.Errorf("cannot create form file %q (%q): %s", k, fv.Filename, err)
			}
			fh, err := fv.Open()
			if err != nil {
				return fmt.Errorf("cannot open form file %q (%q): %s", k, fv.Filename, err)
			}
			if _, err = copyZeroAlloc(vw, fh); err != nil {
				return fmt.Errorf("error when copying form file %q (%q): %s", k, fv.Filename, err)
			}
			if err = fh.Close(); err != nil {
				return fmt.Errorf("cannot close form file %q (%q): %s", k, fv.Filename, err)
			}
		}
	}

	if err := mw.Close(); err != nil {
		return fmt.Errorf("error when closing multipart form writer: %s", err)
	}

	return nil
}

// Reset clears response contents.
func (resp *Response) Reset() {
	resp.Header.Reset()
	resp.resetSkipHeader()
	resp.SkipBody = false
}

func (resp *Response) resetSkipHeader() {
	resp.closeBodyStream()
	resp.body = reuseBody(resp.body)
}

func reuseBody(body []byte) []byte {
	// Production monitoring shows that it is very difficult to create
	// an optimal strategy for body buffer re-use for real-world loads.
	//
	// For instance, the strategy 'throw away big buffers, while re-use
	// small buffers' fails when small and large responses are equally
	// distributed.
	//
	// So just re-use all body buffers irregardless of their sizes
	// in the hope sync.Pool+GC finds out an optimal strategy :)
	return body[:0]
}

// ReadHeader reads response headers from the given r
//
// io.EOF is returned if r is closed before reading the first header byte.
func (resp *Response) ReadHeader(r *bufio.Reader) error {
	resp.resetSkipHeader()
	err := resp.Header.Read(r)
	if err != nil {
		return err
	}
	if resp.Header.StatusCode() == StatusContinue {
		// Read the next response according to http://www.w3.org/Protocols/rfc2616/rfc2616-sec8.html .
		if err = resp.Header.Read(r); err != nil {
			return err
		}
	}
	return nil
}

func (resp *Response) mustSkipBody() bool {
	return resp.SkipBody || resp.Header.mustSkipContentLength()
}

var errRequestHostRequired = errors.New("missing required Host header in request")

func (resp *Response) closeBodyStream() error {
	if resp.bodyStream == nil {
		return nil
	}
	var err error
	if bsc, ok := resp.bodyStream.(io.Closer); ok {
		err = bsc.Close()
	}
	resp.bodyStream = nil
	return err
}

func writeBodyChunked(w *bufio.Writer, r io.Reader) error {
	vbuf := copyBufPool.Get()
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

func limitedReaderSize(r io.Reader) int64 {
	lr, ok := r.(*io.LimitedReader)
	if !ok {
		return -1
	}
	return lr.N
}

func writeBodyFixedSize(w *bufio.Writer, r io.Reader, size int64) error {
	if size > maxSmallFileSize {
		// w buffer must be empty for triggering
		// sendfile path in bufio.Writer.ReadFrom.
		if err := w.Flush(); err != nil {
			return err
		}
	}

	// Unwrap a single limited reader for triggering sendfile path
	// in net.TCPConn.ReadFrom.
	lr, ok := r.(*io.LimitedReader)
	if ok {
		r = lr.R
	}

	n, err := copyZeroAlloc(w, r)

	if ok {
		lr.N -= n
	}

	if n != size && err == nil {
		err = fmt.Errorf("copied %d bytes from body stream instead of %d bytes", n, size)
	}
	return err
}

func copyZeroAlloc(w io.Writer, r io.Reader) (int64, error) {
	vbuf := copyBufPool.Get()
	buf := vbuf.([]byte)
	n, err := io.CopyBuffer(w, r, buf)
	copyBufPool.Put(vbuf)
	return n, err
}

var copyBufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 4096)
	},
}

func writeChunk(w *bufio.Writer, b []byte) error {
	n := len(b)
	writeHexInt(w, n)
	w.Write(strCRLF)
	w.Write(b)
	_, err := w.Write(strCRLF)
	err1 := w.Flush()
	if err == nil {
		err = err1
	}
	return err
}

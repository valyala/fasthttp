package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"os"
	"sync"
)

// Request represents HTTP request.
//
// It is forbidden copying Request instances. Create new instances
// and use CopyTo() instead.
//
// It is unsafe modifying/reading Request instance from concurrently
// running goroutines.
type Request struct {
	// Request header
	//
	// Copying Header by value is forbidden. Use pointer to Header instead.
	Header RequestHeader

	body []byte
	w    requestBodyWriter

	uri       URI
	parsedURI bool

	postArgs       Args
	parsedPostArgs bool

	multipartForm         *multipart.Form
	multipartFormBoundary string
}

// Response represents HTTP response.
//
// It is forbidden copying Response instances. Create new instances
// and use CopyTo() instead.
//
// It is unsafe modifying/reading Response instance from concurrently
// running goroutines.
type Response struct {
	// Response header
	//
	// Copying Header by value is forbidden. Use pointer to Header instead.
	Header ResponseHeader

	// Response.Read() skips reading body if set to true.
	// Use it for reading HEAD responses.
	//
	// Response.Write() skips writing body if set to true.
	// Use it for writing HEAD responses.
	SkipBody bool

	body []byte
	w    responseBodyWriter

	bodyStream io.Reader
}

// SetRequestURI sets RequestURI.
func (req *Request) SetRequestURI(requestURI string) {
	req.Header.SetRequestURI(requestURI)
	req.parsedURI = false
}

// SetRequestURIBytes sets RequestURI.
func (req *Request) SetRequestURIBytes(requestURI []byte) {
	req.Header.SetRequestURIBytes(requestURI)
	req.parsedURI = false
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

// SendFile registers file on the given path to be used as response body
// when Write is called.
//
// Note that SendFile doesn't set Content-Type, so set it yourself
// with Header.SetContentType.
func (resp *Response) SendFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	fileInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	size64 := fileInfo.Size()
	size := int(size64)
	if int64(size) != size64 {
		size = -1
	}

	resp.Header.SetLastModified(fileInfo.ModTime())
	resp.SetBodyStream(f, size)
	return nil
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
func (resp *Response) SetBodyStream(bodyStream io.Reader, bodySize int) {
	resp.body = resp.body[:0]
	resp.closeBodyStream()
	resp.bodyStream = bodyStream
	resp.Header.SetContentLength(bodySize)
}

// SetBodyStreamWriter registers the given sw for populating response body.
//
// This function may be used in the following cases:
//
//     * if response body is too big (more than 10MB).
//     * if response body is streamed from slow external sources.
//     * if response body must be streamed to the client in chunks
//     (aka `http server push`).
func (resp *Response) SetBodyStreamWriter(sw StreamWriter) {
	sr := NewStreamReader(sw)
	resp.SetBodyStream(sr, -1)
}

// BodyWriter returns writer for populating response body.
//
// If used inside RequestHandler, the returned writer must not be used
// after returning from RequestHandler. Use RequestCtx.Write
// or SetBodyStreamWriter in this case.
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
	w.r.AppendBody(p)
	return len(p), nil
}

type requestBodyWriter struct {
	r *Request
}

func (w *requestBodyWriter) Write(p []byte) (int, error) {
	w.r.AppendBody(p)
	return len(p), nil
}

// Body returns response body.
func (resp *Response) Body() []byte {
	return resp.body
}

// BodyGunzip returns un-gzipped body data.
//
// This method may be used if the response header contains
// 'Content-Encoding: gzip' for reading un-gzipped response body.
// Use Body for reading gzipped response body.
func (resp *Response) BodyGunzip() ([]byte, error) {
	// Do not care about memory allocations here,
	// since gzip is slow and generates a lot of memory allocations
	// by itself.
	r := bytes.NewBuffer(resp.body)
	zr, err := acquireGzipReader(r)
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(zr)
	releaseGzipReader(zr)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// BodyInflate returns un-deflated body data.
//
// This method may be used if the response header contains
// 'Content-Encoding: deflate' for reading un-deflated response body.
// Use Body for reading deflated response body.
func (resp *Response) BodyInflate() ([]byte, error) {
	// Do not care about memory allocations here,
	// since flate is slow and generates a lot of memory allocations
	// by itself.
	r := bytes.NewBuffer(resp.body)
	zr, err := acquireFlateReader(r)
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(zr)
	releaseFlateReader(zr)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// AppendBody appends p to response body.
func (resp *Response) AppendBody(p []byte) {
	resp.closeBodyStream()
	resp.body = append(resp.body, p...)
}

// AppendBodyString appends s to response body.
func (resp *Response) AppendBodyString(s string) {
	resp.closeBodyStream()
	resp.body = append(resp.body, s...)
}

// SetBody sets response body.
func (resp *Response) SetBody(body []byte) {
	resp.closeBodyStream()
	resp.body = append(resp.body[:0], body...)
}

// SetBodyString sets response body.
func (resp *Response) SetBodyString(body string) {
	resp.closeBodyStream()
	resp.body = append(resp.body[:0], body...)
}

// ResetBody resets response body.
func (resp *Response) ResetBody() {
	resp.closeBodyStream()
	resp.body = resp.body[:0]
}

// Body returns request body.
func (req *Request) Body() []byte {
	if req.multipartForm != nil && len(req.body) == 0 {
		body, err := marshalMultipartForm(req.multipartForm, req.multipartFormBoundary)
		if err != nil {
			return []byte(err.Error())
		}
		return body
	}
	return req.body
}

// AppendBody appends p to request body.
func (req *Request) AppendBody(p []byte) {
	req.RemoveMultipartFormFiles()
	req.body = append(req.body, p...)
}

// AppendBodyString appends s to request body.
func (req *Request) AppendBodyString(s string) {
	req.RemoveMultipartFormFiles()
	req.body = append(req.body, s...)
}

// SetBody sets request body.
func (req *Request) SetBody(body []byte) {
	req.RemoveMultipartFormFiles()
	req.body = append(req.body[:0], body...)
}

// SetBodyString sets request body.
func (req *Request) SetBodyString(body string) {
	req.RemoveMultipartFormFiles()
	req.body = append(req.body[:0], body...)
}

// ResetBody resets request body.
func (req *Request) ResetBody() {
	req.RemoveMultipartFormFiles()
	req.body = req.body[:0]
}

// CopyTo copies req contents to dst.
func (req *Request) CopyTo(dst *Request) {
	dst.Reset()
	req.Header.CopyTo(&dst.Header)
	dst.body = append(dst.body[:0], req.Body()...)

	req.uri.CopyTo(&dst.uri)
	dst.parsedURI = req.parsedURI

	req.postArgs.CopyTo(&dst.postArgs)
	dst.parsedPostArgs = req.parsedPostArgs

	// do not copy multipartForm - it will be automatically
	// re-created on the first call to MultipartForm.
}

// CopyTo copies resp contents to dst except of body stream.
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

	if !bytes.Equal(req.Header.ContentType(), strPostArgsContentType) {
		return
	}
	req.postArgs.ParseBytes(req.body)
	return
}

// ErrNoMultipartForm means that the request's Content-Type
// isn't 'multipart/form-data'.
var ErrNoMultipartForm = errors.New("request has no multipart/form-data Content-Type")

// MultipartForm returns requests's multipart form.
//
// Returns ErrNoMultipartForm if request's Content-Type
// isn't 'multipart/form-data'.
//
// RemoveMultipartFormFiles must be called after returned multipart form
// is processed.
func (req *Request) MultipartForm() (*multipart.Form, error) {
	if req.multipartForm != nil {
		return req.multipartForm, nil
	}

	req.multipartFormBoundary = string(req.Header.MultipartFormBoundary())
	if len(req.multipartFormBoundary) == 0 {
		return nil, ErrNoMultipartForm
	}

	ce := req.Header.peek(strContentEncoding)
	body := req.body
	if bytes.Equal(ce, strGzip) {
		// Do not care about memory usage here.
		var err error
		if body, err = AppendGunzipBytes(nil, body); err != nil {
			return nil, fmt.Errorf("cannot gunzip request body: %s", err)
		}
	} else if len(ce) > 0 {
		return nil, fmt.Errorf("unsupported Content-Encoding: %q", ce)
	}

	f, err := readMultipartForm(bytes.NewReader(body), req.multipartFormBoundary, 0, len(body))
	if err != nil {
		return nil, err
	}
	req.multipartForm = f
	return f, nil
}

func marshalMultipartForm(f *multipart.Form, boundary string) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteMultipartForm(&buf, f, boundary); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
			if _, err = io.Copy(vw, fh); err != nil {
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

func readMultipartForm(r io.Reader, boundary string, maxBodySize, maxInMemoryFileSize int) (*multipart.Form, error) {
	// Do not care about memory allocations here, since they are tiny
	// compared to multipart data (aka multi-MB files) usually sent
	// in multipart/form-data requests.

	if maxBodySize > 0 {
		r = io.LimitReader(r, int64(maxBodySize))
	}
	mr := multipart.NewReader(r, boundary)
	f, err := mr.ReadForm(int64(maxInMemoryFileSize))
	if err != nil {
		return nil, fmt.Errorf("cannot read multipart/form-data body: %s", err)
	}
	return f, nil
}

// Reset clears request contents.
func (req *Request) Reset() {
	req.Header.Reset()
	req.resetSkipHeader()
}

func (req *Request) resetSkipHeader() {
	req.body = req.body[:0]
	req.uri.Reset()
	req.parsedURI = false
	req.postArgs.Reset()
	req.parsedPostArgs = false
	req.RemoveMultipartFormFiles()
}

// RemoveMultipartFormFiles removes multipart/form-data temporary files
// associated with the request.
func (req *Request) RemoveMultipartFormFiles() {
	if req.multipartForm != nil {
		// Do not check for error, since these files may be deleted or moved
		// to new places by user code.
		req.multipartForm.RemoveAll()
		req.multipartForm = nil
	}
	req.multipartFormBoundary = ""
}

// Reset clears response contents.
func (resp *Response) Reset() {
	resp.Header.Reset()
	resp.resetSkipHeader()
	resp.SkipBody = false
}

func (resp *Response) resetSkipHeader() {
	resp.closeBodyStream()
	resp.body = resp.body[:0]
}

// Read reads request (including body) from the given r.
//
// RemoveMultipartFormFiles or Reset must be called after
// reading multipart/form-data request in order to delete temporarily
// uploaded files.
//
// If MayContinue returns true, the caller must:
//
//     - Either send StatusExpectationFailed response if request headers don't
//       satisfy the caller.
//     - Or send StatusContinue response before reading request body
//       with ContinueReadBody.
//     - Or close the connection.
func (req *Request) Read(r *bufio.Reader) error {
	return req.ReadLimitBody(r, 0)
}

const defaultMaxInMemoryFileSize = 16 * 1024 * 1024

var errGetOnly = errors.New("non-GET request received")

// ReadLimitBody reads request from the given r, limiting the body size.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
//
// RemoveMultipartFormFiles or Reset must be called after
// reading multipart/form-data request in order to delete temporarily
// uploaded files.
//
// If MayContinue returns true, the caller must:
//
//     - Either send StatusExpectationFailed response if request headers don't
//       satisfy the caller.
//     - Or send StatusContinue response before reading request body
//       with ContinueReadBody.
//     - Or close the connection.
func (req *Request) ReadLimitBody(r *bufio.Reader, maxBodySize int) error {
	return req.readLimitBody(r, maxBodySize, false)
}

func (req *Request) readLimitBody(r *bufio.Reader, maxBodySize int, getOnly bool) error {
	req.resetSkipHeader()
	err := req.Header.Read(r)
	if err != nil {
		return err
	}
	if getOnly && !req.Header.IsGet() {
		return errGetOnly
	}

	if req.Header.noBody() {
		return nil
	}

	if req.MayContinue() {
		// 'Expect: 100-continue' header found. Let the caller deciding
		// whether to read request body or
		// to return StatusExpectationFailed.
		return nil
	}

	return req.ContinueReadBody(r, maxBodySize)
}

// MayContinue returns true if the request contains
// 'Expect: 100-continue' header.
//
// The caller must do one of the following actions if MayContinue returns true:
//
//     - Either send StatusExpectationFailed response if request headers don't
//       satisfy the caller.
//     - Or send StatusContinue response before reading request body
//       with ContinueReadBody.
//     - Or close the connection.
func (req *Request) MayContinue() bool {
	return bytes.Equal(req.Header.peek(strExpect), str100Continue)
}

// ContinueReadBody reads request body if request header contains
// 'Expect: 100-continue'.
//
// The caller must send StatusContinue response before calling this method.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
func (req *Request) ContinueReadBody(r *bufio.Reader, maxBodySize int) error {
	var err error
	contentLength := req.Header.ContentLength()
	if contentLength > 0 {
		// Pre-read multipart form data of known length.
		// This way we limit memory usage for large file uploads, since their contents
		// is streamed into temporary files if file size exceeds defaultMaxInMemoryFileSize.
		req.multipartFormBoundary = string(req.Header.MultipartFormBoundary())
		if len(req.multipartFormBoundary) > 0 && len(req.Header.peek(strContentEncoding)) == 0 {
			req.multipartForm, err = readMultipartForm(r, req.multipartFormBoundary, maxBodySize, defaultMaxInMemoryFileSize)
			if err != nil {
				req.Reset()
			}
			return err
		}
	}

	if contentLength == -2 {
		// identity body has no sense for http requests, since
		// the end of body is determined by connection close.
		// So just ignore request body for requests without
		// 'Content-Length' and 'Transfer-Encoding' headers.
		req.Header.SetContentLength(0)
		return nil
	}

	req.body, err = readBody(r, contentLength, maxBodySize, req.body)
	if err != nil {
		req.Reset()
		return err
	}
	req.Header.SetContentLength(len(req.body))
	return nil
}

// Read reads response (including body) from the given r.
func (resp *Response) Read(r *bufio.Reader) error {
	return resp.ReadLimitBody(r, 0)
}

// ReadLimitBody reads response from the given r, limiting the body size.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
func (resp *Response) ReadLimitBody(r *bufio.Reader, maxBodySize int) error {
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

	if !resp.mustSkipBody() {
		resp.body, err = readBody(r, resp.Header.ContentLength(), maxBodySize, resp.body)
		if err != nil {
			resp.Reset()
			return err
		}
		resp.Header.SetContentLength(len(resp.body))
	}
	return nil
}

func (resp *Response) mustSkipBody() bool {
	return resp.SkipBody || resp.Header.mustSkipContentLength()
}

var errRequestHostRequired = errors.New("missing required Host header in request")

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

	body := req.body
	var err error
	if req.multipartForm != nil && len(req.body) == 0 {
		body, err = marshalMultipartForm(req.multipartForm, req.multipartFormBoundary)
		if err != nil {
			return fmt.Errorf("error when marshaling multipart form: %s", err)
		}
		req.Header.SetMultipartFormBoundary(req.multipartFormBoundary)
	}

	hasBody := !req.Header.noBody()
	if hasBody {
		req.Header.SetContentLength(len(body))
	}
	if err = req.Header.Write(w); err != nil {
		return err
	}
	if hasBody {
		_, err = w.Write(body)
	} else if len(body) > 0 {
		return fmt.Errorf("non-zero body for non-POST request. body=%q", body)
	}
	return err
}

// WriteGzip writes response with gzipped body to w.
//
// The method sets 'Content-Encoding: gzip' header.
//
// WriteGzip doesn't flush response to w for performance reasons.
func (resp *Response) WriteGzip(w *bufio.Writer) error {
	return resp.WriteGzipLevel(w, CompressDefaultCompression)
}

// WriteGzipLevel writes response with gzipped body to w.
//
// Level is the desired compression level:
//
//     * CompressNoCompression
//     * CompressBestSpeed
//     * CompressBestCompression
//     * CompressDefaultCompression
//
// The method sets 'Content-Encoding: gzip' header.
//
// WriteGzipLevel doesn't flush response to w for performance reasons.
func (resp *Response) WriteGzipLevel(w *bufio.Writer, level int) error {
	if err := resp.gzipBody(level); err != nil {
		return err
	}
	return resp.Write(w)
}

// WriteDeflate writes response with deflated body to w.
//
// The method sets 'Content-Encoding: deflate' header.
//
// WriteDeflate doesn't flush response to w for performance reasons.
func (resp *Response) WriteDeflate(w *bufio.Writer) error {
	return resp.WriteDeflateLevel(w, CompressDefaultCompression)
}

// WriteDeflateLevel writes response with deflated body to w.
//
// Level is the desired compression level:
//
//     * CompressNoCompression
//     * CompressBestSpeed
//     * CompressBestCompression
//     * CompressDefaultCompression
//
// The method sets 'Content-Encoding: deflate' header.
//
// WriteDeflateLevel doesn't flush response to w for performance reasons.
func (resp *Response) WriteDeflateLevel(w *bufio.Writer, level int) error {
	if err := resp.deflateBody(level); err != nil {
		return err
	}
	return resp.Write(w)
}

func (resp *Response) gzipBody(level int) error {
	// Do not care about memory allocations here, since gzip is slow
	// and allocates a lot of memory by itself.
	if resp.bodyStream != nil {
		bs := resp.bodyStream
		resp.bodyStream = NewStreamReader(func(sw *bufio.Writer) {
			zw := acquireGzipWriter(sw, level)
			io.Copy(zw, bs)
			releaseGzipWriter(zw)
		})
	} else {
		var buf bytes.Buffer
		zw := acquireGzipWriter(&buf, level)
		_, err := zw.Write(resp.body)
		releaseGzipWriter(zw)
		if err != nil {
			return err
		}
		resp.body = buf.Bytes()
	}
	resp.Header.SetCanonical(strContentEncoding, strGzip)
	return nil
}

func (resp *Response) deflateBody(level int) error {
	// Do not care about memory allocations here, since flate is slow
	// and allocates a lot of memory by itself.
	if resp.bodyStream != nil {
		bs := resp.bodyStream
		resp.bodyStream = NewStreamReader(func(sw *bufio.Writer) {
			zw := acquireFlateWriter(sw, level)
			io.Copy(zw, bs)
			releaseFlateWriter(zw)
		})
	} else {
		var buf bytes.Buffer
		zw := acquireFlateWriter(&buf, level)
		_, err := zw.Write(resp.body)
		releaseFlateWriter(zw)
		if err != nil {
			return err
		}
		resp.body = buf.Bytes()
	}
	resp.Header.SetCanonical(strContentEncoding, strDeflate)
	return nil
}

// Write writes response to w.
//
// Write doesn't flush response to w for performance reasons.
func (resp *Response) Write(w *bufio.Writer) error {
	var err error
	sendBody := !resp.mustSkipBody()

	if resp.bodyStream != nil {
		contentLength := resp.Header.ContentLength()
		if contentLength < 0 {
			lrSize := limitedReaderSize(resp.bodyStream)
			if lrSize >= 0 {
				contentLength = int(lrSize)
				if int64(contentLength) != lrSize {
					contentLength = -1
				}
			}
		}
		if contentLength >= 0 {
			if err = resp.Header.Write(w); err != nil {
				return err
			}
			if sendBody {
				if err = writeBodyFixedSize(w, resp.bodyStream, int64(contentLength)); err != nil {
					return err
				}
			}
		} else {
			resp.Header.SetContentLength(-1)
			if err = resp.Header.Write(w); err != nil {
				return err
			}
			if sendBody {
				if err = writeBodyChunked(w, resp.bodyStream); err != nil {
					return err
				}
			}
		}
		return resp.closeBodyStream()
	}

	bodyLen := len(resp.body)
	if sendBody || bodyLen > 0 {
		resp.Header.SetContentLength(bodyLen)
	}
	if err = resp.Header.Write(w); err != nil {
		return err
	}
	if sendBody {
		_, err = w.Write(resp.body)
	}
	return err
}

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

// String returns request representation.
//
// Returns error message instead of request representation on error.
//
// Use Write instead of String for performance-critical code.
func (req *Request) String() string {
	return getHTTPString(req)
}

// String returns response representation.
//
// Returns error message instead of response representation on error.
//
// Use Write instead of String for performance-critical code.
func (resp *Response) String() string {
	return getHTTPString(resp)
}

func getHTTPString(hw httpWriter) string {
	var w bytes.Buffer
	bw := bufio.NewWriter(&w)
	if err := hw.Write(bw); err != nil {
		return err.Error()
	}
	if err := bw.Flush(); err != nil {
		return err.Error()
	}
	return string(w.Bytes())
}

type httpWriter interface {
	Write(w *bufio.Writer) error
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
		err = fmt.Errorf("copied %d bytes from response body stream instead of %d bytes", n, size)
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
	return err
}

// ErrBodyTooLarge is returned if either request or response body exceeds
// the given limit.
var ErrBodyTooLarge = errors.New("body size exceeds the given limit")

func readBody(r *bufio.Reader, contentLength int, maxBodySize int, dst []byte) ([]byte, error) {
	dst = dst[:0]
	if contentLength >= 0 {
		if maxBodySize > 0 && contentLength > maxBodySize {
			return dst, ErrBodyTooLarge
		}
		return appendBodyFixedSize(r, dst, contentLength)
	}
	if contentLength == -1 {
		return readBodyChunked(r, maxBodySize, dst)
	}
	return readBodyIdentity(r, maxBodySize, dst)
}

func readBodyIdentity(r *bufio.Reader, maxBodySize int, dst []byte) ([]byte, error) {
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
		if maxBodySize > 0 && offset > maxBodySize {
			return dst[:offset], ErrBodyTooLarge
		}
		if len(dst) == offset {
			n := round2(2 * offset)
			if maxBodySize > 0 && n > maxBodySize {
				n = maxBodySize + 1
			}
			b := make([]byte, n)
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

func readBodyChunked(r *bufio.Reader, maxBodySize int, dst []byte) ([]byte, error) {
	if len(dst) > 0 {
		panic("BUG: expected zero-length buffer")
	}

	strCRLFLen := len(strCRLF)
	for {
		chunkSize, err := parseChunkSize(r)
		if err != nil {
			return dst, err
		}
		if maxBodySize > 0 && len(dst)+chunkSize > maxBodySize {
			return dst, ErrBodyTooLarge
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

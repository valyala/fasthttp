package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

type Request struct {
	Header RequestHeader
	Body   []byte

	// URI becomes available only after Request.ParseURI() call.
	URI       URI
	parsedURI bool

	// PostArgs becomes available only after Request.ParsePostArgs() call.
	PostArgs       Args
	parsedPostArgs bool
}

type Response struct {
	Header ResponseHeader
	Body   []byte

	// if set to true, Response.Read() skips reading body.
	// Use it for HEAD requests.
	SkipBody bool
}

func (req *Request) ParseURI() {
	if req.parsedURI {
		return
	}
	req.URI.Parse(req.Header.Host, req.Header.RequestURI)
	req.parsedURI = true
}

func (req *Request) ParsePostArgs() error {
	if req.parsedPostArgs {
		return nil
	}

	if !req.Header.IsMethodPost() {
		return fmt.Errorf("Cannot parse POST args for %q request", req.Header.Method)
	}
	if !bytes.Equal(req.Header.ContentType, strPostArgsContentType) {
		return fmt.Errorf("Cannot parse POST args for %q Content-Type. Required %q Content-Type",
			req.Header.ContentType, strPostArgsContentType)
	}
	req.PostArgs.ParseBytes(req.Body)
	req.parsedPostArgs = true
	return nil
}

func (req *Request) Clear() {
	req.Header.Clear()
	req.Body = req.Body[:0]
	req.URI.Clear()
	req.parsedURI = false
	req.PostArgs.Clear()
	req.parsedPostArgs = false
}

func (resp *Response) Clear() {
	resp.Header.Clear()
	resp.Body = resp.Body[:0]
}

func (req *Request) Read(r *bufio.Reader) error {
	req.Body = req.Body[:0]
	req.URI.Clear()
	req.parsedURI = false
	req.PostArgs.Clear()
	req.parsedPostArgs = false

	err := req.Header.Read(r)
	if err != nil {
		return err
	}

	if req.Header.IsMethodPost() {
		body, err := readBody(r, req.Header.ContentLength, req.Body)
		if err != nil {
			req.Clear()
			return err
		}
		req.Body = body
	}
	return nil
}

func (resp *Response) Read(r *bufio.Reader) error {
	resp.Body = resp.Body[:0]

	err := resp.Header.Read(r)
	if err != nil {
		return err
	}

	if isSkipResponseBody(resp.Header.StatusCode) || resp.SkipBody {
		resp.SkipBody = false
		return nil
	}

	body, err := readBody(r, resp.Header.ContentLength, resp.Body)
	if err != nil {
		resp.Clear()
		return err
	}
	resp.Body = body
	return nil
}

func isSkipResponseBody(statusCode int) bool {
	// From http/1.1 specs:
	// All 1xx (informational), 204 (no content), and 304 (not modified) responses MUST NOT include a message-body
	if statusCode >= 100 && statusCode < 200 {
		return true
	}
	return statusCode == 204 || statusCode == 304
}

func (req *Request) Write(w *bufio.Writer) error {
	contentLengthOld := req.Header.ContentLength
	req.Header.ContentLength = len(req.Body)
	err := req.Header.Write(w)
	req.Header.ContentLength = contentLengthOld
	if err != nil {
		return err
	}
	if req.Header.IsMethodPost() {
		_, err = w.Write(req.Body)
	} else if len(req.Body) > 0 {
		return fmt.Errorf("Non-zero body for non-POST request. body=%q", req.Body)
	}
	return err
}

func (resp *Response) Write(w *bufio.Writer) error {
	contentLengthOld := resp.Header.ContentLength
	resp.Header.ContentLength = len(resp.Body)
	err := resp.Header.Write(w)
	resp.Header.ContentLength = contentLengthOld
	if err != nil {
		return err
	}
	_, err = w.Write(resp.Body)
	return err
}

func readBody(r *bufio.Reader, contentLength int, b []byte) ([]byte, error) {
	b = b[:0]
	if contentLength >= 0 {
		return readBodyFixedSize(r, contentLength, b)
	}
	return readBodyChunked(r, b)
}

func readBodyFixedSize(r *bufio.Reader, n int, buf []byte) ([]byte, error) {
	if n == 0 {
		return buf, nil
	}

	bufLen := len(buf)
	bufCap := bufLen + n
	if cap(buf) < bufCap {
		b := make([]byte, bufLen, bufCap)
		copy(b, buf)
		buf = b
	}
	buf = buf[:bufCap]
	b := buf[bufLen:]

	for {
		nn, err := r.Read(b)
		if nn <= 0 {
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return nil, err
			}
			panic(fmt.Sprintf("BUF: bufio.Read() returned (%d, nil)", nn))
		}
		if nn == n {
			return buf, nil
		}
		if nn > n {
			panic(fmt.Sprintf("BUF: read more than requested: %d vs %d", nn, n))
		}
		n -= nn
		b = b[nn:]
	}
}

func readBodyChunked(r *bufio.Reader, b []byte) ([]byte, error) {
	if len(b) > 0 {
		panic("Expected zero-length buffer")
	}

	strCRLFLen := len(strCRLF)
	for {
		chunkSize, err := parseChunkSize(r)
		if err != nil {
			return nil, err
		}
		b, err = readBodyFixedSize(r, chunkSize+strCRLFLen, b)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(b[len(b)-strCRLFLen:], strCRLF) {
			return nil, fmt.Errorf("cannot find crlf at the end of chunk")
		}
		b = b[:len(b)-strCRLFLen]
		if chunkSize == 0 {
			return b, nil
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

package fasthttp

import (
	"bufio"
	"bytes"
	"io"
	"sync"

	"github.com/valyala/bytebufferpool"
)

type bodyStreamHeader interface {
	ContentLength() int
	ReadTrailer(r *bufio.Reader) error
}

type requestStream struct {
	header          bodyStreamHeader
	prefetchedBytes bytes.Reader
	// body is the request body buffer holding prefetchedBytes, once the
	// request has let go of it while the server still has to drain the
	// stream. It goes back to the pool when the stream is released.
	body           *bytebufferpool.ByteBuffer
	reader         *bufio.Reader
	contentLength  int
	totalBytesRead int
	chunkLeft      int
	strictEOF      bool
	eof            bool
	// releaseOnClose is set on a response stream, which is released when the
	// response body is closed. A request stream is instead released by the
	// server loop once it has drained what the handler left unread, so a
	// response body streaming from it must not release it.
	releaseOnClose bool
	// failed is set once a Read returns an error other than io.EOF. The
	// stream is then at an unknown offset in the body, so the connection can't
	// be reused, and a later Read that happens to succeed doesn't change that.
	// The server loop owns the stream, so a handler resetting the request
	// can't clear it.
	failed bool
}

func (rs *requestStream) Read(p []byte) (int, error) {
	n, err := rs.read(p)
	if err != nil && err != io.EOF {
		rs.failed = true
	}
	return n, err
}

func (rs *requestStream) read(p []byte) (int, error) {
	if rs.reader == nil {
		panic("BUG: reading released body stream")
	}

	// The stream is terminal once the body has ended. Without this, a chunked
	// stream re-enters parseChunkSize on the next Read and blocks waiting for a
	// chunk header that is never coming: a keep-alive connection stays open
	// after the body ends, so nothing wakes the read. Any caller that reads a
	// streamed body to EOF and then reads again - draining before release is the
	// common case - would park a goroutine and never release the connection.
	if rs.eof {
		return 0, io.EOF
	}

	var (
		n   int
		err error
	)
	contentLength := rs.contentLength
	if contentLength == -1 {
		if rs.chunkLeft == 0 {
			chunkSize, err := parseChunkSize(rs.reader)
			if err != nil {
				return 0, err
			}
			if chunkSize == 0 {
				err = rs.header.ReadTrailer(rs.reader)
				if err != nil && err != io.EOF {
					return 0, err
				}
				rs.eof = true
				return 0, io.EOF
			}
			rs.chunkLeft = chunkSize
		}
		bytesToRead := min(rs.chunkLeft, len(p))
		n, err = rs.reader.Read(p[:bytesToRead])
		rs.totalBytesRead += n
		rs.chunkLeft -= n
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if err == nil && rs.chunkLeft == 0 {
			err = readCrLf(rs.reader)
		}
		return n, err
	}
	if rs.totalBytesRead == contentLength {
		rs.eof = true
		return 0, io.EOF
	}
	prefetchedSize := int(rs.prefetchedBytes.Size())
	if prefetchedSize > rs.totalBytesRead {
		left := prefetchedSize - rs.totalBytesRead
		if len(p) > left {
			p = p[:left]
		}
		n, err := rs.prefetchedBytes.Read(p)
		rs.totalBytesRead += n
		if rs.totalBytesRead == contentLength {
			rs.eof = true
			return n, io.EOF
		}
		return n, err
	}
	left := contentLength - rs.totalBytesRead
	if left > 0 && len(p) > left {
		p = p[:left]
	}
	n, err = rs.reader.Read(p)
	rs.totalBytesRead += n
	if err == io.EOF && rs.strictEOF && contentLength >= 0 && rs.totalBytesRead < contentLength {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		if err == io.EOF {
			rs.eof = true
		}
		return n, err
	}

	if rs.totalBytesRead == contentLength {
		rs.eof = true
		err = io.EOF
	}
	return n, err
}

// truncated reports whether a fixed-length body ended before Content-Length
// bytes of it were read.
func (rs *requestStream) truncated() bool {
	return rs.eof && rs.contentLength >= 0 && rs.totalBytesRead < rs.contentLength
}

// bodyOverflows reports whether more than limit bytes of a known-length body
// are still unread. A chunked body has no declared length, so it always
// returns false and is drained best-effort by discard instead. The framing
// comes from contentLength captured when the stream was created, so a handler
// mutating the request header cannot change the answer.
func (rs *requestStream) bodyOverflows(limit int) bool {
	return rs.contentLength >= 0 && rs.contentLength-rs.totalBytesRead > limit
}

// discard reads and drops what is left of the body, so that the reader ends
// where the next request starts. It gives up after limit bytes and reports
// whether the end of the body was reached.
func (rs *requestStream) discard(limit int) bool {
	_, err := io.CopyN(io.Discard, rs, int64(limit)+1)
	return err == io.EOF
}

func acquireRequestStream(b *bytebufferpool.ByteBuffer, r *bufio.Reader, h bodyStreamHeader) *requestStream {
	rs := requestStreamPool.Get().(*requestStream) //nolint:forcetypeassert
	rs.prefetchedBytes.Reset(b.B)
	rs.reader = r
	rs.header = h
	rs.contentLength = h.ContentLength()
	return rs
}

func acquireResponseStream(b *bytebufferpool.ByteBuffer, r *bufio.Reader, h bodyStreamHeader) *requestStream {
	rs := acquireRequestStream(b, r, h)
	rs.strictEOF = true
	rs.releaseOnClose = true
	return rs
}

func releaseRequestStream(rs *requestStream) {
	rs.prefetchedBytes.Reset(nil)
	if rs.body != nil {
		requestBodyPool.Put(rs.body)
		rs.body = nil
	}
	rs.totalBytesRead = 0
	rs.chunkLeft = 0
	rs.reader = nil
	rs.failed = false
	rs.header = nil
	rs.contentLength = 0
	rs.eof = false
	rs.strictEOF = false
	rs.releaseOnClose = false
	requestStreamPool.Put(rs)
}

var requestStreamPool = sync.Pool{
	New: func() any {
		return &requestStream{}
	},
}

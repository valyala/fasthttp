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
	reader          *bufio.Reader
	contentLength   int
	totalBytesRead  int
	chunkLeft       int
	strictEOF       bool
}

func (rs *requestStream) Read(p []byte) (int, error) {
	if rs.reader == nil {
		panic("BUG: reading released body stream")
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
		return n, err
	}

	if rs.totalBytesRead == contentLength {
		err = io.EOF
	}
	return n, err
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
	return rs
}

func releaseRequestStream(rs *requestStream) {
	rs.prefetchedBytes.Reset(nil)
	rs.totalBytesRead = 0
	rs.chunkLeft = 0
	rs.reader = nil
	rs.header = nil
	rs.contentLength = 0
	rs.strictEOF = false
	requestStreamPool.Put(rs)
}

var requestStreamPool = sync.Pool{
	New: func() any {
		return &requestStream{}
	},
}

package fasthttp

import (
	"bufio"
	"bytes"
	"io"
	"sync"

	"github.com/valyala/bytebufferpool"
)

type requestStream struct {
	prefetchedBytes *bytes.Reader
	reader          *bufio.Reader
	totalBytesRead  int
	contentLength   int
	chunkLeft       int
}

func (rs *requestStream) Read(p []byte) (int, error) {
	var (
		n   int
		err error
	)
	if rs.contentLength == -1 {
		if rs.chunkLeft == 0 {
			chunkSize, err := parseChunkSize(rs.reader)
			if err != nil {
				return 0, err
			}
			if chunkSize == 0 {
				err = readCrLf(rs.reader)
				if err == nil {
					err = io.EOF
				}
				return 0, err
			}
			rs.chunkLeft = chunkSize
		}
		bytesToRead := len(p)
		if rs.chunkLeft < len(p) {
			bytesToRead = rs.chunkLeft
		}
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
	if rs.totalBytesRead == rs.contentLength {
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
		if n == rs.contentLength {
			return n, io.EOF
		}
		return n, err
	} else {
		left := rs.contentLength - rs.totalBytesRead
		if len(p) > left {
			p = p[:left]
		}
		n, err = rs.reader.Read(p)
		rs.totalBytesRead += n
		if err != nil {
			return n, err
		}
	}

	if rs.totalBytesRead == rs.contentLength {
		err = io.EOF
	}
	return n, err
}

func acquireRequestStream(b *bytebufferpool.ByteBuffer, r *bufio.Reader, contentLength int) *requestStream {
	rs := requestStreamPool.Get().(*requestStream)
	rs.prefetchedBytes = bytes.NewReader(b.B)
	rs.reader = r
	rs.contentLength = contentLength

	return rs
}

func releaseRequestStream(rs *requestStream) {
	rs.prefetchedBytes = nil
	rs.totalBytesRead = 0
	rs.chunkLeft = 0
	rs.reader = nil
	requestStreamPool.Put(rs)
}

var requestStreamPool = sync.Pool{
	New: func() interface{} {
		return &requestStream{}
	},
}

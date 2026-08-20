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
	prefetchedBytes *bytes.Reader
	reader          *bufio.Reader
	totalBytesRead  int
	chunkLeft       int
	// chunkedEOF is set once the last chunk and the trailer of a chunked
	// body have been read, so that further reads don't consume the next
	// request from the reader.
	chunkedEOF bool
}

func (rs *requestStream) Read(p []byte) (int, error) {
	var (
		n   int
		err error
	)
	if rs.header.ContentLength() == -1 {
		if rs.chunkedEOF {
			return 0, io.EOF
		}
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
				rs.chunkedEOF = true
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
	if rs.totalBytesRead == rs.header.ContentLength() {
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
		if n == rs.header.ContentLength() {
			return n, io.EOF
		}
		return n, err
	}
	left := rs.header.ContentLength() - rs.totalBytesRead
	if left > 0 && len(p) > left {
		p = p[:left]
	}
	n, err = rs.reader.Read(p)
	rs.totalBytesRead += n
	if err != nil {
		return n, err
	}

	if rs.totalBytesRead == rs.header.ContentLength() {
		err = io.EOF
	}
	return n, err
}

// discard reads and drops what is left of the body, so that the reader ends
// where the next request starts. It gives up after limit bytes and reports
// whether the end of the body was reached.
func (rs *requestStream) discard(limit int) bool {
	if cl := rs.header.ContentLength(); cl >= 0 && cl-rs.totalBytesRead > limit {
		return false
	}
	_, err := io.CopyN(io.Discard, rs, int64(limit)+1)
	return err == io.EOF
}

func acquireRequestStream(b *bytebufferpool.ByteBuffer, r *bufio.Reader, h bodyStreamHeader) *requestStream {
	rs := requestStreamPool.Get().(*requestStream) //nolint:forcetypeassert
	rs.prefetchedBytes = bytes.NewReader(b.B)
	rs.reader = r
	rs.header = h
	return rs
}

func releaseRequestStream(rs *requestStream) {
	rs.prefetchedBytes = nil
	rs.totalBytesRead = 0
	rs.chunkLeft = 0
	rs.chunkedEOF = false
	rs.reader = nil
	rs.header = nil
	requestStreamPool.Put(rs)
}

var requestStreamPool = sync.Pool{
	New: func() any {
		return &requestStream{}
	},
}

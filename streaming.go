package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/valyala/bytebufferpool"
)

type requestStream struct {
	prefetchedBytes *bytes.Reader
	reader          *bufio.Reader
	totalBytesRead  int
	contentLength   int
}

func (rs *requestStream) Read(p []byte) (int, error) {
	if rs.contentLength == -1 {
		p = p[:0]
		strCRLFLen := len(strCRLF)
		chunkSize, err := parseChunkSize(rs.reader)
		if err != nil {
			return len(p), err
		}
		p, err = appendBodyFixedSize(rs.reader, p, chunkSize+strCRLFLen)
		if err != nil {
			return len(p), err
		}
		if !bytes.Equal(p[len(p)-strCRLFLen:], strCRLF) {
			return len(p), ErrBrokenChunk{
				error: fmt.Errorf("cannot find crlf at the end of chunk"),
			}
		}
		p = p[:len(p)-strCRLFLen]
		if chunkSize == 0 {
			return len(p), io.EOF
		}
		return len(p), nil
	}
	if rs.totalBytesRead == rs.contentLength {
		return 0, io.EOF
	}
	var n int
	var err error
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
	rs.reader = nil
	requestStreamPool.Put(rs)
}

var requestStreamPool = sync.Pool{
	New: func() interface{} {
		return &requestStream{}
	},
}

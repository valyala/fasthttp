package fasthttp

import (
	"bufio"
	"bytes"
	"io"
	"sync"

	"github.com/valyala/bytebufferpool"
)

type headerInterface interface {
	ContentLength() int
	ReadTrailer(r *bufio.Reader) error
}

type RequestStream struct {
	header          headerInterface
	PrefetchedBytes *bytes.Reader
	Reader          *bufio.Reader
	totalBytesRead  int
	chunkLeft       int
}

func (rs *RequestStream) Read(p []byte) (int, error) {
	var (
		n   int
		err error
	)
	if rs.header.ContentLength() == -1 {
		if rs.chunkLeft == 0 {
			chunkSize, err := parseChunkSize(rs.Reader)
			if err != nil {
				return 0, err
			}
			if chunkSize == 0 {
				err = rs.header.ReadTrailer(rs.Reader)
				if err != nil && err != io.EOF {
					return 0, err
				}
				return 0, io.EOF
			}
			rs.chunkLeft = chunkSize
		}
		bytesToRead := len(p)
		if rs.chunkLeft < len(p) {
			bytesToRead = rs.chunkLeft
		}
		n, err = rs.Reader.Read(p[:bytesToRead])
		rs.totalBytesRead += n
		rs.chunkLeft -= n
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if err == nil && rs.chunkLeft == 0 {
			err = readCrLf(rs.Reader)
		}
		return n, err
	}
	if rs.totalBytesRead == rs.header.ContentLength() {
		return 0, io.EOF
	}
	prefetchedSize := int(rs.PrefetchedBytes.Size())
	if prefetchedSize > rs.totalBytesRead {
		left := prefetchedSize - rs.totalBytesRead
		if len(p) > left {
			p = p[:left]
		}
		n, err := rs.PrefetchedBytes.Read(p)
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
	n, err = rs.Reader.Read(p)
	rs.totalBytesRead += n
	if err != nil {
		return n, err
	}

	if rs.totalBytesRead == rs.header.ContentLength() {
		err = io.EOF
	}
	return n, err
}

func acquireRequestStream(b *bytebufferpool.ByteBuffer, br *bufio.Reader, h headerInterface) *RequestStream {
	rs := requestStreamPool.Get().(*RequestStream)
	rs.PrefetchedBytes = bytes.NewReader(b.B)
	rs.Reader = br
	rs.header = h
	return rs
}

func releaseRequestStream(rs *RequestStream) {
	rs.PrefetchedBytes = nil
	rs.totalBytesRead = 0
	rs.chunkLeft = 0
	rs.Reader = nil
	rs.header = nil
	requestStreamPool.Put(rs)
}

var requestStreamPool = sync.Pool{
	New: func() any {
		return &RequestStream{}
	},
}

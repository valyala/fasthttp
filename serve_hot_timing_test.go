package fasthttp

import (
	"io"
	"net"
	"testing"
	"time"
)

// pipelineConn feeds batches of identical requests on every Read, exactly
// as many requests as asked for, and discards writes, reproducing the
// pipelining cadence of a load generator without syscalls.
type pipelineConn struct {
	batch     []byte
	reqLen    int
	remaining int
	written   int64
}

func (c *pipelineConn) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	depth := len(c.batch) / c.reqLen
	n := min(c.remaining, depth)
	c.remaining -= n
	return copy(p, c.batch[:n*c.reqLen]), nil
}

func (c *pipelineConn) Write(p []byte) (int, error) {
	c.written += int64(len(p))
	return len(p), nil
}

func (c *pipelineConn) Close() error                       { return nil }
func (c *pipelineConn) LocalAddr() net.Addr                { return zeroTCPAddr }
func (c *pipelineConn) RemoteAddr() net.Addr               { return zeroTCPAddr }
func (c *pipelineConn) SetDeadline(_ time.Time) error      { return nil }
func (c *pipelineConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *pipelineConn) SetWriteDeadline(_ time.Time) error { return nil }

func BenchmarkServeWrkPipeline64(b *testing.B) {
	const depth = 64
	const request = "GET / HTTP/1.1\r\nHost: 127.0.0.1:8081\r\n\r\n"
	batch := make([]byte, 0, depth*len(request))
	for range depth {
		batch = append(batch, request...)
	}
	body := []byte("Hello, World!")
	s := &Server{
		ReadBufferSize:  16384,
		WriteBufferSize: 16384,
		Handler: func(ctx *RequestCtx) {
			ctx.SetContentType("text/plain")
			ctx.SetBody(body)
		},
	}
	b.SetBytes(int64(len(request)))
	b.ResetTimer()
	c := &pipelineConn{batch: batch, reqLen: len(request), remaining: b.N}
	if err := s.ServeConn(c); err != nil {
		b.Fatal(err)
	}
	if want := int64(b.N) * 117; c.written < want {
		b.Fatalf("short write: %d < %d", c.written, want)
	}
}

func BenchmarkServeCurlPipeline16(b *testing.B) {
	const depth = 16
	const request = "GET /api/v1/items?page=2&limit=50 HTTP/1.1\r\n" +
		"Host: api.example.com\r\n" +
		"User-Agent: Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36\r\n" +
		"Accept: application/json, text/plain, */*\r\n" +
		"Accept-Language: en-US,en;q=0.9\r\n" +
		"Accept-Encoding: identity\r\n" +
		"Referer: https://app.example.com/dashboard\r\n" +
		"Cookie: session=abc123def456; theme=dark\r\n" +
		"Connection: keep-alive\r\n\r\n"
	batch := make([]byte, 0, depth*len(request))
	for range depth {
		batch = append(batch, request...)
	}
	body := []byte(`{"items":[],"page":2,"total":0}`)
	s := &Server{
		ReadBufferSize:  16384,
		WriteBufferSize: 16384,
		Handler: func(ctx *RequestCtx) {
			ctx.SetContentType("application/json")
			ctx.SetBody(body)
		},
	}
	b.SetBytes(int64(len(request)))
	b.ResetTimer()
	c := &pipelineConn{batch: batch, reqLen: len(request), remaining: b.N}
	if err := s.ServeConn(c); err != nil {
		b.Fatal(err)
	}
}

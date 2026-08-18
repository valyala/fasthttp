//go:build !race

package main

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// TestZeroAllocation mirrors TestAllocationServeConn in allocation_test.go.
func TestZeroAllocation(t *testing.T) {
	s := &fasthttp.Server{
		Handler: requestHandler,
	}

	rw := &readWriter{}
	rw.r.Grow(1024)
	rw.w.Grow(1024)

	n := testing.AllocsPerRun(100, func() {
		rw.r.WriteString("GET /foo?bar=baz HTTP/1.1\r\nHost: google.com\r\nCookie: foo=bar\r\n\r\n")
		if err := s.ServeConn(rw); err != nil {
			t.Fatal(err)
		}
		rw.w.Reset()
	})

	if n != 0 {
		t.Fatalf("expected 0 allocations, got %f", n)
	}
}

type readWriter struct {
	net.Conn

	r bytes.Buffer
	w bytes.Buffer
}

func (rw *readWriter) Close() error {
	return nil
}

func (rw *readWriter) Read(b []byte) (int, error) {
	return rw.r.Read(b)
}

func (rw *readWriter) Write(b []byte) (int, error) {
	return rw.w.Write(b)
}

var zeroTCPAddr = &net.TCPAddr{
	IP: net.IPv4zero,
}

func (rw *readWriter) RemoteAddr() net.Addr {
	return zeroTCPAddr
}

func (rw *readWriter) LocalAddr() net.Addr {
	return zeroTCPAddr
}

func (rw *readWriter) SetDeadline(t time.Time) error      { return nil }
func (rw *readWriter) SetReadDeadline(t time.Time) error  { return nil }
func (rw *readWriter) SetWriteDeadline(t time.Time) error { return nil }

package websocket

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

var benchSizes = []int{64, 4 << 10, 64 << 10, 1 << 20}

func benchServer(b *testing.B, h func(*Conn)) string {
	return startServerWith(b, nil, h, func(*fasthttp.Server) {}).addr
}

// BenchmarkEcho sends a message to an echoing fasthttp server over loopback
// TCP and reads it back.
func BenchmarkEcho(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			c, err := Dial(b.Context(), "ws://"+benchServer(b, echo)+"/", nil)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = c.CloseNow() }()
			msg := make([]byte, n)
			ctx := context.Background()
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for b.Loop() {
				if err := c.Write(ctx, MessageBinary, msg); err != nil {
					b.Fatal(err)
				}
				if _, _, err := c.Read(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkEchoMixed alternates small and large messages on one connection,
// which a pool calibrated on the common size would penalize.
func BenchmarkEchoMixed(b *testing.B) {
	c, err := Dial(b.Context(), "ws://"+benchServer(b, echo)+"/", nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	small, large := make([]byte, 64), make([]byte, 64<<10)
	ctx := context.Background()
	b.SetBytes(int64(len(small) + len(large)))
	b.ReportAllocs()
	for b.Loop() {
		for _, msg := range [][]byte{small, large} {
			if err := c.Write(ctx, MessageBinary, msg); err != nil {
				b.Fatal(err)
			}
			if _, _, err := c.Read(ctx); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkWrite measures Write over loopback TCP to a peer that discards,
// from the server (unmasked) and the client (masked) side.
func BenchmarkWrite(b *testing.B) {
	for _, client := range []bool{false, true} {
		for _, n := range benchSizes {
			b.Run(map[bool]string{false: "server", true: "client"}[client]+"/"+strconv.Itoa(n), func(b *testing.B) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					b.Fatal(err)
				}
				defer ln.Close()
				go func() {
					peer, err := ln.Accept()
					if err == nil {
						_, _ = io.Copy(io.Discard, peer)
					}
				}()
				nc, err := net.Dial("tcp", ln.Addr().String())
				if err != nil {
					b.Fatal(err)
				}
				c := newConn(nc, nc, client, "", connConfig{})
				defer func() { _ = c.CloseNow() }()
				msg := make([]byte, n)
				ctx := context.Background()
				b.SetBytes(int64(n))
				b.ReportAllocs()
				for b.Loop() {
					if err := c.Write(ctx, MessageBinary, msg); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// repeatReader serves the same bytes over and over.
type repeatReader struct {
	b   []byte
	off int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := copy(p, r.b[r.off:])
	if r.off += n; r.off == len(r.b) {
		r.off = 0
	}
	return n, nil
}

// nopConn is a net.Conn whose deadlines do nothing; the data comes from
// elsewhere.
type nopConn struct{ net.Conn }

func (nopConn) SetDeadline(time.Time) error      { return nil }
func (nopConn) SetReadDeadline(time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(time.Time) error { return nil }

// BenchmarkRead parses masked frames from memory, which isolates the frame
// path from the network.
func BenchmarkRead(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			frame := appendFrame(nil, opBinary, true, true, make([]byte, n))
			c := newConn(nopConn{}, &repeatReader{b: frame}, false, "", connConfig{})
			ctx := context.Background()
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := c.Read(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMask(b *testing.B) {
	for _, n := range []int{64, 4 << 10, 64 << 10} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			buf := make([]byte, n)
			b.SetBytes(int64(n))
			for b.Loop() {
				maskBytes([4]byte{1, 2, 3, 4}, 0, buf)
			}
		})
	}
}

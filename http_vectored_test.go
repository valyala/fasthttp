package fasthttp

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serveOneOverConn runs one request through a real server on a real socket and
// returns the exact response bytes, so the vectored path is exercised end to
// end rather than through a stand-in writer.
func serveOneOverConn(t *testing.T, s *Server, network, request string) []byte {
	t.Helper()

	var ln net.Listener
	var err error
	switch network {
	case "unix":
		// Not t.TempDir(): its path can exceed the sockaddr_un limit.
		f, ferr := os.CreateTemp("", "fh*.sock") //nolint:usetesting // t.TempDir() is too long for sockaddr_un
		if ferr != nil {
			t.Fatalf("temp socket: %v", ferr)
		}
		path := f.Name()
		f.Close()
		os.Remove(path)
		defer os.Remove(path)
		ln, err = net.Listen("unix", path)
	default:
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ln) }()
	defer s.Shutdown() //nolint:errcheck

	c, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.SetDeadline(time.Now().Add(testTimeout(10 * time.Second))); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := c.Write([]byte(request)); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp
}

func TestResponseVectoredWriteMatchesBuffered(t *testing.T) {
	t.Parallel()

	// Bodies straddling the write buffer on both sides, so the same response is
	// produced by the buffered path in one server and the vectored path in the
	// other.
	const bufSize = 4096
	sizes := []int{0, 1, 100, bufSize - 200, bufSize - 1, bufSize, bufSize + 1, 4 * bufSize, 100000}

	for _, size := range sizes {
		body := make([]byte, size)
		for i := range body {
			body[i] = byte('a' + i%26)
		}
		handler := func(ctx *RequestCtx) {
			ctx.SetContentType("application/octet-stream")
			ctx.Response.Header.Set("X-Fixed", "value")
			ctx.Response.SetBodyRaw(body)
		}

		// The reference path: Response.Write with no connection, which is what
		// every non-socket caller uses.
		var ref bytes.Buffer
		func() {
			var ctx RequestCtx
			ctx.Init(&Request{}, nil, nil)
			handler(&ctx)
			ctx.Response.Header.SetServer("ref")
			ctx.Response.Header.noDefaultDate = true
			bw := bufio.NewWriterSize(&ref, bufSize)
			if err := ctx.Response.Write(bw); err != nil {
				t.Fatalf("size %d: reference write: %v", size, err)
			}
			if err := bw.Flush(); err != nil {
				t.Fatalf("size %d: reference flush: %v", size, err)
			}
		}()

		s := &Server{
			Name:            "ref",
			Handler:         handler,
			ReadBufferSize:  bufSize,
			WriteBufferSize: bufSize,
			NoDefaultDate:   true,
		}
		got := serveOneOverConn(t, s, "tcp",
			"GET / HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")

		// The served response ends its header with the Connection: close the
		// request asked for; everything before it must match byte for byte.
		gotHead, gotBody, ok := bytes.Cut(got, []byte("\r\n\r\n"))
		if !ok {
			t.Fatalf("size %d: no header terminator in %q", size, got)
		}
		if !bytes.Equal(gotBody, body) {
			t.Fatalf("size %d: body differs: got %d bytes, want %d", size, len(gotBody), len(body))
		}
		refHead, _, _ := bytes.Cut(ref.Bytes(), []byte("\r\n\r\n"))
		if head := bytes.TrimSuffix(gotHead, []byte("\r\nConnection: close")); !bytes.Equal(head, refHead) {
			t.Fatalf("size %d: served header %q, expecting %q", size, gotHead, refHead)
		}
	}
}

func TestResponseVectoredWriteOverUnixSocket(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("u"), 40000)
	s := &Server{
		Handler:         func(ctx *RequestCtx) { ctx.Response.SetBodyRaw(body) },
		WriteBufferSize: 4096,
	}
	got := serveOneOverConn(t, s, "unix",
		"GET / HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")
	if _, gotBody, ok := bytes.Cut(got, []byte("\r\n\r\n")); !ok || !bytes.Equal(gotBody, body) {
		t.Fatalf("unix socket body mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

func TestResponseVectoredWriteSkippedForTLS(t *testing.T) {
	t.Parallel()

	// A TLS connection must keep the buffered path: net.Buffers would fall back
	// to writing the pieces one at a time, which costs an extra record.
	cert, priv, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	pair, err := tls.X509KeyPair(cert, priv)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}

	body := bytes.Repeat([]byte("t"), 40000)
	s := &Server{
		Handler:         func(ctx *RequestCtx) { ctx.Response.SetBodyRaw(body) },
		WriteBufferSize: 4096,
		TLSConfig:       &tls.Config{Certificates: []tls.Certificate{pair}},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go s.ServeTLS(ln, "", "") //nolint:errcheck
	defer s.Shutdown()        //nolint:errcheck

	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(testTimeout(10 * time.Second))); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, gotBody, ok := bytes.Cut(got, []byte("\r\n\r\n")); !ok || !bytes.Equal(gotBody, body) {
		t.Fatalf("tls body mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

func TestResponseVectoredWriteKeepsPipelineOrder(t *testing.T) {
	t.Parallel()

	// Responses that take the vectored path must not overtake the ones still
	// sitting in the buffered writer.
	const bufSize = 4096
	small := []byte("small")
	large := bytes.Repeat([]byte("L"), 3*bufSize)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if string(ctx.Path()) == "/large" {
				ctx.Response.SetBodyRaw(large)
				return
			}
			ctx.Response.SetBodyRaw(small)
		},
		ReadBufferSize:  bufSize,
		WriteBufferSize: bufSize,
	}

	var req strings.Builder
	want := make([][]byte, 0, 8)
	for i := range 4 {
		req.WriteString("GET /small HTTP/1.1\r\nHost: h\r\n\r\n")
		want = append(want, small)
		if i == 3 {
			req.WriteString("GET /large HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")
		} else {
			req.WriteString("GET /large HTTP/1.1\r\nHost: h\r\n\r\n")
		}
		want = append(want, large)
	}

	got := serveOneOverConn(t, s, "tcp", req.String())

	rest := got
	for i, wantBody := range want {
		_, after, ok := bytes.Cut(rest, []byte("\r\n\r\n"))
		if !ok {
			t.Fatalf("response %d: no header terminator left", i)
		}
		if len(after) < len(wantBody) {
			t.Fatalf("response %d: %d bytes left, want at least %d", i, len(after), len(wantBody))
		}
		if !bytes.Equal(after[:len(wantBody)], wantBody) {
			t.Fatalf("response %d: body out of order", i)
		}
		rest = after[len(wantBody):]
	}
	if len(rest) != 0 {
		t.Fatalf("%d trailing bytes after the last response", len(rest))
	}
}

func TestResponseVectoredWriteSkipsBodylessResponses(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("h"), 40000)
	s := &Server{
		Handler:         func(ctx *RequestCtx) { ctx.Response.SetBodyRaw(body) },
		WriteBufferSize: 4096,
	}
	got := serveOneOverConn(t, s, "tcp",
		"HEAD / HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")

	head, after, ok := bytes.Cut(got, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("no header terminator in %q", got)
	}
	if len(after) != 0 {
		t.Fatalf("HEAD response carried %d body bytes", len(after))
	}
	if !bytes.Contains(head, []byte("Content-Length: 40000")) {
		t.Fatalf("HEAD response lost its Content-Length: %q", head)
	}
}

func TestResponseVectoredWriteAllocations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows write path draws its WSABUF and operation objects from sync.Pools, which the race detector drops at random")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(io.Discard, c) //nolint:errcheck
		c.Close()
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	w := bufio.NewWriterSize(c, 16384)
	body := bytes.Repeat([]byte("b"), 32000)
	for _, headerSize := range []int{64, 20000} {
		var resp Response
		resp.Header.Set("X-Pad", string(bytes.Repeat([]byte("v"), headerSize)))
		resp.SetBodyRaw(body)
		if err := resp.write(c, w); err != nil {
			t.Fatal(err)
		}
		n := testing.AllocsPerRun(20, func() {
			if err := resp.write(c, w); err != nil {
				t.Fatal(err)
			}
		})
		if n > 0 {
			t.Errorf("%d byte header: %v allocs/op on the vectored path, expecting 0", headerSize, n)
		}
	}
}

func TestResponseVectoredWriteThroughPerIPConn(t *testing.T) {
	t.Parallel()

	// MaxConnsPerIP wraps the connection; the wrapper only overrides Close,
	// so the vectored path still applies underneath it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(io.Discard, c) //nolint:errcheck
		c.Close()
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	counter := &perIPConnCounter{}
	counter.Register(1)
	wrapped := acquirePerIPConn(c, 1, counter)
	defer wrapped.Close()
	if vectorWriter(wrapped) != c {
		t.Fatalf("vectorWriter(%T) = %v, expecting the TCP connection", wrapped, vectorWriter(wrapped))
	}
}

package fasthttp

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

const testProxyV1Header = "PROXY TCP4 192.168.0.1 192.168.0.11 56324 443\r\n"

func testProxyV2Header(verCmd, famProto byte, body ...byte) []byte {
	h := []byte("\r\n\r\n\x00\r\nQUIT\n")
	h = append(h, verCmd, famProto, byte(len(body)>>8), byte(len(body)))
	return append(h, body...)
}

func testProxyV2TCP4(tlvs ...byte) []byte {
	body := append([]byte{192, 168, 0, 1, 192, 168, 0, 11, 0xdc, 0x04, 0x01, 0xbb}, tlvs...)
	return testProxyV2Header(0x21, 0x11, body...)
}

func testProxyV2TCP6(tlvs ...byte) []byte {
	body := make([]byte, 36, 36+len(tlvs))
	copy(body, net.ParseIP("2001:db8::1"))
	copy(body[16:], net.ParseIP("2001:db8::2"))
	binary.BigEndian.PutUint16(body[32:], 56324)
	binary.BigEndian.PutUint16(body[34:], 443)
	return testProxyV2Header(0x21, 0x21, append(body, tlvs...)...)
}

func TestReadProxyHeader(t *testing.T) {
	t.Parallel()

	const payload = "GET / HTTP/1.1\r\n\r\n"
	addrString := func(a net.Addr) string {
		if a == nil {
			return ""
		}
		return a.String()
	}

	valid := []struct {
		name   string
		header []byte
		remote string
		local  string
	}{
		{name: "v1 tcp4", header: []byte(testProxyV1Header), remote: "192.168.0.1:56324", local: "192.168.0.11:443"},
		{name: "v1 tcp6", header: []byte("PROXY TCP6 2001:db8::1 ::ffff:1.2.3.4 65535 0\r\n"), remote: "[2001:db8::1]:65535", local: "1.2.3.4:0"},
		{name: "v1 unknown", header: []byte("PROXY UNKNOWN\r\n")},
		{name: "v1 unknown with addresses", header: []byte("PROXY UNKNOWN ::1 ::1  1 2 3\r\n")},
		{name: "v1 unknown with lf", header: []byte("PROXY UNKNOWN a\nb\r\n")},
		{name: "v1 unknown with lf elsewhere", header: []byte("PROXY UNKNOWN \nab\r\n")},
		{name: "v1 of 107 bytes", header: []byte("PROXY UNKNOWN " + strings.Repeat("x", 91) + "\r\n")},
		{name: "v2 tcp4", header: testProxyV2TCP4(), remote: "192.168.0.1:56324", local: "192.168.0.11:443"},
		{name: "v2 tcp6 with tlv", header: testProxyV2TCP6(0xea, 0, 3, 'a', 'b', 'c'), remote: "[2001:db8::1]:56324", local: "[2001:db8::2]:443"},
		{name: "v2 long tlv", header: testProxyV2TCP4(append([]byte{0x04, 0x01, 0x2c}, make([]byte, 300)...)...), remote: "192.168.0.1:56324", local: "192.168.0.11:443"},
		{name: "v2 local", header: testProxyV2Header(0x20, 0xff, 1, 2, 3, 4, 5)},
		{name: "v2 unspec", header: testProxyV2Header(0x21, 0x00, 1, 2, 3)},
		{name: "v2 udp4", header: testProxyV2Header(0x21, 0x12, make([]byte, 12)...)},
		{name: "v2 unix", header: testProxyV2Header(0x21, 0x31, make([]byte, 216)...)},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Whole and a byte at a time: either way the payload stays unread.
			for _, wrap := range []func(io.Reader) io.Reader{
				func(r io.Reader) io.Reader { return r },
				iotest.OneByteReader,
			} {
				r := wrap(bytes.NewReader(append(slices.Clip(tt.header), payload...)))
				remote, local, err := readProxyHeader(r)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got := addrString(remote); got != tt.remote {
					t.Fatalf("remote = %q, want %q", got, tt.remote)
				}
				if got := addrString(local); got != tt.local {
					t.Fatalf("local = %q, want %q", got, tt.local)
				}
				rest, err := io.ReadAll(r)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if string(rest) != payload {
					t.Fatalf("left after the header: %q, want %q", rest, payload)
				}
			}

			// Cut anywhere, the header is truncated; nothing at all is a clean EOF.
			for n := range len(tt.header) {
				want := io.ErrUnexpectedEOF
				if n == 0 {
					want = io.EOF
				}
				if _, _, err := readProxyHeader(bytes.NewReader(tt.header[:n])); err != want {
					t.Fatalf("first %d bytes: error = %v, want %v", n, err, want)
				}
			}
		})
	}

	invalid := []struct {
		name   string
		header []byte
	}{
		{name: "http request", header: []byte(payload)},
		{name: "v1 of 108 bytes", header: []byte("PROXY UNKNOWN " + strings.Repeat("x", 92) + "\r\n")},
		{name: "v1 lf after the protocol", header: []byte("PROXY UNKNOWN\n\r\n")},
		{name: "v1 too many spaces", header: []byte("PROXY TCP4" + strings.Repeat(" ", 8) + "\r\n")},
		{name: "v1 unknown protocol", header: []byte("PROXY UNKNOWNS\r\n")},
		{name: "v1 udp4", header: []byte("PROXY UDP4 1.2.3.4 5.6.7.8 1 2\r\n")},
		{name: "v1 family mismatch", header: []byte("PROXY TCP4 ::1 ::1 1 2\r\n")},
		{name: "v1 ipv6 zone", header: []byte("PROXY TCP6 fe80::1%eth0 ::1 1 2\r\n")},
		{name: "v1 missing port", header: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1\r\n")},
		{name: "v1 extra field", header: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1 2 3\r\n")},
		{name: "v1 double space", header: []byte("PROXY TCP4 1.2.3.4  5.6.7.8 1 2\r\n")},
		{name: "v1 port leading zero", header: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 080 443\r\n")},
		{name: "v1 port out of range", header: []byte("PROXY TCP4 1.2.3.4 5.6.7.8 65536 443\r\n")},
		{name: "v2 version", header: testProxyV2Header(0x11, 0x11, make([]byte, 12)...)},
		{name: "v2 command", header: testProxyV2Header(0x22, 0x11, make([]byte, 12)...)},
		{name: "v2 family", header: testProxyV2Header(0x21, 0x41, make([]byte, 12)...)},
		{name: "v2 protocol", header: testProxyV2Header(0x21, 0x13, make([]byte, 12)...)},
		{name: "v2 short tcp4", header: testProxyV2Header(0x21, 0x11, make([]byte, 11)...)},
		{name: "v2 short udp6", header: testProxyV2Header(0x21, 0x22, make([]byte, 35)...)},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := bytes.NewReader(append(slices.Clip(tt.header), payload...))
			if _, _, err := readProxyHeader(r); err != errProxyHeader {
				t.Fatalf("error = %v, want %v", err, errProxyHeader)
			}
		})
	}
}

// Every valid version 1 line is read to its CRLF and no further, whatever the
// lengths of its fields.
func TestReadProxyHeaderV1Exact(t *testing.T) {
	t.Parallel()

	addrs := map[string][]string{
		"TCP4": {"0.0.0.0", "10.0.0.1", "255.255.255.255"},
		"TCP6": {"::", "::1", "2001:db8::1", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"},
	}
	ports := []string{"0", "80", "65535"}
	for proto, ips := range addrs {
		for _, src := range ips {
			for _, dst := range ips {
				for _, sport := range ports {
					for _, dport := range ports {
						line := "PROXY " + proto + " " + src + " " + dst + " " + sport + " " + dport + "\r\n"
						r := strings.NewReader(line + "\r\n")
						if _, _, err := readProxyHeader(r); err != nil {
							t.Fatalf("%q: unexpected error: %v", line, err)
						}
						if r.Len() != 2 {
							t.Fatalf("%q: %d bytes left after the header, want 2", line, r.Len())
						}
					}
				}
			}
		}
	}
}

func FuzzReadProxyHeader(f *testing.F) {
	f.Add([]byte(testProxyV1Header + "GET / HTTP/1.1\r\n\r\n"))
	f.Add([]byte("PROXY UNKNOWN \r\r\n\n"))
	f.Add(testProxyV2TCP4(0xea, 0, 1, 'a', 'G', 'E', 'T'))
	f.Add(testProxyV2Header(0x20, 0x00))
	f.Add(append(testProxyV2Header(0x21, 0x00, make([]byte, 65535)...), "GET"...))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		_, _, err := readProxyHeader(r)
		n := len(data) - r.Len()
		if n > 16+65535 {
			t.Fatalf("read %d bytes, more than any header has", n)
		}
		if err != nil {
			return
		}
		want := bytes.Index(data, strCRLF) + len(strCRLF)
		if bytes.HasPrefix(data, proxyV2Signature) {
			want = 16 + int(binary.BigEndian.Uint16(data[14:]))
		}
		if n != want {
			t.Fatalf("read %d bytes of %q, want %d", n, data, want)
		}
	})
}

func testProxyServe(t *testing.T, s *Server, ln net.Listener) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- s.Serve(ln) }()
	t.Cleanup(func() {
		ln.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		case <-time.After(testTimeout(time.Second)):
			t.Error("timeout")
		}
	})
}

// testProxyGet sends a request on c, when there is one to send, and returns
// the status code and body of the response.
func testProxyGet(t *testing.T, c net.Conn, request string) (int, string) {
	t.Helper()

	if request != "" {
		if _, err := c.Write([]byte(request)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	var resp Response
	if err := resp.Read(bufio.NewReader(c)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return resp.StatusCode(), string(resp.Body())
}

const testProxyRequest = "GET / HTTP/1.1\r\nHost: a\r\n\r\n"

func TestServerProxyProtocol(t *testing.T) {
	t.Parallel()

	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatal(err)
	}

	const want = "192.168.0.1:56324 192.168.0.11:443"
	for _, tt := range []struct {
		name   string
		header []byte
		tls    bool
		want   string
	}{
		{name: "v1", header: []byte(testProxyV1Header), want: want + " false"},
		{name: "v2", header: testProxyV2TCP4(), want: want + " false"},
		{name: "local", header: testProxyV2Header(0x20, 0x00), want: "pipe InmemoryListener false"},
		// The header comes ahead of the handshake, for a listener that has
		// wrapped the connection in TLS already.
		{name: "tls", header: testProxyV2TCP4(), tls: true, want: want + " true"},
		{name: "no header"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var served atomic.Bool
			s := &Server{
				ProxyProtocol: true,
				Logger:        &testLogger{},
				Handler: func(ctx *RequestCtx) {
					served.Store(true)
					if nc, ok := ctx.Conn().(netConner); ctx.IsTLS() && (!ok || nc.NetConn() == nil) {
						t.Error("the TLS connection has no connection under it")
					}
					fmt.Fprintf(ctx, "%s %s %v", ctx.RemoteAddr(), ctx.LocalAddr(), ctx.IsTLS())
				},
			}
			ln := fasthttputil.NewInmemoryListener()
			if tt.tls {
				testProxyServe(t, s, tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}}))
			} else {
				testProxyServe(t, s, ln)
			}

			c, err := ln.Dial()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer c.Close()
			if err := c.SetDeadline(time.Now().Add(testTimeout(time.Second))); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.want == "" {
				if _, err := c.Write([]byte(testProxyRequest)); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if b, err := io.ReadAll(c); err != nil || len(b) != 0 {
					t.Fatalf("read %q, %v from a connection without a header, want it closed", b, err)
				}
				if served.Load() {
					t.Fatal("a request without a header was served")
				}
				return
			}

			if _, err := c.Write(tt.header); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.tls {
				c = tls.Client(c, &tls.Config{InsecureSkipVerify: true})
			}
			// The address stays for every request of the connection.
			for range 2 {
				if code, body := testProxyGet(t, c, testProxyRequest); code != StatusOK || body != tt.want {
					t.Fatalf("response = %d %q, want %d %q", code, body, StatusOK, tt.want)
				}
			}
		})
	}
}

func TestServerProxyProtocolMaxConnsPerIP(t *testing.T) {
	t.Parallel()

	s := &Server{
		ProxyProtocol: true,
		MaxConnsPerIP: 1,
		Logger:        &testLogger{},
		Handler:       func(ctx *RequestCtx) {},
	}
	ln := fasthttputil.NewInmemoryListener()
	testProxyServe(t, s, ln)

	dial := func(ip string) net.Conn {
		t.Helper()
		c, err := ln.Dial()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Cleanup(func() { c.Close() })
		if err := c.SetDeadline(time.Now().Add(testTimeout(time.Second))); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := c.Write([]byte("PROXY TCP4 " + ip + " 10.0.0.1 1000 80\r\n")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return c
	}

	// The limit follows the address in the header: the connections all come
	// from the one listener.
	first := dial("1.1.1.1")
	if code, _ := testProxyGet(t, first, testProxyRequest); code != StatusOK {
		t.Fatalf("status code = %d, want %d", code, StatusOK)
	}
	if code, _ := testProxyGet(t, dial("1.1.1.1"), ""); code != StatusTooManyRequests {
		t.Fatalf("status code of a second connection from the address = %d, want %d", code, StatusTooManyRequests)
	}
	other := dial("2.2.2.2")
	if code, _ := testProxyGet(t, other, testProxyRequest); code != StatusOK {
		t.Fatalf("status code of a connection from another address = %d, want %d", code, StatusOK)
	}

	// ServeConn hands the limit to its caller.
	server, client := net.Pipe()
	defer client.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.ServeConn(server) }()
	if err := client.SetDeadline(time.Now().Add(testTimeout(time.Second))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := client.Write([]byte("PROXY TCP4 2.2.2.2 10.0.0.1 1000 80\r\n")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code, _ := testProxyGet(t, client, ""); code != StatusTooManyRequests {
		t.Fatalf("status code from ServeConn = %d, want %d", code, StatusTooManyRequests)
	}
	if err := <-serveErr; err != ErrPerIPConnLimit {
		t.Fatalf("ServeConn error = %v, want %v", err, ErrPerIPConnLimit)
	}

	// Closing a connection gives its place back.
	first.Close()
	deadline := time.Now().Add(testTimeout(time.Second))
	for {
		c := dial("1.1.1.1")
		// A connection still over the limit may be closed by now.
		c.Write([]byte(testProxyRequest)) //nolint:errcheck
		code, _ := testProxyGet(t, c, "")
		if code == StatusTooManyRequests && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			continue
		}
		if code != StatusOK {
			t.Fatalf("status code after the first connection closed = %d, want %d", code, StatusOK)
		}
		break
	}
}

// Nothing is written ahead of the header, a rejection included.
func TestServerProxyProtocolConcurrencyLimit(t *testing.T) {
	t.Parallel()

	serving, release := make(chan struct{}), make(chan struct{})
	s := &Server{
		ProxyProtocol: true,
		Concurrency:   1,
		Logger:        &testLogger{},
		Handler: func(ctx *RequestCtx) {
			close(serving)
			<-release
		},
	}
	ln := fasthttputil.NewInmemoryListener()
	testProxyServe(t, s, ln)

	dial := func() net.Conn {
		t.Helper()
		c, err := ln.Dial()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Cleanup(func() { c.Close() })
		if err := c.SetDeadline(time.Now().Add(testTimeout(time.Second))); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return c
	}

	first := dial()
	if _, err := first.Write([]byte(testProxyV1Header + testProxyRequest)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-serving

	if b, err := io.ReadAll(dial()); err != nil || len(b) != 0 {
		t.Fatalf("read %q, %v from a connection over the limit, want it closed", b, err)
	}

	close(release)
	if code, _ := testProxyGet(t, first, ""); code != StatusOK {
		t.Fatalf("status code = %d, want %d", code, StatusOK)
	}
}

// A connection over MaxConnsPerIP that doesn't read its response is given 5
// seconds as well.
func TestServerProxyProtocolMaxConnsPerIPDeadline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// Keep the shared Date refresher out of the synctest bubble.
		s := &Server{ProxyProtocol: true, MaxConnsPerIP: 1, NoDefaultDate: true}
		accept := func() error {
			server, client := net.Pipe()
			go client.Write([]byte(testProxyV1Header)) //nolint:errcheck
			return s.acceptProxyHeader(newProxyConn(testProxyKeptConn{server}))
		}
		if err := accept(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		start := time.Now()
		if err := accept(); err != ErrPerIPConnLimit {
			t.Fatalf("error = %v, want %v", err, ErrPerIPConnLimit)
		}
		if d := time.Since(start); d != 5*time.Second {
			t.Fatalf("the response is given %v, want %v", d, 5*time.Second)
		}
	})
}

// A hijacked connection shows what is under a wrapper of its own server only.
func TestHijackConnUnwrappedConn(t *testing.T) {
	t.Parallel()

	inner, peer := net.Pipe()
	defer inner.Close()
	defer peer.Close()
	tlsInner := tls.Server(inner, &tls.Config{})
	s, other, proxy := &Server{}, &Server{}, &Server{ProxyProtocol: true}
	for i, tt := range []struct {
		s    *Server
		c    net.Conn
		want net.Conn
	}{
		{s: s, c: inner},
		{s: s, c: newPerIPConn(inner, perIPKey{}, &s.perIPConnCounter), want: inner},
		{s: s, c: newPerIPConn(inner, perIPKey{}, &other.perIPConnCounter)},
		{s: s, c: newPerIPConn(tlsInner, perIPKey{}, &s.perIPConnCounter), want: tlsInner},
		{s: s, c: newPerIPConn(tlsInner, perIPKey{}, &other.perIPConnCounter)},
		{s: proxy, c: newProxyConn(inner), want: inner},
		{s: s, c: newProxyConn(inner)},
		{s: proxy, c: newProxyConn(tlsInner), want: tlsInner},
		{s: s, c: newProxyConn(tlsInner)},
	} {
		hjc := &hijackConn{Conn: tt.c, s: tt.s}
		if got := hjc.UnwrappedConn(); got != tt.want {
			t.Errorf("%d: %T under %T, want %T", i, got, tt.c, tt.want)
		}
	}
}

// testProxyDeadlineConn turns EOF into a read timeout while a read deadline
// is set.
type testProxyDeadlineConn struct {
	readWriter

	deadline time.Time
	closed   chan struct{}
}

func (c *testProxyDeadlineConn) SetReadDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

func (c *testProxyDeadlineConn) Read(b []byte) (int, error) {
	n, err := c.readWriter.Read(b)
	if err == io.EOF && !c.deadline.IsZero() {
		err = &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	}
	return n, err
}

func (c *testProxyDeadlineConn) Close() error {
	close(c.closed)
	return nil
}

// A read deadline set on the connection ahead of its header holds for the
// first request, as it does without a header.
func TestServerProxyProtocolKeepsReadDeadline(t *testing.T) {
	t.Parallel()

	for _, proxy := range []bool{false, true} {
		deadline := time.Now().Add(time.Hour)
		s := &Server{
			ProxyProtocol: proxy,
			Logger:        &testLogger{},
			Handler:       func(ctx *RequestCtx) {},
			ConnState: func(c net.Conn, state ConnState) {
				if state == StateNew {
					c.SetReadDeadline(deadline) //nolint:errcheck
				}
			},
		}
		c := &testProxyDeadlineConn{closed: make(chan struct{})}
		if proxy {
			c.r.WriteString(testProxyV1Header)
		}
		testProxyServe(t, s, newOneConnListener(c))
		select {
		case <-c.closed:
		case <-time.After(testTimeout(time.Second)):
			t.Fatalf("ProxyProtocol %v: timeout", proxy)
		}

		var resp Response
		if err := resp.Read(bufio.NewReader(&c.w)); err != nil {
			t.Fatalf("ProxyProtocol %v: unexpected error: %v", proxy, err)
		}
		if resp.StatusCode() != StatusRequestTimeout {
			t.Fatalf("ProxyProtocol %v: status code = %d, want %d", proxy, resp.StatusCode(), StatusRequestTimeout)
		}
		if !c.deadline.Equal(deadline) {
			t.Fatalf("ProxyProtocol %v: read deadline = %v, want %v", proxy, c.deadline, deadline)
		}
	}
}

// testProxyKeptConn is for someone else to close, as a hijacked connection is.
type testProxyKeptConn struct {
	net.Conn
}

func (testProxyKeptConn) Close() error {
	return nil
}

// A header that doesn't come is given 5 seconds, or ReadTimeout if that is
// less. A read deadline the connection already has ends the wait sooner.
func TestServerProxyProtocolHeaderDeadline(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		readTimeout time.Duration
		deadline    time.Duration
		tls         bool
		kept        bool
		want        time.Duration
	}{
		{readTimeout: 0, want: 5 * time.Second},
		{readTimeout: time.Minute, want: 5 * time.Second},
		{readTimeout: time.Second, want: time.Second},
		{deadline: 2 * time.Second, want: 2 * time.Second},
		{tls: true, want: 5 * time.Second},
		{kept: true, want: 5 * time.Second},
	} {
		synctest.Test(t, func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()
			defer server.Close()
			if tt.deadline > 0 {
				server.SetReadDeadline(time.Now().Add(tt.deadline)) //nolint:errcheck
			}
			c := server
			if tt.tls {
				c = tls.Server(server, &tls.Config{})
			}
			if tt.kept {
				c = testProxyKeptConn{server}
			}
			s := &Server{ProxyProtocol: true, ReadTimeout: tt.readTimeout}
			start := time.Now()
			err := s.acceptProxyHeader(newProxyConn(c))
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("%+v: error = %v, want %v", tt, err, os.ErrDeadlineExceeded)
			}
			if d := time.Since(start); d != tt.want {
				t.Fatalf("%+v: the header is given %v", tt, d)
			}
		})
	}
}

// testProxyCloseConn reads until it is being closed, which lasts until release
// is closed. It has no deadlines.
type testProxyCloseConn struct {
	net.Conn

	closing chan struct{}
	release chan struct{}
}

func (c *testProxyCloseConn) Read([]byte) (int, error) {
	<-c.closing
	return 0, net.ErrClosed
}

func (c *testProxyCloseConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *testProxyCloseConn) Close() error {
	close(c.closing)
	<-c.release
	return nil
}

// The header timeout is reported after Close returns.
func TestServerProxyProtocolHeaderDeadlineCloses(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		c := &testProxyCloseConn{closing: make(chan struct{}), release: make(chan struct{})}
		s := &Server{ProxyProtocol: true}
		done := make(chan error, 1)
		go func() { done <- s.acceptProxyHeader(newProxyConn(c)) }()
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case err := <-done:
			close(c.release)
			t.Fatalf("error %v reported before the connection is closed", err)
		default:
		}
		close(c.release)
		if err := <-done; !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("error = %v, want %v", err, os.ErrDeadlineExceeded)
		}
	})
}

func TestProxyConnPerIP(t *testing.T) {
	t.Parallel()

	var counter perIPConnCounter
	newConn := func() *proxyConn {
		server, client := net.Pipe()
		t.Cleanup(func() { client.Close() })
		pc := unwrapProxyConn(newProxyConn(server))
		pc.addrs.Store(&proxyAddrs{remote: &net.TCPAddr{IP: net.IPv4(1, 2, 3, 4)}, local: &net.TCPAddr{}})
		return pc
	}
	key, _ := getPerIPKey(newConn())

	first, second, third := newConn(), newConn(), newConn()
	for _, pc := range []*proxyConn{first, second} {
		if err := pc.countPerIP(&counter, 2); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if err := third.countPerIP(&counter, 2); err != ErrPerIPConnLimit {
		t.Fatalf("error = %v, want %v", err, ErrPerIPConnLimit)
	}

	// Each connection gives back the one place it holds, however often it is
	// closed.
	for range 2 {
		if err := first.Close(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := third.Close(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if n := counter.m[key]; n != 1 {
		t.Fatalf("%d connections counted, want 1", n)
	}

	// A connection closed before it is counted must not be: nothing would
	// give its place back.
	if err := third.countPerIP(&counter, 2); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("error = %v, want %v", err, net.ErrClosed)
	}
	if n := counter.m[key]; n != 1 {
		t.Fatalf("%d connections counted, want 1", n)
	}
}

func TestVectorWriterThroughProxyConn(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if w := vectorWriter(newProxyConn(c)); w != c {
		t.Fatalf("vectorWriter = %v, want the TCP connection", w)
	}
	// TLS has to stay in the way.
	if w := vectorWriter(newProxyConn(tls.Client(c, &tls.Config{}))); w != nil {
		t.Fatalf("vectorWriter of a TLS connection = %v, want nil", w)
	}
}

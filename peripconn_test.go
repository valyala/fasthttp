package fasthttp

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

var _ tlsConn = &perIPTLSConn{}

func TestPerIPConnCounter(t *testing.T) {
	t.Parallel()

	var cc perIPConnCounter

	first := perIPKey{1, 2, 3, 4}
	second := perIPKey{5, 6, 7, 8}

	for i := 1; i < 100; i++ {
		if n := cc.Register(first); n != i {
			t.Fatalf("Unexpected counter value=%d. Expected %d", n, i)
		}
	}

	n := cc.Register(second)
	if n != 1 {
		t.Fatalf("Unexpected counter value=%d. Expected 1", n)
	}

	cc.Unregister(first)
	if n := cc.Register(first); n != 99 {
		t.Fatalf("Unexpected counter value=%d. Expected 99", n)
	}

	for i := 1; i < 100; i++ {
		cc.Unregister(first)
	}
	cc.Unregister(second)

	n = cc.Register(first)
	if n != 1 {
		t.Fatalf("Unexpected counter value=%d. Expected 1", n)
	}
	cc.Unregister(first)

	if len(cc.m) != 0 {
		t.Fatalf("Unexpected counter map size=%d. Expected 0", len(cc.m))
	}
}

// perIPTestConn reports a fixed remote address; wrapPerIPConn only reads that,
// writes the refusal and closes.
type perIPTestConn struct {
	net.Conn

	remoteAddr net.Addr
}

func (c *perIPTestConn) RemoteAddr() net.Addr        { return c.remoteAddr }
func (c *perIPTestConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *perIPTestConn) Close() error                { return nil }

func perIPTestDial(t *testing.T, addr string) net.Conn {
	t.Helper()

	remoteAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("resolving %q: %v", addr, err)
	}
	return &perIPTestConn{remoteAddr: remoteAddr}
}

func TestPerIPConnCounterIPv6(t *testing.T) {
	t.Parallel()

	s := &Server{MaxConnsPerIP: 2, Logger: &testLogger{}}

	// Two connections from one IPv6 client reach the limit, and the third is
	// refused like an IPv4 one would be.
	for i, addr := range []string{"[2001:db8::1]:1111", "[2001:db8::1]:2222"} {
		if wrapPerIPConn(s, perIPTestDial(t, addr)) == nil {
			t.Fatalf("connection %d from %s refused below MaxConnsPerIP", i, addr)
		}
	}
	if c := wrapPerIPConn(s, perIPTestDial(t, "[2001:db8::1]:3333")); c != nil {
		t.Fatal("third connection from 2001:db8::1 served above MaxConnsPerIP=2")
	}

	// A different IPv6 client keeps its own count.
	if wrapPerIPConn(s, perIPTestDial(t, "[2001:db8::2]:4444")) == nil {
		t.Fatal("connection from 2001:db8::2 refused for 2001:db8::1 connections")
	}

	// An IPv4 peer and its IPv4-mapped form are the same client.
	s = &Server{MaxConnsPerIP: 1, Logger: &testLogger{}}
	if wrapPerIPConn(s, perIPTestDial(t, "1.2.3.4:1111")) == nil {
		t.Fatal("first connection from 1.2.3.4 refused below MaxConnsPerIP")
	}
	if c := wrapPerIPConn(s, perIPTestDial(t, "[::ffff:1.2.3.4]:2222")); c != nil {
		t.Fatal("::ffff:1.2.3.4 counted apart from 1.2.3.4")
	}
}

type closeRecorder struct {
	net.Conn

	closed *bool
}

func (c closeRecorder) Close() error { *c.closed = true; return nil }

// A stale Close, which the shutdown path performs routinely, must not reach
// another connection or release its per-IP slot.
func TestPerIPConnStaleCloseIsNoop(t *testing.T) {
	counter := &perIPConnCounter{}
	var firstClosed, secondClosed bool
	firstIP, secondIP := perIPKey{1}, perIPKey{2}
	first := newPerIPConn(closeRecorder{closed: &firstClosed}, firstIP, counter)
	counter.Register(firstIP)

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if !firstClosed {
		t.Fatal("first Close didn't close the connection")
	}

	// Shutdown closes an idle conn, then the worker closes it again after
	// another connection from a different IP was accepted.
	counter.Register(secondIP)
	second := newPerIPConn(closeRecorder{closed: &secondClosed}, secondIP, counter)

	_ = first.Close() // stale reference

	if secondClosed {
		t.Fatal("a stale Close closed an unrelated connection")
	}
	if got := counter.m[secondIP]; got != 1 {
		t.Fatalf("per-IP count for the live connection = %d, want 1", got)
	}
	_ = second
}

func TestPerIPConnCloseWrite(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	counter := &perIPConnCounter{}
	key, _ := getPerIPKey(accepted)
	counter.Register(key)
	conn := newPerIPConn(accepted, key, counter)
	defer conn.Close()
	halfCloser, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("perIPConn doesn't offer CloseWrite")
	}
	if err := halfCloser.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error: %v", err)
	}
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read after CloseWrite = %v, want EOF", err)
	}

	pipeConn, pipePeer := net.Pipe()
	defer pipePeer.Close()
	counter.Register(key)
	piped := newPerIPConn(pipeConn, key, counter)
	defer piped.Close()
	pipedHalfCloser, ok := piped.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("perIPConn doesn't offer CloseWrite")
	}
	if err := pipedHalfCloser.CloseWrite(); !errors.Is(err, errCloseWriteUnsupported) {
		t.Fatalf("CloseWrite() on a connection without half-close = %v", err)
	}
}

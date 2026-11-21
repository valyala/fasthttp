package http2

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestAsyncFrameWriterPreservesOrderAndOwnership(t *testing.T) {
	conn, peer := net.Pipe()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = peer.Close()
	})

	w := newAsyncFrameWriter(conn, 8, 4, time.Second)
	want := []byte("abcdefghijkl")
	got := make(chan []byte, 1)
	go func() {
		buffer := make([]byte, len(want))
		_, err := io.ReadFull(peer, buffer)
		if err != nil {
			got <- nil
			return
		}
		got <- buffer
	}()

	first := []byte("abcdefgh")
	if _, err := w.Write(first); err != nil {
		t.Fatalf("Write(first) error: %v", err)
	}
	for i := range first {
		first[i] = 'x'
	}
	second := []byte("ijkl")
	if _, err := w.Write(second); err != nil {
		t.Fatalf("Write(second) error: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}
	for i := range second {
		second[i] = 'y'
	}
	if err := w.closeAndWait(time.Second); err != nil {
		t.Fatalf("closeAndWait() error: %v", err)
	}

	select {
	case data := <-got:
		if !bytes.Equal(data, want) {
			t.Fatalf("peer read %q; want %q", data, want)
		}
	case <-time.After(time.Second):
		t.Fatal("peer didn't receive queued bytes")
	}
}

func TestAsyncFrameWriterQueueIsBounded(t *testing.T) {
	conn, peer := net.Pipe()
	signaled := &writeSignalConn{Conn: conn, started: make(chan struct{})}
	t.Cleanup(func() {
		_ = signaled.Close()
		_ = peer.Close()
	})
	w := newAsyncFrameWriter(signaled, 4, 1, 50*time.Millisecond)

	if _, err := w.Write([]byte("aaaa")); err != nil {
		t.Fatalf("first Write() error: %v", err)
	}
	select {
	case <-signaled.started:
	case <-time.After(time.Second):
		t.Fatal("physical writer didn't start")
	}
	if _, err := w.Write([]byte("bbbb")); err != nil {
		t.Fatalf("second Write() error: %v", err)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte("cccc"))
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		t.Fatalf("third Write() returned before bounded queue made space: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	select {
	case err := <-writeDone:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("third Write() error = %v; want physical write timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded queue didn't wake after physical write timeout")
	}

	select {
	case <-w.done:
	case <-time.After(time.Second):
		t.Fatal("queue overflow didn't stop physical writer")
	}
}

func TestAsyncFrameWriterWriteTimeoutTracksProgress(t *testing.T) {
	conn, peer := net.Pipe()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = peer.Close()
	})
	w := newAsyncFrameWriter(conn, 4, 2, 20*time.Millisecond)
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	select {
	case <-w.done:
		var timeout net.Error
		if err := w.err(); !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("writer error = %v; want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked write didn't time out")
	}
}

func TestAsyncFrameWriterRejectsZeroProgress(t *testing.T) {
	conn := &zeroProgressConn{closed: make(chan struct{})}
	w := newAsyncFrameWriter(conn, 4, 2, 0)
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	select {
	case <-w.done:
		if !errors.Is(w.err(), io.ErrNoProgress) {
			t.Fatalf("writer error = %v; want %v", w.err(), io.ErrNoProgress)
		}
	case <-time.After(time.Second):
		t.Fatal("zero-progress writer didn't stop")
	}
}

type writeSignalConn struct {
	net.Conn

	started chan struct{}
	once    sync.Once
}

func (c *writeSignalConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

type zeroProgressConn struct {
	closed chan struct{}
	once   sync.Once
}

func (c *zeroProgressConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *zeroProgressConn) Write([]byte) (int, error) {
	return 0, nil
}

func (c *zeroProgressConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *zeroProgressConn) LocalAddr() net.Addr              { return testAddr("local") }
func (c *zeroProgressConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (c *zeroProgressConn) SetDeadline(time.Time) error      { return nil }
func (c *zeroProgressConn) SetReadDeadline(time.Time) error  { return nil }
func (c *zeroProgressConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

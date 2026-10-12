package fasthttputil

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

func TestPipeConnsWriteTimeout(t *testing.T) {
	t.Parallel()

	pc := NewPipeConns()
	c1 := pc.Conn1()

	deadline := time.Now().Add(time.Millisecond)
	if err := c1.SetWriteDeadline(deadline); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data := []byte("foobar")
	for {
		_, err := c1.Write(data)
		if err != nil {
			if err == ErrTimeout {
				break
			}
			t.Fatalf("unexpected error: %v", err)
		}
	}

	for range 10 {
		_, err := c1.Write(data)
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != ErrTimeout {
			t.Fatalf("unexpected error: %v. Expecting %v", err, ErrTimeout)
		}
	}

	// read the written data
	c2 := pc.Conn2()
	if err := c2.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for {
		_, err := c2.Read(data)
		if err != nil {
			if err == ErrTimeout {
				break
			}
			t.Fatalf("unexpected error: %v", err)
		}
	}

	for range 10 {
		_, err := c2.Read(data)
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != ErrTimeout {
			t.Fatalf("unexpected error: %v. Expecting %v", err, ErrTimeout)
		}
	}
}

func TestPipeConnsSetDeadlineConcurrent(t *testing.T) {
	t.Parallel()

	pc := NewPipeConns()
	c1 := pc.Conn1()

	readErr := make(chan error, 1)
	go func() {
		_, err := c1.Read(make([]byte, 1))
		readErr <- err
	}()
	writeErr := make(chan error, 1)
	go func() {
		for {
			if _, err := c1.Write([]byte("foobar")); err != nil {
				writeErr <- err
				return
			}
		}
	}()

	// Move the deadlines while the other side neither reads nor writes,
	// so that Read and Write are blocked.
	for range 100 {
		if err := c1.SetDeadline(time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		time.Sleep(100 * time.Microsecond)
	}

	// A deadline in the past interrupts the pending operations.
	if err := c1.SetDeadline(time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, ch := range []chan error{readErr, writeErr} {
		select {
		case err := <-ch:
			if err != ErrTimeout {
				t.Fatalf("unexpected error: %v. Expecting %v", err, ErrTimeout)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("pending operation not interrupted by the deadline")
		}
	}

	// Clearing the deadline makes the connection usable again.
	if err := c1.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := pc.Conn2().Write([]byte("x")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := c1.Read(make([]byte, 1)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := pc.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPipeConnsPositiveReadTimeout(t *testing.T) {
	t.Parallel()

	testPipeConnsReadTimeout(t, time.Millisecond)
}

func TestPipeConnsNegativeReadTimeout(t *testing.T) {
	t.Parallel()

	testPipeConnsReadTimeout(t, -time.Second)
}

var zeroTime time.Time

func testPipeConnsReadTimeout(t *testing.T, timeout time.Duration) {
	pc := NewPipeConns()
	c1 := pc.Conn1()

	deadline := time.Now().Add(timeout)
	if err := c1.SetReadDeadline(deadline); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var buf [1]byte
	for i := range 10 {
		_, err := c1.Read(buf[:])
		if err == nil {
			t.Fatalf("expecting error on iteration %d", i)
		}
		if err != ErrTimeout {
			t.Fatalf("unexpected error on iteration %d: %v. Expecting %v", i, err, ErrTimeout)
		}
	}

	// disable deadline and send data from c2 to c1
	if err := c1.SetReadDeadline(zeroTime); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data := []byte("foobar")
	c2 := pc.Conn2()
	if _, err := c2.Write(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dataBuf := make([]byte, len(data))
	if _, err := io.ReadFull(c1, dataBuf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(data, dataBuf) {
		t.Fatalf("unexpected data received: %q. Expecting %q", dataBuf, data)
	}
}

func TestPipeConnsCloseWhileReadWriteConcurrent(t *testing.T) {
	t.Parallel()

	concurrency := 4
	ch := make(chan struct{}, concurrency)
	for range concurrency {
		go func() {
			testPipeConnsCloseWhileReadWriteSerial(t)
			ch <- struct{}{}
		}()
	}

	for range concurrency {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout")
		}
	}
}

func TestPipeConnsCloseWhileReadWriteSerial(t *testing.T) {
	t.Parallel()

	testPipeConnsCloseWhileReadWriteSerial(t)
}

func testPipeConnsCloseWhileReadWriteSerial(t *testing.T) {
	for range 10 {
		testPipeConnsCloseWhileReadWrite(t)
	}
}

func testPipeConnsCloseWhileReadWrite(t *testing.T) {
	pc := NewPipeConns()
	c1 := pc.Conn1()
	c2 := pc.Conn2()

	readCh := make(chan error)
	go func() {
		var err error
		if _, err = io.Copy(io.Discard, c1); err != nil {
			if err != ErrConnectionClosed {
				err = fmt.Errorf("unexpected error: %w", err)
			} else {
				err = nil
			}
		}
		readCh <- err
	}()

	writeCh := make(chan error)
	go func() {
		var err error
		for {
			if _, err = c2.Write([]byte("foobar")); err != nil {
				if err != ErrConnectionClosed {
					err = fmt.Errorf("unexpected error: %w", err)
				} else {
					err = nil
				}
				break
			}
		}
		writeCh <- err
	}()

	time.Sleep(10 * time.Millisecond)
	if err := c1.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case err := <-readCh:
		if err != nil {
			t.Fatalf("unexpected error in reader: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
	select {
	case err := <-writeCh:
		if err != nil {
			t.Fatalf("unexpected error in writer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
}

func TestPipeConnsReadWriteSerial(t *testing.T) {
	t.Parallel()

	testPipeConnsReadWriteSerial(t)
}

func TestPipeConnsReadWriteConcurrent(t *testing.T) {
	t.Parallel()

	testConcurrency(t, 10, testPipeConnsReadWriteSerial)
}

func testPipeConnsReadWriteSerial(t *testing.T) {
	pc := NewPipeConns()
	testPipeConnsReadWrite(t, pc.Conn1(), pc.Conn2())

	pc = NewPipeConns()
	testPipeConnsReadWrite(t, pc.Conn2(), pc.Conn1())
}

func testPipeConnsReadWrite(t *testing.T, c1, c2 net.Conn) {
	defer c1.Close()
	defer c2.Close()

	var buf [32]byte
	for i := range 10 {
		// The first write
		s1 := fmt.Sprintf("foo_%d", i)
		n, err := c1.Write([]byte(s1))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != len(s1) {
			t.Fatalf("unexpected number of bytes written: %d. Expecting %d", n, len(s1))
		}

		// The second write
		s2 := fmt.Sprintf("bar_%d", i)
		n, err = c1.Write([]byte(s2))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != len(s2) {
			t.Fatalf("unexpected number of bytes written: %d. Expecting %d", n, len(s2))
		}

		// Read data written above in two writes
		s := s1 + s2
		n, err = c2.Read(buf[:])
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != len(s) {
			t.Fatalf("unexpected number of bytes read: %d. Expecting %d", n, len(s))
		}
		if string(buf[:n]) != s {
			t.Fatalf("unexpected string read: %q. Expecting %q", buf[:n], s)
		}
	}
}

func TestPipeConnsCloseSerial(t *testing.T) {
	t.Parallel()

	testPipeConnsCloseSerial(t)
}

func TestPipeConnsCloseConcurrent(t *testing.T) {
	t.Parallel()

	testConcurrency(t, 10, testPipeConnsCloseSerial)
}

func testPipeConnsCloseSerial(t *testing.T) {
	pc := NewPipeConns()
	testPipeConnsClose(t, pc.Conn1(), pc.Conn2())

	pc = NewPipeConns()
	testPipeConnsClose(t, pc.Conn2(), pc.Conn1())
}

func testPipeConnsClose(t *testing.T, c1, c2 net.Conn) {
	if err := c1.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var buf [10]byte

	// attempt writing to closed conn
	for range 10 {
		n, err := c1.Write(buf[:])
		if err == nil {
			t.Fatalf("expecting error")
		}
		if n != 0 {
			t.Fatalf("unexpected number of bytes written: %d. Expecting 0", n)
		}
	}

	// attempt reading from closed conn
	for range 10 {
		n, err := c2.Read(buf[:])
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != io.EOF {
			t.Fatalf("unexpected error: %v. Expecting %v", err, io.EOF)
		}
		if n != 0 {
			t.Fatalf("unexpected number of bytes read: %d. Expecting 0", n)
		}
	}

	if err := c2.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// attempt closing already closed conns
	for range 10 {
		if err := c1.Close(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := c2.Close(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func testConcurrency(t *testing.T, concurrency int, f func(*testing.T)) {
	ch := make(chan struct{}, concurrency)
	for range concurrency {
		go func() {
			f(t)
			ch <- struct{}{}
		}()
	}

	for range concurrency {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}
}

func TestPipeConnsAddrDefault(t *testing.T) {
	t.Parallel()

	pc := NewPipeConns()
	c1 := pc.Conn1()

	if c1.LocalAddr() != pipeAddr(0) {
		t.Fatalf("unexpected local address: %v", c1.LocalAddr())
	}

	if c1.RemoteAddr() != pipeAddr(0) {
		t.Fatalf("unexpected remote address: %v", c1.RemoteAddr())
	}
}

func TestPipeConnsAddrCustom(t *testing.T) {
	t.Parallel()

	pc := NewPipeConns()

	addr1 := &net.TCPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1234}
	addr2 := &net.TCPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 5678}
	addr3 := &net.TCPAddr{IP: net.IPv4(9, 10, 11, 12), Port: 9012}
	addr4 := &net.TCPAddr{IP: net.IPv4(13, 14, 15, 16), Port: 3456}

	pc.SetAddresses(addr1, addr2, addr3, addr4)

	c1 := pc.Conn1()

	if c1.LocalAddr() != addr1 {
		t.Fatalf("unexpected local address: %v", c1.LocalAddr())
	}

	if c1.RemoteAddr() != addr2 {
		t.Fatalf("unexpected remote address: %v", c1.RemoteAddr())
	}

	c2 := pc.Conn1()

	if c2.LocalAddr() != addr1 {
		t.Fatalf("unexpected local address: %v", c2.LocalAddr())
	}

	if c2.RemoteAddr() != addr2 {
		t.Fatalf("unexpected remote address: %v", c2.RemoteAddr())
	}
}

func TestPipeConnsCollectedWithPendingDeadline(t *testing.T) {
	t.Parallel()

	collected := make(chan struct{})
	func() {
		pc := NewPipeConns()
		runtime.AddCleanup(pc, func(ch chan struct{}) { close(ch) }, collected)
		c := pc.Conn1()
		if err := c.SetDeadline(time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := pc.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	for range 100 {
		runtime.GC()
		select {
		case <-collected:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("a closed pipe with a pending deadline was not collected")
}

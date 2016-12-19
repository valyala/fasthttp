package fasthttputil

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"testing"
	"time"
)

func TestPipeConnsWriteTimeout(t *testing.T) {
	pc := NewPipeConns()
	c1 := pc.Conn1()

	deadline := time.Now().Add(time.Millisecond)
	if err := c1.SetWriteDeadline(deadline); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	data := []byte("foobar")
	for {
		_, err := c1.Write(data)
		if err != nil {
			if err == ErrTimeout {
				break
			}
			t.Fatalf("unexpected error: %s", err)
		}
	}

	for i := 0; i < 10; i++ {
		_, err := c1.Write(data)
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != ErrTimeout {
			t.Fatalf("unexpected error: %s. Expecting %s", err, ErrTimeout)
		}
	}

	// read the written data
	c2 := pc.Conn2()
	if err := c2.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	for {
		_, err := c2.Read(data)
		if err != nil {
			if err == ErrTimeout {
				break
			}
			t.Fatalf("unexpected error: %s", err)
		}
	}

	for i := 0; i < 10; i++ {
		_, err := c2.Read(data)
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != ErrTimeout {
			t.Fatalf("unexpected error: %s. Expecting %s", err, ErrTimeout)
		}
	}
}

func TestPipeConnsPositiveReadTimeout(t *testing.T) {
	testPipeConnsReadTimeout(t, time.Millisecond)
}

func TestPipeConnsNegativeReadTimeout(t *testing.T) {
	testPipeConnsReadTimeout(t, -time.Second)
}

var zeroTime time.Time

func testPipeConnsReadTimeout(t *testing.T, timeout time.Duration) {
	pc := NewPipeConns()
	c1 := pc.Conn1()

	deadline := time.Now().Add(timeout)
	if err := c1.SetReadDeadline(deadline); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var buf [1]byte
	for i := 0; i < 10; i++ {
		_, err := c1.Read(buf[:])
		if err == nil {
			t.Fatalf("expecting error on iteration %d", i)
		}
		if err != ErrTimeout {
			t.Fatalf("unexpected error on iteration %d: %s. Expecting %s", i, err, ErrTimeout)
		}
	}

	// disable deadline and send data from c2 to c1
	if err := c1.SetReadDeadline(zeroTime); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	data := []byte("foobar")
	c2 := pc.Conn2()
	if _, err := c2.Write(data); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	dataBuf := make([]byte, len(data))
	if _, err := io.ReadFull(c1, dataBuf); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !bytes.Equal(data, dataBuf) {
		t.Fatalf("unexpected data received: %q. Expecting %q", dataBuf, data)
	}
}

func TestPipeConnsCloseWhileReadWriteConcurrent(t *testing.T) {
	concurrency := 4
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			testPipeConnsCloseWhileReadWriteSerial(t)
			ch <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout")
		}
	}
}

func TestPipeConnsCloseWhileReadWriteSerial(t *testing.T) {
	testPipeConnsCloseWhileReadWriteSerial(t)
}

func testPipeConnsCloseWhileReadWriteSerial(t *testing.T) {
	for i := 0; i < 10; i++ {
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
		if _, err = io.Copy(ioutil.Discard, c1); err != nil {
			if err != errConnectionClosed {
				err = fmt.Errorf("unexpected error: %s", err)
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
				if err != errConnectionClosed {
					err = fmt.Errorf("unexpected error: %s", err)
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
		t.Fatalf("unexpected error: %s", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case err := <-readCh:
		if err != nil {
			t.Fatalf("unexpected error in reader: %s", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
	select {
	case err := <-writeCh:
		if err != nil {
			t.Fatalf("unexpected error in writer: %s", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
}

func TestPipeConnsReadWriteSerial(t *testing.T) {
	testPipeConnsReadWriteSerial(t)
}

func TestPipeConnsReadWriteConcurrent(t *testing.T) {
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
	for i := 0; i < 10; i++ {
		// The first write
		s1 := fmt.Sprintf("foo_%d", i)
		n, err := c1.Write([]byte(s1))
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if n != len(s1) {
			t.Fatalf("unexpected number of bytes written: %d. Expecting %d", n, len(s1))
		}

		// The second write
		s2 := fmt.Sprintf("bar_%d", i)
		n, err = c1.Write([]byte(s2))
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if n != len(s2) {
			t.Fatalf("unexpected number of bytes written: %d. Expecting %d", n, len(s2))
		}

		// Read data written above in two writes
		s := s1 + s2
		n, err = c2.Read(buf[:])
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
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
	testPipeConnsCloseSerial(t)
}

func TestPipeConnsCloseConcurrent(t *testing.T) {
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
		t.Fatalf("unexpected error: %s", err)
	}
	var buf [10]byte

	// attempt writing to closed conn
	for i := 0; i < 10; i++ {
		n, err := c1.Write(buf[:])
		if err == nil {
			t.Fatalf("expecting error")
		}
		if n != 0 {
			t.Fatalf("unexpected number of bytes written: %d. Expecting 0", n)
		}
	}

	// attempt reading from closed conn
	for i := 0; i < 10; i++ {
		n, err := c2.Read(buf[:])
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != io.EOF {
			t.Fatalf("unexpected error: %s. Expecting %s", err, io.EOF)
		}
		if n != 0 {
			t.Fatalf("unexpected number of bytes read: %d. Expecting 0", n)
		}
	}

	if err := c2.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// attempt closing already closed conns
	for i := 0; i < 10; i++ {
		if err := c1.Close(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if err := c2.Close(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	}
}

func testConcurrency(t *testing.T, concurrency int, f func(*testing.T)) {
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			f(t)
			ch <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}
}

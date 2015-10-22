package fasthttp

import (
	"net"
	"os"
	"testing"
	"time"
)

func TestTimeoutListener(t *testing.T) {
	addr := "./TestTimeoutListener.unix"
	os.Remove(addr)
	ln, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatalf("Cannot listen %q: %s", addr, err)
	}
	defer ln.Close()

	ln = &TimeoutListener{
		Listener:    ln,
		ReadTimeout: 10 * time.Millisecond,
	}

	stopCh := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Fatalf("unexpected error when accepting conn: %s", err)
		}

		ch := make(chan struct{})
		go func() {
			buf := make([]byte, 6)
			_, err := c.Read(buf)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if string(buf) != "123456" {
				t.Fatalf("Unexpected data read %q. Expected %q", buf, "123456")
			}

			if _, err = c.Read(buf); err == nil {
				t.Fatalf("expecting timeout error")
			}
			if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
				t.Fatalf("unexpected error: %s. Expecting timeout error", err)
			}
			close(ch)
		}()
		select {
		case <-ch:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout")
		}
		close(stopCh)
	}()

	c, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("cannot dial %q: %s", addr, err)
	}
	defer c.Close()

	if _, err = c.Write([]byte("123456")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-stopCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("listener wasn't finished yet")
	}
}

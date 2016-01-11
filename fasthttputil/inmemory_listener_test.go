package fasthttputil

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func TestInmemoryListener(t *testing.T) {
	ln := NewInmemoryListener()

	ch := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(n int) {
			conn, err := ln.Dial()
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			defer conn.Close()
			req := fmt.Sprintf("request_%d", n)
			if _, err = conn.Write([]byte(req)); err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			buf := make([]byte, 30)
			nn, err := conn.Read(buf)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			buf = buf[:nn]
			resp := fmt.Sprintf("response_%d", n)
			if string(buf) != resp {
				t.Fatalf("unexpected response %q. Expecting %q", buf, resp)
			}
			ch <- struct{}{}
		}(i)
	}

	serverCh := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(serverCh)
				return
			}
			defer conn.Close()
			buf := make([]byte, 30)
			n, err := conn.Read(buf)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			buf = buf[:n]
			if !bytes.HasPrefix(buf, []byte("request_")) {
				t.Fatalf("unexpected request prefix %q. Expecting %q", buf, "request_")
			}
			resp := fmt.Sprintf("response_%s", buf[len("request_"):])
			if _, err = conn.Write([]byte(resp)); err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
		}
	}()

	for i := 0; i < 10; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
}

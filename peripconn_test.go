package fasthttp

import (
	"net"
	"testing"
)

var _ tlsConn = &perIPTLSConn{}

func TestPerIPConnCounter(t *testing.T) {
	t.Parallel()

	var cc perIPConnCounter

	for i := 1; i < 100; i++ {
		if n := cc.Register(123); n != i {
			t.Fatalf("Unexpected counter value=%d. Expected %d", n, i)
		}
	}

	n := cc.Register(456)
	if n != 1 {
		t.Fatalf("Unexpected counter value=%d. Expected 1", n)
	}

	cc.Unregister(123)
	if n := cc.Register(123); n != 99 {
		t.Fatalf("Unexpected counter value=%d. Expected 99", n)
	}

	for i := 1; i < 100; i++ {
		cc.Unregister(123)
	}
	cc.Unregister(456)

	n = cc.Register(123)
	if n != 1 {
		t.Fatalf("Unexpected counter value=%d. Expected 1", n)
	}
	cc.Unregister(123)

	if len(cc.m) != 0 {
		t.Fatalf("Unexpected counter map size=%d. Expected 0", len(cc.m))
	}
}

type closeRecorder struct {
	net.Conn

	closed *bool
}

func (c closeRecorder) Close() error { *c.closed = true; return nil }

// A stale Close, which the shutdown path performs routinely, must not reach a
// connection the wrapper was re-acquired for meanwhile.
func TestPerIPConnCloseDoesNotAliasPooledWrapper(t *testing.T) {
	counter := &perIPConnCounter{}
	var firstClosed, secondClosed bool
	first := acquirePerIPConn(closeRecorder{closed: &firstClosed}, 1, counter)
	counter.Register(1)

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if !firstClosed {
		t.Fatal("first Close didn't close the connection")
	}

	// Shutdown closes an idle conn, the worker closes it again, and an accept
	// re-acquires the wrapper in between.
	counter.Register(2)
	second := acquirePerIPConn(closeRecorder{closed: &secondClosed}, 2, counter)

	_ = first.Close() // stale reference

	if secondClosed {
		t.Fatal("a stale Close closed an unrelated connection")
	}
	if got := counter.m[2]; got != 1 {
		t.Fatalf("per-IP count for the live connection = %d, want 1", got)
	}
	_ = second
}

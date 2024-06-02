package fasthttp

import (
	"testing"
)

var _ connTLSer = &perIPTLSConn{}

func TestIPxUint32(t *testing.T) {
	t.Parallel()

	testIPxUint32(t, 0)
	testIPxUint32(t, 10)
	testIPxUint32(t, 0x12892392)
}

func testIPxUint32(t *testing.T, n uint32) {
	ip := uint322ip(n)
	nn := ip2uint32(ip)
	if n != nn {
		t.Fatalf("Unexpected value=%d for ip=%q. Expected %d", nn, ip, n)
	}
}

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

	for i := 1; i < 100; i++ {
		cc.Unregister(123)
	}
	cc.Unregister(456)

	n = cc.Register(123)
	if n != 1 {
		t.Fatalf("Unexpected counter value=%d. Expected 1", n)
	}
	cc.Unregister(123)
}

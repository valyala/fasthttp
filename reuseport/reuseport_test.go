package reuseport

import (
	"net"
	"testing"
)

func TestTCP4(t *testing.T) {
	t.Parallel()

	testNewListener(t, "tcp4", "localhost:10081")
}

func TestTCP6(t *testing.T) {
	t.Parallel()

	// Run this test only if tcp6 interface exists.
	if hasLocalIPv6(t) {
		testNewListener(t, "tcp6", "[::1]:10082")
	}
}

func hasLocalIPv6(t *testing.T) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("cannot obtain local interfaces: %v", err)
	}
	for _, a := range addrs {
		if a.String() == "::1/128" {
			return true
		}
	}
	return false
}

func testNewListener(t *testing.T, network, addr string) {
	ln1, err := Listen(network, addr)
	if err != nil {
		t.Fatalf("cannot create listener %v", err)
	}

	ln2, err := Listen(network, addr)
	if err != nil {
		t.Fatalf("cannot create listener %v", err)
	}

	_ = ln1.Close()
	_ = ln2.Close()
}

// +build windows

package reuseport

import (
	"testing"
)

func TestListen(t *testing.T) {
	_, err := Listen("tcp6", "[::1]:10082")
	if err == nil {
		t.Fatalf("unexpected non-error creating listener")
	}

	if _, errnoreuseport := err.(*ErrNoReusePort); !errnoreuseport {
		t.Fatalf("unexpected error creating listener: %s", err)
	}
}

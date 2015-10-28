package fasthttp

import (
	"bytes"
	"testing"
)

var strFoobar = []byte("foobar.com")

func BenchmarkHeaderPeekBytesCanonical(b *testing.B) {
	var h RequestHeader
	h.SetBytesV("Host", strFoobar)
	for i := 0; i < b.N; i++ {
		v := h.PeekBytes(strHost)
		if !bytes.Equal(v, strFoobar) {
			b.Fatalf("unexpected result: %q. Expected %q", v, strFoobar)
		}
	}
}

func BenchmarkHeaderPeekBytesNonCanonical(b *testing.B) {
	var h RequestHeader
	h.SetBytesV("Host", strFoobar)
	hostBytes := []byte("HOST")
	for i := 0; i < b.N; i++ {
		v := h.PeekBytes(hostBytes)
		if !bytes.Equal(v, strFoobar) {
			b.Fatalf("unexpected result: %q. Expected %q", v, strFoobar)
		}
	}
}

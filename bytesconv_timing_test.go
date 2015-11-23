package fasthttp

import (
	"testing"
)

func BenchmarkLowercaseBytesNoop(b *testing.B) {
	src := []byte("foobarbaz_lowercased_all")
	b.RunParallel(func(pb *testing.PB) {
		s := make([]byte, len(src))
		for pb.Next() {
			copy(s, src)
			lowercaseBytes(s)
		}
	})
}

func BenchmarkLowercaseBytesAll(b *testing.B) {
	src := []byte("FOOBARBAZ_UPPERCASED_ALL")
	b.RunParallel(func(pb *testing.PB) {
		s := make([]byte, len(src))
		for pb.Next() {
			copy(s, src)
			lowercaseBytes(s)
		}
	})
}

func BenchmarkLowercaseBytesMixed(b *testing.B) {
	src := []byte("Foobarbaz_Uppercased_Mix")
	b.RunParallel(func(pb *testing.PB) {
		s := make([]byte, len(src))
		for pb.Next() {
			copy(s, src)
			lowercaseBytes(s)
		}
	})
}

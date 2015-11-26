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

func BenchmarkEqualBytesStrEq(b *testing.B) {
	s := "foobarbaraz"
	bs := []byte(s)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !EqualBytesStr(bs, s) {
				b.Fatalf("unexpected result: %q != %q", bs, s)
			}
		}
	})
}

func BenchmarkEqualBytesStrNe(b *testing.B) {
	s := "foobarbaraz"
	bs := []byte(s)
	bs[len(s)-1] = 'a'
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if EqualBytesStr(bs, s) {
				b.Fatalf("unexpected result: %q = %q", bs, s)
			}
		}
	})
}

func BenchmarkAppendBytesStr(b *testing.B) {
	s := "foobarbazbaraz"
	b.RunParallel(func(pb *testing.PB) {
		var dst []byte
		for pb.Next() {
			dst = AppendBytesStr(dst[:0], s)
		}
	})
}

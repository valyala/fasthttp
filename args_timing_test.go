package fasthttp

import (
	"bytes"
	"testing"
)

func BenchmarkArgsParse(b *testing.B) {
	s := []byte("foo=bar&baz=qqq&aaaaa=bbbb")
	b.RunParallel(func(pb *testing.PB) {
		var a Args
		for pb.Next() {
			a.ParseBytes(s)
		}
	})
}

func BenchmarkArgsParseQuery(b *testing.B) {
	s := []byte("page=2&limit=50&sort=name&order=desc&filter=active&fields=id,name,email")
	b.RunParallel(func(pb *testing.PB) {
		var a Args
		for pb.Next() {
			a.ParseBytes(s)
		}
	})
}

func BenchmarkArgsParseEncoded(b *testing.B) {
	s := []byte("q=hello+world&redirect=https%3A%2F%2Fexample.com%2Fpath&lang=en-US")
	b.RunParallel(func(pb *testing.PB) {
		var a Args
		for pb.Next() {
			a.ParseBytes(s)
		}
	})
}

func BenchmarkArgsPeek(b *testing.B) {
	value := []byte("foobarbaz1234")
	key := "foobarbaz"
	b.RunParallel(func(pb *testing.PB) {
		var a Args
		a.SetBytesV(key, value)
		for pb.Next() {
			if !bytes.Equal(a.Peek(key), value) {
				b.Fatalf("unexpected arg value %q. Expecting %q", a.Peek(key), value)
			}
		}
	})
}

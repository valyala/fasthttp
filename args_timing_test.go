package fasthttp

import (
	"testing"
)

func BenchmarkArgsParse(b *testing.B) {
	var a Args
	s := []byte("foo=bar&baz=qqq&aaaaa=bbbb")
	for i := 0; i < b.N; i++ {
		a.ParseBytes(s)
	}
}

package fasthttp

import (
	"strings"
	"testing"
)

func BenchmarkURIParsePath(b *testing.B) {
	benchmarkURIParse(b, "google.com", "/foo/bar")
}

func BenchmarkURIParsePathQueryString(b *testing.B) {
	benchmarkURIParse(b, "google.com", "/foo/bar?query=string&other=value")
}

func BenchmarkURIParsePathQueryStringHash(b *testing.B) {
	benchmarkURIParse(b, "google.com", "/foo/bar?query=string&other=value#hashstring")
}

// A request URI made of nothing but separators or dot segments normalizes to
// "/", so these measure the cost of getting there rather than the result.
func BenchmarkURIParsePathSlashRun(b *testing.B) {
	benchmarkURIParse(b, "google.com", strings.Repeat("/", 8*1024))
}

func BenchmarkURIParsePathDotSegments(b *testing.B) {
	benchmarkURIParse(b, "google.com", strings.Repeat("/a/..", 1638))
}

func BenchmarkURIParseHostname(b *testing.B) {
	benchmarkURIParse(b, "google.com", "http://foobar.com/foo/bar?query=string&other=value#hashstring")
}

func BenchmarkURIFullURI(b *testing.B) {
	host := []byte("foobar.com")
	requestURI := []byte("/foobar/baz?aaa=bbb&ccc=ddd")
	uriLen := len(host) + len(requestURI) + 7

	b.RunParallel(func(pb *testing.PB) {
		var u URI
		u.Parse(host, requestURI) //nolint:errcheck
		for pb.Next() {
			uri := u.FullURI()
			if len(uri) != uriLen {
				b.Fatalf("unexpected uri len %d. Expecting %d", len(uri), uriLen)
			}
		}
	})
}

func benchmarkURIParse(b *testing.B, host, uri string) {
	strHost, strURI := []byte(host), []byte(uri)

	b.RunParallel(func(pb *testing.PB) {
		var u URI
		for pb.Next() {
			u.Parse(strHost, strURI) //nolint:errcheck
		}
	})
}

func BenchmarkStringContainsCTLByte(b *testing.B) {
	uri := []byte("/api/v1/items?page=2&limit=50&sort=name")
	for range b.N {
		if stringContainsCTLByte(uri) {
			b.Fatal("clean uri flagged")
		}
	}
}

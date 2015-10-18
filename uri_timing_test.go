package fasthttp

import (
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

func BenchmarkURIParseHostname(b *testing.B) {
	benchmarkURIParse(b, "google.com", "http://foobar.com/foo/bar?query=string&other=value#hashstring")
}

func benchmarkURIParse(b *testing.B, host, uri string) {
	strHost, strURI := []byte(host), []byte(uri)

	b.RunParallel(func(pb *testing.PB) {
		var uri URI
		for pb.Next() {
			uri.Parse(strHost, strURI)
		}
	})
}

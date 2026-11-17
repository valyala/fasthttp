package fasthttp

import (
	"bufio"
	"io"
	"reflect"
	"strings"
	"testing"
)

type trailerValueHeader interface {
	PeekTrailer(string) []byte
	PeekAllTrailer(string) [][]byte
	Peek(string) []byte
	PeekAll(string) [][]byte
	SetTrailer(string) error
	Set(string, string)
	Add(string, string)
	Del(string)
	Reset()
}

func TestHeaderPeekTrailer(t *testing.T) {
	t.Parallel()

	var req Request
	var resp Response
	for _, tc := range []struct {
		name      string
		firstLine string
		header    trailerValueHeader
		read      func(*bufio.Reader) error
	}{
		{"request", "POST / HTTP/1.1\r\nHost: example.com\r\n", &req.Header, req.Read},
		{"response", "HTTP/1.1 200 OK\r\n", &resp.Header, resp.Read},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.firstLine +
				"X-Mixed: upfront\r\nX-Upfront: header-only\r\n" +
				"Server: upfront-server\r\nUser-Agent: upfront-agent\r\n" +
				"Transfer-Encoding: chunked\r\n\r\n0\r\n" +
				"X-Mixed: one\r\nX-Mixed: two\r\nX-Empty:\r\n" +
				"Server: trailer-server\r\nUser-Agent: trailer-agent\r\n\r\n"
			if err := tc.read(bufio.NewReader(strings.NewReader(raw))); err != nil {
				t.Fatal(err)
			}
			for _, lookup := range []struct {
				key    string
				values []string
			}{
				{"X-Mixed", []string{"one", "two"}},
				{"x-mixed", []string{"one", "two"}},
				{"X-Empty", []string{""}},
				{"Server", []string{"trailer-server"}},
				{"User-Agent", []string{"trailer-agent"}},
				{"X-Upfront", nil},
				{"X-Missing", nil},
				{"Transfer-Encoding", nil},
			} {
				t.Run(lookup.key, func(t *testing.T) {
					first := tc.header.PeekTrailer(lookup.key)
					if len(lookup.values) == 0 {
						if first != nil {
							t.Fatalf("PeekTrailer(%q) = %q, want nil", lookup.key, first)
						}
					} else if string(first) != lookup.values[0] {
						t.Fatalf("PeekTrailer(%q) = %q, want %q", lookup.key, first, lookup.values[0])
					}
					var values []string
					for _, value := range tc.header.PeekAllTrailer(lookup.key) {
						values = append(values, string(value))
					}
					if !reflect.DeepEqual(values, lookup.values) {
						t.Fatalf("PeekAllTrailer(%q) = %q, want %q", lookup.key, values, lookup.values)
					}
				})
			}
			if got := string(tc.header.Peek("X-Mixed")); got != "upfront" {
				t.Fatalf("Peek(X-Mixed) = %q, want upfront", got)
			}
			values := tc.header.PeekAll("X-Mixed")
			if len(values) != 3 || string(values[0]) != "upfront" ||
				string(values[1]) != "one" || string(values[2]) != "two" {
				t.Fatalf("PeekAll(X-Mixed) = %q, want [upfront one two]", values)
			}
		})
	}
}

func TestHeaderPeekTrailerNormalizing(t *testing.T) {
	t.Parallel()

	for _, disable := range []bool{false, true} {
		for _, h := range []interface {
			PeekTrailer(string) []byte
			PeekAllTrailer(string) [][]byte
			ReadTrailer(*bufio.Reader) error
			DisableNormalizing() bool
		}{&RequestHeader{}, &ResponseHeader{}} {
			if disable {
				h.DisableNormalizing()
			}
			err := h.ReadTrailer(bufio.NewReader(strings.NewReader(
				"mIxEd-Name: one\r\nmIxEd-Name: two\r\n\r\n")))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(h.PeekTrailer("mIxEd-Name")); got != "one" {
				t.Fatalf("PeekTrailer(mIxEd-Name) = %q, want one (normalizing disabled: %v)", got, disable)
			}
			values := h.PeekAllTrailer("mIxEd-Name")
			if len(values) != 2 || string(values[0]) != "one" || string(values[1]) != "two" {
				t.Fatalf("PeekAllTrailer(mIxEd-Name) = %q, want [one two]", values)
			}
			if disable {
				if h.PeekTrailer("mixed-name") != nil || len(h.PeekAllTrailer("mixed-name")) != 0 {
					t.Fatal("lookup normalized a key while normalizing was disabled")
				}
			} else if got := string(h.PeekTrailer("mixed-name")); got != "one" {
				t.Fatalf("PeekTrailer(mixed-name) = %q, want one", got)
			}
		}
	}
}

func TestHeaderPeekTrailerLifecycle(t *testing.T) {
	t.Parallel()

	for _, h := range []trailerValueHeader{&RequestHeader{}, &ResponseHeader{}} {
		h.Set("X-Trailer", "one")
		if h.PeekTrailer("X-Trailer") != nil || len(h.PeekAllTrailer("X-Trailer")) != 0 {
			t.Fatal("ordinary header appeared in trailer lookup")
		}
		if err := h.SetTrailer("X-Trailer"); err != nil {
			t.Fatal(err)
		}
		h.Add("X-Trailer", "two")
		values := h.PeekAllTrailer("X-Trailer")
		if string(h.PeekTrailer("X-Trailer")) != "one" ||
			len(values) != 2 || string(values[1]) != "two" {
			t.Fatalf("announced values = %q, want [one two]", values)
		}
		if err := h.SetTrailer(""); err != nil {
			t.Fatal(err)
		}
		if h.PeekTrailer("X-Trailer") != nil || len(h.PeekAllTrailer("X-Trailer")) != 0 {
			t.Fatal("unannounced value remained visible as a trailer")
		}
		if got := string(h.Peek("X-Trailer")); got != "one" {
			t.Fatalf("unannouncing changed the ordinary header: %q", got)
		}
		if err := h.SetTrailer("X-Trailer"); err != nil {
			t.Fatal(err)
		}
		h.Del("X-Trailer")
		if h.PeekTrailer("X-Trailer") != nil || len(h.PeekAllTrailer("X-Trailer")) != 0 {
			t.Fatal("deleted values remained visible")
		}
		h.Set("X-Trailer", "before-reset")
		h.Reset()
		h.Set("X-Trailer", "after-reset")
		if h.PeekTrailer("X-Trailer") != nil || len(h.PeekAllTrailer("X-Trailer")) != 0 {
			t.Fatal("Reset retained a trailer or exposed a new ordinary header")
		}
	}
}

func TestHeaderPeekTrailerStream(t *testing.T) {
	t.Parallel()

	var resp Response
	defer resp.Reset()
	resp.StreamBody = true
	raw := "HTTP/1.1 200 OK\r\nTrailer: X-Result\r\n" +
		"Transfer-Encoding: chunked\r\n\r\n" +
		"4\r\ndata\r\n0\r\nX-Result: one\r\nX-Result: two\r\n\r\n"
	if err := resp.Read(bufio.NewReader(strings.NewReader(raw))); err != nil {
		t.Fatal(err)
	}
	if resp.Header.PeekTrailer("X-Result") != nil ||
		len(resp.Header.PeekAllTrailer("X-Result")) != 0 {
		t.Fatal("received trailer values appeared before the body was read")
	}
	body, err := io.ReadAll(resp.BodyStream())
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "data" {
		t.Fatalf("body = %q, want data", body)
	}
	if got := string(resp.Header.PeekTrailer("X-Result")); got != "one" {
		t.Fatalf("PeekTrailer(X-Result) = %q, want one", got)
	}
	values := resp.Header.PeekAllTrailer("X-Result")
	if len(values) != 2 || string(values[0]) != "one" || string(values[1]) != "two" {
		t.Fatalf("PeekAllTrailer(X-Result) = %q, want [one two]", values)
	}
}

func BenchmarkHeaderPeekTrailer(b *testing.B) {
	var h ResponseHeader
	if err := h.SetTrailer("X-Trailer"); err != nil {
		b.Fatal(err)
	}
	h.Add("X-Trailer", "one")
	h.Add("X-Trailer", "two")
	b.Run("first", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			h.PeekTrailer("x-trailer")
		}
	})
	b.Run("all", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			h.PeekAllTrailer("x-trailer")
		}
	})
}

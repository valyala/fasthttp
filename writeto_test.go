package fasthttp

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func testWriteToValues() []struct {
	name string
	wt   io.WriterTo
} {
	var a Args
	a.Parse("foo=bar&baz=123&aaa=bbb")
	var c Cookie
	c.SetKey("sid")
	c.SetValue("abc123")
	var u URI
	u.SetPath("/some/path?x=1")
	var rh ResponseHeader
	rh.SetStatusCode(200)
	rh.SetContentType("text/plain")
	var qh RequestHeader
	qh.SetMethod("GET")
	qh.SetRequestURI("/some/path")

	return []struct {
		name string
		wt   io.WriterTo
	}{
		{"Args", &a},
		{"Cookie", &c},
		{"URI", &u},
		{"ResponseHeader", &rh},
		{"RequestHeader", &qh},
	}
}

func TestWriteToReportsShortWrite(t *testing.T) {
	t.Parallel()

	for _, tc := range testWriteToValues() {
		t.Run(tc.name, func(t *testing.T) {
			w := &streamBoundaryWriter{failAt: 1, n: 3}
			n, err := tc.wt.WriteTo(w)
			if n != 3 {
				t.Errorf("wrote %d bytes, want 3", n)
			}
			if err != io.ErrShortWrite {
				t.Errorf("got %v, want io.ErrShortWrite", err)
			}
			if w.calls != 1 {
				t.Errorf("destination Write called %d times, want 1", w.calls)
			}
		})
	}
}

func TestWriteToPreservesWriterError(t *testing.T) {
	t.Parallel()

	want := errors.New("write failed")
	for _, tc := range testWriteToValues() {
		t.Run(tc.name, func(t *testing.T) {
			w := &streamBoundaryWriter{failAt: 1, n: 3, err: want}
			n, err := tc.wt.WriteTo(w)
			if n != 3 {
				t.Errorf("wrote %d bytes, want 3", n)
			}
			if err != want {
				t.Errorf("got %v, want %v", err, want)
			}
			if errors.Is(err, io.ErrShortWrite) {
				t.Errorf("original writer error was replaced: %v", err)
			}
		})
	}
}

func TestWriteToFullWrite(t *testing.T) {
	t.Parallel()

	for _, tc := range testWriteToValues() {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := tc.wt.WriteTo(&buf)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n != int64(buf.Len()) {
				t.Errorf("wrote %d bytes, buffer has %d", n, buf.Len())
			}
			if buf.Len() == 0 {
				t.Error("no bytes written")
			}
		})
	}
}

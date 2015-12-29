package fasthttp

import (
	"bytes"
	"io/ioutil"
	"testing"
)

func TestGzipCompress(t *testing.T) {
	testGzipCompress(t, "")
	testGzipCompress(t, "foobar")
	testGzipCompress(t, "ajjnkn asdlkjfqoijfw  jfqkwj foj  eowjiq")
}

func TestFlateCompress(t *testing.T) {
	testFlateCompress(t, "")
	testFlateCompress(t, "foobar")
	testFlateCompress(t, "adf asd asd fasd fasd")
}

func testGzipCompress(t *testing.T, s string) {
	var buf bytes.Buffer
	zw := acquireGzipWriter(&buf, CompressDefaultCompression)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("unexpected error: %s. s=%q", err, s)
	}
	releaseGzipWriter(zw)

	zr, err := acquireGzipReader(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %s. s=%q", err, s)
	}
	body, err := ioutil.ReadAll(zr)
	if err != nil {
		t.Fatalf("unexpected error: %s. s=%q", err, s)
	}
	if string(body) != s {
		t.Fatalf("unexpected string after decompression: %q. Expecting %q", body, s)
	}
	releaseGzipReader(zr)
}

func testFlateCompress(t *testing.T, s string) {
	var buf bytes.Buffer
	zw := acquireFlateWriter(&buf, CompressDefaultCompression)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("unexpected error: %s. s=%q", err, s)
	}
	releaseFlateWriter(zw)

	zr, err := acquireFlateReader(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %s. s=%q", err, s)
	}
	body, err := ioutil.ReadAll(zr)
	if err != nil {
		t.Fatalf("unexpected error: %s. s=%q", err, s)
	}
	if string(body) != s {
		t.Fatalf("unexpected string after decompression: %q. Expecting %q", body, s)
	}
	releaseFlateReader(zr)
}

package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

var compressTestcases = func() []string {
	a := make([]string, 0, 4)
	a = append(a,
		"",
		"foobar",
		"выфаодлодл одлфываыв sd2 k34",
	)
	bigS := createFixedBody(1e4)
	a = append(a, string(bigS))
	return a
}()

func TestGzipBytesSerial(t *testing.T) {
	t.Parallel()

	if err := testGzipBytes(); err != nil {
		t.Fatal(err)
	}
}

func TestGzipBytesConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testGzipBytes); err != nil {
		t.Fatal(err)
	}
}

func TestDeflateBytesSerial(t *testing.T) {
	t.Parallel()

	if err := testDeflateBytes(); err != nil {
		t.Fatal(err)
	}
}

func TestDeflateBytesConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testDeflateBytes); err != nil {
		t.Fatal(err)
	}
}

func testGzipBytes() error {
	for _, s := range compressTestcases {
		if err := testGzipBytesSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testDeflateBytes() error {
	for _, s := range compressTestcases {
		if err := testDeflateBytesSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testGzipBytesSingleCase(s string) error {
	prefix := []byte("foobar")
	gzippedS := AppendGzipBytes(prefix, []byte(s))
	if !bytes.Equal(gzippedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when compressing %q: %q. Expecting %q", s, gzippedS[:len(prefix)], prefix)
	}

	gunzippedS, err := AppendGunzipBytes(prefix, gzippedS[len(prefix):])
	if err != nil {
		return fmt.Errorf("unexpected error when uncompressing %q: %w", s, err)
	}
	if !bytes.Equal(gunzippedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when uncompressing %q: %q. Expecting %q", s, gunzippedS[:len(prefix)], prefix)
	}
	gunzippedS = gunzippedS[len(prefix):]
	if string(gunzippedS) != s {
		return fmt.Errorf("unexpected uncompressed string %q. Expecting %q", gunzippedS, s)
	}
	return nil
}

func testDeflateBytesSingleCase(s string) error {
	prefix := []byte("foobar")
	deflatedS := AppendDeflateBytes(prefix, []byte(s))
	if !bytes.Equal(deflatedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when compressing %q: %q. Expecting %q", s, deflatedS[:len(prefix)], prefix)
	}

	inflatedS, err := AppendInflateBytes(prefix, deflatedS[len(prefix):])
	if err != nil {
		return fmt.Errorf("unexpected error when uncompressing %q: %w", s, err)
	}
	if !bytes.Equal(inflatedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when uncompressing %q: %q. Expecting %q", s, inflatedS[:len(prefix)], prefix)
	}
	inflatedS = inflatedS[len(prefix):]
	if string(inflatedS) != s {
		return fmt.Errorf("unexpected uncompressed string %q. Expecting %q", inflatedS, s)
	}
	return nil
}

func TestGzipCompressSerial(t *testing.T) {
	t.Parallel()

	if err := testGzipCompress(); err != nil {
		t.Fatal(err)
	}
}

func TestGzipCompressConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testGzipCompress); err != nil {
		t.Fatal(err)
	}
}

func TestFlateCompressSerial(t *testing.T) {
	t.Parallel()

	if err := testFlateCompress(); err != nil {
		t.Fatal(err)
	}
}

func TestFlateCompressConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testFlateCompress); err != nil {
		t.Fatal(err)
	}
}

func testGzipCompress() error {
	for _, s := range compressTestcases {
		if err := testGzipCompressSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testFlateCompress() error {
	for _, s := range compressTestcases {
		if err := testFlateCompressSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testGzipCompressSingleCase(s string) error {
	var buf bytes.Buffer
	zw := acquireStacklessGzipWriter(&buf, CompressDefaultCompression)
	if _, err := zw.Write([]byte(s)); err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	_ = releaseStacklessGzipWriter(zw, CompressDefaultCompression)

	zr, err := acquireGzipReader(&buf)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	if string(body) != s {
		return fmt.Errorf("unexpected string after decompression: %q. Expecting %q", body, s)
	}
	releaseGzipReader(zr)
	return nil
}

func testFlateCompressSingleCase(s string) error {
	var buf bytes.Buffer
	zw := acquireStacklessDeflateWriter(&buf, CompressDefaultCompression)
	if _, err := zw.Write([]byte(s)); err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	_ = releaseStacklessDeflateWriter(zw, CompressDefaultCompression)

	zr, err := acquireFlateReader(&buf)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	if string(body) != s {
		return fmt.Errorf("unexpected string after decompression: %q. Expecting %q", body, s)
	}
	releaseFlateReader(zr)
	return nil
}

func testConcurrent(concurrency int, f func() error) error {
	ch := make(chan error, concurrency)
	for i := range concurrency {
		go func(idx int) {
			err := f()
			if err != nil {
				ch <- fmt.Errorf("error in goroutine %d: %w", idx, err)
				return
			}
			ch <- nil
		}(i)
	}
	for range concurrency {
		select {
		case err := <-ch:
			if err != nil {
				return err
			}
		case <-time.After(testTimeout(time.Second)):
			return errors.New("timeout")
		}
	}
	return nil
}

// varyTokens returns the set of comma-separated tokens found in every
// 'Vary' header line.
func varyTokens(h *ResponseHeader) map[string]bool {
	tokens := make(map[string]bool)
	for _, v := range h.PeekAll("Vary") {
		for _, token := range strings.Split(string(v), ",") {
			token = strings.TrimSpace(token)
			if len(token) > 0 {
				tokens[token] = true
			}
		}
	}
	return tokens
}

func TestCompressHandlerKeepsMultipleVaryLines(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("vary header test ", 100)
	handler := CompressHandler(func(ctx *RequestCtx) {
		ctx.Response.Header.Add("Vary", "Origin")
		ctx.Response.Header.Add("Vary", "Cookie")
		ctx.SetBodyString(body)
	})

	var ctx RequestCtx
	wire := "GET / HTTP/1.1\r\nHost: example.com\r\nAccept-Encoding: gzip\r\n\r\n"
	if err := ctx.Request.Read(bufio.NewReader(strings.NewReader(wire))); err != nil {
		t.Fatal(err)
	}
	handler(&ctx)

	var response Response
	if err := response.Read(bufio.NewReader(bytes.NewBufferString(ctx.Response.String()))); err != nil {
		t.Fatal(err)
	}
	if got := string(response.Header.ContentEncoding()); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	tokens := varyTokens(&response.Header)
	for _, want := range []string{"Origin", "Cookie", "Accept-Encoding"} {
		if !tokens[want] {
			t.Fatalf("Vary tokens = %v, missing %q", tokens, want)
		}
	}
	decoded, err := response.BodyGunzip()
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != body {
		t.Fatal("decompressed response differs from the original body")
	}
}

func TestFSServeFileCompressedKeepsMultipleVaryLines(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)
	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip")
	ctx.Response.Header.Add("Vary", "Origin")
	ctx.Response.Header.Add("Vary", "Cookie")
	ServeFS(&ctx, fsTestFilesystem, "fs.go")

	var resp Response
	if err := resp.Read(bufio.NewReader(bytes.NewBufferString(ctx.Response.String()))); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
	if got := string(resp.Header.ContentEncoding()); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	tokens := varyTokens(&resp.Header)
	for _, want := range []string{"Origin", "Cookie", "Accept-Encoding"} {
		if !tokens[want] {
			t.Fatalf("Vary tokens = %v, missing %q", tokens, want)
		}
	}
}

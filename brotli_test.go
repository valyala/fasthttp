package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

func TestBrotliBytesSerial(t *testing.T) {
	t.Parallel()

	if err := testBrotliBytes(); err != nil {
		t.Fatal(err)
	}
}

func TestBrotliBytesConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testBrotliBytes); err != nil {
		t.Fatal(err)
	}
}

func testBrotliBytes() error {
	for _, s := range compressTestcases {
		if err := testBrotliBytesSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testBrotliBytesSingleCase(s string) error {
	prefix := []byte("foobar")
	brotlipedS := AppendBrotliBytes(prefix, []byte(s))
	if !bytes.Equal(brotlipedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when compressing %q: %q. Expecting %q", s, brotlipedS[:len(prefix)], prefix)
	}

	unbrotliedS, err := AppendUnbrotliBytes(prefix, brotlipedS[len(prefix):])
	if err != nil {
		return fmt.Errorf("unexpected error when uncompressing %q: %w", s, err)
	}
	if !bytes.Equal(unbrotliedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when uncompressing %q: %q. Expecting %q", s, unbrotliedS[:len(prefix)], prefix)
	}
	unbrotliedS = unbrotliedS[len(prefix):]
	if string(unbrotliedS) != s {
		return fmt.Errorf("unexpected uncompressed string %q. Expecting %q", unbrotliedS, s)
	}
	return nil
}

func TestBrotliCompressSerial(t *testing.T) {
	t.Parallel()

	if err := testBrotliCompress(); err != nil {
		t.Fatal(err)
	}
}

func TestBrotliCompressConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testBrotliCompress); err != nil {
		t.Fatal(err)
	}
}

func testBrotliCompress() error {
	for _, s := range compressTestcases {
		if err := testBrotliCompressSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testBrotliCompressSingleCase(s string) error {
	var buf bytes.Buffer
	zw := acquireStacklessBrotliWriter(&buf, CompressDefaultCompression)
	if _, err := zw.Write([]byte(s)); err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	releaseStacklessBrotliWriter(zw, CompressDefaultCompression)

	zr := acquireBrotliReader(&buf)
	body, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	if string(body) != s {
		return fmt.Errorf("unexpected string after decompression: %q. Expecting %q", body, s)
	}
	releaseBrotliReader(zr)
	return nil
}

func TestCompressHandlerBrotliLevel(t *testing.T) {
	t.Parallel()

	expectedBody := createFixedBody(2e4)
	h := CompressHandlerBrotliLevel(func(ctx *RequestCtx) {
		ctx.Write(expectedBody) //nolint:errcheck
	}, CompressBrotliDefaultCompression, CompressDefaultCompression)

	var ctx RequestCtx
	var resp Response

	// verify uncompressed response
	h(&ctx)
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ce := resp.Header.ContentEncoding()
	if len(ce) != 0 {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "")
	}
	body := resp.Body()
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}

	// verify gzip-compressed response
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.Request.Header.Set("Accept-Encoding", "gzip, deflate, sdhc")

	h(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ce = resp.Header.ContentEncoding()
	if string(ce) != "gzip" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "gzip")
	}
	body, err := resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}

	// verify brotli-compressed response
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.Request.Header.Set("Accept-Encoding", "gzip, deflate, sdhc, br")

	h(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ce = resp.Header.ContentEncoding()
	if string(ce) != "br" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "br")
	}
	body, err = resp.BodyUnbrotli()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}
}

func TestBrotliExcessiveInput(t *testing.T) {
	t.Parallel()

	for _, payload := range [][]byte{
		nil,
		createFixedBody(1),
		createFixedBody(1e3),
		createFixedBody(1e5),
		createIncompressibleBody(1e5),
	} {
		size := len(payload)
		for _, level := range []int{
			CompressBrotliNoCompression,
			CompressBrotliBestSpeed,
			CompressBrotliDefaultCompression,
			CompressBrotliBestCompression,
		} {
			compressed := AppendBrotliBytesLevel(nil, payload, level)

			unbrotlied, err := AppendUnbrotliBytes(nil, compressed)
			if err != nil {
				t.Fatalf("unexpected error for a clean stream: %v. size=%d, level=%d", err, size, level)
			}
			if !bytes.Equal(unbrotlied, payload) {
				t.Fatalf("unexpected uncompressed body of %d bytes. Expecting %d bytes. size=%d, level=%d",
					len(unbrotlied), size, size, level)
			}

			for _, trailing := range [][]byte{
				{0xde},
				{0xde, 0xad, 0xbe, 0xef},
				createFixedBody(1e5),
			} {
				excessive := append(append([]byte(nil), compressed...), trailing...)
				testBrotliExcessiveInputCase(t, excessive, size, level, len(trailing))
			}
		}
	}
}

func testBrotliExcessiveInputCase(t *testing.T, excessive []byte, size, level, trailing int) {
	t.Helper()

	fail := func(api string) {
		t.Errorf("%s accepted %d trailing bytes. size=%d, level=%d", api, trailing, size, level)
	}

	if _, err := AppendUnbrotliBytes(nil, excessive); err == nil {
		fail("AppendUnbrotliBytes")
	}

	var buf bytes.Buffer
	if _, err := WriteUnbrotli(&buf, excessive); err == nil {
		fail("WriteUnbrotli")
	}

	var req Request
	req.Header.SetContentEncoding("br")
	req.SetBody(excessive)
	if _, err := req.BodyUnbrotli(); err == nil {
		fail("Request.BodyUnbrotli")
	}
	if _, err := req.BodyUncompressed(); err == nil {
		fail("Request.BodyUncompressed")
	}

	var resp Response
	resp.Header.SetContentEncoding("br")
	resp.SetBody(excessive)
	if _, err := resp.BodyUnbrotli(); err == nil {
		fail("Response.BodyUnbrotli")
	}
	if _, err := resp.BodyUncompressed(); err == nil {
		fail("Response.BodyUncompressed")
	}
}

func TestBrotliTruncatedInput(t *testing.T) {
	t.Parallel()

	compressed := AppendBrotliBytes(nil, createFixedBody(1e3))
	for _, truncated := range [][]byte{
		nil,
		compressed[:1],
		compressed[:len(compressed)/2],
		compressed[:len(compressed)-1],
	} {
		if _, err := AppendUnbrotliBytes(nil, truncated); err == nil {
			t.Errorf("AppendUnbrotliBytes accepted a stream truncated to %d of %d bytes",
				len(truncated), len(compressed))
		}
	}
}

func TestBrotliExcessiveInputBodyTooLarge(t *testing.T) {
	t.Parallel()

	compressed := AppendBrotliBytes(nil, createFixedBody(1e4))
	excessive := append(append([]byte(nil), compressed...), 0xde, 0xad, 0xbe, 0xef)

	var resp Response
	resp.SetBody(excessive)
	if _, err := resp.BodyUnbrotliWithLimit(1e3); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("unexpected error: %v. Expecting %v", err, ErrBodyTooLarge)
	}
}

func createIncompressibleBody(bodySize int) []byte {
	b := make([]byte, bodySize)
	rng := rand.New(rand.NewSource(42))
	rng.Read(b)
	return b
}

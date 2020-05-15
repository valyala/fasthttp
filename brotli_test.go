package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
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
		return fmt.Errorf("unexpected error when uncompressing %q: %s", s, err)
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
		return fmt.Errorf("unexpected error: %s. s=%q", err, s)
	}
	releaseStacklessBrotliWriter(zw, CompressDefaultCompression)

	zr, err := acquireBrotliReader(&buf)
	if err != nil {
		return fmt.Errorf("unexpected error: %s. s=%q", err, s)
	}
	body, err := ioutil.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("unexpected error: %s. s=%q", err, s)
	}
	if string(body) != s {
		return fmt.Errorf("unexpected string after decompression: %q. Expecting %q", body, s)
	}
	releaseBrotliReader(zr)
	return nil
}

func TestCompressHandlerBrotliLevel(t *testing.T) {
	t.Parallel()

	expectedBody := string(createFixedBody(2e4))
	h := CompressHandlerBrotliLevel(func(ctx *RequestCtx) {
		ctx.Write([]byte(expectedBody)) //nolint:errcheck
	}, CompressBrotliDefaultCompression, CompressDefaultCompression)

	var ctx RequestCtx
	var resp Response

	// verify uncompressed response
	h(&ctx)
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce := resp.Header.Peek(HeaderContentEncoding)
	if string(ce) != "" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "")
	}
	body := resp.Body()
	if string(body) != expectedBody {
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
		t.Fatalf("unexpected error: %s", err)
	}
	ce = resp.Header.Peek(HeaderContentEncoding)
	if string(ce) != "gzip" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "gzip")
	}
	body, err := resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != expectedBody {
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
		t.Fatalf("unexpected error: %s", err)
	}
	ce = resp.Header.Peek(HeaderContentEncoding)
	if string(ce) != "br" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "br")
	}
	body, err = resp.BodyUnbrotli()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}
}

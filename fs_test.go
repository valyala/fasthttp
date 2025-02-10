package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

type TestLogger struct {
	t *testing.T
}

func (t TestLogger) Printf(format string, args ...any) {
	t.t.Logf(format, args...)
}

func TestNewVHostPathRewriter(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.Header.SetHost("foobar.com")
	req.SetRequestURI("/foo/bar/baz")
	ctx.Init(&req, nil, nil)

	f := NewVHostPathRewriter(0)
	path := f(&ctx)
	expectedPath := "/foobar.com/foo/bar/baz"
	if string(path) != expectedPath {
		t.Fatalf("unexpected path %q. Expecting %q", path, expectedPath)
	}

	ctx.Request.Reset()
	ctx.Request.SetRequestURI("https://aaa.bbb.cc/one/two/three/four?asdf=dsf")
	f = NewVHostPathRewriter(2)
	path = f(&ctx)
	expectedPath = "/aaa.bbb.cc/three/four"
	if string(path) != expectedPath {
		t.Fatalf("unexpected path %q. Expecting %q", path, expectedPath)
	}
}

func TestNewVHostPathRewriterMaliciousHost(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.Header.SetHost("/../../../etc/passwd")
	req.SetRequestURI("/foo/bar/baz")
	ctx.Init(&req, nil, nil)

	f := NewVHostPathRewriter(0)
	path := f(&ctx)
	expectedPath := "/invalid-host/"
	if string(path) != expectedPath {
		t.Fatalf("unexpected path %q. Expecting %q", path, expectedPath)
	}
}

func testPathNotFound(t *testing.T, pathNotFoundFunc RequestHandler) {
	t.Helper()

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http//some.url/file")
	ctx.Init(&req, nil, TestLogger{t: t})

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		Root:         "./",
		PathNotFound: pathNotFoundFunc,
		CleanStop:    stop,
	}
	fs.NewRequestHandler()(&ctx)

	if pathNotFoundFunc == nil {
		// different to ...
		if !bytes.Equal(ctx.Response.Body(),
			[]byte("Cannot open requested path")) {
			t.Fatalf("response defers. Response: %q", ctx.Response.Body())
		}
	} else {
		// Equals to ...
		if bytes.Equal(ctx.Response.Body(),
			[]byte("Cannot open requested path")) {
			t.Fatalf("response defers. Response: %q", ctx.Response.Body())
		}
	}
}

func TestPathNotFound(t *testing.T) {
	t.Parallel()

	testPathNotFound(t, nil)
}

func TestPathNotFoundFunc(t *testing.T) {
	t.Parallel()

	testPathNotFound(t, func(ctx *RequestCtx) {
		ctx.WriteString("Not found hehe") //nolint:errcheck
	})
}

func TestServeFileHead(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	var ctx RequestCtx
	var req Request
	req.Header.SetMethod(MethodHead)
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFile(&ctx, "fs.go")

	var resp Response
	resp.SkipBody = true
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ce := resp.Header.ContentEncoding()
	if len(ce) > 0 {
		t.Fatalf("Unexpected 'Content-Encoding' %q", ce)
	}

	body := resp.Body()
	if len(body) > 0 {
		t.Fatalf("unexpected response body %q. Expecting empty body", body)
	}

	expectedBody, err := getFileContents("/fs.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	contentLength := resp.Header.ContentLength()
	if contentLength != len(expectedBody) {
		t.Fatalf("unexpected Content-Length: %d. expecting %d", contentLength, len(expectedBody))
	}
}

func TestServeFileSmallNoReadFrom(t *testing.T) {
	t.Parallel()

	expectedStr := "hello, world!"
	tempFile := filepath.Join(t.TempDir(), "hello")

	if err := os.WriteFile(tempFile, []byte(expectedStr), 0o666); err != nil {
		t.Fatal(err)
	}

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFile(&ctx, tempFile)

	reader, ok := ctx.Response.bodyStream.(*fsSmallFileReader)
	if !ok {
		t.Fatal("expected fsSmallFileReader")
	}
	defer reader.ff.Release()

	buf := bytes.NewBuffer(nil)

	n, err := reader.WriteTo(pureWriter{w: buf})
	if err != nil {
		t.Fatal(err)
	}

	if n != int64(len(expectedStr)) {
		t.Fatalf("expected %d bytes, got %d bytes", len(expectedStr), n)
	}

	body := buf.String()
	if body != expectedStr {
		t.Fatalf("expected '%q'", expectedStr)
	}
}

type pureWriter struct {
	w io.Writer
}

func (pw pureWriter) Write(p []byte) (nn int, err error) {
	return pw.w.Write(p)
}

func TestServeFileCompressed(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)

	var resp Response

	expectedBody, err := getFileContents("/fs.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// should prefer brotli over zstd, gzip and ignore unknown encoding
	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, zstd, br, wompwomp")
	ServeFile(&ctx, "fs.go")

	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err = resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	ce := resp.Header.ContentEncoding()
	if string(ce) != "br" {
		t.Fatalf("unexpected 'Content-Encoding': %q. Expecting %q", string(ce), "br")
	}

	vary := resp.Header.PeekBytes(strVary)
	if !bytes.Equal(vary, strAcceptEncoding) {
		t.Fatalf("unexpected 'Vary': %q. Expecting %q", string(vary), HeaderAcceptEncoding)
	}

	body, err := resp.BodyUnbrotli()
	if err != nil {
		t.Fatalf("unexpected error on unbrotli response body: %v", err)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body: len=%d. Expecting len=%d", len(body), len(expectedBody))
	}

	// should prefer zstd over gzip and ignore unknown encoding
	ctx.Request.Reset()
	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, zstd, wompwomp")
	ServeFile(&ctx, "fs.go")

	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err = resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	ce = resp.Header.ContentEncoding()
	if string(ce) != "zstd" {
		t.Fatalf("unexpected 'Content-Encoding' %q. Expecting %q", string(ce), "zstd")
	}

	vary = resp.Header.PeekBytes(strVary)
	if !bytes.Equal(vary, strAcceptEncoding) {
		t.Fatalf("unexpected 'Vary': %q. Expecting %q", string(vary), HeaderAcceptEncoding)
	}

	body, err = resp.BodyUnzstd()
	if err != nil {
		t.Fatalf("unexpected error on unzstd response body: %v", err)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body: len=%d. Expecting len=%d", len(body), len(expectedBody))
	}

	// should prefer gzip and ignore unknown encoding
	ctx.Request.Reset()
	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, wompwomp")
	ServeFile(&ctx, "fs.go")

	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err = resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	ce = resp.Header.ContentEncoding()
	if string(ce) != "gzip" {
		t.Fatalf("unexpected 'Content-Encoding' %q. Expecting %q", string(ce), "gzip")
	}

	vary = resp.Header.PeekBytes(strVary)
	if !bytes.Equal(vary, strAcceptEncoding) {
		t.Fatalf("unexpected 'Vary': %q. Expecting %q", string(vary), HeaderAcceptEncoding)
	}

	body, err = resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error on gunzip response body: %v", err)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body: len=%d. Expecting len=%d", len(body), len(expectedBody))
	}
}

func TestServeFileUncompressed(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)

	var resp Response

	expectedBody, err := getFileContents("/fs.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, zstd, br, wompwomp")
	ServeFileUncompressed(&ctx, "fs.go")

	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err = resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	ce := resp.Header.ContentEncoding()
	if len(ce) > 0 {
		t.Fatalf("unexpected 'Content-Encoding': %q. Expecting \"\"", string(ce))
	}

	vary := resp.Header.PeekBytes(strVary)
	if len(vary) > 0 {
		t.Fatalf("unexpected 'Vary': %q. Expecting \"\"", string(vary))
	}

	body := resp.Body()
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body: len=%d. Expecting len=%d", len(body), len(expectedBody))
	}
}

func TestFSByteRangeConcurrent(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSByteRangeConcurrent(t, &FS{
		Root:            ".",
		AcceptByteRange: true,
		CleanStop:       stop,
	})
}

func TestFSByteRangeConcurrentSkipCache(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSByteRangeConcurrent(t, &FS{
		Root:            ".",
		SkipCache:       true,
		AcceptByteRange: true,
		CleanStop:       stop,
	})
}

func runFSByteRangeConcurrent(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandler()

	concurrency := 10
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			for j := 0; j < 5; j++ {
				testFSByteRange(t, h, "/fs.go")
				testFSByteRange(t, h, "/README.md")
			}
			ch <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		case <-ch:
		}
	}
}

func TestFSByteRangeSingleThread(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSByteRangeSingleThread(t, &FS{
		Root:            ".",
		AcceptByteRange: true,
		CleanStop:       stop,
	})
}

func TestFSByteRangeSingleThreadSkipCache(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSByteRangeSingleThread(t, &FS{
		Root:            ".",
		AcceptByteRange: true,
		SkipCache:       true,
		CleanStop:       stop,
	})
}

func runFSByteRangeSingleThread(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandler()

	testFSByteRange(t, h, "/fs.go")
	testFSByteRange(t, h, "/README.md")
}

func testFSByteRange(t *testing.T, h RequestHandler, filePath string) {
	t.Helper()

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)

	expectedBody, err := getFileContents(filePath)
	if err != nil {
		t.Fatalf("cannot read file %q: %v", filePath, err)
	}

	fileSize := len(expectedBody)
	startPos := rand.Intn(fileSize)
	endPos := rand.Intn(fileSize)
	if endPos < startPos {
		startPos, endPos = endPos, startPos
	}

	ctx.Request.SetRequestURI(filePath)
	ctx.Request.Header.SetByteRange(startPos, endPos)
	h(&ctx)

	var resp Response
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v. filePath=%q", err, filePath)
	}
	if resp.StatusCode() != StatusPartialContent {
		t.Fatalf("unexpected status code: %d. Expecting %d. filePath=%q", resp.StatusCode(), StatusPartialContent, filePath)
	}
	cr := resp.Header.Peek(HeaderContentRange)

	expectedCR := fmt.Sprintf("bytes %d-%d/%d", startPos, endPos, fileSize)
	if string(cr) != expectedCR {
		t.Fatalf("unexpected content-range %q. Expecting %q. filePath=%q", cr, expectedCR, filePath)
	}
	body := resp.Body()
	bodySize := endPos - startPos + 1
	if len(body) != bodySize {
		t.Fatalf("unexpected body size %d. Expecting %d. filePath=%q, startPos=%d, endPos=%d",
			len(body), bodySize, filePath, startPos, endPos)
	}

	expectedBody = expectedBody[startPos : endPos+1]
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body %q. Expecting %q. filePath=%q, startPos=%d, endPos=%d",
			body, expectedBody, filePath, startPos, endPos)
	}
}

func getFileContents(path string) ([]byte, error) {
	path = "." + path
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func TestParseByteRangeSuccess(t *testing.T) {
	t.Parallel()

	testParseByteRangeSuccess(t, "bytes=0-0", 1, 0, 0)
	testParseByteRangeSuccess(t, "bytes=1234-6789", 6790, 1234, 6789)

	testParseByteRangeSuccess(t, "bytes=123-", 456, 123, 455)
	testParseByteRangeSuccess(t, "bytes=-1", 1, 0, 0)
	testParseByteRangeSuccess(t, "bytes=-123", 456, 333, 455)

	// End position exceeding content-length. It should be updated to content-length-1.
	// See https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35
	testParseByteRangeSuccess(t, "bytes=1-2345", 234, 1, 233)
	testParseByteRangeSuccess(t, "bytes=0-2345", 2345, 0, 2344)

	// Start position overflow. Whole range must be returned.
	// See https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35
	testParseByteRangeSuccess(t, "bytes=-567", 56, 0, 55)
}

func testParseByteRangeSuccess(t *testing.T, v string, contentLength, startPos, endPos int) {
	t.Helper()

	startPos1, endPos1, err := ParseByteRange([]byte(v), contentLength)
	if err != nil {
		t.Fatalf("unexpected error: %v. v=%q, contentLength=%d", err, v, contentLength)
	}
	if startPos1 != startPos {
		t.Fatalf("unexpected startPos=%d. Expecting %d. v=%q, contentLength=%d", startPos1, startPos, v, contentLength)
	}
	if endPos1 != endPos {
		t.Fatalf("unexpected endPos=%d. Expecting %d. v=%q, contentLength=%d", endPos1, endPos, v, contentLength)
	}
}

func TestParseByteRangeError(t *testing.T) {
	t.Parallel()

	// invalid value
	testParseByteRangeError(t, "asdfasdfas", 1234)

	// invalid units
	testParseByteRangeError(t, "foobar=1-34", 600)

	// missing '-'
	testParseByteRangeError(t, "bytes=1234", 1235)

	// non-numeric range
	testParseByteRangeError(t, "bytes=foobar", 123)
	testParseByteRangeError(t, "bytes=1-foobar", 123)
	testParseByteRangeError(t, "bytes=df-344", 545)

	// multiple byte ranges
	testParseByteRangeError(t, "bytes=1-2,4-6", 123)

	// byte range exceeding contentLength
	testParseByteRangeError(t, "bytes=123-", 12)

	// startPos exceeding endPos
	testParseByteRangeError(t, "bytes=123-34", 1234)
}

func testParseByteRangeError(t *testing.T, v string, contentLength int) {
	t.Helper()

	_, _, err := ParseByteRange([]byte(v), contentLength)
	if err == nil {
		t.Fatalf("expecting error when parsing byte range %q", v)
	}
}

func TestFSCompressConcurrent(t *testing.T) {
	// Don't run this test on Windows, the Windows GitHub actions are too slow and timeout too often.
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	// This test can't run parallel as files in / might be changed by other tests.
	stop := make(chan struct{})
	defer close(stop)

	runFSCompressConcurrent(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		CleanStop:          stop,
	})
}

func TestFSCompressConcurrentSkipCache(t *testing.T) {
	// Don't run this test on Windows, the Windows GitHub actions are too slow and timeout too often.
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	// This test can't run parallel as files in / might be changed by other tests.
	stop := make(chan struct{})
	defer close(stop)

	runFSCompressConcurrent(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		SkipCache:          true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		CleanStop:          stop,
	})
}

func runFSCompressConcurrent(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandler()

	concurrency := 4
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			for j := 0; j < 5; j++ {
				testFSCompress(t, h, "/fs.go")
				testFSCompress(t, h, "/examples/")
				testFSCompress(t, h, "/README.md")
			}
			ch <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second * 2):
			t.Fatalf("timeout")
		}
	}
}

func TestFSCompressSingleThread(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSCompressSingleThread(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		CleanStop:          stop,
	})
}

func TestFSCompressSingleThreadSkipCache(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	stop := make(chan struct{})
	defer close(stop)

	runFSCompressSingleThread(t, &FS{
		Root:               ".",
		GenerateIndexPages: true,
		SkipCache:          true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		CleanStop:          stop,
	})
}

func runFSCompressSingleThread(t *testing.T, fs *FS) {
	t.Helper()

	h := fs.NewRequestHandler()

	testFSCompress(t, h, "/fs.go")
	testFSCompress(t, h, "/")
	testFSCompress(t, h, "/README.md")
}

func testFSCompress(t *testing.T, h RequestHandler, filePath string) {
	t.Helper()

	// File locking is flaky on Windows.
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, nil)

	var resp Response

	// get uncompressed
	ctx.Request.SetRequestURI(filePath)
	h(&ctx)

	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	ce := resp.Header.ContentEncoding()
	if len(ce) > 0 {
		t.Fatalf("unexpected 'Content-Encoding': %q. Expecting \"\"", string(ce))
	}

	vary := resp.Header.PeekBytes(strVary)
	if len(vary) > 0 {
		t.Fatalf("unexpected 'Vary': %q. Expecting \"\"", string(vary))
	}

	expectedBody := bytes.Clone(resp.Body())

	// should prefer brotli over zstd, gzip and ignore unknown encoding
	ctx.Request.Reset()
	ctx.Request.SetRequestURI(filePath)
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, zstd, br, wompwomp")
	h(&ctx)

	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v. filePath=%q", err, filePath)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d. filePath=%q", resp.StatusCode(), StatusOK, filePath)
	}

	ce = resp.Header.ContentEncoding()
	if string(ce) != "br" {
		t.Fatalf("unexpected 'Content-Encoding': %q. Expecting %q. filePath=%q", string(ce), "br", filePath)
	}

	vary = resp.Header.PeekBytes(strVary)
	if !bytes.Equal(vary, strAcceptEncoding) {
		t.Fatalf("unexpected 'Vary': %q. Expecting %q", string(vary), HeaderAcceptEncoding)
	}

	body, err := resp.BodyUnbrotli()
	if err != nil {
		t.Fatalf("unexpected error on unbrotli response body: %v. filePath=%q", err, filePath)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body: len=%d. Expecting len=%d. filePath=%q", len(body), len(expectedBody), filePath)
	}

	// should prefer zstd over gzip and ignore unknown encoding
	ctx.Request.Reset()
	ctx.Request.SetRequestURI(filePath)
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, zstd, wompwomp")
	h(&ctx)

	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err = resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v. filePath=%q", err, filePath)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d. filePath=%q", resp.StatusCode(), StatusOK, filePath)
	}

	ce = resp.Header.ContentEncoding()
	if string(ce) != "zstd" {
		t.Fatalf("unexpected 'Content-Encoding': %q. Expecting %q. filePath=%q", string(ce), "zstd", filePath)
	}

	vary = resp.Header.PeekBytes(strVary)
	if !bytes.Equal(vary, strAcceptEncoding) {
		t.Fatalf("unexpected 'Vary': %q. Expecting %q", string(vary), HeaderAcceptEncoding)
	}

	body, err = resp.BodyUnzstd()
	if err != nil {
		t.Fatalf("unexpected error on unzstd response body: %v. filePath=%q", err, filePath)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body: len=%d. Expecting len=%d. filePath=%q", len(body), len(expectedBody), filePath)
	}

	// should prefer gzip and ignore unknown encoding
	ctx.Request.Reset()
	ctx.Request.SetRequestURI(filePath)
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, wompwomp")
	h(&ctx)

	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err = resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v. filePath=%q", err, filePath)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d. filePath=%q", resp.StatusCode(), StatusOK, filePath)
	}

	ce = resp.Header.ContentEncoding()
	if string(ce) != "gzip" {
		t.Fatalf("unexpected 'Content-Encoding': %q. Expecting %q. filePath=%q", string(ce), "gzip", filePath)
	}

	vary = resp.Header.PeekBytes(strVary)
	if !bytes.Equal(vary, strAcceptEncoding) {
		t.Fatalf("unexpected 'Vary': %q. Expecting %q", string(vary), HeaderAcceptEncoding)
	}

	body, err = resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error on gunzip response body: %v. filePath=%q", err, filePath)
	}
	if !bytes.Equal(body, expectedBody) {
		t.Fatalf("unexpected body: len=%d. Expecting len=%d. filePath=%q", len(body), len(expectedBody), filePath)
	}
}

func TestFSHandlerSingleThread(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	requestHandler := FSHandler(".", 0)

	f, err := os.Open(".")
	if err != nil {
		t.Fatalf("cannot open cwd: %v", err)
	}

	filenames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		t.Fatalf("cannot read dirnames in cwd: %v", err)
	}
	sort.Strings(filenames)

	for i := 0; i < 3; i++ {
		fsHandlerTest(t, requestHandler, filenames)
	}
}

func TestFSHandlerConcurrent(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	requestHandler := FSHandler(".", 0)

	f, err := os.Open(".")
	if err != nil {
		t.Fatalf("cannot open cwd: %v", err)
	}

	filenames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		t.Fatalf("cannot read dirnames in cwd: %v", err)
	}
	sort.Strings(filenames)

	concurrency := 10
	ch := make(chan struct{}, concurrency)
	for j := 0; j < concurrency; j++ {
		go func() {
			for i := 0; i < 3; i++ {
				fsHandlerTest(t, requestHandler, filenames)
			}
			ch <- struct{}{}
		}()
	}

	for j := 0; j < concurrency; j++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}
}

func fsHandlerTest(t *testing.T, requestHandler RequestHandler, filenames []string) {
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)
	ctx.Request.Header.SetHost("foobar.com")

	filesTested := 0
	for _, name := range filenames {
		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("cannot open file %q: %v", name, err)
		}
		stat, err := f.Stat()
		if err != nil {
			t.Fatalf("cannot get file stat %q: %v", name, err)
		}
		if stat.IsDir() {
			f.Close()
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("cannot read file contents %q: %v", name, err)
		}

		ctx.URI().Update(name)
		requestHandler(&ctx)
		if ctx.Response.bodyStream == nil {
			t.Fatalf("response body stream must be non-empty")
		}
		body, err := io.ReadAll(ctx.Response.bodyStream)
		if err != nil {
			t.Fatalf("error when reading response body stream: %v", err)
		}
		if !bytes.Equal(body, data) {
			t.Fatalf("unexpected body returned: %q. Expecting %q", body, data)
		}
		filesTested++
		if filesTested >= 10 {
			break
		}
	}

	// verify index page generation
	ctx.URI().Update("/")
	requestHandler(&ctx)
	if ctx.Response.bodyStream == nil {
		t.Fatalf("response body stream must be non-empty")
	}
	body, err := io.ReadAll(ctx.Response.bodyStream)
	if err != nil {
		t.Fatalf("error when reading response body stream: %v", err)
	}
	if len(body) == 0 {
		t.Fatalf("index page must be non-empty")
	}
}

func TestStripPathSlashes(t *testing.T) {
	t.Parallel()

	testStripPathSlashes(t, "", 0, "")
	testStripPathSlashes(t, "", 10, "")
	testStripPathSlashes(t, "/", 0, "")
	testStripPathSlashes(t, "/", 1, "")
	testStripPathSlashes(t, "/", 10, "")
	testStripPathSlashes(t, "/foo/bar/baz", 0, "/foo/bar/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 1, "/bar/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 2, "/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 3, "")
	testStripPathSlashes(t, "/foo/bar/baz", 10, "")

	// trailing slash
	testStripPathSlashes(t, "/foo/bar/", 0, "/foo/bar")
	testStripPathSlashes(t, "/foo/bar/", 1, "/bar")
	testStripPathSlashes(t, "/foo/bar/", 2, "")
	testStripPathSlashes(t, "/foo/bar/", 3, "")
}

func testStripPathSlashes(t *testing.T, path string, stripSlashes int, expectedPath string) {
	t.Helper()

	s := stripLeadingSlashes([]byte(path), stripSlashes)
	s = stripTrailingSlashes(s)
	if string(s) != expectedPath {
		t.Fatalf("unexpected path after stripping %q with stripSlashes=%d: %q. Expecting %q", path, stripSlashes, s, expectedPath)
	}
}

func TestFileExtension(t *testing.T) {
	t.Parallel()

	testFileExtension(t, "foo.bar", false, "zzz", ".bar")
	testFileExtension(t, "foobar", false, "zzz", "")
	testFileExtension(t, "foo.bar.baz", false, "zzz", ".baz")
	testFileExtension(t, "", false, "zzz", "")
	testFileExtension(t, "/a/b/c.d/efg.jpg", false, ".zzz", ".jpg")

	testFileExtension(t, "foo.bar", true, ".zzz", ".bar")
	testFileExtension(t, "foobar.zzz", true, ".zzz", "")
	testFileExtension(t, "foo.bar.baz.fasthttp.gz", true, ".fasthttp.gz", ".baz")
	testFileExtension(t, "", true, ".zzz", "")
	testFileExtension(t, "/a/b/c.d/efg.jpg.xxx", true, ".xxx", ".jpg")
}

func testFileExtension(t *testing.T, path string, compressed bool, compressedFileSuffix, expectedExt string) {
	t.Helper()

	ext := fileExtension(path, compressed, compressedFileSuffix)
	if ext != expectedExt {
		t.Fatalf("unexpected file extension for file %q: %q. Expecting %q", path, ext, expectedExt)
	}
}

func TestServeFileContentType(t *testing.T) {
	// This test can't run parallel as files in / might be changed by other tests.

	var ctx RequestCtx
	var req Request
	req.Header.SetMethod(MethodGet)
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFile(&ctx, "testdata/test.png")

	var resp Response
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []byte("image/png")
	if !bytes.Equal(resp.Header.ContentType(), expected) {
		t.Fatalf("Unexpected Content-Type, expected: %q got %q", expected, resp.Header.ContentType())
	}
}

func TestServeFileDirectoryRedirect(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http://foobar.com")
	ctx.Init(&req, nil, nil)

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFile(&ctx, "fasthttputil")
	if ctx.Response.StatusCode() != StatusFound {
		t.Fatalf("Unexpected status code %d for directory '/fasthttputil' without trailing slash. Expecting %d.", ctx.Response.StatusCode(), StatusFound)
	}

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFile(&ctx, "fasthttputil/")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("Unexpected status code %d for directory '/fasthttputil/' with trailing slash. Expecting %d.", ctx.Response.StatusCode(), StatusOK)
	}

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFile(&ctx, "fs.go")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("Unexpected status code %d for file '/fs.go'. Expecting %d.", ctx.Response.StatusCode(), StatusOK)
	}
}

func TestFileCacheForZstd(t *testing.T) {
	f, err := os.CreateTemp(os.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("1"), 1000)
	changedData := bytes.Repeat([]byte("2"), 1000)
	_, err = f.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Sync()
	if err != nil {
		t.Fatal(err)
	}
	fs := FS{Root: os.TempDir(), Compress: true, CompressZstd: true, CacheDuration: time.Second * 60}
	h := fs.NewRequestHandler()
	var ctx RequestCtx
	var req Request
	req.Header.Set("Accept-Encoding", "zstd")
	req.SetRequestURI("http://foobar.com/" + strings.TrimPrefix(f.Name(), os.TempDir()))
	ctx.Init(&req, nil, nil)
	h(&ctx)
	if !bytes.Equal(ctx.Response.Header.ContentEncoding(), []byte("zstd")) {
		t.Fatalf("Unexpected 'Content-Encoding' %q. Expecting %q", ctx.Response.Header.ContentEncoding(), "zstd")
	}
	ctx.Response.Reset()
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(changedData)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	h(&ctx)
	if !bytes.Equal(ctx.Response.Header.ContentEncoding(), []byte("zstd")) {
		t.Fatalf("Unexpected 'Content-Encoding' %q. Expecting %q", ctx.Response.Header.ContentEncoding(), "zstd")
	}
	d, err := acquireZstdReader(strings.NewReader(string(ctx.Response.Body())))
	if err != nil {
		t.Fatalf("invalid zstd reader")
	}
	plainText, err := io.ReadAll(d)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plainText, data) {
		t.Fatalf("Unexpected response body %q. Expecting %q . Zstd cache doesn't work", plainText, data)
	}
	ctx.Request.Header.Del("Accept-Encoding")
	ctx.Response.Reset()
	h(&ctx)
	if !bytes.Equal(ctx.Response.Header.ContentEncoding(), []byte("")) {
		t.Fatalf("Unexpected 'Content-Encoding' %q. Expecting %q", ctx.Response.Header.ContentEncoding(), "")
	}
	if !bytes.Equal(ctx.Response.Body(), changedData) {
		t.Fatalf("Unexpected response body %q. Expecting %q", ctx.Response.Body(), data)
	}
}

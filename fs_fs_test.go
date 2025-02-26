package fasthttp

import (
	"bufio"
	"bytes"
	"embed"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

//go:embed fasthttputil fs.go README.md testdata examples
var fsTestFilesystem embed.FS

func TestFSServeFileHead(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.Header.SetMethod(MethodHead)
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFS(&ctx, fsTestFilesystem, "fs.go")

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

func TestFSServeFileCompressed(t *testing.T) {
	t.Parallel()

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
	ServeFS(&ctx, fsTestFilesystem, "fs.go")

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
		t.Fatalf("unexpected 'Content-Encoding' %q. Expecting %q", string(ce), "br")
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
		t.Fatalf("unexpected body: len=%d. Expected len=%d", len(body), len(expectedBody))
	}

	// should prefer zstd over gzip and ignore unknown encoding
	ctx.Request.Reset()
	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, zstd, wompwomp")
	ServeFS(&ctx, fsTestFilesystem, "fs.go")

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
		t.Fatalf("unexpected body: len=%d. Expected len=%d", len(body), len(expectedBody))
	}

	// should prefer gzip and ignore unknown encoding
	ctx.Request.Reset()
	ctx.Request.SetRequestURI("http://foobar.com/baz")
	ctx.Request.Header.Set(HeaderAcceptEncoding, "gzip, wompwomp")
	ServeFS(&ctx, fsTestFilesystem, "fs.go")

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
		t.Fatalf("unexpected body: len=%d. Expected len=%d", len(body), len(expectedBody))
	}
}

func TestFSServeFileUncompressed(t *testing.T) {
	t.Parallel()

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

func TestFSFSByteRangeConcurrent(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		FS:              fsTestFilesystem,
		Root:            "",
		AcceptByteRange: true,
		CleanStop:       stop,
	}
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

func TestFSFSByteRangeSingleThread(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		FS:              fsTestFilesystem,
		Root:            ".",
		AcceptByteRange: true,
		CleanStop:       stop,
	}
	h := fs.NewRequestHandler()

	testFSByteRange(t, h, "/fs.go")
	testFSByteRange(t, h, "/README.md")
}

func TestFSFSCompressConcurrent(t *testing.T) {
	t.Parallel()
	// go 1.16 timeout may occur
	if strings.HasPrefix(runtime.Version(), "go1.16") {
		t.SkipNow()
	}

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		FS:                 fsTestFilesystem,
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		CleanStop:          stop,
	}
	h := fs.NewRequestHandler()

	concurrency := 4
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			for j := 0; j < 5; j++ {
				testFSFSCompress(t, h, "/fs.go")
				testFSFSCompress(t, h, "/examples/")
				testFSFSCompress(t, h, "/README.md")
			}
			ch <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second * 4):
			t.Fatalf("timeout")
		}
	}
}

func TestFSFSCompressSingleThread(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		FS:                 fsTestFilesystem,
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		CleanStop:          stop,
	}
	h := fs.NewRequestHandler()

	testFSFSCompress(t, h, "/fs.go")
	testFSFSCompress(t, h, "/examples/")
	testFSFSCompress(t, h, "/README.md")
}

func testFSFSCompress(t *testing.T, h RequestHandler, filePath string) {
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
		t.Fatalf("unexpected 'Content-Encoding' %q. Expecting %q. filePath=%q", string(ce), "br", filePath)
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
		t.Fatalf("unexpected 'Content-Encoding' %q. Expecting %q. filePath=%q", string(ce), "zstd", filePath)
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
		t.Fatalf("unexpected 'Content-Encoding' %q. Expecting %q. filePath=%q", string(ce), "gzip", filePath)
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

func TestFSServeFileContentType(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.Header.SetMethod(MethodGet)
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFS(&ctx, fsTestFilesystem, "testdata/test.png")

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

func TestFSServeFileDirectoryRedirect(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http://foobar.com")
	ctx.Init(&req, nil, nil)

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFS(&ctx, fsTestFilesystem, "fasthttputil")
	if ctx.Response.StatusCode() != StatusFound {
		t.Fatalf("Unexpected status code %d for directory '/fasthttputil' without trailing slash. Expecting %d.", ctx.Response.StatusCode(), StatusFound)
	}

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFS(&ctx, fsTestFilesystem, "fasthttputil/")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("Unexpected status code %d for directory '/fasthttputil/' with trailing slash. Expecting %d.", ctx.Response.StatusCode(), StatusOK)
	}

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFS(&ctx, fsTestFilesystem, "fs.go")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("Unexpected status code %d for file '/fs.go'. Expecting %d.", ctx.Response.StatusCode(), StatusOK)
	}
}

var dirTestFilesystem = os.DirFS(".")

func TestDirFSServeFileHead(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.Header.SetMethod(MethodHead)
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFS(&ctx, dirTestFilesystem, "fs.go")

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

func TestDirFSServeFileCompressed(t *testing.T) {
	t.Parallel()

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
	ServeFS(&ctx, dirTestFilesystem, "fs.go")

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
		t.Fatalf("unexpected 'Content-Encoding' %q. Expecting %q", string(ce), "br")
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
	ServeFS(&ctx, dirTestFilesystem, "fs.go")

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
	ServeFS(&ctx, dirTestFilesystem, "fs.go")

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

func TestDirFSFSByteRangeConcurrent(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		FS:              dirTestFilesystem,
		Root:            "",
		AcceptByteRange: true,
		CleanStop:       stop,
	}
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

func TestDirFSFSByteRangeSingleThread(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		FS:              dirTestFilesystem,
		Root:            ".",
		AcceptByteRange: true,
		CleanStop:       stop,
	}
	h := fs.NewRequestHandler()

	testFSByteRange(t, h, "/fs.go")
	testFSByteRange(t, h, "/README.md")
}

func TestDirFSFSCompressConcurrent(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		FS:                 dirTestFilesystem,
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		CleanStop:          stop,
	}
	h := fs.NewRequestHandler()

	concurrency := 4
	ch := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			for j := 0; j < 5; j++ {
				testFSFSCompress(t, h, "/fs.go")
				testFSFSCompress(t, h, "/examples/")
				testFSFSCompress(t, h, "/README.md")
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

func TestDirFSFSCompressSingleThread(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	defer close(stop)

	fs := &FS{
		FS:                 dirTestFilesystem,
		Root:               ".",
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		CompressZstd:       true,
		CleanStop:          stop,
	}
	h := fs.NewRequestHandler()

	testFSFSCompress(t, h, "/fs.go")
	testFSFSCompress(t, h, "/examples/")
	testFSFSCompress(t, h, "/README.md")
}

func TestDirFSServeFileContentType(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.Header.SetMethod(MethodGet)
	req.SetRequestURI("http://foobar.com/baz")
	ctx.Init(&req, nil, nil)

	ServeFS(&ctx, dirTestFilesystem, "testdata/test.png")

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

func TestDirFSServeFileDirectoryRedirect(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("http://foobar.com")
	ctx.Init(&req, nil, nil)

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFS(&ctx, dirTestFilesystem, "fasthttputil")
	if ctx.Response.StatusCode() != StatusFound {
		t.Fatalf("Unexpected status code %d for directory '/fasthttputil' without trailing slash. Expecting %d.", ctx.Response.StatusCode(), StatusFound)
	}

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFS(&ctx, dirTestFilesystem, "fasthttputil/")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("Unexpected status code %d for directory '/fasthttputil/' with trailing slash. Expecting %d.", ctx.Response.StatusCode(), StatusOK)
	}

	ctx.Request.Reset()
	ctx.Response.Reset()
	ServeFS(&ctx, dirTestFilesystem, "fs.go")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("Unexpected status code %d for file '/fs.go'. Expecting %d.", ctx.Response.StatusCode(), StatusOK)
	}
}

func TestFSFSGenerateIndexOsDirFS(t *testing.T) {
	t.Parallel()

	t.Run("dirFS", func(t *testing.T) {
		t.Parallel()

		fs := &FS{
			FS:                 dirTestFilesystem,
			Root:               ".",
			GenerateIndexPages: true,
		}
		h := fs.NewRequestHandler()

		var ctx RequestCtx
		var req Request
		ctx.Init(&req, nil, nil)

		h(&ctx)

		cases := []string{"/", "//", ""}
		for _, c := range cases {
			ctx.Request.Reset()
			ctx.Response.Reset()

			req.Header.SetMethod(MethodGet)
			req.SetRequestURI("http://foobar.com" + c)
			h(&ctx)

			if ctx.Response.StatusCode() != StatusOK {
				t.Fatalf("unexpected status code %d for path %q. Expecting %d", ctx.Response.StatusCode(), ctx.Response.StatusCode(), StatusOK)
			}

			if !bytes.Contains(ctx.Response.Body(), []byte("fasthttputil")) {
				t.Fatalf("unexpected body %q. Expecting to contain %q", ctx.Response.Body(), "fasthttputil")
			}

			if !bytes.Contains(ctx.Response.Body(), []byte("fs.go")) {
				t.Fatalf("unexpected body %q. Expecting to contain %q", ctx.Response.Body(), "fs.go")
			}
		}
	})

	t.Run("embedFS", func(t *testing.T) {
		t.Parallel()

		fs := &FS{
			FS:                 fsTestFilesystem,
			Root:               ".",
			GenerateIndexPages: true,
		}
		h := fs.NewRequestHandler()

		var ctx RequestCtx
		var req Request
		ctx.Init(&req, nil, nil)

		h(&ctx)

		cases := []string{"/", "//", ""}
		for _, c := range cases {
			ctx.Request.Reset()
			ctx.Response.Reset()

			req.Header.SetMethod(MethodGet)
			req.SetRequestURI("http://foobar.com" + c)
			h(&ctx)

			if ctx.Response.StatusCode() != StatusOK {
				t.Fatalf("unexpected status code %d for path %q. Expecting %d", ctx.Response.StatusCode(), ctx.Response.StatusCode(), StatusOK)
			}

			if !bytes.Contains(ctx.Response.Body(), []byte("fasthttputil")) {
				t.Fatalf("unexpected body %q. Expecting to contain %q", ctx.Response.Body(), "fasthttputil")
			}

			if !bytes.Contains(ctx.Response.Body(), []byte("fs.go")) {
				t.Fatalf("unexpected body %q. Expecting to contain %q", ctx.Response.Body(), "fs.go")
			}
		}
	})
}

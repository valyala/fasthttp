package fasthttp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFSWindowsRejectsAlternateDataStream(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	publicPath := filepath.Join(root, "public.txt")
	publicBody := []byte("public")
	privateBody := []byte("private")
	if err := os.WriteFile(publicPath, publicBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath+":secret", privateBody, 0o600); err != nil {
		t.Skipf("filesystem does not support alternate data streams: %v", err)
	}

	h := (&FS{Root: root, SkipCache: true}).NewRequestHandler()
	request := func(path string) (int, []byte) {
		var ctx RequestCtx
		ctx.Init(&Request{}, nil, TestLogger{t: t})
		ctx.Request.SetRequestURI(path)
		h(&ctx)
		return ctx.Response.StatusCode(), bytes.Clone(ctx.Response.Body())
	}

	if status, body := request("/public.txt"); status != StatusOK || !bytes.Equal(body, publicBody) {
		t.Fatalf("public file: status=%d body=%q", status, body)
	}
	if status, body := request("/public.txt:secret"); status != StatusForbidden || bytes.Contains(body, privateBody) {
		t.Fatalf("alternate data stream: status=%d body=%q", status, body)
	}
}

func TestFSWindowsServeFileAllowsDriveLetter(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	ctx.Init(&Request{}, nil, TestLogger{t: t})
	ctx.Request.SetRequestURI("http://localhost/")
	ServeFile(&ctx, "fs.go")
	if ctx.Response.StatusCode() != StatusOK {
		t.Fatalf("unexpected status %d; want %d", ctx.Response.StatusCode(), StatusOK)
	}
}

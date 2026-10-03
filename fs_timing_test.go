package fasthttp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkFSSmallFile serves a cached small file over a pipelined
// connection, from a directory and from an fs.FS.
func BenchmarkFSSmallFile(b *testing.B) {
	body := bytes.Repeat([]byte("a"), 1024)
	dir := b.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), body, 0o600); err != nil {
		b.Fatal(err)
	}

	b.Run("dir", func(b *testing.B) {
		benchmarkFSSmallFile(b, &FS{Root: dir}, len(body))
	})
	b.Run("fs", func(b *testing.B) {
		benchmarkFSSmallFile(b, &FS{FS: os.DirFS(dir), AllowEmptyRoot: true}, len(body))
	})
}

func benchmarkFSSmallFile(b *testing.B, fs *FS, size int) {
	const depth = 16
	const request = "GET /file.txt HTTP/1.1\r\nHost: 127.0.0.1:8081\r\n\r\n"
	batch := make([]byte, 0, depth*len(request))
	for range depth {
		batch = append(batch, request...)
	}
	s := &Server{
		ReadBufferSize:  16384,
		WriteBufferSize: 16384,
		Handler:         fs.NewRequestHandler(),
	}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	c := &pipelineConn{batch: batch, reqLen: len(request), remaining: b.N}
	if err := s.ServeConn(c); err != nil {
		b.Fatal(err)
	}
	if want := int64(b.N) * int64(size); c.written < want {
		b.Fatalf("short write: %d < %d", c.written, want)
	}
}

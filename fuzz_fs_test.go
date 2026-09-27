package fasthttp

import (
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Check every attempted file access, including nonexistent filenames, without
// filesystem I/O, cache goroutines, or directory listings in the fuzz loop.
type fuzzContainedFS struct {
	t *testing.T
}

func (f fuzzContainedFS) Open(name string) (fs.File, error) {
	clean := path.Clean(name)
	if clean != "public" && !strings.HasPrefix(clean, "public/") {
		f.t.Fatalf("file handler escaped its root: %q", name)
	}
	// On Windows, account for the separators used by the native filesystem.
	native := filepath.Clean(filepath.FromSlash(name))
	rel, err := filepath.Rel("public", native)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		f.t.Fatalf("file handler escaped native root: %q", name)
	}
	return nil, fs.ErrNotExist
}

type fuzzDiscardLogger struct{}

func (fuzzDiscardLogger) Printf(string, ...any) {}

func FuzzFSPathContainment(f *testing.F) {
	for _, s := range []string{"/public.txt", "/a/../../secret", "../secret", "/%2e%2e/secret", "/%252e%252e/secret", `/a\..\..\secret`, `/a\../secret`, "/a\x00b", "/public.txt:secret"} {
		f.Add(s, uint8(0))
		f.Add(s, uint8(1))
		f.Add(s, uint8(2))
	}
	f.Fuzz(func(t *testing.T, target string, mode uint8) {
		if len(target) > defaultReadBufferSize {
			return
		}
		var ctx RequestCtx
		ctx.Init(&Request{}, nil, fuzzDiscardLogger{})
		defer ctx.Request.Reset()
		defer ctx.Response.Reset()
		ctx.Request.SetRequestURI("http://example.com/" + strings.TrimLeft(target, "/"))
		files := FS{Root: "public", FS: fuzzContainedFS{t: t}, SkipCache: true}
		switch mode % 3 {
		case 1:
			files.PathRewrite = func(*RequestCtx) []byte { return []byte(target) }
		case 2:
			files.PathRewrite = NewPathPrefixStripper(len(target) / 2)
		}
		files.NewRequestHandler()(&ctx)
	})
}

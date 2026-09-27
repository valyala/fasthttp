package fasthttp

import (
	"bytes"
	"testing"
)

func FuzzFSWindowsReservedPaths(f *testing.F) {
	for _, target := range []string{"/public.txt:secret", "/public.txt::$DATA", "/public.txt%3asecret", `C:\public.txt`, `/a\..\public.txt:secret`} {
		f.Add(target, false)
		f.Add(target, true)
	}
	root := f.TempDir()
	f.Fuzz(func(t *testing.T, target string, rewrite bool) {
		if len(target) > defaultReadBufferSize {
			return
		}
		var ctx RequestCtx
		ctx.Init(&Request{}, nil, fuzzDiscardLogger{})
		defer ctx.Request.Reset()
		defer ctx.Response.Reset()
		ctx.Request.SetRequestURI("http://example.com/")
		ctx.URI().SetPath(target)
		files := FS{Root: root, SkipCache: true}
		checkedPath := ctx.Path()
		if rewrite {
			files.PathRewrite = func(*RequestCtx) []byte { return []byte(target) }
			checkedPath = []byte(target)
		}
		files.NewRequestHandler()(&ctx)
		// With a configured root no colon can be a legitimate drive letter.
		// This assertion does not require the host volume to support NTFS ADS.
		if bytes.IndexByte(checkedPath, ':') >= 0 && bytes.IndexByte(checkedPath, 0) < 0 &&
			ctx.Response.StatusCode() != StatusForbidden {
			t.Fatalf("reserved Windows path was not forbidden: %q status=%d", checkedPath, ctx.Response.StatusCode())
		}
	})
}

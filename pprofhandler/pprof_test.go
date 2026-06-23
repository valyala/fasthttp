package pprofhandler

import (
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

func TestMatchPprofPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		endpoint string
		want     bool
	}{
		{"exact match", "/debug/pprof/cmdline", "/debug/pprof/cmdline", true},
		{"trailing slash", "/debug/pprof/cmdline/", "/debug/pprof/cmdline", true},
		{"prefix mismatch", "/debug/pprof/cmdlineFoo", "/debug/pprof/cmdline", false},
		{"prefix mismatch profile", "/debug/pprof/profileX", "/debug/pprof/profile", false},
		{"prefix mismatch symbol", "/debug/pprof/symbolExtra", "/debug/pprof/symbol", false},
		{"prefix mismatch trace", "/debug/pprof/tracebar", "/debug/pprof/trace", false},
		{"different path entirely", "/debug/pprof/heap", "/debug/pprof/cmdline", false},
		{"empty path", "", "/debug/pprof/cmdline", false},
		{"root path", "/", "/debug/pprof/cmdline", false},
		{"double trailing slash", "/debug/pprof/cmdline//", "/debug/pprof/cmdline", false},
		{"extra path segment", "/debug/pprof/cmdline/extra", "/debug/pprof/cmdline", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchPprofPath([]byte(tt.path), []byte(tt.endpoint))
			if got != tt.want {
				t.Errorf("matchPprofPath(%q, %q) = %v, want %v", tt.path, tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestPprofHandlerRejectsPrefixMismatches(t *testing.T) {
	// Before this fix, HasPrefix matched /debug/pprof/cmdlineFoo as a valid
	// cmdline request, leaking the process command line. Now these paths
	// should be rejected at the routing level by matchPprofPath.
	offendingPaths := []struct {
		path     string
		endpoint string
	}{
		{"/debug/pprof/cmdlineFoo", "/debug/pprof/cmdline"},
		{"/debug/pprof/profileBar", "/debug/pprof/profile"},
		{"/debug/pprof/symbolBaz", "/debug/pprof/symbol"},
		{"/debug/pprof/traceQux", "/debug/pprof/trace"},
	}

	for _, tt := range offendingPaths {
		t.Run(tt.path, func(t *testing.T) {
			if matchPprofPath([]byte(tt.path), []byte(tt.endpoint)) {
				t.Errorf("matchPprofPath(%q, %q) = true, want false (prefix mismatch should be rejected)", tt.path, tt.endpoint)
			}
		})
	}
}

func TestPprofHandlerAcceptsValidPaths(t *testing.T) {
	// Verify that exact-match routing still works for known endpoints
	// by checking that the handler sets Content-Type correctly.
	// cmdline and symbol are safe to call outside a server context.
	validPaths := []struct {
		path        string
		contentType string
	}{
		{"/debug/pprof/cmdline", "text/plain; charset=utf-8"},
		{"/debug/pprof/symbol", "text/plain; charset=utf-8"},
	}

	for _, tt := range validPaths {
		t.Run(tt.path, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.SetRequestURI(tt.path)
			PprofHandler(&ctx)

			ct := string(ctx.Response.Header.ContentType())
			if ct != tt.contentType {
				t.Errorf("path %q: expected content-type %q, got %q", tt.path, tt.contentType, ct)
			}
		})
	}
}

func TestPprofHandlerAcceptsTrailingSlash(t *testing.T) {
	// Trailing slash should still route to the correct handler
	paths := []struct {
		path        string
		contentType string
	}{
		{"/debug/pprof/cmdline/", "text/plain; charset=utf-8"},
		{"/debug/pprof/symbol/", "text/plain; charset=utf-8"},
	}

	for _, tt := range paths {
		t.Run(tt.path, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.SetRequestURI(tt.path)
			PprofHandler(&ctx)

			ct := string(ctx.Response.Header.ContentType())
			if ct != tt.contentType {
				t.Errorf("path %q: expected content-type %q, got %q", tt.path, tt.contentType, ct)
			}
		})
	}
}

func TestPprofHandlerPrefixMismatchFallsThroughToIndex(t *testing.T) {
	// Paths with trailing garbage that look like valid endpoints should
	// NOT match the specific profile handlers. They fall through to
	// the index handler which returns "Unknown profile" for unknown names.
	// This is the security fix: previously /debug/pprof/cmdlineFoo would
	// leak the process command line via HasPrefix match.
	invalidPaths := []string{
		"/debug/pprof/cmdlineFoo",
		"/debug/pprof/symbolExtra",
	}

	for _, path := range invalidPaths {
		t.Run(path, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.SetRequestURI(path)
			PprofHandler(&ctx)

			body := string(ctx.Response.Body())
			// The index handler returns "Unknown profile" for unknown profile names
			// instead of leaking the cmdline or symbol data
			if !strings.HasPrefix(body, "Unknown profile") {
				t.Errorf("path %q: expected index fallback with 'Unknown profile', got body starting with: %q", path, body[:min(len(body), 100)])
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
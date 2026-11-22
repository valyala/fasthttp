package fasthttp

import "testing"

func TestURIPathNormalizeIssue86(t *testing.T) {
	t.Parallel()

	// see https://github.com/valyala/fasthttp/issues/86
	var u URI

	testURIPathNormalize(t, &u, `C:\a\b\c\fs.go`, `/C:\a\b\c\fs.go`)

	testURIPathNormalize(t, &u, `a`, `/a`)
	testURIPathNormalize(t, &u, `/a\..\b`, `/b`)

	testURIPathNormalize(t, &u, "/../../../../../foo", "/foo")

	testURIPathNormalize(t, &u, "/..\\..\\..\\..\\..\\", "/")

	testURIPathNormalize(t, &u, "/..%5c..%5cfoo", "/foo")

	testURIPathNormalize(t, &u, "/public/x/..%5c../admin/keys", "/admin/keys")
	testURIPathNormalize(t, &u, "/x/..%5c/evil.example", "/evil.example")
	testURIPathNormalize(t, &u, "/a/x/..%5c./b", "/a/b")
	testURIPathNormalize(t, &u, "/a/b/x/..%5c..", "/a/")
	testURIPathNormalize(t, &u, `/a\b\..\c`, `/a\c`)
}

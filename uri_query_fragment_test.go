package fasthttp

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestURIUpdateQueryFragment(t *testing.T) {
	t.Parallel()

	updates := []struct {
		name  string
		apply func(*URI, string) error
	}{
		{"Update", func(u *URI, s string) error { u.Update(s); return nil }},
		{"UpdateBytes", func(u *URI, s string) error { u.UpdateBytes([]byte(s)); return nil }},
		{"UpdateBytesErr", func(u *URI, s string) error { return u.UpdateBytesErr([]byte(s)) }},
	}
	for _, update := range updates {
		for _, oldHash := range []string{"", "before"} {
			for _, tc := range []struct{ ref, query, hash string }{
				{"?next=2#new", "next=2", "new"},
				{"?#new", "", "new"},
				{"?next=2#", "next=2", ""},
				{"?#", "", ""},
				{"?next=a%23b#new?x=y", "next=a%23b", "new?x=y"},
				{"?next=2", "next=2", oldHash}, // Preserve existing no-fragment behavior.
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", update.name, oldHash, tc.ref), func(t *testing.T) {
					var u URI
					base := "http://example.com/path?old=1"
					if oldHash != "" {
						base += "#" + oldHash
					}
					if err := u.Parse(nil, []byte(base)); err != nil {
						t.Fatal(err)
					}
					u.QueryArgs() // Also invalidate an already-parsed query cache.
					if err := update.apply(&u, tc.ref); err != nil {
						t.Fatal(err)
					}
					if got := string(u.QueryString()); got != tc.query {
						t.Errorf("query = %q, want %q", got, tc.query)
					}
					if got := string(u.Hash()); got != tc.hash {
						t.Errorf("hash = %q, want %q", got, tc.hash)
					}
					requestURI := "/path"
					if strings.HasPrefix(tc.ref, "?") {
						requestURI += "?" + tc.query
					}
					if got := string(u.RequestURI()); got != requestURI {
						t.Errorf("request URI = %q, want %q", got, requestURI)
					}
					var req Request
					req.SetURI(&u)
					if got, _, _ := strings.Cut(req.String(), "\r\n"); got != "GET "+requestURI+" HTTP/1.1" {
						t.Errorf("wire request line = %q, want GET %s HTTP/1.1", got, requestURI)
					}
					fullURI := "http://example.com" + requestURI
					if strings.Contains(tc.ref, "#") || tc.hash != "" {
						fullURI += "#" + tc.hash
					}
					// net/url preserves the explicit empty query on the wire, but its
					// URL.String omits an empty fragment. Redirects need the latter
					// delimiter too, so browsers do not inherit the old fragment.
					standardBase, err := url.Parse(base)
					if err != nil {
						t.Fatal(err)
					}
					reference, err := url.Parse(tc.ref)
					if err != nil {
						t.Fatal(err)
					}
					if got := standardBase.ResolveReference(reference).RequestURI(); got != requestURI {
						t.Errorf("net/url request URI = %q, want %q", got, requestURI)
					}
					if got := string(u.FullURI()); got != fullURI {
						t.Errorf("full URI = %q, want %q", got, fullURI)
					}
					if got := string(u.QueryArgs().Peek("old")); got != "" {
						t.Errorf("old query argument remains: %q", got)
					}
				})
			}
		}
	}
}

func TestURIUpdateEmptyDelimitersLifecycle(t *testing.T) {
	t.Parallel()
	var u, copied URI
	u.Update("http://example.com/path?old=1#before")
	u.Update("?#")
	u.CopyTo(&copied)
	check := func(want string) {
		t.Helper()
		if got := string(copied.FullURI()); got != want {
			t.Fatalf("full URI = %q, want %q", got, want)
		}
	}
	check("http://example.com/path?#")
	copied.QueryArgs()
	check("http://example.com/path?#")
	copied.QueryArgs().Set("a", "b")
	check("http://example.com/path?a=b#")
	copied.QueryArgs().Del("a")
	check("http://example.com/path?#")
	copied.SetQueryString("")
	check("http://example.com/path#")
	copied.SetHash("")
	check("http://example.com/path")
	u.CopyTo(&copied)
	copied.SetQueryStringBytes(nil)
	copied.SetHashBytes(nil)
	check("http://example.com/path")
	u.CopyTo(&copied)
	copied.Update("?new=1#new")
	check("http://example.com/path?new=1#new")
	u.CopyTo(&copied)
	copied.Update("/other")
	check("http://example.com/other")
	u.CopyTo(&copied)
	copied.Reset()
	check("http:///")
}

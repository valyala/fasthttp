package fasthttp

import (
	"fmt"
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
					if tc.query != "" {
						requestURI += "?" + tc.query
					}
					if got := string(u.RequestURI()); got != requestURI {
						t.Errorf("request URI = %q, want %q", got, requestURI)
					}
					var req Request
					req.SetURI(&u)
					if got := strings.SplitN(req.String(), "\r\n", 2)[0]; got != "GET "+requestURI+" HTTP/1.1" {
						t.Errorf("wire request line = %q, want GET %s HTTP/1.1", got, requestURI)
					}
					fullURI := "http://example.com" + requestURI
					if tc.hash != "" {
						fullURI += "#" + tc.hash
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

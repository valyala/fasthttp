package fasthttp

import (
	"bytes"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func FuzzURIPath(f *testing.F) {
	for _, target := range []string{"/A/B?Key=Value", "/a/%2e%2e/b", "/a%2fb//c/.", "/%252e%252e/x", "/%zz", `/a\..\b`, "/?x=1#Fragment"} {
		f.Add("Example.COM", target)
	}
	f.Fuzz(func(t *testing.T, host, target string) {
		if len(host)+len(target) > defaultReadBufferSize {
			return
		}
		if !strings.HasPrefix(target, "/") {
			target = "/" + target
		}
		var u URI
		for range 2 {
			err := u.Parse([]byte(host), []byte(target))
			var fresh URI
			freshErr := fresh.Parse([]byte(host), []byte(target))
			if (err == nil) != (freshErr == nil) || !bytes.Equal(u.FullURI(), fresh.FullURI()) {
				t.Fatal("reused URI differs from fresh URI")
			}
			if err != nil {
				return
			}
			// PathUnescape is deliberately used rather than QueryUnescape: a
			// literal '+' in a path must remain a plus. Malformed escapes have
			// a fasthttp-specific permissive policy, tested by the round trip.
			decoded, err := url.PathUnescape(string(u.PathOriginal()))
			if err == nil && (filepath.Separator != '\\' || !strings.Contains(decoded, `\`)) {
				want := path.Clean("/" + decoded)
				if want != "/" && (strings.HasSuffix(decoded, "/") || strings.HasSuffix(decoded, "/.") || strings.HasSuffix(decoded, "/..")) {
					want += "/"
				}
				if string(u.Path()) != want {
					t.Fatalf("path normalization: %q became %q, want %q", u.PathOriginal(), u.Path(), want)
				}
			}
			var roundTrip URI
			if err := roundTrip.Parse(bytes.Clone(u.Host()), bytes.Clone(u.RequestURI())); err != nil {
				t.Fatalf("serialized URI failed to parse: %v", err)
			}
			if !bytes.Equal(roundTrip.Path(), u.Path()) || !bytes.Equal(roundTrip.QueryString(), u.QueryString()) {
				t.Fatal("URI serialization changed path or query")
			}
			_ = u.QueryArgs() // Populate lazy state before the next Parse.
		}
	})
}

func FuzzURIResolveReference(f *testing.F) {
	for _, ref := range []string{"../Next?Key=Value", "./", "/Other", "?Key=Value", "#Fragment", "//other.example/A", "https://other.example/B"} {
		f.Add("/Base/Dir/index?old=value", ref)
	}
	f.Fuzz(func(t *testing.T, basePath, ref string) {
		if len(basePath)+len(ref) > defaultReadBufferSize {
			return
		}
		base, err := url.Parse("https://example.com/" + strings.TrimLeft(basePath, "/"))
		if err != nil {
			return
		}
		reference, err := url.Parse(ref)
		if err != nil {
			return
		}
		// net/url preserves escaped separators and duplicate slashes, while
		// fasthttp normalizes them. Query-only updates containing '#' also
		// follow a different API contract. Keep the differential subset explicit.
		if ref == "" || strings.ContainsAny(basePath+ref, "%\\") || strings.Contains(base.Path, "//") ||
			strings.Contains(reference.Path, "//") || base.Opaque != "" || reference.Opaque != "" ||
			base.User != nil || reference.User != nil ||
			(strings.HasPrefix(ref, "?") && strings.Contains(ref, "#")) ||
			(reference.Scheme != "" && (reference.Host == "" || !strings.Contains(ref, "://"))) ||
			(reference.Scheme != "" && reference.Scheme != "http" && reference.Scheme != "https") {
			return
		}
		var u URI
		if u.Parse(nil, []byte(base.String())) != nil {
			return
		}
		// Resolve against the normalized base to account for fasthttp's
		// documented path normalization before applying the relative reference.
		base, err = url.Parse(string(u.FullURI()))
		if err != nil {
			return
		}
		want := base.ResolveReference(reference)
		if strings.HasPrefix(ref, "?") {
			// UpdateBytes documents query-only updates as replacing just the
			// query string; unlike ResolveReference, they retain the old hash.
			want.Fragment, want.RawFragment = base.Fragment, base.RawFragment
		}
		var normalized URI
		if normalized.Parse(nil, []byte(want.String())) != nil {
			return
		}
		// net/url escapes spaces and non-ASCII bytes in fragments when
		// serializing; fasthttp stores the fragment's wire representation.
		u.UpdateBytes([]byte(reference.String()))
		if !bytes.Equal(u.FullURI(), normalized.FullURI()) {
			t.Fatalf("resolve %q against %q: got %q want %q", ref, base, u.FullURI(), normalized.FullURI())
		}
	})
}

func FuzzArgsParse(f *testing.F) {
	for _, s := range []string{"a=1&a=2&empty=&flag", "a+b=%2B+%20", "x=%zz&y=%&z=%00", "&&=&&", "Key=Value;Other"} {
		f.Add([]byte(s), []byte("old=state&old=again"))
	}
	f.Fuzz(func(t *testing.T, data, previous []byte) {
		if len(data)+len(previous) > defaultReadBufferSize {
			return
		}
		var a, fresh, roundTrip Args
		a.ParseBytes(previous)
		a.ParseBytes(data)
		fresh.ParseBytes(data)
		canonical := bytes.Clone(a.QueryString())
		roundTrip.ParseBytes(canonical)
		if !bytes.Equal(canonical, fresh.QueryString()) || !bytes.Equal(canonical, roundTrip.QueryString()) {
			t.Fatal("argument reuse or round trip changed canonical arguments")
		}
		var req Request
		defer req.Reset()
		req.Header.SetContentType("application/x-www-form-urlencoded")
		req.SetBody(data)
		if !bytes.Equal(req.PostArgs().QueryString(), canonical) {
			t.Fatal("POST arguments differ from Args.ParseBytes")
		}
		var u URI
		u.SetQueryStringBytes(previous)
		_ = u.QueryArgs()
		u.SetQueryStringBytes(data)
		if !bytes.Equal(u.QueryArgs().QueryString(), canonical) {
			t.Fatal("lazy query arguments differ from Args.ParseBytes")
		}
		want, err := url.ParseQuery(string(data))
		if err != nil {
			return // net/url rejects malformed escapes and unescaped semicolons.
		}
		// fasthttp discards a pair when both its key and value are empty.
		if values, ok := want[""]; ok {
			filtered := values[:0]
			for _, v := range values {
				if v != "" {
					filtered = append(filtered, v)
				}
			}
			if len(filtered) == 0 {
				delete(want, "")
			} else {
				want[""] = filtered
			}
		}
		got := make(url.Values)
		for k, v := range a.All() {
			got[string(k)] = append(got[string(k)], string(v))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("query mismatch: got %v want %v", got, want)
		}
	})
}

func FuzzRequestCookies(f *testing.F) {
	for _, s := range []string{`a=1; a=2; empty=`, `quoted="hello"; other=x`, `flag; a=b=c`, "a=bad\x00value; good=yes"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data string) {
		if len(data) > defaultReadBufferSize {
			return
		}
		first := parseRequestCookies(nil, []byte(data))
		reused := parseRequestCookies(nil, []byte("old=one; stale=two; third=three"))
		reused = parseRequestCookies(reused[:0], []byte(data))
		if !bytes.Equal(appendRequestCookieBytes(nil, first), appendRequestCookieBytes(nil, reused)) {
			t.Fatal("request cookie reuse retained stale state")
		}
		var h RequestHeader
		h.Set("Cookie", data)
		var lazy [][2]string
		for k, v := range h.Cookies() {
			lazy = append(lazy, [2]string{string(k), string(v)})
		}
		// Header setters sanitize CR/LF, so compare their actual stored value
		// against the direct parser only when no sanitization is needed.
		if strings.ContainsAny(data, "\r\n") {
			return
		}
		var direct [][2]string
		for _, kv := range first {
			direct = append(direct, [2]string{string(kv.key), string(kv.value)})
		}
		if !reflect.DeepEqual(lazy, direct) {
			t.Fatalf("lazy cookie parser mismatch: %v vs %v", lazy, direct)
		}
		// A generated, valid named cookie supplies an independent net/http
		// oracle without conflating the parsers' malformed-cookie policies.
		value := url.QueryEscape(data)
		wire := "fuzz=" + value + "; second=ok; fuzz=duplicate"
		h.Reset()
		h.Set("Cookie", wire)
		var got [][2]string
		for k, v := range h.Cookies() {
			got = append(got, [2]string{string(k), string(v)})
		}
		nr := http.Request{Header: http.Header{"Cookie": {wire}}}
		var want [][2]string
		for _, c := range nr.Cookies() {
			want = append(want, [2]string{c.Name, c.Value})
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cookie mismatch: got %v want %v", got, want)
		}
	})
}

func FuzzParseByteRange(f *testing.F) {
	for _, s := range []string{"bytes=0-0", "bytes=1-", "bytes=-2", "bytes=-0", "bytes=0-999999999999999999999", "bytes=2-1", "bytes=0-1,3-4"} {
		f.Add(s, uint64(10))
	}
	f.Add("bytes=-1", uint64(0))
	f.Fuzz(func(t *testing.T, data string, size uint64) {
		if len(data) > 1024 {
			return
		}
		length := int(size & uint64(^uint(0)>>1))
		start, end, err := ParseByteRange([]byte(data), length)
		wantStart, wantEnd, ok := fuzzByteRangeOracle(data, length)
		if (err == nil) != ok || (ok && (start != wantStart || end != wantEnd)) {
			t.Fatalf("range %q length=%d: got %d-%d err=%v, want %d-%d valid=%v", data, length, start, end, err, wantStart, wantEnd, ok)
		}
		if err == nil && (start < 0 || end >= length || start > end+1) {
			t.Fatalf("range outside content: %d-%d of %d", start, end, length)
		}
	})
}

func fuzzByteRangeOracle(s string, length int) (int, int, bool) {
	if !strings.HasPrefix(s, "bytes=") {
		return 0, 0, false
	}
	a, b, found := strings.Cut(strings.TrimPrefix(s, "bytes="), "-")
	if !found {
		return 0, 0, false
	}
	parse := func(s string) (int, bool) {
		if s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, false
		}
		n, err := strconv.ParseUint(s, 10, strconv.IntSize-1)
		return int(n), err == nil
	}
	if a == "" {
		n, ok := parse(b)
		// fasthttp represents a zero suffix as the empty range [length,length).
		return max(length-n, 0), length - 1, ok && length > 0
	}
	start, ok := parse(a)
	if !ok || start >= length {
		return 0, 0, false
	}
	if b == "" {
		return start, length - 1, true
	}
	end, ok := parse(b)
	end = min(end, length-1)
	return start, end, ok && start <= end
}

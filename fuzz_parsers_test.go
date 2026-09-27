package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
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
	f.Add("%25", "0")
	f.Add("[fe80::1%25en0]", "/a?b=c")
	f.Add(" ", "//://")
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
			// Host is decoded, and an absolute target can override the input
			// host. Use a valid fixed host to test just the request target.
			requestURI := bytes.Clone(u.RequestURI())
			var roundTrip URI
			if err := roundTrip.Parse([]byte("example.com"), requestURI); err != nil {
				t.Fatalf("request URI %q failed to parse: %v", requestURI, err)
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
	// net/url.String loses the empty authority in "//?" and the empty
	// fragment in "#". Keep these and adjacent update forms as regression
	// seeds so the target does not change the input before exercising it.
	for _, ref := range []string{"//?", "?", "#", "# ", "/?", "//other.example?"} {
		f.Add("#0", ref)
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
		// fasthttp normalizes them. It also loses an empty authority when
		// parsing: "//?" becomes indistinguishable from the query-only "?".
		// fasthttp treats the former as a host replacement, so exclude that
		// incompatible case. Query-only updates containing '#' have a
		// different API contract too. net/url permits raw control bytes in
		// fragments, which URI.Parse rejects. Keep the subset explicit.
		if ref == "" || strings.ContainsAny(basePath+ref, "%\\") || strings.Contains(base.Path, "//") ||
			strings.Contains(reference.Path, "//") || base.Opaque != "" || reference.Opaque != "" ||
			base.User != nil || reference.User != nil ||
			strings.IndexFunc(ref, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 ||
			(strings.HasPrefix(ref, "//") && reference.Host == "") ||
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
		switch ref[0] {
		case '?':
			// UpdateBytes documents query-only updates as replacing just the
			// query string; unlike ResolveReference, they retain the old hash.
			want.Fragment, want.RawFragment = base.Fragment, base.RawFragment
		case '#':
			// An explicit fragment-only update replaces the old hash even
			// when empty. net/url cannot distinguish "#" from an empty ref.
			want.Fragment, want.RawFragment = reference.Fragment, reference.RawFragment
		}
		var normalized URI
		if normalized.Parse(nil, []byte(want.String())) != nil {
			return
		}
		// Exercise the original bytes. Serializing reference first can erase
		// syntax that selects a different UpdateBytes branch. Compare decoded
		// fragments because net/url escapes them when serializing, whereas
		// fasthttp retains their wire representation.
		u.UpdateBytes([]byte(ref))
		fragment, fragmentErr := url.PathUnescape(string(u.Hash()))
		if !bytes.Equal(u.Scheme(), normalized.Scheme()) || !bytes.Equal(u.Host(), normalized.Host()) ||
			!bytes.Equal(u.Path(), normalized.Path()) || !bytes.Equal(u.QueryString(), normalized.QueryString()) ||
			fragmentErr != nil || fragment != want.Fragment {
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
		return max(length-n, 0), length - 1, ok && length > 0 && n > 0
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

func FuzzAcceptEncoding(f *testing.F) {
	for _, q := range []uint16{0, 1, 500, 1000} {
		for mode := range uint8(4) {
			f.Add([]byte("codec"), q, mode)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, quality uint16, mode uint8) {
		if len(data) > 128 {
			return
		}
		// Generate valid, distinct tokens; the oracle knows which exact token was
		// inserted without duplicating the production scanner or q-value parser.
		token := fmt.Sprintf("x%x^codec", data)
		q := int(quality) % 1001
		weight := fmt.Sprintf("%d.%03d", q/1000, q%1000)
		member := strings.ToUpper(token)
		if mode&1 != 0 {
			member += " \t; Q=" + weight
		}
		wire := "br,\t " + token + "-extra, x" + token + ", " + member + " \t, deflate"
		want := mode&1 == 0 || q > 0
		var h RequestHeader
		if mode&2 == 0 {
			h.Set(HeaderAcceptEncoding, wire)
		} else if err := h.Read(bufio.NewReader(strings.NewReader("GET / HTTP/1.1\r\nHost: example.com\r\nAccept-Encoding: " + wire + "\r\n\r\n"))); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{token, strings.ToUpper(token)} {
			if h.HasAcceptEncoding(query) != want || h.HasAcceptEncodingBytes([]byte(query)) != want {
				t.Fatalf("coding %q in %q: want %v", query, wire, want)
			}
		}
		if h.HasAcceptEncoding(strings.ReplaceAll(token, "^", "~")) || h.HasAcceptEncoding("gzip") {
			t.Fatalf("accepted absent coding in %q", wire)
		}
	})
}

func FuzzCookieMaxAge(f *testing.F) {
	for mode := range uint8(9) {
		f.Add(uint64(100), mode, true)
	}
	f.Add(^uint64(0), uint8(0), false)
	f.Add(uint64(0), uint8(0), false)
	f.Add(uint64(0), uint8(6), false)
	f.Add(uint64(^uint(0)>>1), uint8(0), false)
	f.Add(uint64(^uint(0)>>1)+1, uint8(1), true)
	f.Fuzz(func(t *testing.T, magnitude uint64, mode uint8, previous bool) {
		value := strconv.FormatUint(magnitude, 10)
		want := 0
		if previous {
			want = 123
		}
		valid := magnitude <= uint64(^uint(0)>>1)
		switch mode % 9 {
		case 1:
			value = "-" + value
		case 2:
			value += "x"
			valid = false
		case 3:
			value += ".5"
			valid = false
		case 4:
			value = ""
			valid = false
		case 5:
			value = "--" + value
			valid = false
		case 6:
			value = "1" + value + "0000000000000000000000000000000"
			valid = false
		case 7:
			value = "-"
			valid = false
		case 8:
			value = "000" + value
		}
		if valid {
			want = int(magnitude)
			if magnitude == 0 || mode%9 == 1 {
				want = -1
			}
		}
		wire := "session=value; "
		if previous {
			wire += "Max-Age=123; "
		}
		wire += "mAx-AgE=" + value + "; Path=/; Secure; HttpOnly"
		var c Cookie
		if err := c.Parse("stale=old; Max-Age=999; Domain=stale.example; SameSite=Strict"); err != nil {
			t.Fatal(err)
		}
		if err := c.Parse(wire); err != nil {
			t.Fatal(err)
		}
		if c.MaxAge() != want || string(c.Key()) != "session" || string(c.Value()) != "value" ||
			string(c.Path()) != "/" || !c.Secure() || !c.HTTPOnly() || len(c.Domain()) != 0 || c.SameSite() != CookieSameSiteDisabled {
			t.Fatalf("cookie %q: got %q, max-age=%d want %d", wire, c.String(), c.MaxAge(), want)
		}
		var fresh, roundTrip Cookie
		if err := fresh.Parse(wire); err != nil {
			t.Fatal(err)
		}
		if c.String() != fresh.String() {
			t.Fatal("cookie reuse changed parsing")
		}
		if err := roundTrip.Parse(c.String()); err != nil || roundTrip.MaxAge() != want || roundTrip.String() != c.String() {
			t.Fatalf("cookie round trip changed result: %v", err)
		}
	})
}

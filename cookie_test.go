package fasthttp

import (
	"strings"
	"testing"
	"time"
)

func TestCookiePanic(t *testing.T) {
	t.Parallel()

	var c Cookie
	if err := c.Parse(";SAMeSITe="); err != nil {
		t.Error(err)
	}
}

func TestCookieSettersSanitizeNewLines(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		set  func(c *Cookie)
	}{
		{
			name: "set key",
			set:  func(c *Cookie) { c.SetKey("sid\r\nSet-Cookie: admin=1") },
		},
		{
			name: "set key bytes",
			set:  func(c *Cookie) { c.SetKeyBytes([]byte("sid\r\nSet-Cookie: admin=1")) },
		},
		{
			name: "set value",
			set:  func(c *Cookie) { c.SetValue("abc\r\nSet-Cookie: admin=1") },
		},
		{
			name: "set value bytes",
			set:  func(c *Cookie) { c.SetValueBytes([]byte("abc\r\nSet-Cookie: admin=1")) },
		},
		{
			name: "set domain",
			set:  func(c *Cookie) { c.SetDomain("example.com\r\nSet-Cookie: admin=1") },
		},
		{
			name: "set domain bytes",
			set:  func(c *Cookie) { c.SetDomainBytes([]byte("example.com\r\nSet-Cookie: admin=1")) },
		},
		{
			name: "set path",
			set:  func(c *Cookie) { c.SetPath("/account\r\nSet-Cookie: admin=1") },
		},
		{
			name: "set path bytes",
			set:  func(c *Cookie) { c.SetPathBytes([]byte("/account\r\nSet-Cookie: admin=1")) },
		},
		{
			name: "set path encoded new line",
			set:  func(c *Cookie) { c.SetPath("/account%0d%0aSet-Cookie:%20admin=1") },
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var c Cookie
			c.SetKey("sid")
			c.SetValue("abc")
			c.SetDomain("example.com")
			c.SetPath("/")

			tc.set(&c)

			s := c.String()
			if strings.ContainsAny(s, "\r\n") {
				t.Fatalf("unexpected newline in cookie %q", s)
			}
		})
	}
}

func TestCookieSettersSanitizeSemicolons(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		set  func(c *Cookie)
		get  func(c *Cookie) []byte
	}{
		{
			name: "set key",
			set:  func(c *Cookie) { c.SetKey("sid; Secure") },
			get:  (*Cookie).Key,
		},
		{
			name: "set key bytes",
			set:  func(c *Cookie) { c.SetKeyBytes([]byte("sid; Secure")) },
			get:  (*Cookie).Key,
		},
		{
			name: "set value",
			set:  func(c *Cookie) { c.SetValue("abc; Secure") },
			get:  (*Cookie).Value,
		},
		{
			name: "set value bytes",
			set:  func(c *Cookie) { c.SetValueBytes([]byte("abc; Secure")) },
			get:  (*Cookie).Value,
		},
		{
			name: "set domain",
			set:  func(c *Cookie) { c.SetDomain("example.com; Secure") },
			get:  (*Cookie).Domain,
		},
		{
			name: "set domain bytes",
			set:  func(c *Cookie) { c.SetDomainBytes([]byte("example.com; Secure")) },
			get:  (*Cookie).Domain,
		},
		{
			name: "set path",
			set:  func(c *Cookie) { c.SetPath("/account; Secure") },
			get:  (*Cookie).Path,
		},
		{
			name: "set path bytes",
			set:  func(c *Cookie) { c.SetPathBytes([]byte("/account; Secure")) },
			get:  (*Cookie).Path,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var c Cookie
			c.SetKey("sid")
			c.SetValue("abc")
			c.SetDomain("example.com")
			c.SetPath("/")

			tc.set(&c)

			if strings.Contains(string(tc.get(&c)), ";") {
				t.Fatalf("%s left a ';' that can inject attributes: %q", tc.name, c.Cookie())
			}

			var got Cookie
			if err := got.ParseBytes(c.Cookie()); err != nil {
				t.Fatalf("cannot parse produced cookie %q: %v", c.Cookie(), err)
			}
			if got.Secure() {
				t.Fatalf("%s injected a Secure attribute: %q", tc.name, c.Cookie())
			}
		})
	}
}

func TestCookieParseSanitizesNewLines(t *testing.T) {
	t.Parallel()

	var c Cookie
	err := c.Parse("sid=abc\r\nSet-Cookie: admin=1; Domain=example.com\r\nSet-Cookie: admin=1; Path=/account\r\nSet-Cookie: admin=1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s := c.String()
	if strings.ContainsAny(s, "\r\n") {
		t.Fatalf("unexpected newline in cookie %q", s)
	}
}

func TestCookieParseRejectsInvalidQuotedValue(t *testing.T) {
	t.Parallel()

	testCases := []string{
		`session="abc; Domain=evil.com`,
		`session=abc"; Domain=evil.com`,
		`session="abc\"; Domain=evil.com`,
	}

	for _, tc := range testCases {
		var c Cookie
		if err := c.Parse(tc); err != ErrInvalidCookieValue {
			t.Fatalf("unexpected error %v. Expecting %v. Cookie %q", err, ErrInvalidCookieValue, tc)
		}
		if len(c.Domain()) != 0 {
			t.Fatalf("unexpected domain %q parsed from invalid cookie %q", c.Domain(), tc)
		}
	}
}

// A Domain or Path that can't be used is ignored, like an Expires or Max-Age
// that isn't valid (RFC 6265, section 5.2): the cookie is kept, it gets no
// such attribute, and the attributes after it are still parsed.
func TestCookieParseIgnoresInvalidAttributeValue(t *testing.T) {
	t.Parallel()

	// Domain values reject the quoted-string bytes (" ; \), the same set as the
	// primary cookie value, since none are legal in a hostname.
	domainCases := []string{
		`sid=ok; Domain=victim.example"`,
		`sid=ok; Domain=e"vil.example`,
		`sid=ok; Domain=e\vil.example`,
	}
	for _, tc := range domainCases {
		var c Cookie
		if err := c.Parse(tc + "; Path=/after; Secure; HttpOnly"); err != nil {
			t.Fatalf("unexpected error %v. Cookie %q", err, tc)
		}
		if len(c.Domain()) != 0 {
			t.Fatalf("unexpected domain %q parsed from invalid attribute %q", c.Domain(), tc)
		}
		if string(c.Key()) != "sid" || string(c.Value()) != "ok" {
			t.Fatalf("unexpected cookie %q=%q parsed from %q", c.Key(), c.Value(), tc)
		}
		if string(c.Path()) != "/after" || !c.Secure() || !c.HTTPOnly() {
			t.Fatalf("attributes after the invalid Domain were lost: path=%q secure=%v httponly=%v. Cookie %q",
				c.Path(), c.Secure(), c.HTTPOnly(), tc)
		}
	}

	// Path values reject the ';' separator and every byte outside 0x20-0x7e,
	// matching net/http's validCookiePathByte; '"' and '\' stay valid there.
	pathCases := []string{
		"sid=ok; Path=/a\x01b",
		"sid=ok; Path=/a\x7fb",
		"sid=ok; Path=/a\x80b",
		"sid=ok; Path=/caf\xc3\xa9",
		"sid=ok; Path=/a\xffb",
	}
	for _, tc := range pathCases {
		var c Cookie
		if err := c.Parse(tc + "; Domain=example.com; Secure; HttpOnly"); err != nil {
			t.Fatalf("unexpected error %v. Cookie %q", err, tc)
		}
		if len(c.Path()) != 0 {
			t.Fatalf("unexpected path %q parsed from invalid attribute %q", c.Path(), tc)
		}
		if string(c.Key()) != "sid" || string(c.Value()) != "ok" {
			t.Fatalf("unexpected cookie %q=%q parsed from %q", c.Key(), c.Value(), tc)
		}
		if string(c.Domain()) != "example.com" || !c.Secure() || !c.HTTPOnly() {
			t.Fatalf("attributes after the invalid Path were lost: domain=%q secure=%v httponly=%v. Cookie %q",
				c.Domain(), c.Secure(), c.HTTPOnly(), tc)
		}
	}

	// A later attribute that can't be used does not undo an earlier one that can,
	// the same as for an invalid Max-Age.
	var last Cookie
	if err := last.Parse(`sid=ok; Domain=trusted.example; Path=/app; Domain=e"vil.example; Path=/a` + "\x01"); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if string(last.Domain()) != "trusted.example" || string(last.Path()) != "/app" {
		t.Fatalf("unexpected domain %q path %q. Expecting the valid ones before the invalid ones", last.Domain(), last.Path())
	}

	// A backslash in Path is accepted and round-trips, matching SetPath.
	var rt Cookie
	rt.SetKey("sid")
	rt.SetValue("ok")
	rt.SetPath(`/a\b`)
	var parsed Cookie
	if err := parsed.ParseBytes(rt.Cookie()); err != nil {
		t.Fatalf("unexpected error %v round-tripping a backslash path", err)
	}
	if string(parsed.Path()) != `/a\b` {
		t.Fatalf("unexpected path %q, expecting %q", parsed.Path(), `/a\b`)
	}

	// A balanced quoted attribute is unquoted during the trim pass and stays valid.
	var c Cookie
	if err := c.Parse(`sid=ok; Domain="trusted.example"; Path="/app"`); err != nil {
		t.Fatalf("unexpected error %v for balanced quoted attributes", err)
	}
	if string(c.Domain()) != "trusted.example" {
		t.Fatalf("unexpected domain %q", c.Domain())
	}
	if string(c.Path()) != "/app" {
		t.Fatalf("unexpected path %q", c.Path())
	}
}

func TestCookieValueWithEqualAndSpaceChars(t *testing.T) {
	t.Parallel()

	testCookieValueWithEqualAndSpaceChars(t, "sth1", "/", "MTQ2NjU5NTcwN3xfUVduVXk4aG9jSmZaNzNEb1dGa1VjekY1bG9vMmxSWlJBZUN2Q1ZtZVFNMTk2YU9YaWtCVmY1eDRWZXd3M3Q5RTJRZnZMbk5mWklSSFZJcVlXTDhiSFFHWWdpdFVLd1hwbXR2UUN4QlJ1N3BITFpkS3Y4PXzDvPNn6JVDBFB2wYVYPHdkdlZBm6n1_0QB3_GWwE40Tg  ==")
	testCookieValueWithEqualAndSpaceChars(t, "sth2", "/", "123")
	testCookieValueWithEqualAndSpaceChars(t, "sth3", "/", "123 ==   1")
}

func testCookieValueWithEqualAndSpaceChars(t *testing.T, expectedName, expectedPath, expectedValue string) {
	var c Cookie
	c.SetKey(expectedName)
	c.SetPath(expectedPath)
	c.SetValue(expectedValue)

	s := c.String()

	var c1 Cookie
	if err := c1.Parse(s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	name := c1.Key()
	if string(name) != expectedName {
		t.Fatalf("unexpected name %q. Expecting %q", name, expectedName)
	}
	path := c1.Path()
	if string(path) != expectedPath {
		t.Fatalf("unexpected path %q. Expecting %q", path, expectedPath)
	}
	value := c1.Value()
	if string(value) != expectedValue {
		t.Fatalf("unexpected value %q. Expecting %q", value, expectedValue)
	}
}

func TestCookieSecureHttpOnly(t *testing.T) {
	t.Parallel()

	var c Cookie

	if err := c.Parse("foo=bar; HttpOnly; secure"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.Secure() {
		t.Fatalf("secure must be set")
	}
	if !c.HTTPOnly() {
		t.Fatalf("HttpOnly must be set")
	}
	s := c.String()
	if !strings.Contains(s, "; secure") {
		t.Fatalf("missing secure flag in cookie %q", s)
	}
	if !strings.Contains(s, "; HttpOnly") {
		t.Fatalf("missing HttpOnly flag in cookie %q", s)
	}
}

func TestCookieSecure(t *testing.T) {
	t.Parallel()

	var c Cookie

	if err := c.Parse("foo=bar; secure"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.Secure() {
		t.Fatalf("secure must be set")
	}
	s := c.String()
	if !strings.Contains(s, "; secure") {
		t.Fatalf("missing secure flag in cookie %q", s)
	}

	if err := c.Parse("foo=bar"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Secure() {
		t.Fatalf("Unexpected secure flag set")
	}
	s = c.String()
	if strings.Contains(s, "secure") {
		t.Fatalf("unexpected secure flag in cookie %q", s)
	}
}

func TestCookieSameSite(t *testing.T) {
	t.Parallel()

	var c Cookie

	if err := c.Parse("foo=bar; samesite"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.SameSite() != CookieSameSiteDefaultMode {
		t.Fatalf("SameSite must be set")
	}
	s := c.String()
	if !strings.Contains(s, "; SameSite") {
		t.Fatalf("missing SameSite flag in cookie %q", s)
	}

	if err := c.Parse("foo=bar; samesite=lax"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.SameSite() != CookieSameSiteLaxMode {
		t.Fatalf("SameSite Lax Mode must be set")
	}
	s = c.String()
	if !strings.Contains(s, "; SameSite=Lax") {
		t.Fatalf("missing SameSite flag in cookie %q", s)
	}

	if err := c.Parse("foo=bar; samesite=strict"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.SameSite() != CookieSameSiteStrictMode {
		t.Fatalf("SameSite Strict Mode must be set")
	}
	s = c.String()
	if !strings.Contains(s, "; SameSite=Strict") {
		t.Fatalf("missing SameSite flag in cookie %q", s)
	}

	if err := c.Parse("foo=bar; samesite=none"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.SameSite() != CookieSameSiteNoneMode {
		t.Fatalf("SameSite None Mode must be set")
	}
	s = c.String()
	if !strings.Contains(s, "; SameSite=None") {
		t.Fatalf("missing SameSite flag in cookie %q", s)
	}

	if err := c.Parse("foo=bar"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c.SetSameSite(CookieSameSiteNoneMode)
	s = c.String()
	if !strings.Contains(s, "; SameSite=None") {
		t.Fatalf("missing SameSite flag in cookie %q", s)
	}
	if !strings.Contains(s, "; secure") {
		t.Fatalf("missing Secure flag in cookie %q", s)
	}

	if err := c.Parse("foo=bar"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.SameSite() != CookieSameSiteDisabled {
		t.Fatalf("Unexpected SameSite flag set")
	}
	s = c.String()
	if strings.Contains(s, "SameSite") {
		t.Fatalf("unexpected SameSite flag in cookie %q", s)
	}
}

func TestCookieMaxAge(t *testing.T) {
	t.Parallel()

	var c Cookie

	maxAge := 100
	if err := c.Parse("foo=bar; max-age=100"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if maxAge != c.MaxAge() {
		t.Fatalf("max-age must be set")
	}
	s := c.String()
	if !strings.Contains(s, "; max-age=100") {
		t.Fatalf("missing max-age flag in cookie %q", s)
	}

	if err := c.Parse("foo=bar; expires=Tue, 10 Nov 2009 23:00:00 GMT; max-age=100;"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if maxAge != c.MaxAge() {
		t.Fatalf("max-age ignored")
	}
	s = c.String()
	if s != "foo=bar; max-age=100" {
		t.Fatalf("missing max-age in cookie %q", s)
	}

	expires := time.Unix(100, 0)
	c.SetExpire(expires)
	s = c.String()
	if s != "foo=bar; max-age=100" {
		t.Fatalf("expires should be ignored due to max-age: %q", s)
	}

	c.SetMaxAge(0)
	s = c.String()
	if s != "foo=bar; expires=Thu, 01 Jan 1970 00:01:40 GMT" {
		t.Fatalf("missing expires %q", s)
	}

	c.SetMaxAge(-100)
	result := strings.ToLower(c.String())
	const expectedMaxAge0 = "max-age=0"
	if !strings.Contains(result, expectedMaxAge0) {
		t.Fatalf("Unexpected cookie %q. Should contain %q", result, expectedMaxAge0)
	}
}

func TestCookieHttpOnly(t *testing.T) {
	t.Parallel()

	var c Cookie

	if err := c.Parse("foo=bar; HttpOnly"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.HTTPOnly() {
		t.Fatalf("HTTPOnly must be set")
	}
	s := c.String()
	if !strings.Contains(s, "; HttpOnly") {
		t.Fatalf("missing HttpOnly flag in cookie %q", s)
	}

	if err := c.Parse("foo=bar"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.HTTPOnly() {
		t.Fatalf("Unexpected HTTPOnly flag set")
	}
	s = c.String()
	if strings.Contains(s, "HttpOnly") {
		t.Fatalf("unexpected HttpOnly flag in cookie %q", s)
	}
}

func TestCookiePartitioned(t *testing.T) {
	t.Parallel()

	var c Cookie

	if err := c.Parse("foo=bar; PATH=/; secure; Partitioned"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.Partitioned() {
		t.Fatalf("Partitioned must be set")
	}
	s := c.String()
	if !strings.Contains(s, "; Partitioned") {
		t.Fatalf("missing Partitioned flag in cookie %q", s)
	}

	if !c.Secure() {
		t.Fatalf("secure must be set")
	}
	s = c.String()
	if !strings.Contains(s, "; secure") {
		t.Fatalf("missing secure flag in cookie %q", s)
	}

	if string(c.Path()) != "/" {
		t.Fatalf("path must be set /")
	}
}

func TestCookieAcquireReleaseSequential(t *testing.T) {
	t.Parallel()

	testCookieAcquireRelease(t)
}

func TestCookieAcquireReleaseConcurrent(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{}, 10)
	for range 10 {
		go func() {
			testCookieAcquireRelease(t)
			ch <- struct{}{}
		}()
	}
	for range 10 {
		select {
		case <-ch:
		case <-time.After(testTimeout(time.Second)):
			t.Fatalf("timeout")
		}
	}
}

func testCookieAcquireRelease(t *testing.T) {
	c := AcquireCookie()

	key := "foo"
	c.SetKey(key)

	value := "bar"
	c.SetValue(value)

	domain := "foo.bar.com"
	c.SetDomain(domain)

	path := "/foi/bar/aaa"
	c.SetPath(path)

	s := c.String()
	c.Reset()
	if err := c.Parse(s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(c.Key()) != key {
		t.Fatalf("unexpected cookie name %q. Expecting %q", c.Key(), key)
	}
	if string(c.Value()) != value {
		t.Fatalf("unexpected cookie value %q. Expecting %q", c.Value(), value)
	}
	if string(c.Domain()) != domain {
		t.Fatalf("unexpected domain %q. Expecting %q", c.Domain(), domain)
	}
	if string(c.Path()) != path {
		t.Fatalf("unexpected path %q. Expecting %q", c.Path(), path)
	}

	ReleaseCookie(c)
}

func TestCookieParse(t *testing.T) {
	t.Parallel()

	testCookieParse(t, "foo", "foo")
	testCookieParse(t, "foo=bar", "foo=bar")
	testCookieParse(t, "foo=", "foo=")
	testCookieParse(t, `foo="bar"`, "foo=bar")
	testCookieParse(t, `"foo"=bar`, `"foo"=bar`)
	testCookieParse(t, "foo=bar; Domain=aaa.com; PATH=/foo/bar", "foo=bar; domain=aaa.com; path=/foo/bar")
	testCookieParse(t, "foo=bar; max-age= 101 ; expires= Tue, 10 Nov 2009 23:00:00 GMT", "foo=bar; max-age=101")
	testCookieParse(t, " xxx = yyy  ; path=/a/b;;;domain=foobar.com ; expires= Tue, 10 Nov 2009 23:00:00 GMT ; ;;",
		"xxx=yyy; expires=Tue, 10 Nov 2009 23:00:00 GMT; domain=foobar.com; path=/a/b")
}

func testCookieParse(t *testing.T, s, expectedS string) {
	var c Cookie
	if err := c.Parse(s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	result := string(c.Cookie())
	if result != expectedS {
		t.Fatalf("unexpected cookies %q. Expecting %q. Original %q", result, expectedS, s)
	}
}

func TestCookieAppendBytes(t *testing.T) {
	t.Parallel()

	c := &Cookie{}

	testCookieAppendBytes(t, c, "", "bar", "bar")
	testCookieAppendBytes(t, c, "foo", "", "foo=")
	testCookieAppendBytes(t, c, "ффф", "12 лодлы", "ффф=12 лодлы")

	c.SetDomain("foobar.com")
	testCookieAppendBytes(t, c, "a", "b", "a=b; domain=foobar.com")

	c.SetPath("/a/b")
	testCookieAppendBytes(t, c, "aa", "bb", "aa=bb; domain=foobar.com; path=/a/b")

	c.SetExpire(CookieExpireDelete)
	testCookieAppendBytes(t, c, "xxx", "yyy", "xxx=yyy; expires=Tue, 10 Nov 2009 23:00:00 GMT; domain=foobar.com; path=/a/b")
}

func testCookieAppendBytes(t *testing.T, c *Cookie, key, value, expectedS string) {
	c.SetKey(key)
	c.SetValue(value)
	result := string(c.AppendBytes(nil))
	if result != expectedS {
		t.Fatalf("Unexpected cookie %q. Expecting %q", result, expectedS)
	}
}

func TestParseRequestCookies(t *testing.T) {
	t.Parallel()

	testParseRequestCookies(t, "", "")
	testParseRequestCookies(t, "=", "")
	testParseRequestCookies(t, "foo", "foo")
	testParseRequestCookies(t, "=foo", "foo")
	testParseRequestCookies(t, "bar=", "bar=")
	testParseRequestCookies(t, "xxx=aa;bb=c; =d; ;;e=g", "xxx=aa; bb=c; d; e=g")
	testParseRequestCookies(t, "a;b;c; d=1;d=2", "a; b; c; d=1; d=2")
	testParseRequestCookies(t, "   %D0%B8%D0%B2%D0%B5%D1%82=a%20b%3Bc   ;s%20s=aaa  ", "%D0%B8%D0%B2%D0%B5%D1%82=a%20b%3Bc; s%20s=aaa")
}

func TestParseRequestCookiesSkipsInvalidQuotedValues(t *testing.T) {
	t.Parallel()

	testParseRequestCookies(t, `session="abc; admin=true`, "admin=true")
	testParseRequestCookies(t, `session=abc"; admin=true`, "admin=true")
	testParseRequestCookies(t, `session="abc\"; admin=true`, "admin=true")
}

func testParseRequestCookies(t *testing.T, s, expectedS string) {
	cookies := parseRequestCookies(nil, []byte(s))
	ss := string(appendRequestCookieBytes(nil, cookies))
	if ss != expectedS {
		t.Fatalf("Unexpected cookies after parsing: %q. Expecting %q. String to parse %q", ss, expectedS, s)
	}
}

func TestAppendRequestCookieBytes(t *testing.T) {
	t.Parallel()

	testAppendRequestCookieBytes(t, "=", "")
	testAppendRequestCookieBytes(t, "foo=", "foo=")
	testAppendRequestCookieBytes(t, "=bar", "bar")
	testAppendRequestCookieBytes(t, "привет=a bc&s s=aaa", "привет=a bc; s s=aaa")
}

func testAppendRequestCookieBytes(t *testing.T, s, expectedS string) {
	kvs := strings.Split(s, "&")
	cookies := make([]argsKV, 0, len(kvs))
	for _, ss := range kvs {
		tmp := strings.SplitN(ss, "=", 2)
		if len(tmp) != 2 {
			t.Fatalf("Cannot find '=' in %q, part of %q", ss, s)
		}
		cookies = append(cookies, argsKV{
			key:   []byte(tmp[0]),
			value: []byte(tmp[1]),
		})
	}

	prefix := "foobar"
	result := string(appendRequestCookieBytes([]byte(prefix), cookies))
	if result[:len(prefix)] != prefix {
		t.Fatalf("unexpected prefix %q. Expecting %q for cookie %q", result[:len(prefix)], prefix, s)
	}
	result = result[len(prefix):]
	if result != expectedS {
		t.Fatalf("Unexpected result %q. Expecting %q for cookie %q", result, expectedS, s)
	}
}

func TestCookieParseInvalidExpires(t *testing.T) {
	t.Parallel()

	var c Cookie
	for _, tc := range []struct {
		name   string
		input  string
		expire time.Time
		output string
	}{
		{"later attributes", "foo=bar; expires=never; path=/; Secure; HttpOnly", CookieExpireUnlimited, "foo=bar; path=/; HttpOnly; secure"},
		{"garbage", "foo=bar; expires=0; samesite=strict", CookieExpireUnlimited, "foo=bar; SameSite=Strict"},
		{"empty", "foo=bar; expires=; domain=example.com", CookieExpireUnlimited, "foo=bar; domain=example.com"},
		{"valid kept", "foo=bar; expires=Tue, 10 Nov 2009 23:00:00 GMT; expires=bogus", time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC), "foo=bar; expires=Tue, 10 Nov 2009 23:00:00 GMT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Parse(tc.input); err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if got := c.Expire(); !got.Equal(tc.expire) {
				t.Errorf("unexpected expire: got %v, want %v", got, tc.expire)
			}
			if got := c.String(); got != tc.output {
				t.Errorf("unexpected cookie: got %q, want %q", got, tc.output)
			}
		})
	}
}

func TestCookieParseExpiresTimeZone(t *testing.T) {
	t.Parallel()

	expire := time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)
	var c Cookie
	for _, input := range []string{
		"foo=bar; expires=Tue, 10 Nov 2009 23:00:00 GMT",
		"foo=bar; expires=Tue, 10 Nov 2009 23:00:00 UTC",
		"foo=bar; expires=Tue, 10-Nov-2009 23:00:00 GMT",
		"foo=bar; expires=Tue, 10-Nov-2009 23:00:00 UTC",
	} {
		if err := c.Parse(input); err != nil {
			t.Fatalf("unexpected parse error for %q: %v", input, err)
		}
		if got := c.Expire(); !got.Equal(expire) {
			t.Errorf("unexpected expire for %q: got %v, want %v", input, got, expire)
		}
	}
}

func TestCookieParseZeroMaxAge(t *testing.T) {
	t.Parallel()

	var c Cookie
	for _, tc := range []struct {
		name   string
		input  string
		maxAge int
		output string
	}{
		{"zero", "foo=bar; max-age=0", -1, "foo=bar; max-age=0"},
		{"case insensitive", "foo=bar; Max-Age=0", -1, "foo=bar; max-age=0"},
		{"expires", "foo=bar; expires=Wed, 10 Nov 2027 23:00:00 GMT; max-age=0", -1, "foo=bar; max-age=0"},
		{"zero overrides positive", "foo=bar; max-age=100; max-age=0", -1, "foo=bar; max-age=0"},
		{"positive overrides zero", "foo=bar; max-age=0; max-age=100", 100, "foo=bar; max-age=100"},
		{"absent", "foo=bar", 0, "foo=bar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Parse(tc.input); err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if got := c.MaxAge(); got != tc.maxAge {
				t.Errorf("unexpected max age: got %d, want %d", got, tc.maxAge)
			}
			if got := c.String(); got != tc.output {
				t.Errorf("unexpected cookie: got %q, want %q", got, tc.output)
			}
		})
	}
}

func TestCookieParseNegativeMaxAge(t *testing.T) {
	t.Parallel()

	var c Cookie
	for _, tc := range []struct {
		name   string
		input  string
		maxAge int
		output string
	}{
		{"negative", "foo=bar; max-age=-1", -1, "foo=bar; max-age=0"},
		{"case insensitive", "foo=bar; Max-Age=-3600", -1, "foo=bar; max-age=0"},
		{"negative zero", "foo=bar; max-age=-0", -1, "foo=bar; max-age=0"},
		{"later attributes", "foo=bar; max-age=-1; path=/; HttpOnly", -1, "foo=bar; max-age=0; path=/; HttpOnly"},
		{"expires", "foo=bar; expires=Wed, 10 Nov 2027 23:00:00 GMT; max-age=-1", -1, "foo=bar; max-age=0"},
		{"negative overrides positive", "foo=bar; max-age=100; max-age=-1", -1, "foo=bar; max-age=0"},
		{"positive overrides negative", "foo=bar; max-age=-1; max-age=100", 100, "foo=bar; max-age=100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Parse(tc.input); err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if got := c.MaxAge(); got != tc.maxAge {
				t.Errorf("unexpected max age: got %d, want %d", got, tc.maxAge)
			}
			if got := c.String(); got != tc.output {
				t.Errorf("unexpected cookie: got %q, want %q", got, tc.output)
			}
		})
	}
}

func TestCookieParseInvalidMaxAge(t *testing.T) {
	t.Parallel()

	var c Cookie
	for _, tc := range []struct {
		name   string
		input  string
		maxAge int
		output string
	}{
		{"later attributes", "foo=bar; max-age=never; path=/; Secure; HttpOnly", 0, "foo=bar; path=/; HttpOnly; secure"},
		{"empty", "foo=bar; max-age=; domain=example.com", 0, "foo=bar; domain=example.com"},
		{"fraction", "foo=bar; max-age=1.5; samesite=strict", 0, "foo=bar; SameSite=Strict"},
		{"overflow", "foo=bar; max-age=99999999999999999999; secure", 0, "foo=bar; secure"},
		{"sign only", "foo=bar; max-age=-", 0, "foo=bar"},
		{"double sign", "foo=bar; max-age=--1", 0, "foo=bar"},
		{"negative garbage", "foo=bar; max-age=-1s", 0, "foo=bar"},
		{"valid kept", "foo=bar; max-age=100; max-age=bogus", 100, "foo=bar; max-age=100"},
		{"expires kept", "foo=bar; expires=Tue, 10 Nov 2009 23:00:00 GMT; max-age=bogus", 0, "foo=bar; expires=Tue, 10 Nov 2009 23:00:00 GMT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Parse(tc.input); err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if got := c.MaxAge(); got != tc.maxAge {
				t.Errorf("unexpected max age: got %d, want %d", got, tc.maxAge)
			}
			if got := c.String(); got != tc.output {
				t.Errorf("unexpected cookie: got %q, want %q", got, tc.output)
			}
		})
	}
}

func TestCookieParseFlagsWithValue(t *testing.T) {
	t.Parallel()

	var c Cookie
	for _, tc := range []struct {
		name   string
		input  string
		output string
	}{
		{"empty values", "foo=bar; Secure=; HttpOnly=; Partitioned=", "foo=bar; HttpOnly; secure; Partitioned"},
		{"values", "foo=bar; secure=true; httponly=1; partitioned=yes", "foo=bar; HttpOnly; secure; Partitioned"},
		{"later attributes", "foo=bar; Secure=1; HttpOnly=1; Partitioned=1; path=/", "foo=bar; path=/; HttpOnly; secure; Partitioned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Parse(tc.input); err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if !c.Secure() {
				t.Error("Secure must be set")
			}
			if !c.HTTPOnly() {
				t.Error("HttpOnly must be set")
			}
			if !c.Partitioned() {
				t.Error("Partitioned must be set")
			}
			if got := c.String(); got != tc.output {
				t.Errorf("unexpected cookie: got %q, want %q", got, tc.output)
			}
		})
	}
}

func TestCookieParseTrimsTabs(t *testing.T) {
	t.Parallel()

	var c Cookie
	if err := c.Parse("sid\t=\tabc\t;\tPath=/;\tSecure;\tHttpOnly\t"); err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if string(c.Key()) != "sid" {
		t.Errorf("unexpected key: %q", c.Key())
	}
	if string(c.Value()) != "abc" {
		t.Errorf("unexpected value: %q", c.Value())
	}
	if string(c.Path()) != "/" {
		t.Errorf("unexpected path: %q", c.Path())
	}
	if !c.Secure() {
		t.Error("Secure must be set")
	}
	if !c.HTTPOnly() {
		t.Error("HttpOnly must be set")
	}

	var h RequestHeader
	h.Set(HeaderCookie, "a=b;\tc=d")
	if v := h.Cookie("c"); string(v) != "d" {
		t.Errorf("unexpected request cookie value: %q", v)
	}
}

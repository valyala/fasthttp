package fasthttp

import (
	"strings"
	"testing"
)

func TestParseCookies(t *testing.T) {
	testParseCookies(t, "")
	testParseCookies(t, "=")
	testParseCookies(t, "=foo")
	testParseCookies(t, "bar=")
	testParseCookies(t, "%D0%BF%D1%80%D0%B8%D0%B2%D0%B5%D1%82=a%20b%3Bc; s%20s=aaa")
}

func testParseCookies(t *testing.T, s string) {
	var kv argsKV
	cookies := parseCookies(nil, []byte(s), &kv)
	ss := string(appendCookieBytes(nil, cookies))
	if s != ss {
		t.Fatalf("Unexpected cookies after parsing: %q. Expected %q. cookies=%#v", ss, s, cookies)
	}
}

func TestAppendCookieBytes(t *testing.T) {
	testAppendCookieBytes(t, "=", "=")
	testAppendCookieBytes(t, "foo=", "foo=")
	testAppendCookieBytes(t, "=bar", "=bar")
	testAppendCookieBytes(t, "привет=a b;c&s s=aaa", "%D0%BF%D1%80%D0%B8%D0%B2%D0%B5%D1%82=a%20b%3Bc; s%20s=aaa")
}

func testAppendCookieBytes(t *testing.T, s, expectedS string) {
	var cookies []argsKV
	for _, ss := range strings.Split(s, "&") {
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
	result := string(appendCookieBytes([]byte(prefix), cookies))
	if result[:len(prefix)] != prefix {
		t.Fatalf("unexpected prefix %q. Expected %q for cookie %q", result[:len(prefix)], prefix, s)
	}
	result = result[len(prefix):]
	if result != expectedS {
		t.Fatalf("Unexpected result %q. Expected %q for cookie %q", result, expectedS, s)
	}
}

package fasthttp

import (
	"bytes"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestURICopyToQueryArgs(t *testing.T) {
	t.Parallel()

	var u URI
	a := u.QueryArgs()
	a.Set("foo", "bar")

	var u1 URI
	u.CopyTo(&u1)
	a1 := u1.QueryArgs()

	if string(a1.Peek("foo")) != "bar" {
		t.Fatalf("unexpected query args value %q. Expecting %q", a1.Peek("foo"), "bar")
	}
}

func TestURIAcquireReleaseSequential(t *testing.T) {
	t.Parallel()

	testURIAcquireRelease(t)
}

func TestURIAcquireReleaseConcurrent(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			testURIAcquireRelease(t)
			ch <- struct{}{}
		}()
	}

	for i := 0; i < 10; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}
}

func testURIAcquireRelease(t *testing.T) {
	for i := 0; i < 10; i++ {
		u := AcquireURI()
		host := fmt.Sprintf("host.%d.com", i*23)
		path := fmt.Sprintf("/foo/%d/bar", i*17)
		queryArgs := "?foo=bar&baz=aass"
		u.Parse([]byte(host), []byte(path+queryArgs)) //nolint:errcheck
		if string(u.Host()) != host {
			t.Fatalf("unexpected host %q. Expecting %q", u.Host(), host)
		}
		if string(u.Path()) != path {
			t.Fatalf("unexpected path %q. Expecting %q", u.Path(), path)
		}
		ReleaseURI(u)
	}
}

func TestURILastPathSegment(t *testing.T) {
	t.Parallel()

	testURILastPathSegment(t, "", "")
	testURILastPathSegment(t, "/", "")
	testURILastPathSegment(t, "/foo/bar/", "")
	testURILastPathSegment(t, "/foobar.js", "foobar.js")
	testURILastPathSegment(t, "/foo/bar/baz.html", "baz.html")
}

func testURILastPathSegment(t *testing.T, path, expectedSegment string) {
	var u URI
	u.SetPath(path)
	segment := u.LastPathSegment()
	if string(segment) != expectedSegment {
		t.Fatalf("unexpected last path segment for path %q: %q. Expecting %q", path, segment, expectedSegment)
	}
}

func TestURIPathEscape(t *testing.T) {
	t.Parallel()

	testURIPathEscape(t, "/foo/bar", "/foo/bar")
	testURIPathEscape(t, "/f_o-o=b:ar,b.c&q", "/f_o-o=b:ar,b.c&q")
	testURIPathEscape(t, "/aa?bb.тест~qq", "/aa%3Fbb.%D1%82%D0%B5%D1%81%D1%82~qq")
}

func testURIPathEscape(t *testing.T, path, expectedRequestURI string) {
	var u URI
	u.SetPath(path)
	requestURI := u.RequestURI()
	if string(requestURI) != expectedRequestURI {
		t.Fatalf("unexpected requestURI %q. Expecting %q. path %q", requestURI, expectedRequestURI, path)
	}
}

func TestURIUpdate(t *testing.T) {
	t.Parallel()

	// full uri
	testURIUpdate(t, "http://example.net/dir/path1.html?param1=val1#fragment1", "https://example.com/dir/path2.html", "https://example.com/dir/path2.html")

	// empty uri
	testURIUpdate(t, "http://example.com/dir/path1.html?param1=val1#fragment1", "", "http://example.com/dir/path1.html?param1=val1#fragment1")

	// request uri
	testURIUpdate(t, "http://example.com/dir/path1.html?param1=val1#fragment1", "/dir/path2.html?param2=val2#fragment2", "http://example.com/dir/path2.html?param2=val2#fragment2")

	// schema
	testURIUpdate(t, "http://example.com/dir/path1.html?param1=val1#fragment1", "https://example.com/dir/path1.html?param1=val1#fragment1", "https://example.com/dir/path1.html?param1=val1#fragment1")

	// relative uri
	testURIUpdate(t, "http://example.com/baz/xxx.html?aaa=22#aaa", "bb.html?xx=12#pp", "http://example.com/baz/bb.html?xx=12#pp")

	testURIUpdate(t, "http://example.com/aaa.html?foo=bar", "?baz=434&aaa#xcv", "http://example.com/aaa.html?baz=434&aaa#xcv")
	testURIUpdate(t, "http://example.com/baz", "~a/%20b=c,тест?йцу=ке", "http://example.com/~a/%20b=c,%D1%82%D0%B5%D1%81%D1%82?йцу=ке")
	testURIUpdate(t, "http://example.com/baz", "/qwe#fragment", "http://example.com/qwe#fragment")
	testURIUpdate(t, "http://example.com/baz/xxx", "aaa.html#bb?cc=dd&ee=dfd", "http://example.com/baz/aaa.html#bb?cc=dd&ee=dfd")

	if runtime.GOOS != "windows" {
		testURIUpdate(t, "http://example.com/a/b/c/d", "../qwe/p?zx=34", "http://example.com/a/b/qwe/p?zx=34")
	}

	// hash
	testURIUpdate(t, "http://example.com/#fragment1", "#fragment2", "http://example.com/#fragment2")

	// uri without scheme
	testURIUpdate(t, "https://example.net/dir/path1.html", "//example.com/dir/path2.html", "https://example.com/dir/path2.html")
	testURIUpdate(t, "http://example.net/dir/path1.html", "//example.com/dir/path2.html", "http://example.com/dir/path2.html")
	// host with port
	testURIUpdate(t, "http://example.net/", "//example.com:8080/", "http://example.com:8080/")
}

func testURIUpdate(t *testing.T, base, update, result string) {
	var u URI
	u.Parse(nil, []byte(base)) //nolint:errcheck
	u.Update(update)
	s := u.String()
	if s != result {
		t.Fatalf("unexpected result %q. Expecting %q. base=%q, update=%q", s, result, base, update)
	}
}

func TestURIPathNormalize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.SkipNow()
	}

	t.Parallel()

	var u URI

	// double slash
	testURIPathNormalize(t, &u, "/aa//bb", "/aa/bb")

	// triple slash
	testURIPathNormalize(t, &u, "/x///y/", "/x/y/")

	// multi slashes
	testURIPathNormalize(t, &u, "/abc//de///fg////", "/abc/de/fg/")

	// encoded slashes
	testURIPathNormalize(t, &u, "/xxxx%2fyyy%2f%2F%2F", "/xxxx/yyy/")

	// dotdot
	testURIPathNormalize(t, &u, "/aaa/..", "/")

	// dotdot with trailing slash
	testURIPathNormalize(t, &u, "/xxx/yyy/../", "/xxx/")

	// multi dotdots
	testURIPathNormalize(t, &u, "/aaa/bbb/ccc/../../ddd", "/aaa/ddd")

	// dotdots separated by other data
	testURIPathNormalize(t, &u, "/a/b/../c/d/../e/..", "/a/c/")

	// too many dotdots
	testURIPathNormalize(t, &u, "/aaa/../../../../xxx", "/xxx")
	testURIPathNormalize(t, &u, "/../../../../../..", "/")
	testURIPathNormalize(t, &u, "/../../../../../../", "/")

	// encoded dotdots
	testURIPathNormalize(t, &u, "/aaa%2Fbbb%2F%2E.%2Fxxx", "/aaa/xxx")

	// double slash with dotdots
	testURIPathNormalize(t, &u, "/aaa////..//b", "/b")

	// fake dotdot
	testURIPathNormalize(t, &u, "/aaa/..bbb/ccc/..", "/aaa/..bbb/")

	// single dot
	testURIPathNormalize(t, &u, "/a/./b/././c/./d.html", "/a/b/c/d.html")
	testURIPathNormalize(t, &u, "./foo/", "/foo/")
	testURIPathNormalize(t, &u, "./../.././../../aaa/bbb/../../../././../", "/")
	testURIPathNormalize(t, &u, "./a/./.././../b/./foo.html", "/b/foo.html")
}

func testURIPathNormalize(t *testing.T, u *URI, requestURI, expectedPath string) {
	u.Parse(nil, []byte(requestURI)) //nolint:errcheck
	if string(u.Path()) != expectedPath {
		t.Fatalf("Unexpected path %q. Expected %q. requestURI=%q", u.Path(), expectedPath, requestURI)
	}
}

func TestURINoNormalization(t *testing.T) {
	t.Parallel()

	var u URI
	irregularPath := "/aaa%2Fbbb%2F%2E.%2Fxxx"
	u.Parse(nil, []byte(irregularPath)) //nolint:errcheck
	u.DisablePathNormalizing = true
	if string(u.RequestURI()) != irregularPath {
		t.Fatalf("Unexpected path %q. Expected %q.", u.Path(), irregularPath)
	}
}

func TestURICopyTo(t *testing.T) {
	t.Parallel()

	var u URI
	var copyU URI
	u.CopyTo(&copyU)
	if !reflect.DeepEqual(u, copyU) { //nolint:govet
		t.Fatalf("URICopyTo fail, u: \n%+v\ncopyu: \n%+v\n", u, copyU) //nolint:govet
	}

	u.UpdateBytes([]byte("https://example.com/foo?bar=baz&baraz#qqqq"))
	u.CopyTo(&copyU)
	if !reflect.DeepEqual(u, copyU) { //nolint:govet
		t.Fatalf("URICopyTo fail, u: \n%+v\ncopyu: \n%+v\n", u, copyU) //nolint:govet
	}

}

func TestURIFullURI(t *testing.T) {
	t.Parallel()

	var args Args

	// empty scheme, path and hash
	testURIFullURI(t, "", "example.com", "", "", &args, "http://example.com/")

	// empty scheme and hash
	testURIFullURI(t, "", "example.com", "/foo/bar", "", &args, "http://example.com/foo/bar")

	// empty hash
	testURIFullURI(t, "fTP", "example.com", "/foo", "", &args, "ftp://example.com/foo")

	// empty args
	testURIFullURI(t, "https", "example.com", "/", "aaa", &args, "https://example.com/#aaa")

	// non-empty args and non-ASCII path
	args.Set("foo", "bar")
	args.Set("xxx", "йух")
	testURIFullURI(t, "", "example.com", "/тест123", "2er", &args, "http://example.com/%D1%82%D0%B5%D1%81%D1%82123?foo=bar&xxx=%D0%B9%D1%83%D1%85#2er")

	// test with empty args and non-empty query string
	var u URI
	u.Parse([]byte("example.com"), []byte("/foo?bar=baz&baraz#qqqq")) //nolint:errcheck
	uri := u.FullURI()
	expectedURI := "http://example.com/foo?bar=baz&baraz#qqqq"
	if string(uri) != expectedURI {
		t.Fatalf("Unexpected URI: %q. Expected %q", uri, expectedURI)
	}
}

func testURIFullURI(t *testing.T, scheme, host, path, hash string, args *Args, expectedURI string) {
	var u URI

	u.SetScheme(scheme)
	u.SetHost(host)
	u.SetPath(path)
	u.SetHash(hash)
	args.CopyTo(u.QueryArgs())

	uri := u.FullURI()
	if string(uri) != expectedURI {
		t.Fatalf("Unexpected URI: %q. Expected %q", uri, expectedURI)
	}
}

func TestURIParseNilHost(t *testing.T) {
	t.Parallel()

	testURIParseScheme(t, "http://example.com/foo?bar#baz", "http", "example.com", "/foo?bar", "baz")
	testURIParseScheme(t, "HTtP://example.com/", "http", "example.com", "/", "")
	testURIParseScheme(t, "://example.com/xyz", "http", "example.com", "/xyz", "")
	testURIParseScheme(t, "//example.com/foobar", "http", "example.com", "/foobar", "")
	testURIParseScheme(t, "fTP://example.com", "ftp", "example.com", "/", "")
	testURIParseScheme(t, "httPS://example.com", "https", "example.com", "/", "")

	// missing slash after hostname
	testURIParseScheme(t, "http://example.com?baz=111", "http", "example.com", "/?baz=111", "")

	// slash in args
	testURIParseScheme(t, "http://example.com?baz=111/222/xyz", "http", "example.com", "/?baz=111/222/xyz", "")
	testURIParseScheme(t, "http://example.com?111/222/xyz", "http", "example.com", "/?111/222/xyz", "")
}

func testURIParseScheme(t *testing.T, uri, expectedScheme, expectedHost, expectedRequestURI, expectedHash string) {
	var u URI
	u.Parse(nil, []byte(uri)) //nolint:errcheck
	if string(u.Scheme()) != expectedScheme {
		t.Fatalf("Unexpected scheme %q. Expecting %q for uri %q", u.Scheme(), expectedScheme, uri)
	}
	if string(u.Host()) != expectedHost {
		t.Fatalf("Unexepcted host %q. Expecting %q for uri %q", u.Host(), expectedHost, uri)
	}
	if string(u.RequestURI()) != expectedRequestURI {
		t.Fatalf("Unexepcted requestURI %q. Expecting %q for uri %q", u.RequestURI(), expectedRequestURI, uri)
	}
	if string(u.hash) != expectedHash {
		t.Fatalf("Unexepcted hash %q. Expecting %q for uri %q", u.hash, expectedHash, uri)
	}
}

func TestIsHttp(t *testing.T) {
	var u URI
	if !u.isHttp() || u.isHttps() {
		t.Fatalf("http scheme is assumed by default and not https")
	}
	u.SetSchemeBytes([]byte{})
	if !u.isHttp() || u.isHttps() {
		t.Fatalf("empty scheme must be threaten as http and not https")
	}
	u.SetScheme("http")
	if !u.isHttp() || u.isHttps() {
		t.Fatalf("scheme must be threaten as http and not https")
	}
	u.SetScheme("https")
	if !u.isHttps() || u.isHttp() {
		t.Fatalf("scheme must be threaten as https and not http")
	}
	u.SetScheme("dav")
	if u.isHttps() || u.isHttp() {
		t.Fatalf("scheme must be threaten as not http and not https")
	}
}

func TestURIParse(t *testing.T) {
	t.Parallel()

	var u URI

	// no args
	testURIParse(t, &u, "example.com", "sdfdsf",
		"http://example.com/sdfdsf", "example.com", "/sdfdsf", "sdfdsf", "", "")

	// args
	testURIParse(t, &u, "example.com", "/aa?ss",
		"http://example.com/aa?ss", "example.com", "/aa", "/aa", "ss", "")

	// args and hash
	testURIParse(t, &u, "example.com", "/a.b.c?def=gkl#mnop",
		"http://example.com/a.b.c?def=gkl#mnop", "example.com", "/a.b.c", "/a.b.c", "def=gkl", "mnop")

	// '?' and '#' in hash
	testURIParse(t, &u, "example.com", "/foo#bar?baz=aaa#bbb",
		"http://example.com/foo#bar?baz=aaa#bbb", "example.com", "/foo", "/foo", "", "bar?baz=aaa#bbb")

	// encoded path
	testURIParse(t, &u, "example.com", "/Test%20+%20%D0%BF%D1%80%D0%B8?asdf=%20%20&s=12#sdf",
		"http://example.com/Test%20+%20%D0%BF%D1%80%D0%B8?asdf=%20%20&s=12#sdf", "example.com", "/Test + при", "/Test%20+%20%D0%BF%D1%80%D0%B8", "asdf=%20%20&s=12", "sdf")

	// host in uppercase
	testURIParse(t, &u, "example.com", "/bC?De=F#Gh",
		"http://example.com/bC?De=F#Gh", "example.com", "/bC", "/bC", "De=F", "Gh")

	// uri with hostname
	testURIParse(t, &u, "example.com", "http://example.com/foo/bar?baz=aaa#ddd",
		"http://example.com/foo/bar?baz=aaa#ddd", "example.com", "/foo/bar", "/foo/bar", "baz=aaa", "ddd")
	testURIParse(t, &u, "example.net", "https://example.com/f/b%20r?baz=aaa#ddd",
		"https://example.com/f/b%20r?baz=aaa#ddd", "example.com", "/f/b r", "/f/b%20r", "baz=aaa", "ddd")

	// no slash after hostname in uri
	testURIParse(t, &u, "example.com", "http://example.com",
		"http://example.com/", "example.com", "/", "/", "", "")

	// uppercase hostname in uri
	testURIParse(t, &u, "example.net", "http://EXAMPLE.COM/aaa",
		"http://example.com/aaa", "example.com", "/aaa", "/aaa", "", "")

	// http:// in query params
	testURIParse(t, &u, "example.com", "/foo?bar=http://example.org",
		"http://example.com/foo?bar=http://example.org", "example.com", "/foo", "/foo", "bar=http://example.org", "")

	testURIParse(t, &u, "example.com", "//relative",
		"http://example.com/relative", "example.com", "/relative", "//relative", "", "")

	testURIParse(t, &u, "", "//example.com//absolute",
		"http://example.com/absolute", "example.com", "/absolute", "//absolute", "", "")

	testURIParse(t, &u, "", "//example.com\r\n\r\nGET x",
		"http:///", "", "/", "", "", "")

	testURIParse(t, &u, "", "http://[fe80::1%25en0]/",
		"http://[fe80::1%en0]/", "[fe80::1%en0]", "/", "/", "", "")

	testURIParse(t, &u, "", "http://[fe80::1%25en0]:8080/",
		"http://[fe80::1%en0]:8080/", "[fe80::1%en0]:8080", "/", "/", "", "")

	testURIParse(t, &u, "", "http://hello.世界.com/foo",
		"http://hello.世界.com/foo", "hello.世界.com", "/foo", "/foo", "", "")

	testURIParse(t, &u, "", "http://hello.%e4%b8%96%e7%95%8c.com/foo",
		"http://hello.世界.com/foo", "hello.世界.com", "/foo", "/foo", "", "")
}

func testURIParse(t *testing.T, u *URI, host, uri,
	expectedURI, expectedHost, expectedPath, expectedPathOriginal, expectedArgs, expectedHash string) {
	u.Parse([]byte(host), []byte(uri)) //nolint:errcheck

	if !bytes.Equal(u.FullURI(), []byte(expectedURI)) {
		t.Fatalf("Unexpected uri %q. Expected %q. host=%q, uri=%q", u.FullURI(), expectedURI, host, uri)
	}
	if !bytes.Equal(u.Host(), []byte(expectedHost)) {
		t.Fatalf("Unexpected host %q. Expected %q. host=%q, uri=%q", u.Host(), expectedHost, host, uri)
	}
	if !bytes.Equal(u.PathOriginal(), []byte(expectedPathOriginal)) {
		t.Fatalf("Unexpected original path %q. Expected %q. host=%q, uri=%q", u.PathOriginal(), expectedPathOriginal, host, uri)
	}
	if !bytes.Equal(u.Path(), []byte(expectedPath)) {
		t.Fatalf("Unexpected path %q. Expected %q. host=%q, uri=%q", u.Path(), expectedPath, host, uri)
	}
	if !bytes.Equal(u.QueryString(), []byte(expectedArgs)) {
		t.Fatalf("Unexpected args %q. Expected %q. host=%q, uri=%q", u.QueryString(), expectedArgs, host, uri)
	}
	if !bytes.Equal(u.Hash(), []byte(expectedHash)) {
		t.Fatalf("Unexpected hash %q. Expected %q. host=%q, uri=%q", u.Hash(), expectedHash, host, uri)
	}
}

func TestURIWithQuerystringOverride(t *testing.T) {
	t.Parallel()

	var u URI
	u.SetQueryString("q1=foo&q2=bar")
	u.QueryArgs().Add("q3", "baz")
	u.SetQueryString("q1=foo&q2=bar&q4=quux")
	uriString := string(u.RequestURI())

	if uriString != "/?q1=foo&q2=bar&q4=quux" {
		t.Fatalf("Expected Querystring to be overridden but was %q ", uriString)
	}
}

func TestInvalidUrl(t *testing.T) {
	url := `https://.çèéà@&~!&:=\\/\"'~<>|+-*()[]{}%$;,¥&&$22|||<>< 4ly8lzjmoNx233AXELDtyaFQiiUH-fd8c-CnXUJVYnGIs4Uwr-bptom5GCnWtsGMQxeM2ZhoKE973eKgs2Sjh6RePnyaLpCi6SiNSLevcMoraARrp88L-SgtKqd-XHAtSI8hiPRiXPQmDIA4BGhSgoc0nfn1PoYuGKKmDcZ04tANRc3iz4aF4-A1UrO8bLHTH7MEJvzx.someqa.fr/A/?&QS_BEGIN<&8{b'Ob=p*f> QS_END`

	u := AcquireURI()
	defer ReleaseURI(u)

	if err := u.Parse(nil, []byte(url)); err == nil {
		t.Fail()
	}
}

func TestNoOverwriteInput(t *testing.T) {
	str := `//%AA`
	url := []byte(str)

	u := AcquireURI()
	defer ReleaseURI(u)

	if err := u.Parse(nil, url); err != nil {
		t.Error(err)
	}

	if string(url) != str {
		t.Error()
	}

	if u.String() != "http://\xaa/" {
		t.Errorf("%q", u.String())
	}
}

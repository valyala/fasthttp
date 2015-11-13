package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"testing"
)

func TestResponseHeaderHTTPVer(t *testing.T) {
	// non-http/1.1
	testResponseHeaderHTTPVer(t, "HTTP/1.0 200 OK\r\nContent-Type: aaa\r\nContent-Length: 123\r\n\r\n", true)
	testResponseHeaderHTTPVer(t, "HTTP/0.9 200 OK\r\nContent-Type: aaa\r\nContent-Length: 123\r\n\r\n", true)
	testResponseHeaderHTTPVer(t, "foobar 200 OK\r\nContent-Type: aaa\r\nContent-Length: 123\r\n\r\n", true)

	// http/1.1
	testResponseHeaderHTTPVer(t, "HTTP/1.1 200 OK\r\nContent-Type: aaa\r\nContent-Length: 123\r\n\r\n", false)
}

func TestRequestHeaderHTTPVer(t *testing.T) {
	// non-http/1.1
	testRequestHeaderHTTPVer(t, "GET / HTTP/1.0\r\nHost: aa.com\r\n\r\n", true)
	testRequestHeaderHTTPVer(t, "GET / HTTP/0.9\r\nHost: aa.com\r\n\r\n", true)
	testRequestHeaderHTTPVer(t, "GET / foobar\r\nHost: aa.com\r\n\r\n", true)

	// empty http version
	testRequestHeaderHTTPVer(t, "GET /\r\nHost: aaa.com\r\n\r\n", true)
	testRequestHeaderHTTPVer(t, "GET / \r\nHost: aaa.com\r\n\r\n", true)

	// http/1.1
	testRequestHeaderHTTPVer(t, "GET / HTTP/1.1\r\nHost: a.com\r\n\r\n", false)
}

func testResponseHeaderHTTPVer(t *testing.T, s string, connectionClose bool) {
	var h ResponseHeader

	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	if err := h.Read(br); err != nil {
		t.Fatalf("unexpected error: %s. response=%q", err, s)
	}
	if h.ConnectionClose != connectionClose {
		t.Fatalf("unexpected connectionClose %v. Expecting %v. response=%q", h.ConnectionClose, connectionClose, s)
	}
}

func testRequestHeaderHTTPVer(t *testing.T, s string, connectionClose bool) {
	var h RequestHeader

	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	if err := h.Read(br); err != nil {
		t.Fatalf("unexpected error: %s. request=%q", err, s)
	}
	if h.ConnectionClose != connectionClose {
		t.Fatalf("unexpected connectionClose %v. Expecting %v. request=%q", h.ConnectionClose, connectionClose, s)
	}
}

func TestResponseHeaderCopyTo(t *testing.T) {
	var h ResponseHeader

	h.Set("Set-Cookie", "foo=bar")
	h.Set("Content-Type", "foobar")
	h.Set("AAA-BBB", "aaaa")

	var h1 ResponseHeader
	h.CopyTo(&h1)
	if h1.Get("Set-cookie") != h.Get("Set-Cookie") {
		t.Fatalf("unexpected cookie %q. Expected %q", h1.Get("set-cookie"), h.Get("set-cookie"))
	}
	if h1.Get("Content-Type") != h.Get("Content-Type") {
		t.Fatalf("unexpected content-type %q. Expected %q", h1.Get("content-type"), h.Get("content-type"))
	}
	if h1.Get("aaa-bbb") != h.Get("AAA-BBB") {
		t.Fatalf("unexpected aaa-bbb %q. Expected %q", h1.Get("aaa-bbb"), h.Get("aaa-bbb"))
	}
}

func TestRequestHeaderCopyTo(t *testing.T) {
	var h RequestHeader

	h.Set("Cookie", "aa=bb; cc=dd")
	h.Set("Content-Type", "foobar")
	h.Set("Host", "aaaa")
	h.Set("aaaxxx", "123")

	var h1 RequestHeader
	h.CopyTo(&h1)
	if h1.Get("cookie") != h.Get("Cookie") {
		t.Fatalf("unexpected cookie after copying: %q. Expected %q", h1.Get("cookie"), h.Get("cookie"))
	}
	if h1.Get("content-type") != h.Get("Content-Type") {
		t.Fatalf("unexpected content-type %q. Expected %q", h1.Get("content-type"), h.Get("content-type"))
	}
	if h1.Get("host") != h.Get("host") {
		t.Fatalf("unexpected host %q. Expected %q", h1.Get("host"), h.Get("host"))
	}
	if h1.Get("aaaxxx") != h.Get("aaaxxx") {
		t.Fatalf("unexpected aaaxxx %q. Expected %q", h1.Get("aaaxxx"), h.Get("aaaxxx"))
	}
}

func TestRequestHeaderConnectionClose(t *testing.T) {
	var h RequestHeader

	h.Set("Connection", "close")
	h.Set("Host", "foobar")
	if !h.ConnectionClose {
		t.Fatalf("connection: close not set")
	}

	var w bytes.Buffer
	bw := bufio.NewWriter(&w)
	if err := h.Write(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var h1 RequestHeader
	br := bufio.NewReader(&w)
	if err := h1.Read(br); err != nil {
		t.Fatalf("error when reading request header: %s", err)
	}

	if !h1.ConnectionClose {
		t.Fatalf("unexpected connection: close value: %v", h1.ConnectionClose)
	}
	if h1.Get("Connection") != "close" {
		t.Fatalf("unexpected connection value: %q. Expecting %q", h.Get("Connection"), "close")
	}
}

func TestRequestHeaderSetCookie(t *testing.T) {
	var h RequestHeader

	h.Set("Cookie", "foo=bar; baz=aaa")
	h.Set("cOOkie", "xx=yyy")

	if h.GetCookie("foo") != "bar" {
		t.Fatalf("Unexpected cookie %q. Expecting %q", h.GetCookie("foo"), "bar")
	}
	if h.GetCookie("baz") != "aaa" {
		t.Fatalf("Unexpected cookie %q. Expecting %q", h.GetCookie("baz"), "aaa")
	}
	if h.GetCookie("xx") != "yyy" {
		t.Fatalf("unexpected cookie %q. Expecting %q", h.GetCookie("xx"), "yyy")
	}
}

func TestResponseHeaderSetCookie(t *testing.T) {
	var h ResponseHeader

	h.Set("set-cookie", "foo=bar; path=/aa/bb; domain=aaa.com")
	h.Set("Set-Cookie", "aaaaa=bxx")

	var c Cookie
	c.Key = []byte("foo")
	if !h.GetCookie(&c) {
		t.Fatalf("cannot obtain %q cookie", c.Key)
	}
	if string(c.Value) != "bar" {
		t.Fatalf("unexpected cookie value %q. Expected %q", c.Value, "bar")
	}
	if string(c.Path) != "/aa/bb" {
		t.Fatalf("unexpected cookie path %q. Expected %q", c.Path, "/aa/bb")
	}
	if string(c.Domain) != "aaa.com" {
		t.Fatalf("unexpected cookie domain %q. Expected %q", c.Domain, "aaa.com")
	}

	c.Key = []byte("aaaaa")
	if !h.GetCookie(&c) {
		t.Fatalf("cannot obtain %q cookie", c.Key)
	}
	if string(c.Value) != "bxx" {
		t.Fatalf("unexpected cookie value %q. Expecting %q", c.Value, "bxx")
	}
}

func TestResponseHeaderVisitAll(t *testing.T) {
	var h ResponseHeader

	r := bytes.NewBufferString("HTTP/1.1 200 OK\r\nContent-Type: aa\r\nContent-Length: 123\r\nSet-Cookie: aa=bb; path=/foo/bar\r\nSet-Cookie: ccc\r\n\r\n")
	br := bufio.NewReader(r)
	if err := h.Read(br); err != nil {
		t.Fatalf("Unepxected error: %s", err)
	}

	if h.Len() != 3 {
		t.Fatalf("Unexpected number of headers: %d. Expected 3", h.Len())
	}
	contentTypeCount := 0
	cookieCount := 0
	h.VisitAll(func(key, value []byte) {
		k := string(key)
		v := string(value)
		switch k {
		case "Content-Type":
			if v != h.Get(k) {
				t.Fatalf("Unexpected content-type: %q. Expected %q", v, h.Get(k))
			}
			contentTypeCount++
		case "Set-Cookie":
			if cookieCount == 0 && v != "aa=bb; path=/foo/bar" {
				t.Fatalf("unexpected cookie header: %q. Expected %q", v, "aa=bb; path=/foo/bar")
			}
			if cookieCount == 1 && v != "ccc" {
				t.Fatalf("unexpected cookie header: %q. Expected %q", v, "ccc")
			}
			cookieCount++
		default:
			t.Fatalf("unexpected header %q=%q", k, v)
		}
	})
	if contentTypeCount != 1 {
		t.Fatalf("unexpected number of content-type headers: %d. Expected 1", contentTypeCount)
	}
	if cookieCount != 2 {
		t.Fatalf("unexpected number of cookie header: %d. Expected 2", cookieCount)
	}
}

func TestRequestHeaderVisitAll(t *testing.T) {
	var h RequestHeader

	r := bytes.NewBufferString("GET / HTTP/1.1\r\nHost: aa.com\r\nXX: YYY\r\nXX: ZZ\r\nCookie: a=b; c=d\r\n\r\n")
	br := bufio.NewReader(r)
	if err := h.Read(br); err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if h.Len() != 4 {
		t.Fatalf("Unexpected number of header: %d. Expected 4", h.Len())
	}
	hostCount := 0
	xxCount := 0
	cookieCount := 0
	h.VisitAll(func(key, value []byte) {
		k := string(key)
		v := string(value)
		switch k {
		case "Host":
			if v != h.Get(k) {
				t.Fatalf("Unexpected host value %q. Expected %q", v, h.Get(k))
			}
			hostCount++
		case "Xx":
			if xxCount == 0 && v != "YYY" {
				t.Fatalf("Unexpected value %q. Expected %q", v, "YYY")
			}
			if xxCount == 1 && v != "ZZ" {
				t.Fatalf("Unexpected value %q. Expected %q", v, "ZZ")
			}
			xxCount++
		case "Cookie":
			if v != "a=b; c=d" {
				t.Fatalf("Unexpected cookie %q. Expected %q", v, "a=b; c=d")
			}
			cookieCount++
		default:
			t.Fatalf("Unepxected header %q=%q", k, v)
		}
	})
	if hostCount != 1 {
		t.Fatalf("Unepxected number of host headers detected %d. Expected 1", hostCount)
	}
	if xxCount != 2 {
		t.Fatalf("Unexpected number of xx headers detected %d. Expected 2", xxCount)
	}
	if cookieCount != 1 {
		t.Fatalf("Unexpected number of cookie headers %d. Expected 1", cookieCount)
	}
}

func TestResponseHeaderCookie(t *testing.T) {
	var h ResponseHeader
	var c Cookie

	c.Key = []byte("foobar")
	c.Value = []byte("aaa")
	h.SetCookie(&c)

	c.Key = []byte("йцук")
	c.Domain = []byte("foobar.com")
	h.SetCookie(&c)

	c.Clear()
	c.Key = []byte("foobar")
	if !h.GetCookie(&c) {
		t.Fatalf("Cannot find cookie %q", c.Key)
	}

	var expectedC1 Cookie
	expectedC1.Key = []byte("foobar")
	expectedC1.Value = []byte("aaa")
	if !equalCookie(&expectedC1, &c) {
		t.Fatalf("unexpected cookie\n%#v\nExpected\n%#v\n", c, expectedC1)
	}

	c.Key = []byte("йцук")
	if !h.GetCookie(&c) {
		t.Fatalf("cannot find cookie %q", c.Key)
	}

	var expectedC2 Cookie
	expectedC2.Key = []byte("йцук")
	expectedC2.Value = []byte("aaa")
	expectedC2.Domain = []byte("foobar.com")
	if !equalCookie(&expectedC2, &c) {
		t.Fatalf("unexpected cookie\n%v\nExpected\n%v\n", c, expectedC2)
	}

	h.VisitAllCookie(func(key, value []byte) {
		var cc Cookie
		cc.ParseBytes(value)
		if !bytes.Equal(key, cc.Key) {
			t.Fatalf("Unexpected cookie key %q. Expected %q", key, cc.Key)
		}
		switch {
		case bytes.Equal(key, []byte("foobar")):
			if !equalCookie(&expectedC1, &cc) {
				t.Fatalf("unexpected cookie\n%v\nExpected\n%v\n", cc, expectedC1)
			}
		case bytes.Equal(key, []byte("йцук")):
			if !equalCookie(&expectedC2, &cc) {
				t.Fatalf("unexpected cookie\n%v\nExpected\n%v\n", cc, expectedC2)
			}
		default:
			t.Fatalf("unexpected cookie key %q", key)
		}
	})

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	if err := h.Write(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var h1 ResponseHeader
	br := bufio.NewReader(w)
	if err := h1.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	c.Key = []byte("foobar")
	if !h1.GetCookie(&c) {
		t.Fatalf("Cannot find cookie %q", c.Key)
	}
	if !equalCookie(&expectedC1, &c) {
		t.Fatalf("unexpected cookie\n%v\nExpected\n%v\n", c, expectedC1)
	}

	c.Key = []byte("йцук")
	if !h1.GetCookie(&c) {
		t.Fatalf("cannot find cookie %q", c.Key)
	}
	if !equalCookie(&expectedC2, &c) {
		t.Fatalf("unexpected cookie\n%v\nExpected\n%v\n", c, expectedC2)
	}
}

func equalCookie(c1, c2 *Cookie) bool {
	if !bytes.Equal(c1.Key, c2.Key) {
		return false
	}
	if !bytes.Equal(c1.Value, c2.Value) {
		return false
	}
	if !c1.Expire.Equal(c2.Expire) {
		return false
	}
	if !bytes.Equal(c1.Domain, c2.Domain) {
		return false
	}
	if !bytes.Equal(c1.Path, c2.Path) {
		return false
	}
	return true
}

func TestRequestHeaderCookie(t *testing.T) {
	var h RequestHeader
	h.RequestURI = []byte("/foobar")
	h.Set("Host", "foobar.com")

	h.SetCookie("foo", "bar")
	h.SetCookie("привет", "мир")

	if h.GetCookie("foo") != "bar" {
		t.Fatalf("Unexpected cookie value %q. Exepcted %q", h.GetCookie("foo"), "bar")
	}
	if h.GetCookie("привет") != "мир" {
		t.Fatalf("Unexpected cookie value %q. Expected %q", h.GetCookie("привет"), "мир")
	}

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	if err := h.Write(bw); err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	var h1 RequestHeader
	br := bufio.NewReader(w)
	if err := h1.Read(br); err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	if h1.GetCookie("foo") != h.GetCookie("foo") {
		t.Fatalf("Unexpected cookie value %q. Exepcted %q", h1.GetCookie("foo"), h.GetCookie("foo"))
	}
	if h1.GetCookie("привет") != h.GetCookie("привет") {
		t.Fatalf("Unexpected cookie value %q. Expected %q", h1.GetCookie("привет"), h.GetCookie("привет"))
	}
}

func TestRequestHeaderSetGet(t *testing.T) {
	h := &RequestHeader{
		Method:     strPost,
		RequestURI: []byte("/aa/bbb"),
	}
	h.Set("foo", "bar")
	h.Set("host", "12345")
	h.Set("content-type", "aaa/bbb")
	h.Set("content-length", "1234")
	h.Set("user-agent", "aaabbb")
	h.Set("referer", "axcv")
	h.Set("baz", "xxxxx")
	h.Set("transfer-encoding", "chunked")
	h.Set("connection", "close")

	expectRequestHeaderGet(t, h, "Foo", "bar")
	expectRequestHeaderGet(t, h, "Host", "12345")
	expectRequestHeaderGet(t, h, "Content-Type", "aaa/bbb")
	expectRequestHeaderGet(t, h, "Content-Length", "")
	expectRequestHeaderGet(t, h, "USER-AGent", "aaabbb")
	expectRequestHeaderGet(t, h, "Referer", "axcv")
	expectRequestHeaderGet(t, h, "baz", "xxxxx")
	expectRequestHeaderGet(t, h, "Transfer-Encoding", "")
	expectRequestHeaderGet(t, h, "connecTION", "close")
	if !h.ConnectionClose {
		t.Fatalf("unset connection: close")
	}

	if h.ContentLength != 0 {
		t.Fatalf("Unexpected content-length %d. Expected %d", h.ContentLength, 0)
	}

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	err := h.Write(bw)
	if err != nil {
		t.Fatalf("Unexpected error when writing request header: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("Unexpected error when flushing request header: %s", err)
	}

	var h1 RequestHeader
	br := bufio.NewReader(w)
	if err = h1.Read(br); err != nil {
		t.Fatalf("Unexpected error when reading request header: %s", err)
	}

	if h1.ContentLength != h.ContentLength {
		t.Fatalf("Unexpected Content-Length %d. Expected %d", h1.ContentLength, h.ContentLength)
	}

	expectRequestHeaderGet(t, &h1, "Foo", "bar")
	expectRequestHeaderGet(t, &h1, "HOST", "12345")
	expectRequestHeaderGet(t, &h1, "Content-Type", "aaa/bbb")
	expectRequestHeaderGet(t, &h1, "Content-Length", "")
	expectRequestHeaderGet(t, &h1, "USER-AGent", "aaabbb")
	expectRequestHeaderGet(t, &h1, "Referer", "axcv")
	expectRequestHeaderGet(t, &h1, "baz", "xxxxx")
	expectRequestHeaderGet(t, &h1, "Transfer-Encoding", "")
	expectRequestHeaderGet(t, &h1, "Connection", "close")
	if !h1.ConnectionClose {
		t.Fatalf("unset connection: close")
	}
}

func TestResponseHeaderSetGet(t *testing.T) {
	h := &ResponseHeader{}
	h.Set("foo", "bar")
	h.Set("content-type", "aaa/bbb")
	h.Set("connection", "close")
	h.Set("content-length", "1234")
	h.Set("Server", "aaaa")
	h.Set("baz", "xxxxx")
	h.Set("Transfer-Encoding", "chunked")

	expectResponseHeaderGet(t, h, "Foo", "bar")
	expectResponseHeaderGet(t, h, "Content-Type", "aaa/bbb")
	expectResponseHeaderGet(t, h, "Connection", "close")
	expectResponseHeaderGet(t, h, "Content-Length", "")
	expectResponseHeaderGet(t, h, "seRVer", "aaaa")
	expectResponseHeaderGet(t, h, "baz", "xxxxx")
	expectResponseHeaderGet(t, h, "Transfer-Encoding", "")

	if h.ContentLength != 0 {
		t.Fatalf("Unexpected content-length %d. Expected %d", h.ContentLength, 0)
	}
	if !h.ConnectionClose {
		t.Fatalf("Unexpected Connection: close value %v. Expected %v", h.ConnectionClose, true)
	}

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	err := h.Write(bw)
	if err != nil {
		t.Fatalf("Unexpected error when writing response header: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("Unexpected error when flushing response header: %s", err)
	}

	var h1 ResponseHeader
	br := bufio.NewReader(w)
	if err = h1.Read(br); err != nil {
		t.Fatalf("Unexpected error when reading response header: %s", err)
	}

	if h1.ContentLength != h.ContentLength {
		t.Fatalf("Unexpected Content-Length %d. Expected %d", h1.ContentLength, h.ContentLength)
	}
	if h1.ConnectionClose != h.ConnectionClose {
		t.Fatalf("unexpected connection: close %v. Expected %v", h1.ConnectionClose, h.ConnectionClose)
	}

	expectResponseHeaderGet(t, &h1, "Foo", "bar")
	expectResponseHeaderGet(t, &h1, "Content-Type", "aaa/bbb")
	expectResponseHeaderGet(t, &h1, "Connection", "close")
	expectResponseHeaderGet(t, &h1, "seRVer", "aaaa")
	expectResponseHeaderGet(t, &h1, "baz", "xxxxx")
}

func expectRequestHeaderGet(t *testing.T, h *RequestHeader, key, expectedValue string) {
	if h.Get(key) != expectedValue {
		t.Fatalf("Unexpected value for key %q: %q. Expected %q", key, h.Get(key), expectedValue)
	}
}

func expectResponseHeaderGet(t *testing.T, h *ResponseHeader, key, expectedValue string) {
	if h.Get(key) != expectedValue {
		t.Fatalf("Unexpected value for key %q: %q. Expected %q", key, h.Get(key), expectedValue)
	}
}

func TestResponseHeaderConnectionClose(t *testing.T) {
	testResponseHeaderConnectionClose(t, true)
	testResponseHeaderConnectionClose(t, false)
}

func testResponseHeaderConnectionClose(t *testing.T, connectionClose bool) {
	h := &ResponseHeader{
		ConnectionClose: connectionClose,
	}
	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	err := h.Write(bw)
	if err != nil {
		t.Fatalf("Unexpected error when writing response header: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("Unexpected error when flushing response header: %s", err)
	}

	var h1 ResponseHeader
	br := bufio.NewReader(w)
	err = h1.Read(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading response header: %s", err)
	}
	if h1.ConnectionClose != h.ConnectionClose {
		t.Fatalf("Unexpected value for ConnectionClose: %v. Expected %v", h1.ConnectionClose, h.ConnectionClose)
	}
}

func TestRequestHeaderTooBig(t *testing.T) {
	s := "GET / HTTP/1.1\r\nHost: aaa.com\r\n" + getHeaders(100500) + "\r\n"
	r := bytes.NewBufferString(s)
	br := bufio.NewReaderSize(r, 4096)
	h := &RequestHeader{}
	err := h.Read(br)
	if err == nil {
		t.Fatalf("Expecting error when reading too big header")
	}
}

func TestResponseHeaderTooBig(t *testing.T) {
	s := "HTTP/1.1 200 OK\r\nContent-Type: sss\r\nContent-Length: 0\r\n" + getHeaders(100500) + "\r\n"
	r := bytes.NewBufferString(s)
	br := bufio.NewReaderSize(r, 4096)
	h := &ResponseHeader{}
	err := h.Read(br)
	if err == nil {
		t.Fatalf("Expecting error when reading too big header")
	}
}

type bufioPeekReader struct {
	s string
	n int
}

func (r *bufioPeekReader) Read(b []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}

	r.n++
	n := r.n
	if len(r.s) < n {
		n = len(r.s)
	}
	src := []byte(r.s[:n])
	r.s = r.s[n:]
	n = copy(b, src)
	return n, nil
}

func TestRequestHeaderBufioPeek(t *testing.T) {
	r := &bufioPeekReader{
		s: "GET / HTTP/1.1\r\nHost: foobar.com\r\n" + getHeaders(10) + "\r\naaaa",
	}
	br := bufio.NewReaderSize(r, 4096)
	h := &RequestHeader{}
	if err := h.Read(br); err != nil {
		t.Fatalf("Unexpected error when reading request: %s", err)
	}
	verifyRequestHeader(t, h, 0, "/", "foobar.com", "", "")
	verifyTrailer(t, br, "aaaa")
}

func TestResponseHeaderBufioPeek(t *testing.T) {
	r := &bufioPeekReader{
		s: "HTTP/1.1 200 OK\r\nContent-Length: 10\r\nContent-Type: aaa\r\n" + getHeaders(10) + "\r\n0123456789",
	}
	br := bufio.NewReaderSize(r, 4096)
	h := &ResponseHeader{}
	if err := h.Read(br); err != nil {
		t.Fatalf("Unexpected error when reading response: %s", err)
	}
	verifyResponseHeader(t, h, 200, 10, "aaa")
	verifyTrailer(t, br, "0123456789")
}

func getHeaders(n int) string {
	var h []string
	for i := 0; i < n; i++ {
		h = append(h, fmt.Sprintf("Header_%d: Value_%d\r\n", i, i))
	}
	return strings.Join(h, "")
}

func TestResponseHeaderReadSuccess(t *testing.T) {
	h := &ResponseHeader{}

	// straight order of content-length and content-type
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\r\nContent-Length: 123\r\nContent-Type: text/html\r\n\r\n",
		200, 123, "text/html", "")
	if h.ConnectionClose {
		t.Fatalf("unexpected connection: close")
	}

	// reverse order of content-length and content-type
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 202 OK\r\nContent-Type: text/plain; encoding=utf-8\r\nContent-Length: 543\r\nConnection: close\r\n\r\n",
		202, 543, "text/plain; encoding=utf-8", "")
	if !h.ConnectionClose {
		t.Fatalf("expecting connection: close")
	}

	// tranfer-encoding: chunked
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 505 Internal error\r\nContent-Type: text/html\r\nTransfer-Encoding: chunked\r\n\r\n",
		505, -1, "text/html", "")
	if h.ConnectionClose {
		t.Fatalf("unexpected connection: close")
	}

	// reverse order of content-type and tranfer-encoding
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 343 foobar\r\nTransfer-Encoding: chunked\r\nContent-Type: text/json\r\n\r\n",
		343, -1, "text/json", "")

	// additional headers
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 100 Continue\r\nFoobar: baz\r\nContent-Type: aaa/bbb\r\nUser-Agent: x\r\nContent-Length: 123\r\nZZZ: werer\r\n\r\n",
		100, 123, "aaa/bbb", "")

	// trailer (aka body)
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 32245\r\n\r\nqwert aaa",
		200, 32245, "text/plain", "qwert aaa")

	// ancient http protocol
	testResponseHeaderReadSuccess(t, h, "HTTP/0.9 300 OK\r\nContent-Length: 123\r\nContent-Type: text/html\r\n\r\nqqqq",
		300, 123, "text/html", "qqqq")

	// lf instead of crlf
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\nContent-Length: 123\nContent-Type: text/html\n\n",
		200, 123, "text/html", "")

	// Zero-length headers with mixed crlf and lf
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 400 OK\nContent-Length: 345\nZero-Value: \r\nContent-Type: aaa\n: zero-key\r\n\r\nooa",
		400, 345, "aaa", "ooa")

	// No space after colon
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\nContent-Length:34\nContent-Type: sss\n\naaaa",
		200, 34, "sss", "aaaa")

	// invalid case
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 400 OK\nconTEnt-leNGTH: 123\nConTENT-TYPE: ass\n\n",
		400, 123, "ass", "")

	// duplicate content-length
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\r\nContent-Length: 456\r\nContent-Type: foo/bar\r\nContent-Length: 321\r\n\r\n",
		200, 321, "foo/bar", "")

	// duplicate content-type
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\r\nContent-Length: 234\r\nContent-Type: foo/bar\r\nContent-Type: baz/bar\r\n\r\n",
		200, 234, "baz/bar", "")

	// both transfer-encoding: chunked and content-length
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\r\nContent-Type: foo/bar\r\nContent-Length: 123\r\nTransfer-Encoding: chunked\r\n\r\n",
		200, -1, "foo/bar", "")

	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 300 OK\r\nContent-Type: foo/barr\r\nTransfer-Encoding: chunked\r\nContent-Length: 354\r\n\r\n",
		300, -1, "foo/barr", "")

	// duplicate transfer-encoding: chunked
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n",
		200, -1, "text/html", "")

	// no reason string in the first line
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 456\r\nContent-Type: xxx/yyy\r\nContent-Length: 134\r\n\r\naaaxxx",
		456, 134, "xxx/yyy", "aaaxxx")

	// blank lines before the first line
	testResponseHeaderReadSuccess(t, h, "\r\nHTTP/1.1 200 OK\r\nContent-Type: aa\r\nContent-Length: 0\r\n\r\nsss",
		200, 0, "aa", "sss")
}

func TestRequestHeaderReadSuccess(t *testing.T) {
	h := &RequestHeader{}

	// simple headers
	testRequestHeaderReadSuccess(t, h, "GET /foo/bar HTTP/1.1\r\nHost: google.com\r\n\r\n",
		0, "/foo/bar", "google.com", "", "", "")
	if h.ConnectionClose {
		t.Fatalf("unexpected connection: close header")
	}

	// simple headers with body
	testRequestHeaderReadSuccess(t, h, "GET /a/bar HTTP/1.1\r\nHost: gole.com\r\nconneCTION: close\r\n\r\nfoobar",
		0, "/a/bar", "gole.com", "", "", "foobar")
	if !h.ConnectionClose {
		t.Fatalf("connection: close unset")
	}

	// ancient http protocol
	testRequestHeaderReadSuccess(t, h, "GET /bar HTTP/1.0\r\nHost: gole\r\n\r\npppp",
		0, "/bar", "gole", "", "", "pppp")
	if !h.ConnectionClose {
		t.Fatalf("expecting connectionClose for ancient http protocol")
	}

	// complex headers with body
	testRequestHeaderReadSuccess(t, h, "GET /aabar HTTP/1.1\r\nAAA: bbb\r\nHost: ole.com\r\nAA: bb\r\n\r\nzzz",
		0, "/aabar", "ole.com", "", "", "zzz")
	if h.ConnectionClose {
		t.Fatalf("unexpected connection: close")
	}

	// lf instead of crlf
	testRequestHeaderReadSuccess(t, h, "GET /foo/bar HTTP/1.1\nHost: google.com\n\n",
		0, "/foo/bar", "google.com", "", "", "")

	// post method
	testRequestHeaderReadSuccess(t, h, "POST /aaa?bbb HTTP/1.1\r\nHost: foobar.com\r\nContent-Length: 1235\r\nContent-Type: aaa\r\n\r\nabcdef",
		1235, "/aaa?bbb", "foobar.com", "", "aaa", "abcdef")

	// zero-length headers with mixed crlf and lf
	testRequestHeaderReadSuccess(t, h, "GET /a HTTP/1.1\nHost: aaa\r\nZero: \n: Zero-Value\n\r\nxccv",
		0, "/a", "aaa", "", "", "xccv")

	// no space after colon
	testRequestHeaderReadSuccess(t, h, "GET /a HTTP/1.1\nHost:aaaxd\n\nsdfds",
		0, "/a", "aaaxd", "", "", "sdfds")

	// get with zero content-length
	testRequestHeaderReadSuccess(t, h, "GET /xxx HTTP/1.1\nHost: aaa.com\nContent-Length: 0\n\n",
		0, "/xxx", "aaa.com", "", "", "")

	// get with non-zero content-length
	testRequestHeaderReadSuccess(t, h, "GET /xxx HTTP/1.1\nHost: aaa.com\nContent-Length: 123\n\n",
		0, "/xxx", "aaa.com", "", "", "")

	// invalid case
	testRequestHeaderReadSuccess(t, h, "GET /aaa HTTP/1.1\nhoST: bbb.com\n\naas",
		0, "/aaa", "bbb.com", "", "", "aas")

	// referer
	testRequestHeaderReadSuccess(t, h, "GET /asdf HTTP/1.1\nHost: aaa.com\nReferer: bb.com\n\naaa",
		0, "/asdf", "aaa.com", "bb.com", "", "aaa")

	// duplicate host
	testRequestHeaderReadSuccess(t, h, "GET /aa HTTP/1.1\r\nHost: aaaaaa.com\r\nHost: bb.com\r\n\r\n",
		0, "/aa", "bb.com", "", "", "")

	// post with duplicate content-type
	testRequestHeaderReadSuccess(t, h, "POST /a HTTP/1.1\r\nHost: aa\r\nContent-Type: ab\r\nContent-Length: 123\r\nContent-Type: xx\r\n\r\n",
		123, "/a", "aa", "", "xx", "")

	// post with duplicate content-length
	testRequestHeaderReadSuccess(t, h, "POST /xx HTTP/1.1\r\nHost: aa\r\nContent-Type: s\r\nContent-Length: 13\r\nContent-Length: 1\r\n\r\n",
		1, "/xx", "aa", "", "s", "")

	// non-post with content-type
	testRequestHeaderReadSuccess(t, h, "GET /aaa HTTP/1.1\r\nHost: bbb.com\r\nContent-Type: aaab\r\n\r\n",
		0, "/aaa", "bbb.com", "", "aaab", "")

	// non-post with content-length
	testRequestHeaderReadSuccess(t, h, "HEAD / HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: 123\r\n\r\n",
		0, "/", "aaa.com", "", "", "")

	// non-post with content-type and content-length
	testRequestHeaderReadSuccess(t, h, "GET /aa HTTP/1.1\r\nHost: aa.com\r\nContent-Type: abd/test\r\nContent-Length: 123\r\n\r\n",
		0, "/aa", "aa.com", "", "abd/test", "")

	// request uri with hostname
	testRequestHeaderReadSuccess(t, h, "GET http://gooGle.com/foO/%20bar?xxx#aaa HTTP/1.1\r\nHost: aa.cOM\r\n\r\ntrail",
		0, "http://gooGle.com/foO/%20bar?xxx#aaa", "aa.cOM", "", "", "trail")

	// no protocol in the first line
	testRequestHeaderReadSuccess(t, h, "GET /foo/bar\r\nHost: google.com\r\n\r\nisdD",
		0, "/foo/bar", "google.com", "", "", "isdD")

	// blank lines before the first line
	testRequestHeaderReadSuccess(t, h, "\r\n\n\r\nGET /aaa HTTP/1.1\r\nHost: aaa.com\r\n\r\nsss",
		0, "/aaa", "aaa.com", "", "", "sss")

	// request uri with spaces
	testRequestHeaderReadSuccess(t, h, "GET /foo/ bar baz HTTP/1.1\r\nHost: aa.com\r\n\r\nxxx",
		0, "/foo/ bar baz", "aa.com", "", "", "xxx")
}

func TestResponseHeaderReadError(t *testing.T) {
	h := &ResponseHeader{}

	// incorrect first line
	testResponseHeaderReadError(t, h, "fo")
	testResponseHeaderReadError(t, h, "foobarbaz")
	testResponseHeaderReadError(t, h, "HTTP/1.1")
	testResponseHeaderReadError(t, h, "HTTP/1.1 ")
	testResponseHeaderReadError(t, h, "HTTP/1.1 s")

	// non-numeric status code
	testResponseHeaderReadError(t, h, "HTTP/1.1 foobar OK\r\nContent-Length: 123\r\nContent-Type: text/html\r\n\r\n")
	testResponseHeaderReadError(t, h, "HTTP/1.1 123foobar OK\r\nContent-Length: 123\r\nContent-Type: text/html\r\n\r\n")
	testResponseHeaderReadError(t, h, "HTTP/1.1 foobar344 OK\r\nContent-Length: 123\r\nContent-Type: text/html\r\n\r\n")

	// no headers
	testResponseHeaderReadError(t, h, "HTTP/1.1 200 OK\r\n")
	testResponseHeaderReadError(t, h, "HTTP/1.1 200 OK\r\n\r\n")

	// no trailing crlf
	testResponseHeaderReadError(t, h, "HTTP/1.1 200 OK\r\nContent-Length: 123\r\nContent-Type: text/html\r\n")

	// non-numeric content-length
	testResponseHeaderReadError(t, h, "HTTP/1.1 200 OK\r\nContent-Length: faaa\r\nContent-Type: text/html\r\n\r\n")
	testResponseHeaderReadError(t, h, "HTTP/1.1 200 OK\r\nContent-Length: 123aa\r\nContent-Type: text/html\r\n\r\n")
	testResponseHeaderReadError(t, h, "HTTP/1.1 200 OK\r\nContent-Length: aa124\r\nContent-Type: text/html\r\n\r\n")

	// no content-type
	testResponseHeaderReadError(t, h, "HTTP/1.1 200 OK\r\nContent-Length: 123\r\n\r\n")

	// no content-length
	testResponseHeaderReadError(t, h, "HTTP/1.1 200 OK\r\nContent-Type: foo/bar\r\n\r\n")
}

func TestRequestHeaderReadError(t *testing.T) {
	h := &RequestHeader{}

	// invalid method
	testRequestHeaderReadError(t, h, "POST /foo/bar HTTP/1.1\r\nHost: google.com\r\n\r\n")

	// missing RequestURI
	testRequestHeaderReadError(t, h, "GET  HTTP/1.1\r\nHost: google.com\r\n\r\n")

	// no host
	testRequestHeaderReadError(t, h, "GET /foo/bar HTTP/1.1\r\nFOObar: assdfd\r\n\r\naaa")

	// no host, no headers
	testRequestHeaderReadError(t, h, "GET /foo/bar HTTP/1.1\r\n\r\nfoobar")

	// post with invalid content-length
	testRequestHeaderReadError(t, h, "POST /a HTTP/1.1\r\nHost: bb\r\nContent-Type: aa\r\nContent-Length: dff\r\n\r\n")

	// post without content-length and content-type
	testRequestHeaderReadError(t, h, "POST /aaa HTTP/1.1\r\nHost: aaa.com\r\n\r\n")

	// post without content-type
	testRequestHeaderReadError(t, h, "POST /abc HTTP/1.1\r\nHost: aa.com\r\nContent-Length: 123\r\n\r\n")

	// post without content-length
	testRequestHeaderReadError(t, h, "POST /abc HTTP/1.1\r\nHost: aa.com\r\nContent-Type: adv\r\n\r\n")
}

func testResponseHeaderReadError(t *testing.T, h *ResponseHeader, headers string) {
	r := bytes.NewBufferString(headers)
	br := bufio.NewReader(r)
	err := h.Read(br)
	if err == nil {
		t.Fatalf("Expecting error when reading response header %q", headers)
	}

	// make sure response header works after error
	testResponseHeaderReadSuccess(t, h, "HTTP/1.1 200 OK\r\nContent-Type: foo/bar\r\nContent-Length: 12345\r\n\r\nsss",
		200, 12345, "foo/bar", "sss")
}

func testRequestHeaderReadError(t *testing.T, h *RequestHeader, headers string) {
	r := bytes.NewBufferString(headers)
	br := bufio.NewReader(r)
	err := h.Read(br)
	if err == nil {
		t.Fatalf("Expecting error when reading request header %q", headers)
	}

	// make sure request header works after error
	testRequestHeaderReadSuccess(t, h, "GET /foo/bar HTTP/1.1\r\nHost: aaaa\r\n\r\nxxx",
		0, "/foo/bar", "aaaa", "", "", "xxx")
}

func testResponseHeaderReadSuccess(t *testing.T, h *ResponseHeader, headers string, expectedStatusCode, expectedContentLength int,
	expectedContentType, expectedTrailer string) {
	r := bytes.NewBufferString(headers)
	br := bufio.NewReader(r)
	err := h.Read(br)
	if err != nil {
		t.Fatalf("Unexpected error when parsing response headers: %s. headers=%q", err, headers)
	}
	verifyResponseHeader(t, h, expectedStatusCode, expectedContentLength, expectedContentType)
	verifyTrailer(t, br, expectedTrailer)
}

func testRequestHeaderReadSuccess(t *testing.T, h *RequestHeader, headers string, expectedContentLength int,
	expectedRequestURI, expectedHost, expectedReferer, expectedContentType, expectedTrailer string) {
	r := bytes.NewBufferString(headers)
	br := bufio.NewReader(r)
	err := h.Read(br)
	if err != nil {
		t.Fatalf("Unexpected error when parsing request headers: %s. headers=%q", err, headers)
	}
	verifyRequestHeader(t, h, expectedContentLength, expectedRequestURI, expectedHost, expectedReferer, expectedContentType)
	verifyTrailer(t, br, expectedTrailer)
}

func verifyResponseHeader(t *testing.T, h *ResponseHeader, expectedStatusCode, expectedContentLength int, expectedContentType string) {
	if h.StatusCode != expectedStatusCode {
		t.Fatalf("Unexpected status code %d. Expected %d", h.StatusCode, expectedStatusCode)
	}
	if h.ContentLength != expectedContentLength {
		t.Fatalf("Unexpected content length %d. Expected %d", h.ContentLength, expectedContentLength)
	}
	if h.Get("Content-Type") != expectedContentType {
		t.Fatalf("Unexpected content type %q. Expected %q", h.Get("Content-Type"), expectedContentType)
	}
}

func verifyRequestHeader(t *testing.T, h *RequestHeader, expectedContentLength int,
	expectedRequestURI, expectedHost, expectedReferer, expectedContentType string) {
	if h.ContentLength != expectedContentLength {
		t.Fatalf("Unexpected Content-Length %d. Expected %d", h.ContentLength, expectedContentLength)
	}
	if !bytes.Equal(h.RequestURI, []byte(expectedRequestURI)) {
		t.Fatalf("Unexpected RequestURI %q. Expected %q", h.RequestURI, expectedRequestURI)
	}
	if h.Get("Host") != expectedHost {
		t.Fatalf("Unexpected host %q. Expected %q", h.Get("Host"), expectedHost)
	}
	if h.Get("Referer") != expectedReferer {
		t.Fatalf("Unexpected referer %q. Expected %q", h.Get("Referer"), expectedReferer)
	}
	if h.Get("Content-Type") != expectedContentType {
		t.Fatalf("Unexpected content-type %q. Expected %q", h.Get("Content-Type"), expectedContentType)
	}
}

func verifyTrailer(t *testing.T, r *bufio.Reader, expectedTrailer string) {
	trailer, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("Cannot read trailer: %s", err)
	}
	if !bytes.Equal(trailer, []byte(expectedTrailer)) {
		t.Fatalf("Unexpected trailer %q. Expected %q", trailer, expectedTrailer)
	}
}

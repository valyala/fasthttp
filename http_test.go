package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"mime/multipart"
	"strings"
	"testing"
)

func TestResponseGzipStream(t *testing.T) {
	var r Response
	r.SetBodyStreamWriter(func(w *bufio.Writer) {
		fmt.Fprintf(w, "foo")
		w.Flush()
		w.Write([]byte("barbaz"))
		w.Flush()
		fmt.Fprintf(w, "1234")
		if err := w.Flush(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	})
	testResponseGzipExt(t, &r, "foobarbaz1234")
}

func TestResponseDeflateStream(t *testing.T) {
	var r Response
	r.SetBodyStreamWriter(func(w *bufio.Writer) {
		w.Write([]byte("foo"))
		w.Flush()
		fmt.Fprintf(w, "barbaz")
		w.Flush()
		w.Write([]byte("1234"))
		if err := w.Flush(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	})
	testResponseDeflateExt(t, &r, "foobarbaz1234")
}

func TestResponseDeflate(t *testing.T) {
	testResponseDeflate(t, "")
	testResponseDeflate(t, "abdasdfsdaa")
	testResponseDeflate(t, "asoiowqoieroqweiruqwoierqo")
}

func TestResponseGzip(t *testing.T) {
	testResponseGzip(t, "")
	testResponseGzip(t, "foobarbaz")
	testResponseGzip(t, "abasdwqpweoweporweprowepr")
}

func testResponseDeflate(t *testing.T, s string) {
	var r Response
	r.SetBodyString(s)
	testResponseDeflateExt(t, &r, s)
}

func testResponseDeflateExt(t *testing.T, r *Response, s string) {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := r.WriteDeflate(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var r1 Response
	br := bufio.NewReader(&buf)
	if err := r1.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce := r1.Header.Peek("Content-Encoding")
	if string(ce) != "deflate" {
		t.Fatalf("unexpected Content-Encoding %q. Expecting %q", ce, "deflate")
	}
	body, err := r1.BodyInflate()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != s {
		t.Fatalf("unexpected body %q. Expecting %q", body, s)
	}
}

func testResponseGzip(t *testing.T, s string) {
	var r Response
	r.SetBodyString(s)
	testResponseGzipExt(t, &r, s)
}

func testResponseGzipExt(t *testing.T, r *Response, s string) {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := r.WriteGzip(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var r1 Response
	br := bufio.NewReader(&buf)
	if err := r1.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce := r1.Header.Peek("Content-Encoding")
	if string(ce) != "gzip" {
		t.Fatalf("unexpected Content-Encoding %q. Expecting %q", ce, "gzip")
	}
	body, err := r1.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != s {
		t.Fatalf("unexpected body %q. Expecting %q", body, s)
	}
}

func TestRequestMultipartForm(t *testing.T) {
	var w bytes.Buffer
	mw := multipart.NewWriter(&w)
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("key_%d", i)
		v := fmt.Sprintf("value_%d", i)
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	}
	boundary := mw.Boundary()
	if err := mw.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	formData := w.Bytes()
	s := fmt.Sprintf("POST / HTTP/1.1\r\nHost: aaa\r\nContent-Type: multipart/form-data; boundary=%s\r\nContent-Length: %d\r\n\r\n%s",
		boundary, len(formData), formData)

	var req Request

	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	if err := req.Read(br, nil, nil); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	f, err := req.MultipartForm()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	defer f.RemoveAll()

	if len(f.File) > 0 {
		t.Fatalf("unexpected files found in the multipart form: %d", len(f.File))
	}

	if len(f.Value) != 10 {
		t.Fatalf("unexpected number of values found: %d. Expecting %d", len(f.Value), 10)
	}

	for k, vv := range f.Value {
		if len(vv) != 1 {
			t.Fatalf("unexpected number of values found for key=%q: %d. Expecting 1", k, len(vv))
		}
		if !strings.HasPrefix(k, "key_") {
			t.Fatalf("unexpected key prefix=%q. Expecting %q", k, "key_")
		}
		v := vv[0]
		if !strings.HasPrefix(v, "value_") {
			t.Fatalf("unexpected value prefix=%q. expecting %q", v, "value_")
		}
		if k[len("key_"):] != v[len("value_"):] {
			t.Fatalf("key and value suffixes don't match: %q vs %q", k, v)
		}
	}
}

func TestOn100Continue(t *testing.T) {
	var w bytes.Buffer
	mw := multipart.NewWriter(&w)
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("key_%d", i)
		v := fmt.Sprintf("value_%d", i)
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	}
	boundary := mw.Boundary()
	if err := mw.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	formData := w.Bytes()
	s := fmt.Sprintf("POST / HTTP/1.1\r\nHost: aaa\r\nContent-Type: multipart/form-data; boundary=%s\r\nExpect: 100-Continue\r\nContent-Length: %d\r\n\r\n%s",
		boundary, len(formData), formData)

	var req Request

	continueBuffer := &bytes.Buffer{}

	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	if err := req.Read(br, continueBuffer, func(req *Request) bool { return true }); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if bytes.Compare(continueBuffer.Bytes(), []byte("HTTP/1.1 100 Continue\r\n\r\n")) != 0 {
		t.Fatalf("Expected 100 continue response on expect header, instead got: %s", continueBuffer.String())
	}

	continueBuffer.Reset()

	r = bytes.NewBufferString(s)
	br = bufio.NewReader(r)
	if err := req.Read(br, continueBuffer, func(req *Request) bool { return false }); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if bytes.Compare(continueBuffer.Bytes(), []byte("HTTP/1.1 417 Expectation Failed\r\n\r\n")) != 0 {
		t.Fatalf("Expected 471 expectation fail, instead got: %s", continueBuffer.String())
	}

}

func TestResponseReadLimitBody(t *testing.T) {
	// response with content-length
	testResponseReadLimitBodySuccess(t, "HTTP/1.1 200 OK\r\nContent-Type: aa\r\nContent-Length: 10\r\n\r\n9876543210", 10)
	testResponseReadLimitBodySuccess(t, "HTTP/1.1 200 OK\r\nContent-Type: aa\r\nContent-Length: 10\r\n\r\n9876543210", 100)
	testResponseReadLimitBodyError(t, "HTTP/1.1 200 OK\r\nContent-Type: aa\r\nContent-Length: 10\r\n\r\n9876543210", 9)

	// chunked response
	testResponseReadLimitBodySuccess(t, "HTTP/1.1 200 OK\r\nContent-Type: aa\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\n\r\n", 9)
	testResponseReadLimitBodySuccess(t, "HTTP/1.1 200 OK\r\nContent-Type: aa\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\n\r\n", 100)
	testResponseReadLimitBodyError(t, "HTTP/1.1 200 OK\r\nContent-Type: aa\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\n\r\n", 2)

	// identity response
	testResponseReadLimitBodySuccess(t, "HTTP/1.1 400 OK\r\nContent-Type: aa\r\n\r\n123456", 6)
	testResponseReadLimitBodySuccess(t, "HTTP/1.1 400 OK\r\nContent-Type: aa\r\n\r\n123456", 106)
	testResponseReadLimitBodyError(t, "HTTP/1.1 400 OK\r\nContent-Type: aa\r\n\r\n123456", 5)
}

func TestRequestReadLimitBody(t *testing.T) {
	// request with content-length
	testRequestReadLimitBodySuccess(t, "POST /foo HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: 9\r\nContent-Type: aaa\r\n\r\n123456789", 9)
	testRequestReadLimitBodySuccess(t, "POST /foo HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: 9\r\nContent-Type: aaa\r\n\r\n123456789", 92)
	testRequestReadLimitBodyError(t, "POST /foo HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: 9\r\nContent-Type: aaa\r\n\r\n123456789", 5)

	// chunked request
	testRequestReadLimitBodySuccess(t, "POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\n\r\n", 9)
	testRequestReadLimitBodySuccess(t, "POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\n\r\n", 999)
	testRequestReadLimitBodyError(t, "POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\n\r\n", 8)
}

func testResponseReadLimitBodyError(t *testing.T, s string, maxBodySize int) {
	var req Response
	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	err := req.ReadLimitBody(br, maxBodySize)
	if err == nil {
		t.Fatalf("expecting error. s=%q, maxBodySize=%d", s, maxBodySize)
	}
	if err != ErrBodyTooLarge {
		t.Fatalf("unexpected error: %s. Expecting %s. s=%q, maxBodySize=%d", err, ErrBodyTooLarge, s, maxBodySize)
	}
}

func testResponseReadLimitBodySuccess(t *testing.T, s string, maxBodySize int) {
	var req Response
	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	if err := req.ReadLimitBody(br, maxBodySize); err != nil {
		t.Fatalf("unexpected error: %s. s=%q, maxBodySize=%d", err, s, maxBodySize)
	}
}

func testRequestReadLimitBodyError(t *testing.T, s string, maxBodySize int) {
	var req Request
	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	err := req.ReadLimitBody(br, maxBodySize, nil, nil)
	if err == nil {
		t.Fatalf("expecting error. s=%q, maxBodySize=%d", s, maxBodySize)
	}
	if err != ErrBodyTooLarge {
		t.Fatalf("unexpected error: %s. Expecting %s. s=%q, maxBodySize=%d", err, ErrBodyTooLarge, s, maxBodySize)
	}
}

func testRequestReadLimitBodySuccess(t *testing.T, s string, maxBodySize int) {
	var req Request
	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	if err := req.ReadLimitBody(br, maxBodySize, nil, nil); err != nil {
		t.Fatalf("unexpected error: %s. s=%q, maxBodySize=%d", err, s, maxBodySize)
	}
}

func TestRequestString(t *testing.T) {
	var r Request
	r.SetRequestURI("http://foobar.com/aaa")
	s := r.String()
	expectedS := "GET /aaa HTTP/1.1\r\nUser-Agent: fasthttp\r\nHost: foobar.com\r\n\r\n"
	if s != expectedS {
		t.Fatalf("unexpected request: %q. Expecting %q", s, expectedS)
	}
}

func TestRequestBodyWriter(t *testing.T) {
	var r Request
	w := r.BodyWriter()
	for i := 0; i < 10; i++ {
		fmt.Fprintf(w, "%d", i)
	}
	if string(r.Body()) != "0123456789" {
		t.Fatalf("unexpected body %q. Expecting %q", r.Body(), "0123456789")
	}
}

func TestResponseBodyWriter(t *testing.T) {
	var r Response
	w := r.BodyWriter()
	for i := 0; i < 10; i++ {
		fmt.Fprintf(w, "%d", i)
	}
	if string(r.Body()) != "0123456789" {
		t.Fatalf("unexpected body %q. Expecting %q", r.Body(), "0123456789")
	}
}

func TestRequestWriteRequestURINoHost(t *testing.T) {
	var req Request
	req.Header.SetRequestURI("http://google.com/foo/bar?baz=aaa")
	var w bytes.Buffer
	bw := bufio.NewWriter(&w)
	if err := req.Write(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}

	var req1 Request
	br := bufio.NewReader(&w)
	if err := req1.Read(br, nil, nil); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(req1.Header.Host()) != "google.com" {
		t.Fatalf("unexpected host: %q. Expecting %q", req1.Header.Host(), "google.com")
	}
	if string(req.Header.RequestURI()) != "/foo/bar?baz=aaa" {
		t.Fatalf("unexpected requestURI: %q. Expecting %q", req.Header.RequestURI(), "/foo/bar?baz=aaa")
	}

	// verify that Request.Write returns error on non-absolute RequestURI
	req.Reset()
	req.Header.SetRequestURI("/foo/bar")
	w.Reset()
	bw.Reset(&w)
	if err := req.Write(bw); err == nil {
		t.Fatalf("expecting error")
	}
}

func TestSetResponseBodyStreamFixedSize(t *testing.T) {
	testSetResponseBodyStream(t, "a", false)
	testSetResponseBodyStream(t, string(createFixedBody(4097)), false)
	testSetResponseBodyStream(t, string(createFixedBody(100500)), false)
}

func TestSetResponseBodyStreamChunked(t *testing.T) {
	testSetResponseBodyStream(t, "", true)

	body := "foobar baz aaa bbb ccc"
	testSetResponseBodyStream(t, body, true)

	body = string(createFixedBody(10001))
	testSetResponseBodyStream(t, body, true)
}

func testSetResponseBodyStream(t *testing.T, body string, chunked bool) {
	var resp Response
	bodySize := len(body)
	if chunked {
		bodySize = -1
	}
	resp.SetBodyStream(bytes.NewBufferString(body), bodySize)

	var w bytes.Buffer
	bw := bufio.NewWriter(&w)
	if err := resp.Write(bw); err != nil {
		t.Fatalf("unexpected error when writing response: %s. body=%q", err, body)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error when flushing response: %s. body=%q", err, body)
	}

	var resp1 Response
	br := bufio.NewReader(&w)
	if err := resp1.Read(br); err != nil {
		t.Fatalf("unexpected error when reading response: %s. body=%q", err, body)
	}
	if string(resp1.Body()) != body {
		t.Fatalf("unexpected body %q. Expecting %q", resp1.Body(), body)
	}
}

func TestRound2(t *testing.T) {
	testRound2(t, 0, 0)
	testRound2(t, 1, 1)
	testRound2(t, 2, 2)
	testRound2(t, 3, 4)
	testRound2(t, 4, 4)
	testRound2(t, 5, 8)
	testRound2(t, 7, 8)
	testRound2(t, 8, 8)
	testRound2(t, 9, 16)
	testRound2(t, 0x10001, 0x20000)
}

func testRound2(t *testing.T, n, expectedRound2 int) {
	if round2(n) != expectedRound2 {
		t.Fatalf("Unexpected round2(%d)=%d. Expected %d", n, round2(n), expectedRound2)
	}
}

func TestRequestReadChunked(t *testing.T) {
	var req Request

	s := "POST /foo HTTP/1.1\r\nHost: google.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa/bb\r\n\r\n3\r\nabc\r\n5\r\n12345\r\n0\r\n\r\ntrail"
	r := bytes.NewBufferString(s)
	rb := bufio.NewReader(r)
	err := req.Read(rb, nil, nil)
	if err != nil {
		t.Fatalf("Unexpected error when reading chunked request: %s", err)
	}
	expectedBody := "abc12345"
	if string(req.Body()) != expectedBody {
		t.Fatalf("Unexpected body %q. Expected %q", req.Body(), expectedBody)
	}
	verifyRequestHeader(t, &req.Header, 8, "/foo", "google.com", "", "aa/bb")
	verifyTrailer(t, rb, "trail")
}

func TestResponseReadWithoutBody(t *testing.T) {
	var resp Response

	testResponseReadWithoutBody(t, &resp, "HTTP/1.1 304 Not Modified\r\nContent-Type: aa\r\nContent-Length: 1235\r\n\r\nfoobar", false,
		304, 1235, "aa", "foobar")

	testResponseReadWithoutBody(t, &resp, "HTTP/1.1 204 Foo Bar\r\nContent-Type: aab\r\nTransfer-Encoding: chunked\r\n\r\n123\r\nss", false,
		204, -1, "aab", "123\r\nss")

	testResponseReadWithoutBody(t, &resp, "HTTP/1.1 100 AAA\r\nContent-Type: xxx\r\nContent-Length: 3434\r\n\r\naaaa", false,
		100, 3434, "xxx", "aaaa")

	testResponseReadWithoutBody(t, &resp, "HTTP 200 OK\r\nContent-Type: text/xml\r\nContent-Length: 123\r\n\r\nxxxx", true,
		200, 123, "text/xml", "xxxx")
}

func testResponseReadWithoutBody(t *testing.T, resp *Response, s string, skipBody bool,
	expectedStatusCode, expectedContentLength int, expectedContentType, expectedTrailer string) {
	r := bytes.NewBufferString(s)
	rb := bufio.NewReader(r)
	resp.SkipBody = skipBody
	err := resp.Read(rb)
	if err != nil {
		t.Fatalf("Unexpected error when reading response without body: %s. response=%q", err, s)
	}
	if len(resp.Body()) != 0 {
		t.Fatalf("Unexpected response body %q. Expected %q. response=%q", resp.Body(), "", s)
	}
	verifyResponseHeader(t, &resp.Header, expectedStatusCode, expectedContentLength, expectedContentType)
	verifyTrailer(t, rb, expectedTrailer)

	// verify that ordinal response is read after null-body response
	resp.SkipBody = false
	testResponseReadSuccess(t, resp, "HTTP/1.1 300 OK\r\nContent-Length: 5\r\nContent-Type: bar\r\n\r\n56789aaa",
		300, 5, "bar", "56789", "aaa")
}

func TestRequestSuccess(t *testing.T) {
	// empty method, user-agent and body
	testRequestSuccess(t, "", "/foo/bar", "google.com", "", "", "GET")

	// non-empty user-agent
	testRequestSuccess(t, "GET", "/foo/bar", "google.com", "MSIE", "", "GET")

	// non-empty method
	testRequestSuccess(t, "HEAD", "/aaa", "fobar", "", "", "HEAD")

	// POST method with body
	testRequestSuccess(t, "POST", "/bbb", "aaa.com", "Chrome aaa", "post body", "POST")

	// PUT method with body
	testRequestSuccess(t, "PUT", "/aa/bb", "a.com", "ome aaa", "put body", "PUT")

	// only host is set
	testRequestSuccess(t, "", "", "gooble.com", "", "", "GET")
}

func TestResponseSuccess(t *testing.T) {
	// 200 response
	testResponseSuccess(t, 200, "test/plain", "server", "foobar",
		200, "test/plain", "server")

	// response with missing statusCode
	testResponseSuccess(t, 0, "text/plain", "server", "foobar",
		200, "text/plain", "server")

	// response with missing server
	testResponseSuccess(t, 500, "aaa", "", "aaadfsd",
		500, "aaa", string(defaultServerName))

	// empty body
	testResponseSuccess(t, 200, "bbb", "qwer", "",
		200, "bbb", "qwer")

	// missing content-type
	testResponseSuccess(t, 200, "", "asdfsd", "asdf",
		200, string(defaultContentType), "asdfsd")
}

func testResponseSuccess(t *testing.T, statusCode int, contentType, serverName, body string,
	expectedStatusCode int, expectedContentType, expectedServerName string) {
	var resp Response
	resp.SetStatusCode(statusCode)
	resp.Header.Set("Content-Type", contentType)
	resp.Header.Set("Server", serverName)
	resp.SetBody([]byte(body))

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	err := resp.Write(bw)
	if err != nil {
		t.Fatalf("Unexpected error when calling Response.Write(): %s", err)
	}
	if err = bw.Flush(); err != nil {
		t.Fatalf("Unexpected error when flushing bufio.Writer: %s", err)
	}

	var resp1 Response
	br := bufio.NewReader(w)
	if err = resp1.Read(br); err != nil {
		t.Fatalf("Unexpected error when calling Response.Read(): %s", err)
	}
	if resp1.StatusCode() != expectedStatusCode {
		t.Fatalf("Unexpected status code: %d. Expected %d", resp1.StatusCode(), expectedStatusCode)
	}
	if resp1.Header.ContentLength() != len(body) {
		t.Fatalf("Unexpected content-length: %d. Expected %d", resp1.Header.ContentLength(), len(body))
	}
	if string(resp1.Header.Peek("Content-Type")) != expectedContentType {
		t.Fatalf("Unexpected content-type: %q. Expected %q", resp1.Header.Peek("Content-Type"), expectedContentType)
	}
	if string(resp1.Header.Peek("Server")) != expectedServerName {
		t.Fatalf("Unexpected server: %q. Expected %q", resp1.Header.Peek("Server"), expectedServerName)
	}
	if !bytes.Equal(resp1.Body(), []byte(body)) {
		t.Fatalf("Unexpected body: %q. Expected %q", resp1.Body(), body)
	}
}

func TestRequestWriteError(t *testing.T) {
	// no host
	testRequestWriteError(t, "", "/foo/bar", "", "", "")

	// get with body
	testRequestWriteError(t, "GET", "/foo/bar", "aaa.com", "", "foobar")
}

func testRequestWriteError(t *testing.T, method, requestURI, host, userAgent, body string) {
	var req Request

	req.Header.SetMethod(method)
	req.Header.SetRequestURI(requestURI)
	req.Header.Set("Host", host)
	req.Header.Set("User-Agent", userAgent)
	req.SetBody([]byte(body))

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	err := req.Write(bw)
	if err == nil {
		t.Fatalf("Expecting error when writing request=%#v", req)
	}
}

func testRequestSuccess(t *testing.T, method, requestURI, host, userAgent, body, expectedMethod string) {
	var req Request

	req.Header.SetMethod(method)
	req.Header.SetRequestURI(requestURI)
	req.Header.Set("Host", host)
	req.Header.Set("User-Agent", userAgent)
	req.SetBody([]byte(body))

	contentType := "foobar"
	if method == "POST" {
		req.Header.Set("Content-Type", contentType)
	}

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	err := req.Write(bw)
	if err != nil {
		t.Fatalf("Unexpected error when calling Request.Write(): %s", err)
	}
	if err = bw.Flush(); err != nil {
		t.Fatalf("Unexpected error when flushing bufio.Writer: %s", err)
	}

	var req1 Request
	br := bufio.NewReader(w)
	if err = req1.Read(br, nil, nil); err != nil {
		t.Fatalf("Unexpected error when calling Request.Read(): %s", err)
	}
	if string(req1.Header.Method()) != expectedMethod {
		t.Fatalf("Unexpected method: %q. Expected %q", req1.Header.Method(), expectedMethod)
	}
	if len(requestURI) == 0 {
		requestURI = "/"
	}
	if string(req1.Header.RequestURI()) != requestURI {
		t.Fatalf("Unexpected RequestURI: %q. Expected %q", req1.Header.RequestURI(), requestURI)
	}
	if string(req1.Header.Peek("Host")) != host {
		t.Fatalf("Unexpected host: %q. Expected %q", req1.Header.Peek("Host"), host)
	}
	if len(userAgent) == 0 {
		userAgent = string(defaultUserAgent)
	}
	if string(req1.Header.Peek("User-Agent")) != userAgent {
		t.Fatalf("Unexpected user-agent: %q. Expected %q", req1.Header.Peek("User-Agent"), userAgent)
	}
	if !bytes.Equal(req1.Body(), []byte(body)) {
		t.Fatalf("Unexpected body: %q. Expected %q", req1.Body(), body)
	}

	if method == "POST" && string(req1.Header.Peek("Content-Type")) != contentType {
		t.Fatalf("Unexpected content-type: %q. Expected %q", req1.Header.Peek("Content-Type"), contentType)
	}
}

func TestResponseReadSuccess(t *testing.T) {
	resp := &Response{}

	// usual response
	testResponseReadSuccess(t, resp, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\nContent-Type: foo/bar\r\n\r\n0123456789",
		200, 10, "foo/bar", "0123456789", "")

	// zero response
	testResponseReadSuccess(t, resp, "HTTP/1.1 500 OK\r\nContent-Length: 0\r\nContent-Type: foo/bar\r\n\r\n",
		500, 0, "foo/bar", "", "")

	// response with trailer
	testResponseReadSuccess(t, resp, "HTTP/1.1 300 OK\r\nContent-Length: 5\r\nContent-Type: bar\r\n\r\n56789aaa",
		300, 5, "bar", "56789", "aaa")

	// no conent-length ('identity' transfer-encoding)
	testResponseReadSuccess(t, resp, "HTTP/1.1 200 OK\r\nContent-Type: foobar\r\n\r\nzxxc",
		200, 4, "foobar", "zxxc", "")

	// explicitly stated 'Transfer-Encoding: identity'
	testResponseReadSuccess(t, resp, "HTTP/1.1 234 ss\r\nContent-Type: xxx\r\n\r\nxag",
		234, 3, "xxx", "xag", "")

	// big 'identity' response
	body := string(createFixedBody(100500))
	testResponseReadSuccess(t, resp, "HTTP/1.1 200 OK\r\nContent-Type: aa\r\n\r\n"+body,
		200, 100500, "aa", body, "")

	// chunked response
	testResponseReadSuccess(t, resp, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nqwer\r\n2\r\nty\r\n0\r\n\r\nzzzzz",
		200, 6, "text/html", "qwerty", "zzzzz")

	// chunked response with non-chunked Transfer-Encoding.
	testResponseReadSuccess(t, resp, "HTTP/1.1 230 OK\r\nContent-Type: text\r\nTransfer-Encoding: aaabbb\r\n\r\n2\r\ner\r\n2\r\nty\r\n0\r\n\r\nwe",
		230, 4, "text", "erty", "we")

	// zero chunked response
	testResponseReadSuccess(t, resp, "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nzzz",
		200, 0, "text/html", "", "zzz")
}

func TestResponseReadError(t *testing.T) {
	resp := &Response{}

	// empty response
	testResponseReadError(t, resp, "")

	// invalid header
	testResponseReadError(t, resp, "foobar")

	// empty body
	testResponseReadError(t, resp, "HTTP/1.1 200 OK\r\nContent-Type: aaa\r\nContent-Length: 1234\r\n\r\n")

	// short body
	testResponseReadError(t, resp, "HTTP/1.1 200 OK\r\nContent-Type: aaa\r\nContent-Length: 1234\r\n\r\nshort")
}

func testResponseReadError(t *testing.T, resp *Response, response string) {
	r := bytes.NewBufferString(response)
	rb := bufio.NewReader(r)
	err := resp.Read(rb)
	if err == nil {
		t.Fatalf("Expecting error for response=%q", response)
	}

	testResponseReadSuccess(t, resp, "HTTP/1.1 303 Redisred sedfs sdf\r\nContent-Type: aaa\r\nContent-Length: 5\r\n\r\nHELLOaaa",
		303, 5, "aaa", "HELLO", "aaa")
}

func testResponseReadSuccess(t *testing.T, resp *Response, response string, expectedStatusCode, expectedContentLength int,
	expectedContenType, expectedBody, expectedTrailer string) {

	r := bytes.NewBufferString(response)
	rb := bufio.NewReader(r)
	err := resp.Read(rb)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	verifyResponseHeader(t, &resp.Header, expectedStatusCode, expectedContentLength, expectedContenType)
	if !bytes.Equal(resp.Body(), []byte(expectedBody)) {
		t.Fatalf("Unexpected body %q. Expected %q", resp.Body(), []byte(expectedBody))
	}
	verifyTrailer(t, rb, expectedTrailer)
}

func TestReadBodyFixedSize(t *testing.T) {
	var b []byte

	// zero-size body
	testReadBodyFixedSize(t, b, 0)

	// small-size body
	testReadBodyFixedSize(t, b, 3)

	// medium-size body
	testReadBodyFixedSize(t, b, 1024)

	// large-size body
	testReadBodyFixedSize(t, b, 1024*1024)

	// smaller body after big one
	testReadBodyFixedSize(t, b, 34345)
}

func TestReadBodyChunked(t *testing.T) {
	var b []byte

	// zero-size body
	testReadBodyChunked(t, b, 0)

	// small-size body
	testReadBodyChunked(t, b, 5)

	// medium-size body
	testReadBodyChunked(t, b, 43488)

	// big body
	testReadBodyChunked(t, b, 3*1024*1024)

	// smaler body after big one
	testReadBodyChunked(t, b, 12343)
}

func TestRequestURI(t *testing.T) {
	host := "foobar.com"
	requestURI := "/aaa/bb+b%20d?ccc=ddd&qqq#1334dfds&=d"
	expectedPathOriginal := "/aaa/bb+b%20d"
	expectedPath := "/aaa/bb+b d"
	expectedQueryString := "ccc=ddd&qqq"
	expectedHash := "1334dfds&=d"

	var req Request
	req.Header.Set("Host", host)
	req.Header.SetRequestURI(requestURI)

	uri := req.URI()
	if string(uri.Host()) != host {
		t.Fatalf("Unexpected host %q. Expected %q", uri.Host(), host)
	}
	if string(uri.PathOriginal()) != expectedPathOriginal {
		t.Fatalf("Unexpected source path %q. Expected %q", uri.PathOriginal(), expectedPathOriginal)
	}
	if string(uri.Path()) != expectedPath {
		t.Fatalf("Unexpected path %q. Expected %q", uri.Path(), expectedPath)
	}
	if string(uri.QueryString()) != expectedQueryString {
		t.Fatalf("Unexpected query string %q. Expected %q", uri.QueryString(), expectedQueryString)
	}
	if string(uri.Hash()) != expectedHash {
		t.Fatalf("Unexpected hash %q. Expected %q", uri.Hash(), expectedHash)
	}
}

func TestRequestPostArgsSuccess(t *testing.T) {
	var req Request

	testRequestPostArgsSuccess(t, &req, "POST / HTTP/1.1\r\nHost: aaa.com\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 0\r\n\r\n", 0, "foo=", "=")

	testRequestPostArgsSuccess(t, &req, "POST / HTTP/1.1\r\nHost: aaa.com\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 18\r\n\r\nfoo&b%20r=b+z=&qwe", 3, "foo=", "b r=b z=", "qwe=")
}

func TestRequestPostArgsError(t *testing.T) {
	var req Request

	// non-post
	testRequestPostArgsError(t, &req, "GET /aa HTTP/1.1\r\nHost: aaa\r\n\r\n")

	// invalid content-type
	testRequestPostArgsError(t, &req, "POST /aa HTTP/1.1\r\nHost: aaa\r\nContent-Type: text/html\r\nContent-Length: 5\r\n\r\nabcde")
}

func testRequestPostArgsError(t *testing.T, req *Request, s string) {
	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	err := req.Read(br, nil, nil)
	if err != nil {
		t.Fatalf("Unexpected error when reading %q: %s", s, err)
	}
	ss := req.PostArgs().String()
	if len(ss) != 0 {
		t.Fatalf("unexpected post args: %q. Expecting empty post args", ss)
	}
}

func testRequestPostArgsSuccess(t *testing.T, req *Request, s string, expectedArgsLen int, expectedArgs ...string) {
	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	err := req.Read(br, nil, nil)
	if err != nil {
		t.Fatalf("Unexpected error when reading %q: %s", s, err)
	}

	args := req.PostArgs()
	if args.Len() != expectedArgsLen {
		t.Fatalf("Unexpected args len %d. Expected %d for %q", args.Len(), expectedArgsLen, s)
	}
	for _, x := range expectedArgs {
		tmp := strings.SplitN(x, "=", 2)
		k := tmp[0]
		v := tmp[1]
		vv := string(args.Peek(k))
		if vv != v {
			t.Fatalf("Unexpected value for key %q: %q. Expected %q for %q", k, vv, v, s)
		}
	}
}

func testReadBodyChunked(t *testing.T, b []byte, bodySize int) {
	body := createFixedBody(bodySize)
	chunkedBody := createChunkedBody(body)
	expectedTrailer := []byte("chunked shit")
	chunkedBody = append(chunkedBody, expectedTrailer...)

	r := bytes.NewBuffer(chunkedBody)
	br := bufio.NewReader(r)
	b, err := readBody(br, -1, 0, nil)
	if err != nil {
		t.Fatalf("Unexpected error for bodySize=%d: %s. body=%q, chunkedBody=%q", bodySize, err, body, chunkedBody)
	}
	if !bytes.Equal(b, body) {
		t.Fatalf("Unexpected response read for bodySize=%d: %q. Expected %q. chunkedBody=%q", bodySize, b, body, chunkedBody)
	}
	verifyTrailer(t, br, string(expectedTrailer))
}

func testReadBodyFixedSize(t *testing.T, b []byte, bodySize int) {
	body := createFixedBody(bodySize)
	expectedTrailer := []byte("traler aaaa")
	bodyWithTrailer := append(body, expectedTrailer...)

	r := bytes.NewBuffer(bodyWithTrailer)
	br := bufio.NewReader(r)
	b, err := readBody(br, bodySize, 0, nil)
	if err != nil {
		t.Fatalf("Unexpected error in ReadResponseBody(%d): %s", bodySize, err)
	}
	if !bytes.Equal(b, body) {
		t.Fatalf("Unexpected response read for bodySize=%d: %q. Expected %q", bodySize, b, body)
	}
	verifyTrailer(t, br, string(expectedTrailer))
}

func createFixedBody(bodySize int) []byte {
	var b []byte
	for i := 0; i < bodySize; i++ {
		b = append(b, byte(i%10)+'0')
	}
	return b
}

func createChunkedBody(body []byte) []byte {
	var b []byte
	chunkSize := 1
	for len(body) > 0 {
		if chunkSize > len(body) {
			chunkSize = len(body)
		}
		b = append(b, []byte(fmt.Sprintf("%x\r\n", chunkSize))...)
		b = append(b, body[:chunkSize]...)
		b = append(b, []byte("\r\n")...)
		body = body[chunkSize:]
		chunkSize++
	}
	return append(b, []byte("0\r\n\r\n")...)
}

package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valyala/bytebufferpool"
)

func TestResponseEmptyTransferEncoding(t *testing.T) {
	t.Parallel()

	var r Response

	body := "Some body"
	br := bufio.NewReader(bytes.NewBufferString("HTTP/1.1 200 OK\r\nContent-Type: aaa\r\nTransfer-Encoding: \r\nContent-Length: 9\r\n\r\n" + body))
	err := r.Read(br)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(r.Body()); got != body {
		t.Fatalf("expected %q got %q", body, got)
	}
}

// Don't send the fragment/hash/# part of a URL to the server.
func TestFragmentInURIRequest(t *testing.T) {
	t.Parallel()

	var req Request
	req.SetRequestURI("https://docs.gitlab.com/ee/user/project/integrations/webhooks.html#events")

	var b bytes.Buffer
	req.WriteTo(&b) //nolint:errcheck
	got := b.String()
	expected := "GET /ee/user/project/integrations/webhooks.html HTTP/1.1\r\nHost: docs.gitlab.com\r\n\r\n"

	if got != expected {
		t.Errorf("got %q expected %q", got, expected)
	}
}

func TestIssue875(t *testing.T) {
	t.Parallel()

	type testcase struct {
		uri              string
		expectedRedirect string
		expectedLocation string
	}

	var testcases = []testcase{
		{
			uri:              `http://localhost:3000/?redirect=foo%0d%0aSet-Cookie:%20SESSIONID=MaliciousValue%0d%0a`,
			expectedRedirect: "foo\r\nSet-Cookie: SESSIONID=MaliciousValue\r\n",
			expectedLocation: "Location: foo  Set-Cookie: SESSIONID=MaliciousValue",
		},
		{
			uri:              `http://localhost:3000/?redirect=foo%0dSet-Cookie:%20SESSIONID=MaliciousValue%0d%0a`,
			expectedRedirect: "foo\rSet-Cookie: SESSIONID=MaliciousValue\r\n",
			expectedLocation: "Location: foo Set-Cookie: SESSIONID=MaliciousValue",
		},
		{
			uri:              `http://localhost:3000/?redirect=foo%0aSet-Cookie:%20SESSIONID=MaliciousValue%0d%0a`,
			expectedRedirect: "foo\nSet-Cookie: SESSIONID=MaliciousValue\r\n",
			expectedLocation: "Location: foo Set-Cookie: SESSIONID=MaliciousValue",
		},
	}

	for i, tcase := range testcases {
		caseName := strconv.FormatInt(int64(i), 10)
		t.Run(caseName, func(subT *testing.T) {
			ctx := &RequestCtx{
				Request:  Request{},
				Response: Response{},
			}
			ctx.Request.SetRequestURI(tcase.uri)

			q := string(ctx.QueryArgs().Peek("redirect"))
			if q != tcase.expectedRedirect {
				subT.Errorf("unexpected redirect query value, got: %+v", q)
			}
			ctx.Response.Header.Set("Location", q)

			if !strings.Contains(ctx.Response.String(), tcase.expectedLocation) {
				subT.Errorf("invalid escaping, got\n%s", ctx.Response.String())
			}
		})
	}
}

func TestRequestCopyTo(t *testing.T) {
	t.Parallel()

	var req Request

	// empty copy
	testRequestCopyTo(t, &req)

	// init
	expectedContentType := "application/x-www-form-urlencoded; charset=UTF-8"
	expectedHost := "test.com"
	expectedBody := "0123=56789"
	s := fmt.Sprintf("POST / HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s",
		expectedHost, expectedContentType, len(expectedBody), expectedBody)
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := req.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	testRequestCopyTo(t, &req)

}

func TestResponseCopyTo(t *testing.T) {
	t.Parallel()

	var resp Response

	// empty copy
	testResponseCopyTo(t, &resp)

	// init resp
	resp.laddr = zeroTCPAddr
	resp.SkipBody = true
	resp.Header.SetStatusCode(200)
	resp.SetBodyString("test")
	testResponseCopyTo(t, &resp)

}

func testRequestCopyTo(t *testing.T, src *Request) {
	var dst Request
	src.CopyTo(&dst)

	if !reflect.DeepEqual(*src, dst) { //nolint:govet
		t.Fatalf("RequestCopyTo fail, src: \n%+v\ndst: \n%+v\n", *src, dst) //nolint:govet
	}
}

func testResponseCopyTo(t *testing.T, src *Response) {
	var dst Response
	src.CopyTo(&dst)

	if !reflect.DeepEqual(*src, dst) { //nolint:govet
		t.Fatalf("ResponseCopyTo fail, src: \n%+v\ndst: \n%+v\n", *src, dst) //nolint:govet
	}
}

func TestResponseBodyStreamDeflate(t *testing.T) {
	t.Parallel()

	body := createFixedBody(1e5)

	// Verifies https://github.com/valyala/fasthttp/issues/176
	// when Content-Length is explicitly set.
	testResponseBodyStreamDeflate(t, body, len(body))

	// Verifies that 'transfer-encoding: chunked' works as expected.
	testResponseBodyStreamDeflate(t, body, -1)
}

func TestResponseBodyStreamGzip(t *testing.T) {
	t.Parallel()

	body := createFixedBody(1e5)

	// Verifies https://github.com/valyala/fasthttp/issues/176
	// when Content-Length is explicitly set.
	testResponseBodyStreamGzip(t, body, len(body))

	// Verifies that 'transfer-encoding: chunked' works as expected.
	testResponseBodyStreamGzip(t, body, -1)
}

func testResponseBodyStreamDeflate(t *testing.T, body []byte, bodySize int) {
	var r Response
	r.SetBodyStream(bytes.NewReader(body), bodySize)

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	if err := r.WriteDeflate(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var resp Response
	br := bufio.NewReader(w)
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	respBody, err := resp.BodyInflate()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !bytes.Equal(respBody, body) {
		t.Fatalf("unexpected body: %q. Expecting %q", respBody, body)
	}
}

func testResponseBodyStreamGzip(t *testing.T, body []byte, bodySize int) {
	var r Response
	r.SetBodyStream(bytes.NewReader(body), bodySize)

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	if err := r.WriteGzip(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var resp Response
	br := bufio.NewReader(w)
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	respBody, err := resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !bytes.Equal(respBody, body) {
		t.Fatalf("unexpected body: %q. Expecting %q", respBody, body)
	}
}

func TestResponseWriteGzipNilBody(t *testing.T) {
	t.Parallel()

	var r Response
	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	if err := r.WriteGzip(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestResponseWriteDeflateNilBody(t *testing.T) {
	t.Parallel()

	var r Response
	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	if err := r.WriteDeflate(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestResponseSwapBodySerial(t *testing.T) {
	t.Parallel()

	testResponseSwapBody(t)
}

func TestResponseSwapBodyConcurrent(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			testResponseSwapBody(t)
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

func testResponseSwapBody(t *testing.T) {
	var b []byte
	r := AcquireResponse()
	for i := 0; i < 20; i++ {
		bOrig := r.Body()
		b = r.SwapBody(b)
		if !bytes.Equal(bOrig, b) {
			t.Fatalf("unexpected body returned: %q. Expecting %q", b, bOrig)
		}
		r.AppendBodyString("foobar")
	}

	s := "aaaabbbbcccc"
	b = b[:0]
	for i := 0; i < 10; i++ {
		r.SetBodyStream(bytes.NewBufferString(s), len(s))
		b = r.SwapBody(b)
		if string(b) != s {
			t.Fatalf("unexpected body returned: %q. Expecting %q", b, s)
		}
		b = r.SwapBody(b)
		if len(b) > 0 {
			t.Fatalf("unexpected body with non-zero size returned: %q", b)
		}
	}
	ReleaseResponse(r)
}

func TestRequestSwapBodySerial(t *testing.T) {
	t.Parallel()

	testRequestSwapBody(t)
}

func TestRequestSwapBodyConcurrent(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			testRequestSwapBody(t)
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

func testRequestSwapBody(t *testing.T) {
	var b []byte
	r := AcquireRequest()
	for i := 0; i < 20; i++ {
		bOrig := r.Body()
		b = r.SwapBody(b)
		if !bytes.Equal(bOrig, b) {
			t.Fatalf("unexpected body returned: %q. Expecting %q", b, bOrig)
		}
		r.AppendBodyString("foobar")
	}

	s := "aaaabbbbcccc"
	b = b[:0]
	for i := 0; i < 10; i++ {
		r.SetBodyStream(bytes.NewBufferString(s), len(s))
		b = r.SwapBody(b)
		if string(b) != s {
			t.Fatalf("unexpected body returned: %q. Expecting %q", b, s)
		}
		b = r.SwapBody(b)
		if len(b) > 0 {
			t.Fatalf("unexpected body with non-zero size returned: %q", b)
		}
	}
	ReleaseRequest(r)
}

func TestRequestHostFromRequestURI(t *testing.T) {
	t.Parallel()

	hExpected := "foobar.com"
	var req Request
	req.SetRequestURI("http://proxy-host:123/foobar?baz")
	req.SetHost(hExpected)
	h := req.Host()
	if string(h) != hExpected {
		t.Fatalf("unexpected host set: %q. Expecting %q", h, hExpected)
	}
}

func TestRequestHostFromHeader(t *testing.T) {
	t.Parallel()

	hExpected := "foobar.com"
	var req Request
	req.Header.SetHost(hExpected)
	h := req.Host()
	if string(h) != hExpected {
		t.Fatalf("unexpected host set: %q. Expecting %q", h, hExpected)
	}
}

func TestRequestContentTypeWithCharsetIssue100(t *testing.T) {
	t.Parallel()

	expectedContentType := "application/x-www-form-urlencoded; charset=UTF-8"
	expectedBody := "0123=56789"
	s := fmt.Sprintf("POST / HTTP/1.1\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s",
		expectedContentType, len(expectedBody), expectedBody)

	br := bufio.NewReader(bytes.NewBufferString(s))
	var r Request
	if err := r.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	body := r.Body()
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}
	ct := r.Header.ContentType()
	if string(ct) != expectedContentType {
		t.Fatalf("unexpected content-type %q. Expecting %q", ct, expectedContentType)
	}
	args := r.PostArgs()
	if args.Len() != 1 {
		t.Fatalf("unexpected number of POST args: %d. Expecting 1", args.Len())
	}
	av := args.Peek("0123")
	if string(av) != "56789" {
		t.Fatalf("unexpected POST arg value: %q. Expecting %q", av, "56789")
	}
}

func TestRequestReadMultipartFormWithFile(t *testing.T) {
	t.Parallel()

	s := `POST /upload HTTP/1.1
Host: localhost:10000
Content-Length: 521
Content-Type: multipart/form-data; boundary=----WebKitFormBoundaryJwfATyF8tmxSJnLg

------WebKitFormBoundaryJwfATyF8tmxSJnLg
Content-Disposition: form-data; name="f1"

value1
------WebKitFormBoundaryJwfATyF8tmxSJnLg
Content-Disposition: form-data; name="fileaaa"; filename="TODO"
Content-Type: application/octet-stream

- SessionClient with referer and cookies support.
- Client with requests' pipelining support.
- ProxyHandler similar to FSHandler.
- WebSockets. See https://tools.ietf.org/html/rfc6455 .
- HTTP/2.0. See https://tools.ietf.org/html/rfc7540 .

------WebKitFormBoundaryJwfATyF8tmxSJnLg--
tailfoobar`

	br := bufio.NewReader(bytes.NewBufferString(s))

	var r Request
	if err := r.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	tail, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(tail) != "tailfoobar" {
		t.Fatalf("unexpected tail %q. Expecting %q", tail, "tailfoobar")
	}

	f, err := r.MultipartForm()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	defer r.RemoveMultipartFormFiles()

	// verify values
	if len(f.Value) != 1 {
		t.Fatalf("unexpected number of values in multipart form: %d. Expecting 1", len(f.Value))
	}
	for k, vv := range f.Value {
		if k != "f1" {
			t.Fatalf("unexpected value name %q. Expecting %q", k, "f1")
		}
		if len(vv) != 1 {
			t.Fatalf("unexpected number of values %d. Expecting 1", len(vv))
		}
		v := vv[0]
		if v != "value1" {
			t.Fatalf("unexpected value %q. Expecting %q", v, "value1")
		}
	}

	// verify files
	if len(f.File) != 1 {
		t.Fatalf("unexpected number of file values in multipart form: %d. Expecting 1", len(f.File))
	}
	for k, vv := range f.File {
		if k != "fileaaa" {
			t.Fatalf("unexpected file value name %q. Expecting %q", k, "fileaaa")
		}
		if len(vv) != 1 {
			t.Fatalf("unexpected number of file values %d. Expecting 1", len(vv))
		}
		v := vv[0]
		if v.Filename != "TODO" {
			t.Fatalf("unexpected filename %q. Expecting %q", v.Filename, "TODO")
		}
		ct := v.Header.Get("Content-Type")
		if ct != "application/octet-stream" {
			t.Fatalf("unexpected content-type %q. Expecting %q", ct, "application/octet-stream")
		}
	}
}

func TestRequestRequestURI(t *testing.T) {
	t.Parallel()

	var r Request

	// Set request uri via SetRequestURI()
	uri := "/foo/bar?baz"
	r.SetRequestURI(uri)
	if string(r.RequestURI()) != uri {
		t.Fatalf("unexpected request uri %q. Expecting %q", r.RequestURI(), uri)
	}

	// Set request uri via Request.URI().Update()
	r.Reset()
	uri = "/aa/bbb?ccc=sdfsdf"
	r.URI().Update(uri)
	if string(r.RequestURI()) != uri {
		t.Fatalf("unexpected request uri %q. Expecting %q", r.RequestURI(), uri)
	}

	// update query args in the request uri
	qa := r.URI().QueryArgs()
	qa.Reset()
	qa.Set("foo", "bar")
	uri = "/aa/bbb?foo=bar"
	if string(r.RequestURI()) != uri {
		t.Fatalf("unexpected request uri %q. Expecting %q", r.RequestURI(), uri)
	}
}

func TestRequestUpdateURI(t *testing.T) {
	t.Parallel()

	var r Request
	r.Header.SetHost("aaa.bbb")
	r.SetRequestURI("/lkjkl/kjl")

	// Modify request uri and host via URI() object and make sure
	// the requestURI and Host header are properly updated
	u := r.URI()
	u.SetPath("/123/432.html")
	u.SetHost("foobar.com")
	a := u.QueryArgs()
	a.Set("aaa", "bcse")

	s := r.String()
	if !strings.HasPrefix(s, "GET /123/432.html?aaa=bcse") {
		t.Fatalf("cannot find %q in %q", "GET /123/432.html?aaa=bcse", s)
	}
	if !strings.Contains(s, "\r\nHost: foobar.com\r\n") {
		t.Fatalf("cannot find %q in %q", "\r\nHost: foobar.com\r\n", s)
	}
}

func TestRequestBodyStreamMultipleBodyCalls(t *testing.T) {
	t.Parallel()

	var r Request

	s := "foobar baz abc"
	if r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return false")
	}
	r.SetBodyStream(bytes.NewBufferString(s), len(s))
	if !r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return true")
	}
	for i := 0; i < 10; i++ {
		body := r.Body()
		if string(body) != s {
			t.Fatalf("unexpected body %q. Expecting %q. iteration %d", body, s, i)
		}
	}
}

func TestResponseBodyStreamMultipleBodyCalls(t *testing.T) {
	t.Parallel()

	var r Response

	s := "foobar baz abc"
	if r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return false")
	}
	r.SetBodyStream(bytes.NewBufferString(s), len(s))
	if !r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return true")
	}
	for i := 0; i < 10; i++ {
		body := r.Body()
		if string(body) != s {
			t.Fatalf("unexpected body %q. Expecting %q. iteration %d", body, s, i)
		}
	}
}

func TestRequestBodyWriteToPlain(t *testing.T) {
	t.Parallel()

	var r Request

	expectedS := "foobarbaz"
	r.AppendBodyString(expectedS)

	testBodyWriteTo(t, &r, expectedS, true)
}

func TestResponseBodyWriteToPlain(t *testing.T) {
	t.Parallel()

	var r Response

	expectedS := "foobarbaz"
	r.AppendBodyString(expectedS)

	testBodyWriteTo(t, &r, expectedS, true)
}

func TestResponseBodyWriteToStream(t *testing.T) {
	t.Parallel()

	var r Response

	expectedS := "aaabbbccc"
	buf := bytes.NewBufferString(expectedS)
	if r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return false")
	}
	r.SetBodyStream(buf, len(expectedS))
	if !r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return true")
	}

	testBodyWriteTo(t, &r, expectedS, false)
}

func TestRequestBodyWriteToMultipart(t *testing.T) {
	t.Parallel()

	expectedS := "--foobar\r\nContent-Disposition: form-data; name=\"key_0\"\r\n\r\nvalue_0\r\n--foobar--\r\n"
	s := fmt.Sprintf("POST / HTTP/1.1\r\nHost: aaa\r\nContent-Type: multipart/form-data; boundary=foobar\r\nContent-Length: %d\r\n\r\n%s",
		len(expectedS), expectedS)

	var r Request
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := r.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	testBodyWriteTo(t, &r, expectedS, true)
}

type bodyWriterTo interface {
	BodyWriteTo(io.Writer) error
	Body() []byte
}

func testBodyWriteTo(t *testing.T, bw bodyWriterTo, expectedS string, isRetainedBody bool) {
	var buf bytebufferpool.ByteBuffer
	if err := bw.BodyWriteTo(&buf); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	s := buf.B
	if string(s) != expectedS {
		t.Fatalf("unexpected result %q. Expecting %q", s, expectedS)
	}

	body := bw.Body()
	if isRetainedBody {
		if string(body) != expectedS {
			t.Fatalf("unexpected body %q. Expecting %q", body, expectedS)
		}
	} else {
		if len(body) > 0 {
			t.Fatalf("unexpected non-zero body after BodyWriteTo: %q", body)
		}
	}
}

func TestRequestReadEOF(t *testing.T) {
	t.Parallel()

	var r Request

	br := bufio.NewReader(&bytes.Buffer{})
	err := r.Read(br)
	if err == nil {
		t.Fatalf("expecting error")
	}
	if err != io.EOF {
		t.Fatalf("unexpected error: %s. Expecting %s", err, io.EOF)
	}

	// incomplete request mustn't return io.EOF
	br = bufio.NewReader(bytes.NewBufferString("POST / HTTP/1.1\r\nContent-Type: aa\r\nContent-Length: 1234\r\n\r\nIncomplete body"))
	err = r.Read(br)
	if err == nil {
		t.Fatalf("expecting error")
	}
	if err == io.EOF {
		t.Fatalf("expecting non-EOF error")
	}
}

func TestResponseReadEOF(t *testing.T) {
	t.Parallel()

	var r Response

	br := bufio.NewReader(&bytes.Buffer{})
	err := r.Read(br)
	if err == nil {
		t.Fatalf("expecting error")
	}
	if err != io.EOF {
		t.Fatalf("unexpected error: %s. Expecting %s", err, io.EOF)
	}

	// incomplete response mustn't return io.EOF
	br = bufio.NewReader(bytes.NewBufferString("HTTP/1.1 200 OK\r\nContent-Type: aaa\r\nContent-Length: 123\r\n\r\nIncomplete body"))
	err = r.Read(br)
	if err == nil {
		t.Fatalf("expecting error")
	}
	if err == io.EOF {
		t.Fatalf("expecting non-EOF error")
	}
}

func TestRequestReadNoBody(t *testing.T) {
	t.Parallel()

	var r Request

	br := bufio.NewReader(bytes.NewBufferString("GET / HTTP/1.1\r\n\r\n"))
	err := r.Read(br)
	r.SetHost("foobar")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	s := r.String()
	if strings.Contains(s, "Content-Length: ") {
		t.Fatalf("unexpected Content-Length")
	}
}

func TestResponseWriteTo(t *testing.T) {
	t.Parallel()

	var r Response

	r.SetBodyString("foobar")

	s := r.String()
	var buf bytebufferpool.ByteBuffer
	n, err := r.WriteTo(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if n != int64(len(s)) {
		t.Fatalf("unexpected response length %d. Expecting %d", n, len(s))
	}
	if string(buf.B) != s {
		t.Fatalf("unexpected response %q. Expecting %q", buf.B, s)
	}
}

func TestRequestWriteTo(t *testing.T) {
	t.Parallel()

	var r Request

	r.SetRequestURI("http://foobar.com/aaa/bbb")

	s := r.String()
	var buf bytebufferpool.ByteBuffer
	n, err := r.WriteTo(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if n != int64(len(s)) {
		t.Fatalf("unexpected request length %d. Expecting %d", n, len(s))
	}
	if string(buf.B) != s {
		t.Fatalf("unexpected request %q. Expecting %q", buf.B, s)
	}
}

func TestResponseSkipBody(t *testing.T) {
	t.Parallel()

	var r Response

	// set StatusNotModified
	r.Header.SetStatusCode(StatusNotModified)
	r.SetBodyString("foobar")
	s := r.String()
	if strings.Contains(s, "\r\n\r\nfoobar") {
		t.Fatalf("unexpected non-zero body in response %q", s)
	}
	if strings.Contains(s, "Content-Length: ") {
		t.Fatalf("unexpected content-length in response %q", s)
	}
	if strings.Contains(s, "Content-Type: ") {
		t.Fatalf("unexpected content-type in response %q", s)
	}

	// set StatusNoContent
	r.Header.SetStatusCode(StatusNoContent)
	r.SetBodyString("foobar")
	s = r.String()
	if strings.Contains(s, "\r\n\r\nfoobar") {
		t.Fatalf("unexpected non-zero body in response %q", s)
	}
	if strings.Contains(s, "Content-Length: ") {
		t.Fatalf("unexpected content-length in response %q", s)
	}
	if strings.Contains(s, "Content-Type: ") {
		t.Fatalf("unexpected content-type in response %q", s)
	}

	// explicitly skip body
	r.Header.SetStatusCode(StatusOK)
	r.SkipBody = true
	r.SetBodyString("foobar")
	s = r.String()
	if strings.Contains(s, "\r\n\r\nfoobar") {
		t.Fatalf("unexpected non-zero body in response %q", s)
	}
	if !strings.Contains(s, "Content-Length: 6\r\n") {
		t.Fatalf("expecting content-length in response %q", s)
	}
	if !strings.Contains(s, "Content-Type: ") {
		t.Fatalf("expecting content-type in response %q", s)
	}
}

func TestRequestNoContentLength(t *testing.T) {
	t.Parallel()

	var r Request

	r.Header.SetMethod(MethodHead)
	r.Header.SetHost("foobar")

	s := r.String()
	if strings.Contains(s, "Content-Length: ") {
		t.Fatalf("unexpected content-length in HEAD request %q", s)
	}

	r.Header.SetMethod(MethodPost)
	fmt.Fprintf(r.BodyWriter(), "foobar body")
	s = r.String()
	if !strings.Contains(s, "Content-Length: ") {
		t.Fatalf("missing content-length header in non-GET request %q", s)
	}
}

func TestRequestReadGzippedBody(t *testing.T) {
	t.Parallel()

	var r Request

	bodyOriginal := "foo bar baz compress me better!"
	body := AppendGzipBytes(nil, []byte(bodyOriginal))
	s := fmt.Sprintf("POST /foobar HTTP/1.1\r\nContent-Type: foo/bar\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n%s",
		len(body), body)
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := r.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if string(r.Header.Peek(HeaderContentEncoding)) != "gzip" {
		t.Fatalf("unexpected content-encoding: %q. Expecting %q", r.Header.Peek(HeaderContentEncoding), "gzip")
	}
	if r.Header.ContentLength() != len(body) {
		t.Fatalf("unexpected content-length: %d. Expecting %d", r.Header.ContentLength(), len(body))
	}
	if string(r.Body()) != string(body) {
		t.Fatalf("unexpected body: %q. Expecting %q", r.Body(), body)
	}

	bodyGunzipped, err := AppendGunzipBytes(nil, r.Body())
	if err != nil {
		t.Fatalf("unexpected error when uncompressing data: %s", err)
	}
	if string(bodyGunzipped) != bodyOriginal {
		t.Fatalf("unexpected uncompressed body %q. Expecting %q", bodyGunzipped, bodyOriginal)
	}
}

func TestRequestReadPostNoBody(t *testing.T) {
	t.Parallel()

	var r Request

	s := "POST /foo/bar HTTP/1.1\r\nContent-Type: aaa/bbb\r\n\r\naaaa"
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := r.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if string(r.Header.RequestURI()) != "/foo/bar" {
		t.Fatalf("unexpected request uri %q. Expecting %q", r.Header.RequestURI(), "/foo/bar")
	}
	if string(r.Header.ContentType()) != "aaa/bbb" {
		t.Fatalf("unexpected content-type %q. Expecting %q", r.Header.ContentType(), "aaa/bbb")
	}
	if len(r.Body()) != 0 {
		t.Fatalf("unexpected body found %q. Expecting empty body", r.Body())
	}
	if r.Header.ContentLength() != 0 {
		t.Fatalf("unexpected content-length: %d. Expecting 0", r.Header.ContentLength())
	}

	tail, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(tail) != "aaaa" {
		t.Fatalf("unexpected tail %q. Expecting %q", tail, "aaaa")
	}
}

func TestRequestContinueReadBody(t *testing.T) {
	t.Parallel()

	s := "PUT /foo/bar HTTP/1.1\r\nExpect: 100-continue\r\nContent-Length: 5\r\nContent-Type: foo/bar\r\n\r\nabcdef4343"
	br := bufio.NewReader(bytes.NewBufferString(s))

	var r Request
	if err := r.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !r.MayContinue() {
		t.Fatalf("MayContinue must return true")
	}

	if err := r.ContinueReadBody(br, 0, true); err != nil {
		t.Fatalf("error when reading request body: %s", err)
	}
	body := r.Body()
	if string(body) != "abcde" {
		t.Fatalf("unexpected body %q. Expecting %q", body, "abcde")
	}

	tail, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(tail) != "f4343" {
		t.Fatalf("unexpected tail %q. Expecting %q", tail, "f4343")
	}
}

func TestRequestContinueReadBodyDisablePrereadMultipartForm(t *testing.T) {
	t.Parallel()

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
	br := bufio.NewReader(bytes.NewBufferString(s))

	var r Request

	if err := r.Header.Read(br); err != nil {
		t.Fatalf("unexpected error reading headers: %s", err)
	}

	if err := r.readLimitBody(br, 10000, false, false); err != nil {
		t.Fatalf("unexpected error reading body: %s", err)
	}

	if r.multipartForm != nil {
		t.Fatalf("The multipartForm of the Request must be nil")
	}

	if string(formData) != string(r.Body()) {
		t.Fatalf("The body given must equal the body in the Request")
	}

}

func TestRequestMayContinue(t *testing.T) {
	t.Parallel()

	var r Request
	if r.MayContinue() {
		t.Fatalf("MayContinue on empty request must return false")
	}

	r.Header.Set("Expect", "123sdfds")
	if r.MayContinue() {
		t.Fatalf("MayContinue on invalid Expect header must return false")
	}

	r.Header.Set("Expect", "100-continue")
	if !r.MayContinue() {
		t.Fatalf("MayContinue on 'Expect: 100-continue' header must return true")
	}
}

func TestResponseGzipStream(t *testing.T) {
	t.Parallel()

	var r Response
	if r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return false")
	}
	r.SetBodyStreamWriter(func(w *bufio.Writer) {
		fmt.Fprintf(w, "foo")
		w.Flush()
		time.Sleep(time.Millisecond)
		w.Write([]byte("barbaz")) //nolint:errcheck
		w.Flush()                 //nolint:errcheck
		time.Sleep(time.Millisecond)
		fmt.Fprintf(w, "1234") //nolint:errcheck
		if err := w.Flush(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	})
	if !r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return true")
	}
	testResponseGzipExt(t, &r, "foobarbaz1234")
}

func TestResponseDeflateStream(t *testing.T) {
	t.Parallel()

	var r Response
	if r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return false")
	}
	r.SetBodyStreamWriter(func(w *bufio.Writer) {
		w.Write([]byte("foo"))   //nolint:errcheck
		w.Flush()                //nolint:errcheck
		fmt.Fprintf(w, "barbaz") //nolint:errcheck
		w.Flush()                //nolint:errcheck
		w.Write([]byte("1234"))  //nolint:errcheck
		if err := w.Flush(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	})
	if !r.IsBodyStream() {
		t.Fatalf("IsBodyStream must return true")
	}
	testResponseDeflateExt(t, &r, "foobarbaz1234")
}

func TestResponseDeflate(t *testing.T) {
	t.Parallel()

	for _, s := range compressTestcases {
		testResponseDeflate(t, s)
	}
}

func TestResponseGzip(t *testing.T) {
	t.Parallel()

	for _, s := range compressTestcases {
		testResponseGzip(t, s)
	}
}

func testResponseDeflate(t *testing.T, s string) {
	var r Response
	r.SetBodyString(s)
	testResponseDeflateExt(t, &r, s)

	// make sure the uncompressible Content-Type isn't compressed
	r.Reset()
	r.Header.SetContentType("image/jpeg")
	r.SetBodyString(s)
	testResponseDeflateExt(t, &r, s)
}

func testResponseDeflateExt(t *testing.T, r *Response, s string) {
	isCompressible := isCompressibleResponse(r, s)

	var buf bytes.Buffer
	var err error
	bw := bufio.NewWriter(&buf)
	if err = r.WriteDeflate(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err = bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var r1 Response
	br := bufio.NewReader(&buf)
	if err = r1.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	ce := r1.Header.Peek(HeaderContentEncoding)
	var body []byte
	if isCompressible {
		if string(ce) != "deflate" {
			t.Fatalf("unexpected Content-Encoding %q. Expecting %q. len(s)=%d, Content-Type: %q",
				ce, "deflate", len(s), r.Header.ContentType())
		}
		body, err = r1.BodyInflate()
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	} else {
		if len(ce) > 0 {
			t.Fatalf("expecting empty Content-Encoding. Got %q", ce)
		}
		body = r1.Body()
	}
	if string(body) != s {
		t.Fatalf("unexpected body %q. Expecting %q", body, s)
	}
}

func testResponseGzip(t *testing.T, s string) {
	var r Response
	r.SetBodyString(s)
	testResponseGzipExt(t, &r, s)

	// make sure the uncompressible Content-Type isn't compressed
	r.Reset()
	r.Header.SetContentType("image/jpeg")
	r.SetBodyString(s)
	testResponseGzipExt(t, &r, s)
}

func testResponseGzipExt(t *testing.T, r *Response, s string) {
	isCompressible := isCompressibleResponse(r, s)

	var buf bytes.Buffer
	var err error
	bw := bufio.NewWriter(&buf)
	if err = r.WriteGzip(bw); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err = bw.Flush(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var r1 Response
	br := bufio.NewReader(&buf)
	if err = r1.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	ce := r1.Header.Peek(HeaderContentEncoding)
	var body []byte
	if isCompressible {
		if string(ce) != "gzip" {
			t.Fatalf("unexpected Content-Encoding %q. Expecting %q. len(s)=%d, Content-Type: %q",
				ce, "gzip", len(s), r.Header.ContentType())
		}
		body, err = r1.BodyGunzip()
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	} else {
		if len(ce) > 0 {
			t.Fatalf("Expecting empty Content-Encoding. Got %q", ce)
		}
		body = r1.Body()
	}
	if string(body) != s {
		t.Fatalf("unexpected body %q. Expecting %q", body, s)
	}
}

func isCompressibleResponse(r *Response, s string) bool {
	isCompressible := r.Header.isCompressibleContentType()
	if isCompressible && len(s) < minCompressLen && !r.IsBodyStream() {
		isCompressible = false
	}
	return isCompressible
}

func TestRequestMultipartForm(t *testing.T) {
	t.Parallel()

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
	for i := 0; i < 5; i++ {
		formData = testRequestMultipartForm(t, boundary, formData, 10)
	}

	// verify request unmarshalling / marshalling
	s := "POST / HTTP/1.1\r\nHost: aaa\r\nContent-Type: multipart/form-data; boundary=foobar\r\nContent-Length: 213\r\n\r\n--foobar\r\nContent-Disposition: form-data; name=\"key_0\"\r\n\r\nvalue_0\r\n--foobar\r\nContent-Disposition: form-data; name=\"key_1\"\r\n\r\nvalue_1\r\n--foobar\r\nContent-Disposition: form-data; name=\"key_2\"\r\n\r\nvalue_2\r\n--foobar--\r\n"

	var req Request
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := req.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	s = req.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := req.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	testRequestMultipartForm(t, "foobar", req.Body(), 3)
}

func testRequestMultipartForm(t *testing.T, boundary string, formData []byte, partsCount int) []byte {
	s := fmt.Sprintf("POST / HTTP/1.1\r\nHost: aaa\r\nContent-Type: multipart/form-data; boundary=%s\r\nContent-Length: %d\r\n\r\n%s",
		boundary, len(formData), formData)

	var req Request

	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	if err := req.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	f, err := req.MultipartForm()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	defer req.RemoveMultipartFormFiles()

	if len(f.File) > 0 {
		t.Fatalf("unexpected files found in the multipart form: %d", len(f.File))
	}

	if len(f.Value) != partsCount {
		t.Fatalf("unexpected number of values found: %d. Expecting %d", len(f.Value), partsCount)
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

	return req.Body()
}

func TestResponseReadLimitBody(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	err := req.ReadLimitBody(br, maxBodySize)
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
	if err := req.ReadLimitBody(br, maxBodySize); err != nil {
		t.Fatalf("unexpected error: %s. s=%q, maxBodySize=%d", err, s, maxBodySize)
	}
}

func TestRequestString(t *testing.T) {
	t.Parallel()

	var r Request
	r.SetRequestURI("http://foobar.com/aaa")
	s := r.String()
	expectedS := "GET /aaa HTTP/1.1\r\nHost: foobar.com\r\n\r\n"
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
	t.Parallel()

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
	t.Parallel()

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
	if err := req1.Read(br); err != nil {
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

func TestSetRequestBodyStreamFixedSize(t *testing.T) {
	t.Parallel()

	testSetRequestBodyStream(t, "a", false)
	testSetRequestBodyStream(t, string(createFixedBody(4097)), false)
	testSetRequestBodyStream(t, string(createFixedBody(100500)), false)
}

func TestSetResponseBodyStreamFixedSize(t *testing.T) {
	t.Parallel()

	testSetResponseBodyStream(t, "a", false)
	testSetResponseBodyStream(t, string(createFixedBody(4097)), false)
	testSetResponseBodyStream(t, string(createFixedBody(100500)), false)
}

func TestSetRequestBodyStreamChunked(t *testing.T) {
	t.Parallel()

	testSetRequestBodyStream(t, "", true)

	body := "foobar baz aaa bbb ccc"
	testSetRequestBodyStream(t, body, true)

	body = string(createFixedBody(10001))
	testSetRequestBodyStream(t, body, true)
}

func TestSetResponseBodyStreamChunked(t *testing.T) {
	t.Parallel()

	testSetResponseBodyStream(t, "", true)

	body := "foobar baz aaa bbb ccc"
	testSetResponseBodyStream(t, body, true)

	body = string(createFixedBody(10001))
	testSetResponseBodyStream(t, body, true)
}

func testSetRequestBodyStream(t *testing.T, body string, chunked bool) {
	var req Request
	req.Header.SetHost("foobar.com")
	req.Header.SetMethod(MethodPost)

	bodySize := len(body)
	if chunked {
		bodySize = -1
	}
	if req.IsBodyStream() {
		t.Fatalf("IsBodyStream must return false")
	}
	req.SetBodyStream(bytes.NewBufferString(body), bodySize)
	if !req.IsBodyStream() {
		t.Fatalf("IsBodyStream must return true")
	}

	var w bytes.Buffer
	bw := bufio.NewWriter(&w)
	if err := req.Write(bw); err != nil {
		t.Fatalf("unexpected error when writing request: %s. body=%q", err, body)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("unexpected error when flushing request: %s. body=%q", err, body)
	}

	var req1 Request
	br := bufio.NewReader(&w)
	if err := req1.Read(br); err != nil {
		t.Fatalf("unexpected error when reading request: %s. body=%q", err, body)
	}
	if string(req1.Body()) != body {
		t.Fatalf("unexpected body %q. Expecting %q", req1.Body(), body)
	}
}

func testSetResponseBodyStream(t *testing.T, body string, chunked bool) {
	var resp Response
	bodySize := len(body)
	if chunked {
		bodySize = -1
	}
	if resp.IsBodyStream() {
		t.Fatalf("IsBodyStream must return false")
	}
	resp.SetBodyStream(bytes.NewBufferString(body), bodySize)
	if !resp.IsBodyStream() {
		t.Fatalf("IsBodyStream must return true")
	}

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
	t.Parallel()

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
	t.Parallel()

	var req Request

	s := "POST /foo HTTP/1.1\r\nHost: google.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa/bb\r\n\r\n3\r\nabc\r\n5\r\n12345\r\n0\r\n\r\ntrail"
	r := bytes.NewBufferString(s)
	rb := bufio.NewReader(r)
	err := req.Read(rb)
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

// See: https://github.com/erikdubbelboer/fasthttp/issues/34
func TestRequestChunkedWhitespace(t *testing.T) {
	t.Parallel()

	var req Request

	s := "POST /foo HTTP/1.1\r\nHost: google.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa/bb\r\n\r\n3  \r\nabc\r\n0\r\n\r\n"
	r := bytes.NewBufferString(s)
	rb := bufio.NewReader(r)
	err := req.Read(rb)
	if err != nil {
		t.Fatalf("Unexpected error when reading chunked request: %s", err)
	}
	expectedBody := "abc"
	if string(req.Body()) != expectedBody {
		t.Fatalf("Unexpected body %q. Expected %q", req.Body(), expectedBody)
	}
}

func TestResponseReadWithoutBody(t *testing.T) {
	t.Parallel()

	var resp Response

	testResponseReadWithoutBody(t, &resp, "HTTP/1.1 304 Not Modified\r\nContent-Type: aa\r\nContent-Length: 1235\r\n\r\nfoobar", false,
		304, 1235, "aa", "foobar")

	testResponseReadWithoutBody(t, &resp, "HTTP/1.1 204 Foo Bar\r\nContent-Type: aab\r\nTransfer-Encoding: chunked\r\n\r\n123\r\nss", false,
		204, -1, "aab", "123\r\nss")

	testResponseReadWithoutBody(t, &resp, "HTTP/1.1 123 AAA\r\nContent-Type: xxx\r\nContent-Length: 3434\r\n\r\naaaa", false,
		123, 3434, "xxx", "aaaa")

	testResponseReadWithoutBody(t, &resp, "HTTP 200 OK\r\nContent-Type: text/xml\r\nContent-Length: 123\r\n\r\nxxxx", true,
		200, 123, "text/xml", "xxxx")

	// '100 Continue' must be skipped.
	testResponseReadWithoutBody(t, &resp, "HTTP/1.1 100 Continue\r\nFoo-bar: baz\r\n\r\nHTTP/1.1 329 aaa\r\nContent-Type: qwe\r\nContent-Length: 894\r\n\r\nfoobar", true,
		329, 894, "qwe", "foobar")
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
	t.Parallel()

	// empty method, user-agent and body
	testRequestSuccess(t, "", "/foo/bar", "google.com", "", "", MethodGet)

	// non-empty user-agent
	testRequestSuccess(t, MethodGet, "/foo/bar", "google.com", "MSIE", "", MethodGet)

	// non-empty method
	testRequestSuccess(t, MethodHead, "/aaa", "fobar", "", "", MethodHead)

	// POST method with body
	testRequestSuccess(t, MethodPost, "/bbb", "aaa.com", "Chrome aaa", "post body", MethodPost)

	// PUT method with body
	testRequestSuccess(t, MethodPut, "/aa/bb", "a.com", "ome aaa", "put body", MethodPut)

	// only host is set
	testRequestSuccess(t, "", "", "gooble.com", "", "", MethodGet)

	// get with body
	testRequestSuccess(t, MethodGet, "/foo/bar", "aaa.com", "", "foobar", MethodGet)
}

func TestResponseSuccess(t *testing.T) {
	t.Parallel()

	// 200 response
	testResponseSuccess(t, 200, "test/plain", "server", "foobar",
		200, "test/plain", "server")

	// response with missing statusCode
	testResponseSuccess(t, 0, "text/plain", "server", "foobar",
		200, "text/plain", "server")

	// response with missing server
	testResponseSuccess(t, 500, "aaa", "", "aaadfsd",
		500, "aaa", "")

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
	if string(resp1.Header.Peek(HeaderContentType)) != expectedContentType {
		t.Fatalf("Unexpected content-type: %q. Expected %q", resp1.Header.Peek(HeaderContentType), expectedContentType)
	}
	if string(resp1.Header.Peek(HeaderServer)) != expectedServerName {
		t.Fatalf("Unexpected server: %q. Expected %q", resp1.Header.Peek(HeaderServer), expectedServerName)
	}
	if !bytes.Equal(resp1.Body(), []byte(body)) {
		t.Fatalf("Unexpected body: %q. Expected %q", resp1.Body(), body)
	}
}

func TestRequestWriteError(t *testing.T) {
	t.Parallel()

	// no host
	testRequestWriteError(t, "", "/foo/bar", "", "", "")
}

func testRequestWriteError(t *testing.T, method, requestURI, host, userAgent, body string) {
	var req Request

	req.Header.SetMethod(method)
	req.Header.SetRequestURI(requestURI)
	req.Header.Set(HeaderHost, host)
	req.Header.Set(HeaderUserAgent, userAgent)
	req.SetBody([]byte(body))

	w := &bytebufferpool.ByteBuffer{}
	bw := bufio.NewWriter(w)
	err := req.Write(bw)
	if err == nil {
		t.Fatalf("Expecting error when writing request=%#v", &req)
	}
}

func testRequestSuccess(t *testing.T, method, requestURI, host, userAgent, body, expectedMethod string) {
	var req Request

	req.Header.SetMethod(method)
	req.Header.SetRequestURI(requestURI)
	req.Header.Set(HeaderHost, host)
	req.Header.Set(HeaderUserAgent, userAgent)
	req.SetBody([]byte(body))

	contentType := "foobar"
	if method == MethodPost {
		req.Header.Set(HeaderContentType, contentType)
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
	if err = req1.Read(br); err != nil {
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
	if string(req1.Header.Peek(HeaderHost)) != host {
		t.Fatalf("Unexpected host: %q. Expected %q", req1.Header.Peek(HeaderHost), host)
	}
	if string(req1.Header.Peek(HeaderUserAgent)) != userAgent {
		t.Fatalf("Unexpected user-agent: %q. Expected %q", req1.Header.Peek(HeaderUserAgent), userAgent)
	}
	if !bytes.Equal(req1.Body(), []byte(body)) {
		t.Fatalf("Unexpected body: %q. Expected %q", req1.Body(), body)
	}

	if method == MethodPost && string(req1.Header.Peek(HeaderContentType)) != contentType {
		t.Fatalf("Unexpected content-type: %q. Expected %q", req1.Header.Peek(HeaderContentType), contentType)
	}
}

func TestResponseReadSuccess(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	// zero-size body
	testReadBodyFixedSize(t, 0)

	// small-size body
	testReadBodyFixedSize(t, 3)

	// medium-size body
	testReadBodyFixedSize(t, 1024)

	// large-size body
	testReadBodyFixedSize(t, 1024*1024)

	// smaller body after big one
	testReadBodyFixedSize(t, 34345)
}

func TestReadBodyChunked(t *testing.T) {
	t.Parallel()

	// zero-size body
	testReadBodyChunked(t, 0)

	// small-size body
	testReadBodyChunked(t, 5)

	// medium-size body
	testReadBodyChunked(t, 43488)

	// big body
	testReadBodyChunked(t, 3*1024*1024)

	// smaler body after big one
	testReadBodyChunked(t, 12343)
}

func TestRequestURITLS(t *testing.T) {
	t.Parallel()

	uriNoScheme := "//foobar.com/baz/aa?bb=dd&dd#sdf"
	requestURI := "http:" + uriNoScheme
	requestURITLS := "https:" + uriNoScheme

	var req Request

	req.isTLS = true
	req.SetRequestURI(requestURI)
	uri := req.URI().String()
	if uri != requestURITLS {
		t.Fatalf("unexpected request uri: %q. Expecting %q", uri, requestURITLS)
	}

	req.Reset()
	req.SetRequestURI(requestURI)
	uri = req.URI().String()
	if uri != requestURI {
		t.Fatalf("unexpected request uri: %q. Expecting %q", uri, requestURI)
	}
}

func TestRequestURI(t *testing.T) {
	t.Parallel()

	host := "foobar.com"
	requestURI := "/aaa/bb+b%20d?ccc=ddd&qqq#1334dfds&=d"
	expectedPathOriginal := "/aaa/bb+b%20d"
	expectedPath := "/aaa/bb+b d"
	expectedQueryString := "ccc=ddd&qqq"
	expectedHash := "1334dfds&=d"

	var req Request
	req.Header.Set(HeaderHost, host)
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
	t.Parallel()

	var req Request

	testRequestPostArgsSuccess(t, &req, "POST / HTTP/1.1\r\nHost: aaa.com\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 0\r\n\r\n", 0, "foo=", "=")

	testRequestPostArgsSuccess(t, &req, "POST / HTTP/1.1\r\nHost: aaa.com\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 18\r\n\r\nfoo&b%20r=b+z=&qwe", 3, "foo=", "b r=b z=", "qwe=")
}

func TestRequestPostArgsError(t *testing.T) {
	t.Parallel()

	var req Request

	// non-post
	testRequestPostArgsError(t, &req, "GET /aa HTTP/1.1\r\nHost: aaa\r\n\r\n")

	// invalid content-type
	testRequestPostArgsError(t, &req, "POST /aa HTTP/1.1\r\nHost: aaa\r\nContent-Type: text/html\r\nContent-Length: 5\r\n\r\nabcde")
}

func testRequestPostArgsError(t *testing.T, req *Request, s string) {
	r := bytes.NewBufferString(s)
	br := bufio.NewReader(r)
	err := req.Read(br)
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
	err := req.Read(br)
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

func testReadBodyChunked(t *testing.T, bodySize int) {
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

func testReadBodyFixedSize(t *testing.T, bodySize int) {
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

func TestWriteMultipartForm(t *testing.T) {
	t.Parallel()

	var w bytes.Buffer
	s := strings.Replace(`--foo
Content-Disposition: form-data; name="key"

value
--foo
Content-Disposition: form-data; name="file"; filename="test.json"
Content-Type: application/json

{"foo": "bar"}
--foo--
`, "\n", "\r\n", -1)
	mr := multipart.NewReader(strings.NewReader(s), "foo")
	form, err := mr.ReadForm(1024)

	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if err := WriteMultipartForm(&w, form, "foo"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if w.String() != s {
		t.Fatalf("unexpected output %q", w.Bytes())
	}
}

func TestResponseRawBodySet(t *testing.T) {
	t.Parallel()

	var resp Response

	expectedS := "test"
	body := []byte(expectedS)
	resp.SetBodyRaw(body)

	testBodyWriteTo(t, &resp, expectedS, true)
}

func TestRequestRawBodySet(t *testing.T) {
	t.Parallel()

	var r Request

	expectedS := "test"
	body := []byte(expectedS)
	r.SetBodyRaw(body)

	testBodyWriteTo(t, &r, expectedS, true)
}

func TestResponseRawBodyReset(t *testing.T) {
	t.Parallel()

	var resp Response

	body := []byte("test")
	resp.SetBodyRaw(body)
	resp.ResetBody()

	testBodyWriteTo(t, &resp, "", true)
}

func TestRequestRawBodyReset(t *testing.T) {
	t.Parallel()

	var r Request

	body := []byte("test")
	r.SetBodyRaw(body)
	r.ResetBody()

	testBodyWriteTo(t, &r, "", true)
}

func TestResponseRawBodyCopyTo(t *testing.T) {
	t.Parallel()

	var resp Response

	expectedS := "test"
	body := []byte(expectedS)
	resp.SetBodyRaw(body)

	testResponseCopyTo(t, &resp)
}

func TestRequestRawBodyCopyTo(t *testing.T) {
	t.Parallel()

	var a Request

	body := []byte("test")
	a.SetBodyRaw(body)

	var b Request

	a.CopyTo(&b)

	testBodyWriteTo(t, &a, "test", true)
	testBodyWriteTo(t, &b, "test", true)
}

type testReader struct {
	read    chan (int)
	cb      chan (struct{})
	onClose func() error
}

func (r *testReader) Read(b []byte) (int, error) {
	read := <-r.read

	if read == -1 {
		return 0, io.EOF
	}

	r.cb <- struct{}{}

	for i := 0; i < read; i++ {
		b[i] = 'x'
	}

	return read, nil
}

func (r *testReader) Close() error {
	if r.onClose != nil {
		return r.onClose()
	}
	return nil
}

func TestResponseImmediateHeaderFlushRegressionFixedLength(t *testing.T) {
	t.Parallel()

	var r Response

	expectedS := "aaabbbccc"
	buf := bytes.NewBufferString(expectedS)
	r.SetBodyStream(buf, len(expectedS))
	r.ImmediateHeaderFlush = true

	testBodyWriteTo(t, &r, expectedS, false)
}

func TestResponseImmediateHeaderFlushRegressionChunked(t *testing.T) {
	t.Parallel()

	var r Response

	expectedS := "aaabbbccc"
	buf := bytes.NewBufferString(expectedS)
	r.SetBodyStream(buf, -1)
	r.ImmediateHeaderFlush = true

	testBodyWriteTo(t, &r, expectedS, false)
}

func TestResponseImmediateHeaderFlushFixedLength(t *testing.T) {
	t.Parallel()

	var r Response

	r.ImmediateHeaderFlush = true

	ch := make(chan int)
	cb := make(chan struct{})

	buf := &testReader{read: ch, cb: cb}

	r.SetBodyStream(buf, 3)

	b := []byte{}
	w := bytes.NewBuffer(b)
	bb := bufio.NewWriter(w)

	bw := &r

	waitForIt := make(chan struct{})

	go func() {
		if err := bw.Write(bb); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		waitForIt <- struct{}{}
	}()

	ch <- 3

	if !strings.Contains(w.String(), "Content-Length: 3") {
		t.Fatalf("Expected headers to be flushed")
	}

	if strings.Contains(w.String(), "xxx") {
		t.Fatalf("Did not expext body to be written yet")
	}

	<-cb
	ch <- -1

	<-waitForIt
}

func TestResponseImmediateHeaderFlushFixedLengthSkipBody(t *testing.T) {
	t.Parallel()

	var r Response

	r.ImmediateHeaderFlush = true
	r.SkipBody = true

	ch := make(chan int)
	cb := make(chan struct{})

	buf := &testReader{read: ch, cb: cb}

	r.SetBodyStream(buf, 0)

	b := []byte{}
	w := bytes.NewBuffer(b)
	bb := bufio.NewWriter(w)

	var headersOnClose string
	buf.onClose = func() error {
		headersOnClose = w.String()
		return nil
	}

	bw := &r

	if err := bw.Write(bb); err != nil {
		t.Errorf("unexpected error: %s", err)
	}

	if !strings.Contains(headersOnClose, "Content-Length: 0") {
		t.Fatalf("Expected headers to be eagerly flushed")
	}
}

func TestResponseImmediateHeaderFlushChunked(t *testing.T) {
	t.Parallel()

	var r Response

	r.ImmediateHeaderFlush = true

	ch := make(chan int)
	cb := make(chan struct{})

	buf := &testReader{read: ch, cb: cb}

	r.SetBodyStream(buf, -1)

	b := []byte{}
	w := bytes.NewBuffer(b)
	bb := bufio.NewWriter(w)

	bw := &r

	waitForIt := make(chan struct{})

	go func() {
		if err := bw.Write(bb); err != nil {
			t.Errorf("unexpected error: %s", err)
		}

		waitForIt <- struct{}{}
	}()

	ch <- 3

	if !strings.Contains(w.String(), "Transfer-Encoding: chunked") {
		t.Fatalf("Expected headers to be flushed")
	}

	if strings.Contains(w.String(), "xxx") {
		t.Fatalf("Did not expext body to be written yet")
	}

	<-cb
	ch <- -1

	<-waitForIt
}

func TestResponseImmediateHeaderFlushChunkedNoBody(t *testing.T) {
	t.Parallel()

	var r Response

	r.ImmediateHeaderFlush = true
	r.SkipBody = true

	ch := make(chan int)
	cb := make(chan struct{})

	buf := &testReader{read: ch, cb: cb}

	r.SetBodyStream(buf, -1)

	b := []byte{}
	w := bytes.NewBuffer(b)
	bb := bufio.NewWriter(w)

	var headersOnClose string
	buf.onClose = func() error {
		headersOnClose = w.String()
		return nil
	}

	bw := &r

	if err := bw.Write(bb); err != nil {
		t.Errorf("unexpected error: %s", err)
	}

	if !strings.Contains(headersOnClose, "Transfer-Encoding: chunked") {
		t.Fatalf("Expected headers to be eagerly flushed")
	}
}

type ErroneousBodyStream struct {
	errOnRead  bool
	errOnClose bool
}

func (ebs *ErroneousBodyStream) Read(p []byte) (n int, err error) {
	if ebs.errOnRead {
		panic("reading erroneous body stream")
	}
	return 0, io.EOF
}

func (ebs *ErroneousBodyStream) Close() error {
	if ebs.errOnClose {
		panic("closing erroneous body stream")
	}
	return nil
}

func TestResponseBodyStreamErrorOnPanicDuringRead(t *testing.T) {
	t.Parallel()
	var resp Response
	var w bytes.Buffer
	bw := bufio.NewWriter(&w)

	ebs := &ErroneousBodyStream{errOnRead: true, errOnClose: false}
	resp.SetBodyStream(ebs, 42)
	err := resp.Write(bw)
	if err == nil {
		t.Fatalf("expected error when writing response.")
	}
	e, ok := err.(*ErrBodyStreamWritePanic)
	if !ok {
		t.Fatalf("expected error struct to be *ErrBodyStreamWritePanic, got: %+v.", e)
	}
	if e.Error() != "panic while writing body stream: reading erroneous body stream" {
		t.Fatalf("unexpected error value, got: %+v.", e.Error())
	}
}

func TestResponseBodyStreamErrorOnPanicDuringClose(t *testing.T) {
	t.Parallel()
	var resp Response
	var w bytes.Buffer
	bw := bufio.NewWriter(&w)

	ebs := &ErroneousBodyStream{errOnRead: false, errOnClose: true}
	resp.SetBodyStream(ebs, 42)
	err := resp.Write(bw)
	if err == nil {
		t.Fatalf("expected error when writing response.")
	}
	e, ok := err.(*ErrBodyStreamWritePanic)
	if !ok {
		t.Fatalf("expected error struct to be *ErrBodyStreamWritePanic, got: %+v.", e)
	}
	if e.Error() != "panic while writing body stream: closing erroneous body stream" {
		t.Fatalf("unexpected error value, got: %+v.", e.Error())
	}
}

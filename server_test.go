package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"testing"
	"time"
)

func TestRequestCtxFormValue(t *testing.T) {
	var ctx RequestCtx
	var req Request
	req.SetRequestURI("/foo/bar?baz=123&aaa=bbb")
	req.SetBodyString("qqq=port&mmm=sddd")
	req.Header.SetContentType("application/x-www-form-urlencoded")

	ctx.Init(&req, nil, nil)

	v := ctx.FormValue("baz")
	if string(v) != "123" {
		t.Fatalf("unexpected value %q. Expecting %q", v, "123")
	}
	v = ctx.FormValue("mmm")
	if string(v) != "sddd" {
		t.Fatalf("unexpected value %q. Expecting %q", v, "sddd")
	}
	v = ctx.FormValue("aaaasdfsdf")
	if len(v) > 0 {
		t.Fatalf("unexpected value for unknown key %q", v)
	}
}

func TestRequestCtxUserValue(t *testing.T) {
	var ctx RequestCtx

	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("key-%d", i)
		ctx.SetUserValue(k, i)
	}
	for i := 5; i < 10; i++ {
		k := fmt.Sprintf("key-%d", i)
		ctx.SetUserValueBytes([]byte(k), i)
	}

	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("key-%d", i)
		v := ctx.UserValue(k)
		n, ok := v.(int)
		if !ok || n != i {
			t.Fatalf("unexpected value obtained for key %q: %v. Expecting %d", k, v, i)
		}
	}
}

func TestServerHeadRequest(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			fmt.Fprintf(ctx, "Request method is %q", ctx.Method())
			ctx.SetContentType("aaa/bbb")
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("HEAD /foobar HTTP/1.1\r\nHost: aaa.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	resp.SkipBody = true
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if resp.Header.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.Header.StatusCode(), StatusOK)
	}
	if len(resp.Body()) > 0 {
		t.Fatalf("Unexpected non-zero body %q", resp.Body())
	}
	if resp.Header.ContentLength() != 24 {
		t.Fatalf("unexpected content-length %d. Expecting %d", resp.Header.ContentLength(), 24)
	}
	if string(resp.Header.ContentType()) != "aaa/bbb" {
		t.Fatalf("unexpected content-type %q. Expecting %q", resp.Header.ContentType(), "aaa/bbb")
	}

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) > 0 {
		t.Fatalf("unexpected remaining data %q", data)
	}
}

func TestServerExpect100Continue(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if !ctx.IsPost() {
				t.Fatalf("unexpected method %q. Expecting POST", ctx.Method())
			}
			if string(ctx.Path()) != "/foo" {
				t.Fatalf("unexpected path %q. Expecting %q", ctx.Path(), "/foo")
			}
			ct := ctx.Request.Header.ContentType()
			if string(ct) != "a/b" {
				t.Fatalf("unexpectected content-type: %q. Expecting %q", ct, "a/b")
			}
			if string(ctx.PostBody()) != "12345" {
				t.Fatalf("unexpected body: %q. Expecting %q", ctx.PostBody(), "12345")
			}
			ctx.WriteString("foobar")
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("POST /foo HTTP/1.1\r\nHost: gle.com\r\nExpect: 100-continue\r\nContent-Length: 5\r\nContent-Type: a/b\r\n\r\n12345")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusOK, string(defaultContentType), "foobar")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) > 0 {
		t.Fatalf("unexpected remaining data %q", data)
	}
}

func TestCompressHandler(t *testing.T) {
	expectedBody := "foo/bar/baz"
	h := CompressHandler(func(ctx *RequestCtx) {
		ctx.Write([]byte(expectedBody))
	})

	var ctx RequestCtx
	var resp Response

	// verify uncompressed response
	h(&ctx)
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce := resp.Header.Peek("Content-Encoding")
	if string(ce) != "" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "")
	}
	body := resp.Body()
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}

	// verify gzip-compressed response
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.Request.Header.Set("Accept-Encoding", "gzip, deflate, sdhc")

	h(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce = resp.Header.Peek("Content-Encoding")
	if string(ce) != "gzip" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "gzip")
	}
	body, err := resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}

	// verify deflate-compressed response
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.Request.Header.Set("Accept-Encoding", "foobar, deflate, sdhc")

	h(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce = resp.Header.Peek("Content-Encoding")
	if string(ce) != "deflate" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "deflate")
	}
	body, err = resp.BodyInflate()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}
}

func TestRequestCtxWriteString(t *testing.T) {
	var ctx RequestCtx
	n, err := ctx.WriteString("foo")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if n != 3 {
		t.Fatalf("unexpected n %d. Expecting 3", n)
	}
	n, err = ctx.WriteString("привет")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if n != 12 {
		t.Fatalf("unexpected n=%d. Expecting 12", n)
	}

	s := ctx.Response.Body()
	if string(s) != "fooпривет" {
		t.Fatalf("unexpected response body %q. Expecting %q", s, "fooпривет")
	}
}

func TestServeConnNonHTTP11KeepAlive(t *testing.T) {
	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.0\r\nConnection: keep-alive\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /bar HTTP/1.0\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /must/be/ignored HTTP/1.0\r\nHost: google.com\r\n\r\n")

	requestsServed := 0

	ch := make(chan struct{})
	go func() {
		err := ServeConn(rw, func(ctx *RequestCtx) {
			requestsServed++
			ctx.SuccessString("aaa/bbb", "foobar")
		})
		if err != nil {
			t.Fatalf("unexpected error in ServeConn: %s", err)
		}
		close(ch)
	}()

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)

	var resp Response

	// verify the first response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if string(resp.Header.Peek("Connection")) != "keep-alive" {
		t.Fatalf("unexpected Connection header %q. Expecting %q", resp.Header.Peek("Connection"), "keep-alive")
	}
	if resp.Header.ConnectionClose() {
		t.Fatalf("unexpected Connection: close")
	}

	// verify the second response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if string(resp.Header.Peek("Connection")) != "close" {
		t.Fatalf("unexpected Connection header %q. Expecting %q", resp.Header.Peek("Connection"), "close")
	}
	if !resp.Header.ConnectionClose() {
		t.Fatalf("expecting Connection: close")
	}

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after responses %q", data)
	}

	if requestsServed != 2 {
		t.Fatalf("unexpected number of requests served: %d. Expecting 2", requestsServed)
	}
}

func TestRequestCtxSetBodyStreamWriter(t *testing.T) {
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		fmt.Fprintf(w, "body writer line 1\n")
		if err := w.Flush(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		fmt.Fprintf(w, "body writer line 2\n")
	})

	s := ctx.Response.String()

	br := bufio.NewReader(bytes.NewBufferString(s))
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Error when reading response: %s", err)
	}

	body := string(resp.Body())
	expectedBody := "body writer line 1\nbody writer line 2\n"
	if body != expectedBody {
		t.Fatalf("unexpected body: %q. Expecting %q", body, expectedBody)
	}
}

func TestRequestCtxIfModifiedSince(t *testing.T) {
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	lastModified := time.Now().Add(-time.Hour)

	if !ctx.IfModifiedSince(lastModified) {
		t.Fatalf("IfModifiedSince must return true for non-existing If-Modified-Since header")
	}

	ctx.Request.Header.Set("If-Modified-Since", string(AppendHTTPDate(nil, lastModified)))

	if ctx.IfModifiedSince(lastModified) {
		t.Fatalf("If-Modified-Since current time must return false")
	}

	past := lastModified.Add(-time.Hour)
	if ctx.IfModifiedSince(past) {
		t.Fatalf("If-Modified-Since past time must return false")
	}

	future := lastModified.Add(time.Hour)
	if !ctx.IfModifiedSince(future) {
		t.Fatalf("If-Modified-Since future time must return true")
	}
}

func TestRequestCtxSendFileNotModified(t *testing.T) {
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	filePath := "./server_test.go"
	lastModified, err := fsLastModified(filePath)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ctx.Request.Header.Set("If-Modified-Since", string(AppendHTTPDate(nil, lastModified)))

	if err := ctx.SendFile(filePath); err != nil {
		t.Fatalf("error in SendFile: %s", err)
	}

	s := ctx.Response.String()

	var resp Response
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}
	if resp.StatusCode() != StatusNotModified {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusNotModified)
	}
	if len(resp.Body()) > 0 {
		t.Fatalf("unexpected non-zero response body: %q", resp.Body())
	}
}

func TestRequestCtxSendFileModified(t *testing.T) {
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	filePath := "./server_test.go"
	lastModified, err := fsLastModified(filePath)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	lastModified = lastModified.Add(-time.Hour)
	ctx.Request.Header.Set("If-Modified-Since", string(AppendHTTPDate(nil, lastModified)))

	if err := ctx.SendFile(filePath); err != nil {
		t.Fatalf("error in SendFile: %s", err)
	}

	s := ctx.Response.String()

	var resp Response
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	f, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("cannot open file: %s", err)
	}
	body, err := ioutil.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("error when reading file: %s", err)
	}

	if !bytes.Equal(resp.Body(), body) {
		t.Fatalf("unexpected response body: %q. Expecting %q", resp.Body(), body)
	}
}

func TestRequestCtxSendFile(t *testing.T) {
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	filePath := "./server_test.go"
	if err := ctx.SendFile(filePath); err != nil {
		t.Fatalf("error in SendFile: %s", err)
	}

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	if err := ctx.Response.Write(bw); err != nil {
		t.Fatalf("error when writing response: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("error when flushing response: %s", err)
	}

	var resp Response
	br := bufio.NewReader(w)
	if err := resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	f, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("cannot open file: %s", err)
	}
	body, err := ioutil.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("error when reading file: %s", err)
	}

	if !bytes.Equal(resp.Body(), body) {
		t.Fatalf("unexpected response body: %q. Expecting %q", resp.Body(), body)
	}
}

func TestRequestCtxHijack(t *testing.T) {
	hijackStartCh := make(chan struct{})
	hijackStopCh := make(chan struct{})
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Hijack(func(c net.Conn) {
				<-hijackStartCh

				b := make([]byte, 1)
				// ping-pong echo via hijacked conn
				for {
					n, err := c.Read(b)
					if n != 1 {
						if err == io.EOF {
							close(hijackStopCh)
							return
						}
						if err != nil {
							t.Fatalf("unexpected error: %s", err)
						}
						t.Fatalf("unexpected number of bytes read: %d. Expecting 1", n)
					}
					if _, err = c.Write(b); err != nil {
						t.Fatalf("unexpected error when writing data: %s", err)
					}
				}
			})
			ctx.Success("foo/bar", []byte("hijack it!"))
		},
	}

	hijackedString := "foobar baz hijacked!!!"
	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString(hijackedString)

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusOK, "foo/bar", "hijack it!")

	close(hijackStartCh)
	select {
	case <-hijackStopCh:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if string(data) != hijackedString {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, hijackedString)
	}
}

func TestRequestCtxInit(t *testing.T) {
	var ctx RequestCtx
	var logger customLogger
	globalCtxID = 0x123456
	ctx.Init(&ctx.Request, zeroTCPAddr, &logger)
	ip := ctx.RemoteIP()
	if !ip.IsUnspecified() {
		t.Fatalf("unexpected ip for bare RequestCtx: %q. Expected 0.0.0.0", ip)
	}
	ctx.Logger().Printf("foo bar %d", 10)

	expectedLog := "#0012345700000000 - 0.0.0.0:0<->0.0.0.0:0 - GET http:/// - foo bar 10\n"
	if logger.out != expectedLog {
		t.Fatalf("Unexpected log output: %q. Expected %q", logger.out, expectedLog)
	}
}

func TestTimeoutHandlerSuccess(t *testing.T) {
	h := func(ctx *RequestCtx) {
		ctx.Success("aaa/bbb", []byte("real response"))
	}
	s := &Server{
		Handler: TimeoutHandler(h, 100*time.Millisecond, "timeout!!!"),
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
}

func TestServerGetOnly(t *testing.T) {
	h := func(ctx *RequestCtx) {
		if !ctx.IsGet() {
			t.Fatalf("non-get request: %q", ctx.Method())
		}
		ctx.Success("foo/bar", []byte("success"))
	}
	s := &Server{
		Handler: h,
		GetOnly: true,
	}

	rw := &readWriter{}
	rw.r.WriteString("POST /foo HTTP/1.1\r\nHost: google.com\r\nContent-Length: 5\r\nContent-Type: aaa\r\n\r\n12345")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != errGetOnly {
			t.Fatalf("Unexpected error from serveConn: %s. Expecting %s", err, errGetOnly)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	resp := rw.w.Bytes()
	if len(resp) > 0 {
		t.Fatalf("unexpected response %q. Expecting zero", resp)
	}
}

func TestTimeoutHandlerTimeout(t *testing.T) {
	h := func(ctx *RequestCtx) {
		time.Sleep(time.Second)
		ctx.Success("aaa/bbb", []byte("this shouldn't pass to client because of timeout"))
	}
	s := &Server{
		Handler: TimeoutHandler(h, 10*time.Millisecond, "timeout!!!"),
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusRequestTimeout, string(defaultContentType), "timeout!!!")
}

func TestServerTimeoutErrorWithResponse(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			go func() {
				ctx.Success("aaa/bbb", []byte("xxxyyy"))
			}()

			var resp Response

			resp.SetStatusCode(123)
			resp.SetBodyString("foobar. Should be ignored")
			ctx.TimeoutErrorWithResponse(&resp)

			resp.SetStatusCode(456)
			resp.ResetBody()
			fmt.Fprintf(resp.BodyWriter(), "path=%s", ctx.Path())
			resp.Header.SetContentType("foo/bar")
			ctx.TimeoutErrorWithResponse(&resp)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /bar HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 456, "foo/bar", "path=/foo")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerTimeoutErrorWithCode(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			go func() {
				ctx.Success("aaa/bbb", []byte("xxxyyy"))
			}()
			ctx.TimeoutErrorWithCode("should be ignored", 234)
			ctx.TimeoutErrorWithCode("stolen ctx", StatusBadRequest)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusBadRequest, string(defaultContentType), "stolen ctx")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerTimeoutError(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			go func() {
				ctx.Success("aaa/bbb", []byte("xxxyyy"))
			}()
			ctx.TimeoutError("should be ignored")
			ctx.TimeoutError("stolen ctx")
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusRequestTimeout, string(defaultContentType), "stolen ctx")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerMaxKeepaliveDuration(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			time.Sleep(20 * time.Millisecond)
		},
		MaxKeepaliveDuration: 10 * time.Millisecond,
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /aaa HTTP/1.1\r\nHost: aa.com\r\n\r\n")
	rw.r.WriteString("GET /bbbb HTTP/1.1\r\nHost: bbb.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if !resp.ConnectionClose() {
		t.Fatalf("Response must have 'connection: close' header")
	}
	verifyResponseHeader(t, &resp.Header, 200, 0, string(defaultContentType))

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerMaxRequestsPerConn(t *testing.T) {
	s := &Server{
		Handler:            func(ctx *RequestCtx) {},
		MaxRequestsPerConn: 1,
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /bar HTTP/1.1\r\nHost: aaa.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if !resp.ConnectionClose() {
		t.Fatalf("Response must have 'connection: close' header")
	}
	verifyResponseHeader(t, &resp.Header, 200, 0, string(defaultContentType))

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerConnectionClose(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetConnectionClose()
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /must/be/ignored HTTP/1.1\r\nHost: aaa.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	var resp Response

	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if !resp.ConnectionClose() {
		t.Fatalf("expecting Connection: close header")
	}

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerRequestNumAndTime(t *testing.T) {
	n := uint64(0)
	var connT time.Time
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			n++
			if ctx.ConnRequestNum() != n {
				t.Fatalf("unexpected request number: %d. Expecting %d", ctx.ConnRequestNum(), n)
			}
			if connT.IsZero() {
				connT = ctx.ConnTime()
			}
			if ctx.ConnTime() != connT {
				t.Fatalf("unexpected serve conn time: %s. Expecting %s", ctx.ConnTime(), connT)
			}
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /bar HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /baz HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	if n != 3 {
		t.Fatalf("unexpected number of requests served: %d. Expecting %d", n, 3)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, string(defaultContentType), "")
}

func TestServerEmptyResponse(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			// do nothing :)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, string(defaultContentType), "")
}

type customLogger struct {
	out string
}

func (cl *customLogger) Printf(format string, args ...interface{}) {
	cl.out += fmt.Sprintf(format, args...)[6:] + "\n"
}

func TestServerLogger(t *testing.T) {
	cl := &customLogger{}
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			logger := ctx.Logger()
			h := &ctx.Request.Header
			logger.Printf("begin")
			ctx.Success("text/html", []byte(fmt.Sprintf("requestURI=%s, body=%q, remoteAddr=%s",
				h.RequestURI(), ctx.Request.Body(), ctx.RemoteAddr())))
			logger.Printf("end")
		},
		Logger: cl,
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("POST /foo2 HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: 5\r\nContent-Type: aa\r\n\r\nabcde")

	rwx := &readWriterRemoteAddr{
		rw: rw,
		addr: &net.TCPAddr{
			IP:   []byte{1, 2, 3, 4},
			Port: 8765,
		},
	}

	globalCtxID = 0
	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rwx)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "text/html", "requestURI=/foo1, body=\"\", remoteAddr=1.2.3.4:8765")
	verifyResponse(t, br, 200, "text/html", "requestURI=/foo2, body=\"abcde\", remoteAddr=1.2.3.4:8765")

	expectedLogOut := `#0000000100000001 - 1.2.3.4:8765<->1.2.3.4:8765 - GET http://google.com/foo1 - begin
#0000000100000001 - 1.2.3.4:8765<->1.2.3.4:8765 - GET http://google.com/foo1 - end
#0000000100000002 - 1.2.3.4:8765<->1.2.3.4:8765 - POST http://aaa.com/foo2 - begin
#0000000100000002 - 1.2.3.4:8765<->1.2.3.4:8765 - POST http://aaa.com/foo2 - end
`
	if cl.out != expectedLogOut {
		t.Fatalf("Unexpected logger output: %q. Expected %q", cl.out, expectedLogOut)
	}
}

func TestServerRemoteAddr(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			h := &ctx.Request.Header
			ctx.Success("text/html", []byte(fmt.Sprintf("requestURI=%s, remoteAddr=%s, remoteIP=%s",
				h.RequestURI(), ctx.RemoteAddr(), ctx.RemoteIP())))
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")

	rwx := &readWriterRemoteAddr{
		rw: rw,
		addr: &net.TCPAddr{
			IP:   []byte{1, 2, 3, 4},
			Port: 8765,
		},
	}

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rwx)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "text/html", "requestURI=/foo1, remoteAddr=1.2.3.4:8765, remoteIP=1.2.3.4")
}

type readWriterRemoteAddr struct {
	net.Conn
	rw   io.ReadWriteCloser
	addr net.Addr
}

func (rw *readWriterRemoteAddr) Close() error {
	return rw.rw.Close()
}

func (rw *readWriterRemoteAddr) Read(b []byte) (int, error) {
	return rw.rw.Read(b)
}

func (rw *readWriterRemoteAddr) Write(b []byte) (int, error) {
	return rw.rw.Write(b)
}

func (rw *readWriterRemoteAddr) RemoteAddr() net.Addr {
	return rw.addr
}

func (rw *readWriterRemoteAddr) LocalAddr() net.Addr {
	return rw.addr
}

func TestServerConnError(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Error("foobar", 423)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo/bar?baz HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when reading response: %s", err)
	}
	if resp.Header.StatusCode() != 423 {
		t.Fatalf("Unexpected status code %d. Expected %d", resp.Header.StatusCode(), 423)
	}
	if resp.Header.ContentLength() != 6 {
		t.Fatalf("Unexpected Content-Length %d. Expected %d", resp.Header.ContentLength(), 6)
	}
	if !bytes.Equal(resp.Header.Peek("Content-Type"), defaultContentType) {
		t.Fatalf("Unexpected Content-Type %q. Expected %q", resp.Header.Peek("Content-Type"), defaultContentType)
	}
	if !bytes.Equal(resp.Body(), []byte("foobar")) {
		t.Fatalf("Unexpected body %q. Expected %q", resp.Body(), "foobar")
	}
}

func TestServeConnSingleRequest(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			h := &ctx.Request.Header
			ctx.Success("aaa", []byte(fmt.Sprintf("requestURI=%s, host=%s", h.RequestURI(), h.Peek("Host"))))
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo/bar?baz HTTP/1.1\r\nHost: google.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "aaa", "requestURI=/foo/bar?baz, host=google.com")
}

func TestServeConnMultiRequests(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			h := &ctx.Request.Header
			ctx.Success("aaa", []byte(fmt.Sprintf("requestURI=%s, host=%s", h.RequestURI(), h.Peek("Host"))))
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo/bar?baz HTTP/1.1\r\nHost: google.com\r\n\r\nGET /abc HTTP/1.1\r\nHost: foobar.com\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "aaa", "requestURI=/foo/bar?baz, host=google.com")
	verifyResponse(t, br, 200, "aaa", "requestURI=/abc, host=foobar.com")
}

func verifyResponse(t *testing.T, r *bufio.Reader, expectedStatusCode int, expectedContentType, expectedBody string) {
	var resp Response
	if err := resp.Read(r); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}

	if !bytes.Equal(resp.Body(), []byte(expectedBody)) {
		t.Fatalf("Unexpected body %q. Expected %q", resp.Body(), []byte(expectedBody))
	}
	verifyResponseHeader(t, &resp.Header, expectedStatusCode, len(resp.Body()), expectedContentType)
}

type readWriter struct {
	net.Conn
	r bytes.Buffer
	w bytes.Buffer
}

func (rw *readWriter) Close() error {
	return nil
}

func (rw *readWriter) Read(b []byte) (int, error) {
	return rw.r.Read(b)
}

func (rw *readWriter) Write(b []byte) (int, error) {
	return rw.w.Write(b)
}

func (rw *readWriter) RemoteAddr() net.Addr {
	return zeroTCPAddr
}

func (rw *readWriter) LocalAddr() net.Addr {
	return zeroTCPAddr
}

func (rw *readWriter) SetReadDeadline(t time.Time) error {
	return nil
}

func (rw *readWriter) SetWriteDeadline(t time.Time) error {
	return nil
}

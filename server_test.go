package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"testing"
	"time"
)

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

func TestServerTimeoutError(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			go func() {
				ctx.Success("aaa/bbb", []byte("xxxyyy"))
			}()
			ctx.TimeoutError("stolen ctx")
			ctx.TimeoutError("should be ignored")
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
	verifyResponse(t, br, StatusRequestTimeout, string(defaultContentType), "stolen ctx")

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
			ctx.Response.Header.ConnectionClose = true
		},
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
	verifyResponse(t, br, 200, string(defaultContentType), "")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
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
	cl.out += fmt.Sprintf(format, args...) + "\n"
}

func TestServerLogger(t *testing.T) {
	cl := &customLogger{}
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			logger := ctx.Logger()
			h := &ctx.Request.Header
			logger.Printf("begin")
			ctx.Success("text/html", []byte(fmt.Sprintf("requestURI=%s, body=%q, remoteAddr=%s",
				h.RequestURI, ctx.Request.Body, ctx.RemoteAddr())))
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

	expectedLogOut := `0.000 #0000000100000001 - 1.2.3.4:8765 - GET http://google.com/foo1 - begin
0.000 #0000000100000001 - 1.2.3.4:8765 - GET http://google.com/foo1 - end
0.000 #0000000100000002 - 1.2.3.4:8765 - POST http://aaa.com/foo2 - begin
0.000 #0000000100000002 - 1.2.3.4:8765 - POST http://aaa.com/foo2 - end
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
				h.RequestURI, ctx.RemoteAddr(), ctx.RemoteIP())))
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
	if resp.Header.StatusCode != 423 {
		t.Fatalf("Unexpected status code %d. Expected %d", resp.Header.StatusCode, 423)
	}
	if resp.Header.ContentLength != 6 {
		t.Fatalf("Unexpected Content-Length %d. Expected %d", resp.Header.ContentLength, 6)
	}
	if resp.Header.Get("Content-Type") != string(defaultContentType) {
		t.Fatalf("Unexpected Content-Type %q. Expected %q", resp.Header.Get("Content-Type"), defaultContentType)
	}
	if !bytes.Equal(resp.Body, []byte("foobar")) {
		t.Fatalf("Unexpected body %q. Expected %q", resp.Body, "foobar")
	}
}

func TestServeConnSingleRequest(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			h := &ctx.Request.Header
			ctx.Success("aaa", []byte(fmt.Sprintf("requestURI=%s, host=%s", h.RequestURI, h.Get("Host"))))
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
			ctx.Success("aaa", []byte(fmt.Sprintf("requestURI=%s, host=%s", h.RequestURI, h.Get("Host"))))
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

	if !bytes.Equal(resp.Body, []byte(expectedBody)) {
		t.Fatalf("Unexpected body %q. Expected %q", resp.Body, []byte(expectedBody))
	}
	verifyResponseHeader(t, &resp.Header, expectedStatusCode, len(resp.Body), expectedContentType)
}

type readWriter struct {
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

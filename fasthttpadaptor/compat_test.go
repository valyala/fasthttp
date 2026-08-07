package fasthttpadaptor

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// serveOnce runs h against a request built by prepare and returns the context
// it answered into.
func serveOnce(t *testing.T, prepare func(*fasthttp.RequestCtx), h http.HandlerFunc) *fasthttp.RequestCtx {
	t.Helper()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodGet)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("example.com")
	if prepare != nil {
		prepare(ctx)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewFastHTTPHandlerFunc(h)(ctx)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("the handler never returned")
	}
	return ctx
}

const testTimeout = 5 * time.Second

func TestConvertRequestReportsHTTP2Version(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodGet)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("example.com")
	ctx.Request.Header.SetProtocol("HTTP/2")

	var r http.Request
	if err := ConvertRequest(ctx, &r, true); err != nil {
		t.Fatalf("ConvertRequest() error: %v", err)
	}
	if r.Proto != "HTTP/2.0" || r.ProtoMajor != 2 || r.ProtoMinor != 0 {
		t.Errorf("Proto = %q %d.%d, want HTTP/2.0 2.0", r.Proto, r.ProtoMajor, r.ProtoMinor)
	}
}

func TestConvertRequestReportsHTTP10Version(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodGet)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("example.com")
	ctx.Request.Header.SetProtocol("HTTP/1.0")

	var r http.Request
	if err := ConvertRequest(ctx, &r, true); err != nil {
		t.Fatalf("ConvertRequest() error: %v", err)
	}
	if r.Proto != "HTTP/1.0" || r.ProtoMajor != 1 || r.ProtoMinor != 0 {
		t.Errorf("Proto = %q %d.%d, want HTTP/1.0 1.0", r.Proto, r.ProtoMajor, r.ProtoMinor)
	}
}

// net/http keeps the authority in Request.Host and out of the header map.
func TestConvertRequestKeepsHostOutOfHeader(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodGet)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("example.com")

	var r http.Request
	if err := ConvertRequest(ctx, &r, true); err != nil {
		t.Fatalf("ConvertRequest() error: %v", err)
	}
	if r.Host != "example.com" {
		t.Errorf("Host = %q, want example.com", r.Host)
	}
	if got := r.Header["Host"]; got != nil {
		t.Errorf("Header[Host] = %q, want it absent", got)
	}
}

// A body whose length the peer never declared reports -1, as in net/http.
func TestConvertRequestUndeclaredHTTP2BodyLength(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("example.com")
	ctx.Request.Header.SetProtocol("HTTP/2")
	ctx.Request.SetBodyString("body without a declared length")
	ctx.Request.Header.SetContentLength(0)

	var r http.Request
	if err := ConvertRequest(ctx, &r, true); err != nil {
		t.Fatalf("ConvertRequest() error: %v", err)
	}
	if r.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1", r.ContentLength)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != "body without a declared length" {
		t.Errorf("body = %q", body)
	}
}

// A streamed body reaches the handler as a stream, so the handler can start
// before the whole body has arrived.
func TestConvertRequestPassesStreamedBodyThrough(t *testing.T) {
	t.Parallel()

	chunks := make(chan []byte, 2)
	chunks <- []byte("first ")
	chunks <- []byte("second")
	close(chunks)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("example.com")
	ctx.Request.SetBodyStream(&chunkReader{chunks: chunks}, -1)

	var r http.Request
	if err := ConvertRequest(ctx, &r, true); err != nil {
		t.Fatalf("ConvertRequest() error: %v", err)
	}
	if r.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1", r.ContentLength)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != "first second" {
		t.Errorf("body = %q, want %q", body, "first second")
	}
}

type chunkReader struct {
	chunks  chan []byte
	pending []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		chunk, ok := <-r.chunks
		if !ok {
			return 0, io.EOF
		}
		r.pending = chunk
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// The names the peer announced are visible before the body is read, and their
// values once it has been.
func TestConvertRequestCarriesTrailers(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("example.com")
	ctx.Request.SetBodyString("body")
	if err := ctx.Request.Header.SetTrailer("Foo, Bar"); err != nil {
		t.Fatalf("SetTrailer() error: %v", err)
	}
	ctx.Request.Header.Set("Foo", "foov")
	ctx.Request.Header.Set("Undeclared", "nope")

	var r http.Request
	if err := ConvertRequest(ctx, &r, true); err != nil {
		t.Fatalf("ConvertRequest() error: %v", err)
	}
	if _, ok := r.Trailer["Bar"]; !ok {
		t.Errorf("Trailer = %v, want it to name Bar", r.Trailer)
	}
	if got := r.Trailer.Get("Foo"); got != "foov" {
		t.Errorf("Trailer[Foo] = %q, want foov", got)
	}
	if _, ok := r.Trailer["Undeclared"]; ok {
		t.Error("an undeclared trailer must not be carried")
	}
}

// Adding to Trailer extends the announced set; only Set replaces it.
func TestRequestHeaderAddTrailerAccumulates(t *testing.T) {
	t.Parallel()

	var h fasthttp.RequestHeader
	h.Add(fasthttp.HeaderTrailer, "Foo, Bar")
	h.Add(fasthttp.HeaderTrailer, "Baz")

	var got []string
	for _, key := range h.PeekTrailerKeys() {
		got = append(got, string(key))
	}
	if want := []string{"Foo", "Bar", "Baz"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("trailer keys = %v, want %v", got, want)
	}

	h.Set(fasthttp.HeaderTrailer, "Only")
	got = got[:0]
	for _, key := range h.PeekTrailerKeys() {
		got = append(got, string(key))
	}
	if len(got) != 1 || got[0] != "Only" {
		t.Errorf("after Set, trailer keys = %v, want [Only]", got)
	}
}

// A handler announces trailers up front and fills them in after the body.
func TestHandlerWritesAnnouncedTrailers(t *testing.T) {
	t.Parallel()

	ctx := serveOnce(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(fasthttp.HeaderTrailer, "X-Result")
		_, _ = w.Write([]byte("body"))
		w.Header().Set("X-Result", "done")
	})

	if got := string(ctx.Response.Header.Peek("X-Result")); got != "done" {
		t.Errorf("trailer X-Result = %q, want done", got)
	}
	if got := string(ctx.Response.Header.Peek(fasthttp.HeaderTrailer)); !strings.Contains(got, "X-Result") {
		t.Errorf("Trailer header = %q, want it to announce X-Result", got)
	}
	if got := string(ctx.Response.Body()); got != "body" {
		t.Errorf("body = %q, want body", got)
	}
}

// http.TrailerPrefix carries a trailer the handler never announced.
func TestHandlerWritesPrefixedTrailer(t *testing.T) {
	t.Parallel()

	ctx := serveOnce(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))
		w.Header().Set(http.TrailerPrefix+"X-Late", "late")
	})

	if got := string(ctx.Response.Header.Peek("X-Late")); got != "late" {
		t.Errorf("trailer X-Late = %q, want late", got)
	}
	if got := ctx.Response.Header.Peek(http.TrailerPrefix + "X-Late"); len(got) != 0 {
		t.Errorf("the prefixed name leaked into the headers as %q", got)
	}
}

// Every value of a repeated trailer is sent.
func TestHandlerWritesRepeatedTrailerValues(t *testing.T) {
	t.Parallel()

	ctx := serveOnce(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(fasthttp.HeaderTrailer, "X-Multi")
		_, _ = w.Write([]byte("body"))
		w.Header()["X-Multi"] = []string{"one", "two"}
	})

	values := ctx.Response.Header.PeekAll("X-Multi")
	got := make([]string, 0, len(values))
	for _, v := range values {
		got = append(got, string(v))
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("trailer values = %q, want [one two]", got)
	}
}

// The status is fixed once the body starts, as it is in net/http.
func TestHandlerIgnoresWriteHeaderAfterWrite(t *testing.T) {
	t.Parallel()

	ctx := serveOnce(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))
		w.WriteHeader(http.StatusInternalServerError)
	})

	if got := ctx.Response.StatusCode(); got != http.StatusOK {
		t.Errorf("status = %d, want 200", got)
	}
}

// Statuses that carry no body do not get one.
func TestHandlerDropsBodyForBodilessStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		ctx := serveOnce(t, nil, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("should not be sent"))
		})
		if got := ctx.Response.Body(); len(got) != 0 {
			t.Errorf("status %d carried body %q", status, got)
		}
	}
}

// A hijacked response takes no more writes, and flushing it does nothing.
func TestHandlerRejectsWriteAndFlushAfterHijack(t *testing.T) {
	t.Parallel()

	server := &fasthttp.Server{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer listener.Close()

	writeErr := make(chan error, 1)
	flushed := make(chan struct{})
	server.Handler = NewFastHTTPHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			writeErr <- io.ErrUnexpectedEOF
			close(flushed)
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			writeErr <- err
			close(flushed)
			return
		}
		defer conn.Close()

		_, err = w.Write([]byte("after hijack"))
		writeErr <- err
		// Flushing a hijacked response must not block.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(flushed)
	})
	go server.Serve(listener) //nolint:errcheck // the listener close ends it
	defer server.Shutdown()   //nolint:errcheck // best effort

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")); err != nil {
		t.Fatalf("writing request: %v", err)
	}

	select {
	case err := <-writeErr:
		if err != http.ErrHijacked {
			t.Errorf("Write() error = %v, want %v", err, http.ErrHijacked)
		}
	case <-time.After(testTimeout):
		t.Fatal("Write() after Hijack never returned")
	}
	select {
	case <-flushed:
	case <-time.After(testTimeout):
		t.Fatal("Flush() after Hijack never returned")
	}
}

// net/http treats ErrAbortHandler as a quiet way to give up on a response.
func TestHandlerAbortIsQuiet(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	ctx := serveOnce(t, func(ctx *fasthttp.RequestCtx) {
		ctx.Init(&ctx.Request, nil, &captureLogger{into: &logged})
	}, func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})

	_ = ctx
	if strings.Contains(logged.String(), "panic") {
		t.Errorf("ErrAbortHandler was logged as a panic: %s", logged.String())
	}
}

type captureLogger struct{ into *bytes.Buffer }

func (l *captureLogger) Printf(format string, args ...any) {
	l.into.WriteString(format)
}

// A handler that asks for CloseNotify gets a channel rather than a panic.
func TestHandlerCloseNotify(t *testing.T) {
	t.Parallel()

	notified := make(chan bool, 1)
	serveOnce(t, nil, func(w http.ResponseWriter, r *http.Request) {
		notifier, ok := w.(http.CloseNotifier) //nolint:staticcheck // deprecated, still used in the wild
		if !ok {
			t.Error("the response writer does not implement http.CloseNotifier")
			notified <- false
			return
		}
		ch := notifier.CloseNotify()
		select {
		case v := <-ch:
			notified <- v
		default:
			notified <- false
		}
		_, _ = w.Write([]byte("ok"))
	})

	select {
	case <-notified:
	case <-time.After(testTimeout):
		t.Fatal("CloseNotify() never produced a channel")
	}
}

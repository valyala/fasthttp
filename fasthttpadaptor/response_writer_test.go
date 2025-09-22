package fasthttpadaptor

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func TestResponseWriter_Reset(t *testing.T) {
	t.Parallel()

	var ctx fasthttp.RequestCtx
	w := acquireResponseWriter(&ctx)
	defer releaseResponseWriter(w)

	w.WriteHeader(http.StatusMethodNotAllowed)
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("test"))

	w.reset()

	if w.StatusCode() != http.StatusOK {
		t.Fatalf("expected status code to be reset to %d, got %d", http.StatusOK, w.StatusCode())
	}
	if len(w.Header()) != 0 {
		t.Fatalf("expected headers to be cleared, got %v", w.Header())
	}
	if len(*w.responseBody) != 0 {
		t.Fatalf("expected response body to be cleared, got %q", *w.responseBody)
	}
	if w.isStreaming {
		t.Fatalf("expected isStreaming to be false, got true")
	}
	if w.streamCond == nil {
		t.Fatalf("expected streamCond to be initialized, got nil")
	}
	if w.handlerConn != nil {
		t.Fatalf("expected handlerConn to be nil, got %q", w.handlerConn)
	}
}

func TestResponseWriter_Pool(t *testing.T) {
	t.Parallel()

	ctx := new(fasthttp.RequestCtx)
	w := acquireResponseWriter(ctx)
	defer releaseResponseWriter(w)

	if w.ctx != ctx {
		t.Fatalf("Passed in context did not match the current context.")
	}

	if w.StatusCode() != http.StatusOK {
		t.Fatalf("expected status code to be reset to %d, got %d", http.StatusOK, w.StatusCode())
	}
	if len(w.Header()) != 0 {
		t.Fatalf("expected headers to be cleared, got %v", w.Header())
	}
	if len(*w.responseBody) != 0 {
		t.Fatalf("expected response body to be cleared, got %q", *w.responseBody)
	}
	if w.isStreaming {
		t.Fatalf("expected isStreaming to be false, got true")
	}
	if w.streamCond == nil {
		t.Fatalf("expected streamCond to be initialized, got nil")
	}
	if w.handlerConn != nil {
		t.Fatalf("expected handlerConn to be nil, got %q", w.handlerConn)
	}
}

func TestResponseWriter_Write(t *testing.T) {
	t.Parallel()

	var ctx fasthttp.RequestCtx
	w := acquireResponseWriter(&ctx)
	defer releaseResponseWriter(w)

	data := []byte("Hello, World!")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes written, got %d", len(data), n)
	}
	if !bytes.Equal(*w.responseBody, data) {
		t.Fatalf("expected response body %q, got %q", data, *w.responseBody)
	}
}

func TestResponseWriter_WriteHeader(t *testing.T) {
	t.Parallel()

	var ctx fasthttp.RequestCtx
	w := acquireResponseWriter(&ctx)
	defer releaseResponseWriter(w)

	statusCode := http.StatusNotFound
	w.WriteHeader(statusCode)

	if w.StatusCode() != statusCode {
		t.Fatalf("expected status code %d, got %d", statusCode, w.StatusCode())
	}
}

func TestResponseWriter_Header(t *testing.T) {
	t.Parallel()

	var ctx fasthttp.RequestCtx
	w := acquireResponseWriter(&ctx)
	defer releaseResponseWriter(w)

	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("expected Content-Type header to be 'application/json', got %q", w.Header().Get("Content-Type"))
	}
}

func TestResponseWriter_Flush(t *testing.T) {
	t.Parallel()

	var ctx fasthttp.RequestCtx
	w := acquireResponseWriter(&ctx)
	defer releaseResponseWriter(w)

	done := make(chan struct{})

	// Start waiting for modeCh in the main thread to avoid infinite blocking.
	go func() {
		w.Flush()
		close(done)
	}()

	// Wait for flush to start running.
	select {
	case <-w.modeCh:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for modeCh signal")
	}

	select {
	case <-done:
		t.Fatal("Flush completed too early")
	default:
	}

	// Signal streaming mode.
	w.streamCond.L.Lock()
	w.isStreaming = true
	w.streamCond.Broadcast()
	w.streamCond.L.Unlock()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Flush did not complete in time")
	}
}

func TestResponseWriter_Hijack(t *testing.T) {
	t.Parallel()

	var ctx fasthttp.RequestCtx
	w := acquireResponseWriter(&ctx)
	defer releaseResponseWriter(w)

	data := []byte("Hijacked data")
	go func() {
		conn, bufRW, _ := w.Hijack()
		defer conn.Close()

		_, _ = bufRW.Write(data)
		_ = bufRW.Flush()
	}()

	select {
	case <-w.modeCh:
	case <-time.After(1 * time.Second):
		t.Fatalf("timeout waiting for modeCh signal")
	}

	// Verify that hijack's returned connection can read from the bufRW.
	readBuf := make([]byte, len(data))
	w.handlerConn.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, err := w.handlerConn.Read(readBuf)
	if err != nil {
		t.Fatalf("unexpected error reading from connection: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected to read %d bytes, got %d", len(data), n)
	}
	if !bytes.Equal(readBuf, data) {
		t.Fatalf("expected to read %q, got %q", data, readBuf)
	}
}

package fasthttpadaptor

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/valyala/fasthttp"
)

type ModeType int

const (
	modeUnknown ModeType = iota
	modeBuffered
	modeFlushed
	modeHijacked
)

var writerPool = &sync.Pool{
	New: func() any {
		pr, pw := io.Pipe()
		return &responseWriter{
			h:            make(http.Header),
			r:            pr,
			w:            pw,
			modeCh:       make(chan ModeType),
			responseBody: acquireBuffer(),
			streamCond:   sync.NewCond(&sync.Mutex{}),
		}
	},
}

// responseWriter represents a net/http adaptor that implements
// the http.ResponseWriter, http.Flusher, and http.Hijacker interfaces.
type responseWriter struct {
	handlerConn  net.Conn
	ctx          *fasthttp.RequestCtx
	h            http.Header
	r            *io.PipeReader
	w            *io.PipeWriter
	modeCh       chan ModeType
	responseBody *[]byte
	streamCond   *sync.Cond
	statusCode   int
	chOnce       sync.Once
	closeOnce    sync.Once
	isStreaming  bool
	wg           sync.WaitGroup
	hijackedWg   sync.WaitGroup
}

func acquireResponseWriter(ctx *fasthttp.RequestCtx) *responseWriter {
	w, ok := writerPool.Get().(*responseWriter)
	if !ok {
		panic("fasthttpadaptor: cannot get *responseWriter from writerPool")
	}
	w.reset()

	w.ctx = ctx
	return w
}

func releaseNetHTTPResponseWriter(w *responseWriter) {
	releaseBuffer(w.responseBody)
	writerPool.Put(w)
}

// Header returns the current response header.
func (w *responseWriter) Header() http.Header {
	return w.h
}

// Write writes the data to the connection.
//
// Write supports both buffered mode and streaming mode.
func (w *responseWriter) Write(p []byte) (int, error) {
	w.streamCond.L.Lock()
	defer w.streamCond.L.Unlock()

	if w.isStreaming {
		// Streaming mode is on.
		// Stream directly to the conn writer.
		return w.w.Write(p)
	}

	// Streaming mode is off.
	// Write to the response body buffer for flushing later.
	*w.responseBody = append(*w.responseBody, p...)
	return len(p), nil
}

// WriteHeader sets the response's status code.
func (w *responseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

// StatusCode returns the response's status code.
//
// If no status code is set, it returns http.StatusOK by default.
func (w *responseWriter) StatusCode() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

// Flush signals streaming mode.
//
// To ensure proper setup when using responseWriter.Write,
// Flush blocks until streaming mode is ready.
func (w *responseWriter) Flush() {
	// Trigger streaming mode setup.
	w.chOnce.Do(func() {
		w.modeCh <- modeFlushed
	})

	// Wait for streaming mode.
	w.streamCond.L.Lock()
	defer w.streamCond.L.Unlock()
	for !w.isStreaming {
		w.streamCond.Wait()
	}
}

// Hijack signals hijack mode.
//
// When called, the net/http handler assumes full control over the returned connection.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	// Prevent fasthttp from closing it or
	// doing anything else with it.
	w.ctx.HijackSetNoResponse(true)

	netHTTPConn, fasthttpConn := net.Pipe()
	w.handlerConn = fasthttpConn

	// Trigger hijacked mode.
	w.chOnce.Do(func() {
		w.modeCh <- modeHijacked
	})

	bufRW := bufio.NewReadWriter(bufio.NewReader(netHTTPConn), bufio.NewWriter(netHTTPConn))

	// Write any unflushed body to the hijacked connection buffer.
	if len(*w.responseBody) > 0 {
		_, _ = bufRW.Write(*w.responseBody)
		_ = bufRW.Flush()
	}
	return netHTTPConn, bufRW, nil
}

// close signals that the net/http handler
// finished using the writer.
//
// When called in streaming mode, the responseWriter can flush the remaining unread data
// and close the client connection. Otherwise, this method does nothing.
func (w *responseWriter) close() error {
	w.closeOnce.Do(func() {
		if w.w != nil {
			_ = w.w.Close()
		}
	})
	return nil
}

// reset clears data from a recycled responseWriter.
func (w *responseWriter) reset() {
	// Note: reset() must only run after a fasthttp handler finishes
	// proxying the full net/http handler response to ensure no data races.
	w.ctx = nil
	w.statusCode = 0

	w.w = nil
	w.r = nil
	w.handlerConn = nil

	// Clear the http Header
	for key := range w.h {
		delete(w.h, key)
	}

	// Get a new buffer for the response body
	w.responseBody = acquireBuffer()

	w.chOnce = sync.Once{}
	w.closeOnce = sync.Once{}
	w.isStreaming = false
}

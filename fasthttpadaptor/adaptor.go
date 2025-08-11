// Package fasthttpadaptor provides helper functions for converting net/http
// request handlers to fasthttp request handlers.
package fasthttpadaptor

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/valyala/fasthttp"
)

// NewFastHTTPHandlerFunc wraps net/http handler func to fasthttp
// request handler, so it can be passed to fasthttp server.
//
// While this function may be used for easy switching from net/http to fasthttp,
// it has the following drawbacks comparing to using manually written fasthttp
// request handler:
//
//   - A lot of useful functionality provided by fasthttp is missing
//     from net/http handler.
//   - net/http -> fasthttp handler conversion has some overhead,
//     so the returned handler will be always slower than manually written
//     fasthttp handler.
//
// So it is advisable using this function only for quick net/http -> fasthttp
// switching. Then manually convert net/http handlers to fasthttp handlers
// according to https://github.com/valyala/fasthttp#switching-from-nethttp-to-fasthttp .
func NewFastHTTPHandlerFunc(h http.HandlerFunc) fasthttp.RequestHandler {
	return NewFastHTTPHandler(h)
}

// NewFastHTTPHandler wraps net/http handler to fasthttp request handler,
// so it can be passed to fasthttp server.
//
// While this function may be used for easy switching from net/http to fasthttp,
// it has the following drawbacks comparing to using manually written fasthttp
// request handler:
//
//   - A lot of useful functionality provided by fasthttp is missing
//     from net/http handler.
//   - net/http -> fasthttp handler conversion has some overhead,
//     so the returned handler will be always slower than manually written
//     fasthttp handler.
//
// So it is advisable using this function only for quick net/http -> fasthttp
// switching. Then manually convert net/http handlers to fasthttp handlers
// according to https://github.com/valyala/fasthttp#switching-from-nethttp-to-fasthttp .
func NewFastHTTPHandler(h http.Handler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		var r http.Request
		if err := ConvertRequest(ctx, &r, true); err != nil {
			ctx.Logger().Printf("cannot parse requestURI %q: %v", r.RequestURI, err)
			ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
			return
		}
		w := newNetHTTPResponseWriter(ctx)

		// Concurrently serve the net/http handler.
		doneCh := make(chan struct{})
		go func() {
			defer func() {
				close(doneCh)
				_ = w.Close()
			}()
			h.ServeHTTP(w, r.WithContext(ctx))
		}()

		select {
		case <-doneCh:
			// No flush occured before the handler returned.
			// Send the data as one chunk.
			ctx.SetStatusCode(w.StatusCode())
			haveContentType := false
			for k, vv := range w.Header() {
				if k == fasthttp.HeaderContentType {
					haveContentType = true
				}

				for _, v := range vv {
					ctx.Response.Header.Add(k, v)
				}
			}

			if !haveContentType {
				// From net/http.ResponseWriter.Write:
				// If the Header does not contain a Content-Type line, Write adds a Content-Type set
				// to the result of passing the initial 512 bytes of written data to DetectContentType.
				l := 512
				b := w.firstChunk
				if len(b) < 512 {
					l = len(b)
				}
				ctx.Response.Header.Set(fasthttp.HeaderContentType, http.DetectContentType(b[:l]))
			}

			if len(w.firstChunk) > 0 {
				ctx.Response.SetBodyRaw(append([]byte(nil), w.firstChunk...))
			}

		case <-w.flushedCh:
			// Flush occured before handler returned.
			// Send the first 512 bytes and start streaming
			// the rest of the first chunk and new data as it arrives.
			ctx.SetStatusCode(w.StatusCode())
			haveContentType := false
			for k, vv := range w.Header() {
				// Don't copy Content-Length header when
				// streaming.
				if k == fasthttp.HeaderContentLength {
					continue
				}

				if k == fasthttp.HeaderContentType {
					haveContentType = true
				}

				for _, v := range vv {
					ctx.Response.Header.Add(k, v)
				}
			}

			if !haveContentType {
				// From net/http.ResponseWriter.Write:
				// If the Header does not contain a Content-Type line, Write adds a Content-Type set
				// to the result of passing the initial 512 bytes of written data to DetectContentType.
				l := 512
				b := w.firstChunk
				if len(b) < 512 {
					l = len(b)
				}
				ctx.Response.Header.Set(fasthttp.HeaderContentType, http.DetectContentType(b[:l]))
			}

			// Start streaming mode on return.
			ctx.SetBodyStreamWriter(func(bw *bufio.Writer) {
				// Stream the first chunk.
				if len(w.firstChunk) > 0 {
					_, _ = bw.Write(w.firstChunk)
					_ = bw.Flush()
					w.firstChunk = nil
				}

				// Stream the rest of the data that is read
				// from the net/http handler in 32 KiB chunks.
				//
				// Note: Data must be manually copied in chunks
				// as data comes in.
				chunk := make([]byte, 32*1024)
				for {
					// Read net/http handler chunk.
					n, err := w.r.Read(chunk)
					if err != nil {
						// Handler ended due to an io.EOF
						// or an error occured.
						return
					}

					// Copy chunk to fasthttp response
					if n > 0 {
						_, _ = bw.Write(chunk[:n])
						_ = bw.Flush()
					}
				}
			})
			close(w.streamingCh)

		case <-w.hijackedCh:
			// The net/http handler called w.Hijack().
			// Copy data bidirectionally between the
			// net/http and fasthttp connections.
			var wg sync.WaitGroup
			wg.Add(2)

			// Note: It is safe to assume that net.Conn automatically
			// flushes data while copying.
			go func() {
				defer wg.Done()
				_, _ = io.Copy(ctx.Conn(), w.handlerConn)
				_ = ctx.Conn().Close()
			}()
			go func() {
				defer wg.Done()
				_, _ = io.Copy(w.handlerConn, ctx.Conn())
				_ = w.handlerConn.Close()
			}()

			wg.Wait()
		}
	}
}

type netHTTPResponseWriter struct {
	handlerConn net.Conn
	ctx         *fasthttp.RequestCtx
	h           http.Header
	r           *io.PipeReader
	w           *io.PipeWriter
	flushedCh   chan struct{}
	streamingCh chan struct{}
	hijackedCh  chan struct{}
	firstChunk  []byte
	statusCode  int
}

func newNetHTTPResponseWriter(ctx *fasthttp.RequestCtx) *netHTTPResponseWriter {
	pr, pw := io.Pipe()
	return &netHTTPResponseWriter{
		ctx:         ctx,
		h:           make(http.Header),
		r:           pr,
		w:           pw,
		flushedCh:   make(chan struct{}),
		streamingCh: make(chan struct{}),
		hijackedCh:  make(chan struct{}),
	}
}

func (w *netHTTPResponseWriter) StatusCode() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

func (w *netHTTPResponseWriter) Header() http.Header {
	return w.h
}

func (w *netHTTPResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *netHTTPResponseWriter) Write(p []byte) (int, error) {
	select {
	case <-w.streamingCh:
		// Streaming mode is on.
		// Stream directly to the conn writer.
		return w.w.Write(p)
	default:
		// Streaming mode is off.
		// Write to the first chunk for flushing later.
		w.firstChunk = append(w.firstChunk, p...)
		return len(p), nil
	}
}

func (w *netHTTPResponseWriter) Flush() {
	// Trigger streaming mode setup.
	select {
	case <-w.flushedCh:
	default:
		close(w.flushedCh)
	}

	// Wait for streaming mode.
	<-w.streamingCh
}

func (w *netHTTPResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	// Hijack assumes control of the connection, so we need to prevent fasthttp from closing it or
	// doing anything else with it.
	w.ctx.HijackSetNoResponse(true)

	netHTTPConn, fasthttpConn := net.Pipe()
	w.handlerConn = fasthttpConn

	// Trigger hijacked mode.
	select {
	case <-w.hijackedCh:
	default:
		close(w.hijackedCh)
	}

	bufW := bufio.NewReadWriter(bufio.NewReader(netHTTPConn), bufio.NewWriter(netHTTPConn))

	// Write any unflushed body to the hijacked connection buffer.
	if len(w.firstChunk) > 0 {
		_, _ = bufW.Write(w.firstChunk)
		w.firstChunk = nil
	}
	return netHTTPConn, bufW, nil
}

func (w *netHTTPResponseWriter) Close() error {
	return w.w.Close()
}

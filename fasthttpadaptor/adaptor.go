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

		w := acquireNetHTTPResponseWriter(ctx)

		// Concurrently serve the net/http handler.
		w.wg.Add(1)
		go func() {
			h.ServeHTTP(w, r.WithContext(ctx))
			w.chOnce.Do(func() {
				w.modeCh <- modeDone
			})
			w.Close()

			// Wait for the net/http handler to complete before releasing.
			// (e.g. wait for hijacked connection)
			w.wg.Wait()
			releaseNetHTTPResponseWriter(w)
		}()

		switch <-w.modeCh {
		case modeDone:
			// No flush occurred before the handler returned.
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
				b := *w.responseBody
				if len(b) < 512 {
					l = len(b)
				}
				ctx.Response.Header.Set(fasthttp.HeaderContentType, http.DetectContentType(b[:l]))
			}

			if len(*w.responseBody) > 0 {
				ctx.Response.SetBody(*w.responseBody)
			}

			// Signal that the net/http -> fasthttp copy is complete.
			w.wg.Done()

		case modeFlushed:
			// Flush occurred before handler returned.
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

			// Lock the current response body until
			// it is sent in the StreamWriter function.
			if !haveContentType {
				// From net/http.ResponseWriter.Write:
				// If the Header does not contain a Content-Type line, Write adds a Content-Type set
				// to the result of passing the initial 512 bytes of written data to DetectContentType.
				l := 512
				b := *w.responseBody
				if len(b) < 512 {
					l = len(b)
				}
				ctx.Response.Header.Set(fasthttp.HeaderContentType, http.DetectContentType(b[:l]))
			}

			// Start streaming mode on return.
			w.r, w.w = io.Pipe()
			ctx.SetBodyStreamWriter(func(bw *bufio.Writer) {
				// Signal that streaming completed on return.
				defer func() {
					w.r.Close()
					w.wg.Done()
				}()

				// Stream the first chunk.
				if len(*w.responseBody) > 0 {
					_, _ = bw.Write(*w.responseBody)
					_ = bw.Flush()
				}
				// The current response body is no longer used
				// past this point.

				// Stream the rest of the data that is read
				// from the net/http handler in 32 KiB chunks.
				//
				// Note: Data must be manually copied in chunks
				// as data comes in.
				chunk := acquireBuffer()
				*chunk = (*chunk)[:minBufferSize]
				defer releaseBuffer(chunk)
				for {
					// Read net/http handler chunk.
					n, err := w.r.Read(*chunk)
					if err != nil {
						// Handler ended due to an io.EOF
						// or an error occurred.
						return
					}

					// Copy chunk to fasthttp response
					if n > 0 {
						_, err = bw.Write((*chunk)[:n])
						if err != nil {
							// Handler ended due to an io.ErrPipeClosed
							// or an error occurred.
							return
						}

						err = bw.Flush()
						if err != nil {
							// Handler ended due to an io.ErrPipeClosed
							// or an error occurred.
							return
						}
					}
				}
			})
			// Activate streaming mode for consequent `w.Flush()`
			// by net/http handler.
			w.streamCond.L.Lock()
			w.isStreaming = true
			w.streamCond.Signal()
			w.streamCond.L.Unlock()

		case modeHijacked:
			// The net/http handler called w.Hijack().
			// Copy data bidirectionally between the
			// net/http and fasthttp connections.
			w.hijackedWg.Add(2)

			// Note: It is safe to assume that net.Conn automatically
			// flushes data while copying.
			go func() {
				defer w.hijackedWg.Done()
				_, _ = io.Copy(ctx.Conn(), w.handlerConn)

				// Close the fasthttp connection when
				// the net/http connection closes.
				_ = ctx.Conn().Close()
			}()
			go func() {
				defer w.hijackedWg.Done()
				_, _ = io.Copy(w.handlerConn, ctx.Conn())
				// Note: Only the net/http handler
				// should close the connection.
			}()
			w.hijackedWg.Wait()

			// Signal that the hijacked connection was closed.
			w.wg.Done()
		}
	}
}

// Use a minimum buffer size of 32 KiB.
const minBufferSize = 32 * 1024

var bufferPool = &sync.Pool{
	New: func() any {
		b := make([]byte, minBufferSize)
		return &b
	},
}

var writerPool = &sync.Pool{
	New: func() any {
		pr, pw := io.Pipe()
		return &netHTTPResponseWriter{
			h:            make(http.Header),
			r:            pr,
			w:            pw,
			modeCh:       make(chan ModeType),
			responseBody: acquireBuffer(),
			streamCond:   sync.NewCond(&sync.Mutex{}),
		}
	},
}

type ModeType int

const (
	modeUnknown ModeType = iota
	modeDone
	modeFlushed
	modeHijacked
)

type netHTTPResponseWriter struct {
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
	statusMutex  sync.Mutex
	isStreaming  bool
	wg           sync.WaitGroup
	hijackedWg   sync.WaitGroup
}

func acquireNetHTTPResponseWriter(ctx *fasthttp.RequestCtx) *netHTTPResponseWriter {
	w, ok := writerPool.Get().(*netHTTPResponseWriter)
	if !ok {
		panic("fasthttpadaptor: cannot get *netHTTPResponseWriter from writerPool")
	}
	w.reset()

	w.ctx = ctx
	return w
}

func releaseNetHTTPResponseWriter(w *netHTTPResponseWriter) {
	releaseBuffer(w.responseBody)
	writerPool.Put(w)
}

func acquireBuffer() *[]byte {
	buf, ok := bufferPool.Get().(*[]byte)
	if !ok {
		panic("fasthttpadaptor: cannot get *[]byte from bufferPool")
	}

	*buf = (*buf)[:0]
	return buf
}

func releaseBuffer(buf *[]byte) {
	bufferPool.Put(buf)
}

func (w *netHTTPResponseWriter) StatusCode() int {
	// w.statusMutex.Lock()
	// defer w.statusMutex.Unlock()

	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

func (w *netHTTPResponseWriter) Header() http.Header {
	return w.h
}

func (w *netHTTPResponseWriter) WriteHeader(statusCode int) {
	// w.statusMutex.Lock()
	// defer w.statusMutex.Unlock()

	w.statusCode = statusCode
}

func (w *netHTTPResponseWriter) Write(p []byte) (int, error) {
	w.streamCond.L.Lock()
	defer w.streamCond.L.Unlock()

	if w.isStreaming {
		// Streaming mode is on.
		// Stream directly to the conn writer.
		return w.w.Write(p)
	}

	// Streaming mode is off.
	// Write to the first chunk for flushing later.
	*w.responseBody = append(*w.responseBody, p...)
	return len(p), nil
}

func (w *netHTTPResponseWriter) Flush() {
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

func (w *netHTTPResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	// Hijack assumes control of the connection, so we need to prevent fasthttp from closing it or
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

func (w *netHTTPResponseWriter) Close() error {
	w.closeOnce.Do(func() {
		if w.w != nil {
			_ = w.w.Close()
		}
	})
	return nil
}

func (w *netHTTPResponseWriter) reset() {
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

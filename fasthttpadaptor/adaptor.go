// Package fasthttpadaptor provides helper functions for converting net/http
// request handlers to fasthttp request handlers.
package fasthttpadaptor

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

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

		w := acquireWriter(ctx)
		// Serve the net/http handler concurrently so we can react to Flush/Hijack.
		go func() {
			h.ServeHTTP(w, r.WithContext(ctx))
			// Signal completion if no other mode was selected yet.
			select {
			case w.modeCh <- modeDone:
			default:
			}
			_ = w.Close()
		}()

		// Decide mode by first event.
		switch <-w.modeCh {
		case modeDone:
			// Buffered, no Flush() nor Hijack().
			ctx.SetStatusCode(w.status())
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
				l := min(len(w.responseBody), 512)
				if l > 0 {
					ctx.Response.Header.Set(fasthttp.HeaderContentType, http.DetectContentType(w.responseBody[:l]))
				}
			}
			if len(w.responseBody) > 0 {
				ctx.Response.SetBody(w.responseBody)
			}
			releaseWriter(w)

		case modeFlushed:
			// Streaming: send headers and start SetBodyStreamWriter.
			ctx.SetStatusCode(w.status())

			haveContentType := false
			for k, vv := range w.Header() {
				// No Content-Length when streaming.
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
				w.mu.Lock()
				if len(w.responseBody) > 0 {
					l := min(len(w.responseBody), 512)
					ctx.Response.Header.Set(fasthttp.HeaderContentType, http.DetectContentType(w.responseBody[:l]))
				}
				w.mu.Unlock()
			}

			ctx.SetBodyStreamWriter(func(bw *bufio.Writer) {
				// Ensure cleanup only after the stream completes.
				defer releaseWriter(w)

				// Send pre-flush bytes.
				if b := w.consumePreflush(); len(b) > 0 {
					_, _ = bw.Write(b)
					_ = bw.Flush()
				}

				// Stream subsequent writes from the pipe until EOF.
				buf := bufferPool.Get().(*[]byte)
				defer bufferPool.Put(buf)

				for {
					n, err := w.pr.Read(*buf)
					if n > 0 {
						if _, e := bw.Write((*buf)[:n]); e != nil {
							return
						}
						if e := bw.Flush(); e != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			})

			// Signal the writer that streaming is ready so Flush() can return.
			close(w.streamReady)

		case modeHijacked:
			// Handler took over the connection.
			// Prevent fasthttp from sending any response.
			// We already set HijackSetNoResponse(true) in w.Hijack().
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(ctx.Conn(), w.hijackedConn)
				_ = ctx.Conn().Close()
			}()
			go func() {
				defer wg.Done()
				_, _ = io.Copy(w.hijackedConn, ctx.Conn())
			}()
			wg.Wait()
			releaseWriter(w)
		}
	}
}

var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

const (
	modeDone = iota + 1
	modeFlushed
	modeHijacked
)

// Writer implements http.ResponseWriter + http.Flusher + http.Hijacker for the adaptor.
type writer struct {
	ctx        *fasthttp.RequestCtx
	h          http.Header
	statusCode atomic.Int32

	mu           sync.Mutex
	responseBody []byte
	bufPool      *[]byte

	pr *io.PipeReader
	pw *io.PipeWriter

	hijackedConn net.Conn

	modeCh chan int

	streamReady chan struct{}

	flushOnce  sync.Once
	hijackOnce sync.Once
	closeOnce  sync.Once
}

func acquireWriter(ctx *fasthttp.RequestCtx) *writer {
	pr, pw := io.Pipe()
	return &writer{
		ctx:          ctx,
		h:            make(http.Header),
		responseBody: nil,
		pr:           pr,
		pw:           pw,
		modeCh:       make(chan int, 1),
		streamReady:  make(chan struct{}),
	}
}

func releaseWriter(w *writer) {
	_ = w.Close()
	if w.bufPool != nil {
		bufferPool.Put(w.bufPool)
		w.bufPool = nil
	}
}

func (w *writer) Header() http.Header {
	return w.h
}

func (w *writer) WriteHeader(code int) {
	if code <= 0 {
		code = http.StatusOK
	}
	w.statusCode.CompareAndSwap(0, int32(code))
}

func (w *writer) Write(p []byte) (int, error) {
	select {
	case <-w.streamReady:
		return w.pw.Write(p)
	default:
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.responseBody == nil {
		w.bufPool = bufferPool.Get().(*[]byte)
		w.responseBody = (*w.bufPool)[:0]
	}
	w.responseBody = append(w.responseBody, p...)
	return len(p), nil
}

func (w *writer) Flush() {
	w.flushOnce.Do(func() {
		select {
		case w.modeCh <- modeFlushed:
		default:
		}
	})
	<-w.streamReady
}

func (w *writer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	// Ensure fasthttp doesn't attempt to write any HTTP response.
	w.ctx.HijackSetNoResponse(true)
	netHTTPConn, fasthttpConn := net.Pipe()
	w.hijackOnce.Do(func() {
		w.hijackedConn = fasthttpConn
		select {
		case w.modeCh <- modeHijacked:
		default:
		}
	})
	rw := bufio.NewReadWriter(bufio.NewReader(netHTTPConn), bufio.NewWriter(netHTTPConn))

	// If the handler had written pre-flush bytes, expose them to the hijacked conn.
	// This mirrors the common expectation that any early writes become part of the stream.
	if b := w.consumePreflush(); len(b) > 0 {
		_, _ = rw.Write(b)
		_ = rw.Flush()
	}
	return netHTTPConn, rw, nil
}

func (w *writer) Close() error {
	w.closeOnce.Do(func() {
		_ = w.pw.Close()
		_ = w.pr.Close()
		if w.hijackedConn != nil {
			_ = w.hijackedConn.Close()
		}
	})
	return nil
}

// status returns the effective status code (defaults to 200).
func (w *writer) status() int {
	code := int(w.statusCode.Load())
	if code == 0 {
		return http.StatusOK
	}
	return code
}

// consumePreflush returns pre-flush bytes and clears the buffer.
func (w *writer) consumePreflush() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.responseBody) == 0 {
		return nil
	}
	out := w.responseBody
	w.responseBody = nil
	return out
}

// Package fasthttpadaptor provides helper functions for converting net/httpG
// request handlers to fasthttp request handlers.
package fasthttpadaptor

import (
	"bufio"
	"io"
	"net/http"

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

		w := acquireResponseWriter(ctx)

		// Concurrently serve the net/http handler.
		w.wg.Add(1)
		go func() {
			h.ServeHTTP(w, r.WithContext(ctx))
			w.chOnce.Do(func() {
				w.modeCh <- modeCopy
			})
			w.close()

			// Wait for the net/http handler to complete before releasing.
			// (e.g. wait for hijacked connection)
			w.wg.Wait()
			releaseResponseWriter(w)
		}()

		switch <-w.modeCh {
		case modeCopy:
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

		case modeStream:
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

		case modeHijack:
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

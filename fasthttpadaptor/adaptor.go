// Package fasthttpadaptor provides helper functions for converting net/http
// request handlers to fasthttp request handlers.
package fasthttpadaptor

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/url"
	"sync"

	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// NewFastHTTPHandlerFunc wraps net/http handler func to fasthttp
// request handler, so it can be passed to fasthttp server.
//
// While this function may be used for easy switching from net/http to fasthttp,
// it has the following drawbacks comparing to using manually written fasthttp
// request handler:
//
//     * A lot of useful functionality provided by fasthttp is missing
//       from net/http handler.
//     * net/http -> fasthttp handler conversion has some overhead,
//       so the returned handler will be always slower than manually written
//       fasthttp handler.
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
//     * A lot of useful functionality provided by fasthttp is missing
//       from net/http handler.
//     * net/http -> fasthttp handler conversion has some overhead,
//       so the returned handler will be always slower than manually written
//       fasthttp handler.
//
// So it is advisable using this function only for quick net/http -> fasthttp
// switching. Then manually convert net/http handlers to fasthttp handlers
// according to https://github.com/valyala/fasthttp#switching-from-nethttp-to-fasthttp .
func NewFastHTTPHandler(h http.Handler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		// transform request
		// http.Request cannot be reused because it has unexported fields
		var r http.Request
		err := initHTTPRequest(&r, ctx)
		if err != nil {
			ctx.Logger().Printf("cannot build http Request %s: %s", ctx.RequestURI(), err)
			ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
			return
		}

		// Serve
		w := acquireResponseWriter()
		h.ServeHTTP(w, &r)

		// transform response
		ctx.SetStatusCode(w.StatusCode())
		for k, vv := range w.Header() {
			for _, v := range vv {
				ctx.Response.Header.Set(k, v)
			}
		}
		if w.body != nil {
			ctx.Write(w.body.B)
		}
		releaseResponseWriter(w)
	}
}

// initHTTPRequest initialize a http.Request instance.
func initHTTPRequest(r *http.Request, ctx *fasthttp.RequestCtx) error {
	slices := fasthttputil.AcquireStringSlices()
	slices.WriteBytes(ctx.Method())
	slices.WriteBytes(ctx.RequestURI())
	slices.WriteBytes(ctx.Host())
	ctx.Request.Header.VisitAll(func(k, v []byte) {
		slices.WriteBytes(k)
		slices.WriteBytes(v)
	})
	if err := slices.LastError(); err != nil {
		fasthttputil.ReleaseStringSlices(slices)
		return err
	}

	r.Method, _ = slices.NextStringSlice()
	r.RequestURI, _ = slices.NextStringSlice()
	r.Host, _ = slices.NextStringSlice()
	r.RemoteAddr = ctx.RemoteAddr().String()
	var err error
	r.URL, err = url.ParseRequestURI(r.RequestURI)
	if err != nil {
		fasthttputil.ReleaseStringSlices(slices)
		return err
	}
	r.Header = make(http.Header, slices.Remain()/2)
	for slices.Remain() > 0 {
		k, _ := slices.NextStringSlice()
		v, _ := slices.NextStringSlice()
		switch k {
		case "Transfer-Encoding":
			r.TransferEncoding = append(r.TransferEncoding, v)
		default:
			r.Header.Set(k, v)
		}
	}
	fasthttputil.ReleaseStringSlices(slices)
	if ctx.Request.Header.IsHTTP11() {
		r.Proto = "HTTP/1.1"
		r.ProtoMajor = 1
		r.ProtoMinor = 1
	} else {
		r.Proto = "HTTP/1.0"
		r.ProtoMajor = 1
		r.ProtoMinor = 0
	}
	reqBody := ctx.PostBody()
	if reqBody != nil {
		r.Body = ioutil.NopCloser(bytes.NewReader(reqBody))
		r.ContentLength = int64(len(reqBody))
	}
	return nil
}

// netHTTPResponseWriter implement http.ResponseWriter.
type netHTTPResponseWriter struct {
	statusCode int
	h          http.Header
	body       *bytebufferpool.ByteBuffer
}

// StatusCode return response status code.
func (w *netHTTPResponseWriter) StatusCode() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

// Header return response header.
func (w *netHTTPResponseWriter) Header() http.Header {
	if w.h == nil {
		w.h = make(http.Header)
	}
	return w.h
}

// WriteHeader set response status code.
func (w *netHTTPResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

// Write write response to w.
func (w *netHTTPResponseWriter) Write(p []byte) (int, error) {
	if w.body == nil {
		w.body = bytebufferpool.Get()
	}
	return w.body.Write(p)
}

// Reset clean up w before release w.
func (w *netHTTPResponseWriter) Reset() {
	w.statusCode = 0
	w.h = nil
	if w.body != nil {
		w.body.Reset()
		bytebufferpool.Put(w.body)
		w.body = nil
	}
}

var (
	responseWriterPool sync.Pool
)

// acquireResponseWriter return an empty netHTTPResponseWriter instance
// from responseWriterPool.
func acquireResponseWriter() *netHTTPResponseWriter {
	v := responseWriterPool.Get()
	if v == nil {
		return new(netHTTPResponseWriter)
	}
	return v.(*netHTTPResponseWriter)
}

// releaseResponseWriter reset and release netHTTPResponseWriter instance.
func releaseResponseWriter(w *netHTTPResponseWriter) {
	w.Reset()
	responseWriterPool.Put(w)
}

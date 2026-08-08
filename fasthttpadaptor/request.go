package fasthttpadaptor

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/valyala/fasthttp"
)

// ConvertRequest converts a fasthttp.Request to an http.Request.
// forServer should be set to true when the http.Request is going to be passed to a http.Handler.
//
// The http.Request must not be used after the fasthttp handler has returned!
// Memory in use by the http.Request will be reused after your handler has returned!
func ConvertRequest(ctx *fasthttp.RequestCtx, r *http.Request, forServer bool) error {
	body := ctx.PostBody()
	strRequestURI := b2s(ctx.RequestURI())

	rURL, err := url.ParseRequestURI(strRequestURI)
	if err != nil {
		return err
	}

	r.Method = b2s(ctx.Method())
	// net/http spells the HTTP/2 version "HTTP/2.0", and its minor version is 0.
	switch r.Proto = b2s(ctx.Request.Header.Protocol()); r.Proto {
	case "HTTP/2":
		r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/2.0", 2, 0
	case "HTTP/1.0":
		r.ProtoMajor, r.ProtoMinor = 1, 0
	default:
		r.ProtoMajor, r.ProtoMinor = 1, 1
	}
	r.ContentLength = int64(len(body))
	r.RemoteAddr = ctx.RemoteAddr().String()
	r.Host = b2s(ctx.Host())
	r.TLS = ctx.TLSConnectionState()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.URL = rURL

	if forServer {
		r.RequestURI = strRequestURI
	}

	if r.Header == nil {
		r.Header = make(http.Header)
	} else if len(r.Header) > 0 {
		for k := range r.Header {
			delete(r.Header, k)
		}
	}

	// A trailer nobody announced is dropped, as in net/http.
	if r.Trailer = announcedRequestTrailers(&ctx.Request.Header); r.Trailer != nil {
		fillTrailer(&ctx.Request.Header, r.Trailer)
	}

	for k, v := range ctx.Request.Header.All() {
		sk := b2s(k)
		sv := b2s(v)

		switch sk {
		case "Transfer-Encoding":
			r.TransferEncoding = append(r.TransferEncoding, sv)
		default:
			if sk == fasthttp.HeaderCookie {
				sv = strings.Clone(sv)
			}
			r.Header.Add(sk, sv)
		}
	}

	return nil
}

func announcedRequestTrailers(h *fasthttp.RequestHeader) http.Header {
	var out http.Header
	for _, list := range h.PeekAll(fasthttp.HeaderTrailer) {
		for name := range strings.SplitSeq(b2s(list), ",") {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if out == nil {
				out = make(http.Header)
			}
			out[name] = nil
		}
	}
	return out
}

func fillTrailer(h *fasthttp.RequestHeader, dst http.Header) {
	for name := range dst {
		if v := h.Peek(name); len(v) > 0 {
			dst[name] = []string{string(v)}
		}
	}
}

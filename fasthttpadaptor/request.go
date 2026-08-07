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
	// Reading a streamed body here would defeat Server.StreamRequestBody.
	streaming := ctx.Request.IsBodyStream()
	var body []byte
	if !streaming {
		body = ctx.PostBody()
	}
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
	// net/http reports -1 for a length the peer never declared. A negative
	// value means chunked or identity; HTTP/2 has neither, so an undeclared
	// length shows up there as zero instead.
	switch cl := ctx.Request.Header.ContentLength(); {
	case cl < 0, r.ProtoMajor == 2 && cl == 0 && len(body) > 0, streaming && cl == 0:
		r.ContentLength = -1
	case streaming:
		r.ContentLength = int64(cl)
	default:
		r.ContentLength = int64(len(body))
	}
	r.RemoteAddr = ctx.RemoteAddr().String()
	r.Host = b2s(ctx.Host())
	r.TLS = ctx.TLSConnectionState()
	if streaming {
		r.Body = io.NopCloser(ctx.RequestBodyStream())
	} else {
		br := &bufferedBody{}
		br.Reset(body)
		r.Body = br
	}
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

	// net/http shows the announced names before the body is read and fills in
	// their values at EOF. A trailer nobody announced is dropped.
	r.Trailer = announcedRequestTrailers(&ctx.Request.Header)
	switch {
	case r.Trailer == nil:
	case streaming:
		r.Body = &trailerReader{Reader: r.Body, header: &ctx.Request.Header, into: r.Trailer}
	default:
		fillTrailer(&ctx.Request.Header, r.Trailer)
	}

	for k, v := range ctx.Request.Header.All() {
		sk := b2s(k)
		sv := b2s(v)

		switch sk {
		case "Transfer-Encoding":
			r.TransferEncoding = append(r.TransferEncoding, sv)
		case fasthttp.HeaderHost:
			// net/http carries the authority in Request.Host, not in the map.
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

// trailerReader fills in the request trailers once the body is exhausted.
type trailerReader struct {
	io.Reader

	header *fasthttp.RequestHeader
	into   http.Header
	done   bool
}

func (r *trailerReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && !r.done {
		r.done = true
		fillTrailer(r.header, r.into)
	}
	return n, err
}

func (r *trailerReader) Close() error {
	if c, ok := r.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// bufferedBody spares the wrapper io.NopCloser would add.
type bufferedBody struct {
	bytes.Reader
}

func (*bufferedBody) Close() error { return nil }

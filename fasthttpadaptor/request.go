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
	case "HTTP/1.1":
		r.ProtoMajor, r.ProtoMinor = 1, 1
	case "HTTP/1.0":
		r.ProtoMajor, r.ProtoMinor = 1, 0
	case "HTTP/2", "HTTP/2.0":
		r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/2.0", 2, 0
	default:
		var ok bool
		if r.ProtoMajor, r.ProtoMinor, ok = http.ParseHTTPVersion(r.Proto); !ok {
			r.ProtoMajor, r.ProtoMinor = 1, 1
		}
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

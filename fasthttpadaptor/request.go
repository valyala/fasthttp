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
	if err := parseRequestURI(r, strRequestURI); err != nil {
		return err
	}

	r.Method = b2s(ctx.Method())
	r.Proto = b2s(ctx.Request.Header.Protocol())
	if r.Proto == "HTTP/2" {
		r.ProtoMajor = 2
	} else {
		r.ProtoMajor = 1
	}
	r.ProtoMinor = 1
	r.ContentLength = int64(len(body))
	r.RemoteAddr = ctx.RemoteAddr().String()
	r.Host = b2s(ctx.Host())
	r.TLS = ctx.TLSConnectionState()
	if len(body) == 0 {
		r.Body = http.NoBody
	} else {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	if forServer {
		r.RequestURI = strRequestURI
	} else {
		r.RequestURI = ""
	}

	r.TransferEncoding = r.TransferEncoding[:0]

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
			r.Header.Set(sk, sv)
		}
	}

	return nil
}

func parseRequestURI(r *http.Request, requestURI string) error {
	// Fast path for the common origin-form request URI that doesn't require unescaping.
	if requestURI != "" && requestURI[0] == '/' && !strings.ContainsAny(requestURI, "%#") {
		if r.URL == nil {
			r.URL = &url.URL{}
		} else {
			*r.URL = url.URL{}
		}
		if n := strings.IndexByte(requestURI, '?'); n >= 0 {
			r.URL.Path = requestURI[:n]
			r.URL.RawQuery = requestURI[n+1:]
		} else {
			r.URL.Path = requestURI
		}
		return nil
	}

	rURL, err := url.ParseRequestURI(requestURI)
	if err != nil {
		return err
	}
	r.URL = rURL
	return nil
}

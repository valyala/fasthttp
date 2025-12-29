package fasthttpadaptor

import (
	"bytes"
	"io"
	"math"
	"net"
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
			r.Header.Set(sk, sv)
		}
	}

	return nil
}

// ConvertNetHTTPRequestToFastHTTPRequest converts an http.Request to a fasthttp.RequestCtx.
//
// The caller is responsible for the lifecycle of the fasthttp.RequestCtx and the
// underlying fasthttp.Request. The ctx (and its Request) must only be used for
// the duration that fasthttp considers it valid (typically within a handler),
// and must not be accessed after the handler has returned.
//
// The request body is not copied. If r.Body is non-nil, it is passed directly to
// ctx.Request via SetBodyStream. This means:
//   - r.Body must remain readable for as long as ctx may need to read it.
//   - r.Body should not be read from, written to, or closed by the caller until
//     fasthttp is done with ctx.
//   - The same r.Body must not be reused concurrently in other goroutines while
//     it is attached to ctx.Request.
//
// After calling this function, you should treat r.Body as effectively owned by
// ctx.Request for the lifetime of that context.
func ConvertNetHTTPRequestToFastHTTPRequest(r *http.Request, ctx *fasthttp.RequestCtx) {
	ctx.Request.Header.SetMethod(r.Method)

	if r.RequestURI != "" {
		ctx.Request.SetRequestURI(r.RequestURI)
	} else if r.URL != nil {
		ctx.Request.SetRequestURI(r.URL.RequestURI())
	}

	ctx.Request.Header.SetProtocol(r.Proto)
	ctx.Request.SetHost(r.Host)

	for k, values := range r.Header {
		for i, v := range values {
			if i == 0 {
				ctx.Request.Header.Set(k, v)
			} else {
				ctx.Request.Header.Add(k, v)
			}
		}
	}

	if r.Body != nil {
		contentLength := int(r.ContentLength)
		if r.ContentLength >= int64(math.MaxInt) {
			contentLength = -1
		}

		ctx.Request.SetBodyStream(r.Body, contentLength)
	}

	if r.RemoteAddr != "" {
		addr := parseRemoteAddr(r.RemoteAddr)
		ctx.SetRemoteAddr(addr)
	}
}

func parseRemoteAddr(addr string) net.Addr {
	if tcpAddr, err := net.ResolveTCPAddr("tcp", addr); err == nil {
		return tcpAddr
	}

	if _, _, err := net.SplitHostPort(addr); err != nil {
		if tcpAddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(addr, "0")); err == nil {
			return tcpAddr
		}
	}

	host := strings.Trim(addr, "[]")
	return &net.TCPAddr{IP: net.ParseIP(host)}
}

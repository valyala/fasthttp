package fasthttpadaptor

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

// ConvertNetHttpToFastHttp converts an http.Request to a fasthttp.RequestCtx.
// The caller is responsible for the lifecycle of the fasthttp.RequestCtx.
func ConvertNetHttpRequestToFastHttpRequest(r *http.Request, ctx *fasthttp.RequestCtx) {
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
		ctx.Request.SetBodyStream(r.Body, int(r.ContentLength))
	}

	if r.RemoteAddr != "" {
		addr := parseRemoteAddr(r.RemoteAddr)
		ctx.SetRemoteAddr(addr)
	}

}

func parseRemoteAddr(addr string) net.Addr {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return &net.TCPAddr{IP: net.ParseIP(addr)}
	}
	return &net.TCPAddr{
		IP:   net.ParseIP(host),
		Port: parsePort(port),
	}
}

func parsePort(port string) int {
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return p
}

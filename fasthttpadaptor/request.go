package fasthttpadaptor

import (
	"bytes"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
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
			r.Header.Add(sk, sv)
		}
	}

	return nil
}

// ConvertNetHTTPRequestToFastHTTPRequest converts an http.Request to a
// fasthttp.RequestCtx.
//
// ctx.Request is not reset before the conversion: if it may contain data from
// a previous use, call ctx.Request.Reset() first. A zero fasthttp.RequestCtx
// is not fully initialized either: if the converted ctx is used as a
// context.Context or passed to code using its connection-level methods,
// initialize it via ctx.Init before converting.
//
// The request body is not copied. If r.Body is non-nil and not http.NoBody,
// it is attached to ctx.Request via SetBodyStream. This means:
//   - r.Body must remain readable for as long as ctx may need to read it.
//   - r.Body should not be read from, written to, or closed by the caller
//     until ctx is done with it.
//   - The same r.Body must not be used concurrently from other goroutines
//     while it is attached to ctx.Request.
//
// The body size is taken from r.ContentLength: zero and negative values mean
// the size is unknown, and the body is then streamed with chunked transfer
// encoding until EOF, mirroring net/http.
//
// Derived state such as r.Form is not converted, and a body that was already
// consumed (e.g. by r.ParseForm) is attached as the drained stream it is.
//
// The host is taken from r.Host, or r.URL.Host if r.Host is empty. A Host
// entry in r.Header is ignored, mirroring net/http.
//
// r.RemoteAddr is parsed without any DNS resolution, so the conversion cannot
// block. It is expected to be an "IP:port" pair as set by net/http, or a bare
// IP. Other values are ignored and leave the remote address of ctx unchanged.
//
// r.TLS cannot be attached to ctx, since ctx derives its TLS state from the
// underlying connection. It is only used to derive the URI scheme, which is
// set from r.URL.Scheme if present, or to "https" if r.TLS is non-nil.
//
// Trailer names that are forbidden in trailers (see fasthttp.ErrBadTrailer)
// are skipped. Trailer values present in r.Trailer are copied, but note that
// for server requests net/http populates them only after the body has been
// read to EOF.
//
// HTTP/2 and newer protocols are normalized to HTTP/1.1, since fasthttp only
// models HTTP/1.x messages and the HTTP version is a hop-by-hop property.
func ConvertNetHTTPRequestToFastHTTPRequest(r *http.Request, ctx *fasthttp.RequestCtx) {
	ctx.Request.Header.SetMethod(r.Method)

	if r.Proto != "" {
		proto := r.Proto
		if r.ProtoAtLeast(2, 0) {
			proto = "HTTP/1.1"
		}
		ctx.Request.Header.SetProtocol(proto)
	}

	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	if host != "" {
		ctx.Request.Header.SetHost(host)
	}

	if r.RequestURI != "" {
		ctx.Request.SetRequestURI(r.RequestURI)
	} else if r.URL != nil {
		ctx.Request.SetRequestURI(r.URL.RequestURI())
	}

	for k, values := range r.Header {
		if strings.EqualFold(k, fasthttp.HeaderHost) {
			continue
		}
		for _, v := range values {
			ctx.Request.Header.Add(k, v)
		}
	}

	for k, values := range r.Trailer {
		if ctx.Request.Header.AddTrailer(k) != nil {
			continue
		}
		if len(values) > 0 {
			ctx.Request.Header.Set(k, strings.Join(values, ", "))
		}
	}

	if r.Close {
		ctx.Request.Header.Del(fasthttp.HeaderConnection)
		ctx.Request.SetConnectionClose()
	}

	if r.Body != nil && r.Body != http.NoBody {
		contentLength := int(r.ContentLength)
		if r.ContentLength <= 0 || r.ContentLength >= int64(math.MaxInt) {
			contentLength = -1
		}
		ctx.Request.SetBodyStream(r.Body, contentLength)
	}

	if r.RemoteAddr != "" {
		if remoteAddr := parseRemoteAddr(r.RemoteAddr); remoteAddr != nil {
			ctx.SetRemoteAddr(remoteAddr)
		}
	}

	scheme := ""
	if r.URL != nil {
		scheme = r.URL.Scheme
	}
	if scheme == "" && r.TLS != nil {
		scheme = "https"
	}
	if scheme != "" && scheme != "http" {
		ctx.Request.URI().SetScheme(scheme)
	}

	if r.URL != nil && r.URL.User != nil {
		uri := ctx.Request.URI()
		uri.SetUsername(r.URL.User.Username())
		if password, hasPassword := r.URL.User.Password(); hasPassword {
			uri.SetPassword(password)
		}
	}
}

// parseRemoteAddr parses an http.Request.RemoteAddr into a net.Addr. It only
// parses the string and never resolves host names, so it cannot block.
// net/http sets RemoteAddr to an "IP:port" pair, but a bare IP without a port
// is accepted too. It returns nil for any other value.
func parseRemoteAddr(addr string) net.Addr {
	if addrPort, err := netip.ParseAddrPort(addr); err == nil {
		return net.TCPAddrFromAddrPort(addrPort)
	}
	if ip, err := netip.ParseAddr(addr); err == nil {
		return net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, 0))
	}
	return nil
}

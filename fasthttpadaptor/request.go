package fasthttpadaptor

import (
	"bytes"
	"errors"
	"io"
	"math"
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

// ConvertNetHTTPRequestToFastHTTPRequest converts an http.Request to a
// fasthttp.Request.
//
// req is not reset before the conversion: if it may contain data from a
// previous use, call req.Reset() first.
//
// The request body is not copied. If r.Body is non-nil and not http.NoBody,
// it is attached to req via SetBodyStream. This means:
//   - r.Body must remain readable for as long as req may need to read it.
//   - r.Body should not be read from, written to, or closed by the caller
//     until req is done with it.
//   - The same r.Body must not be used concurrently from other goroutines
//     while it is attached to req.
//
// The body framing is derived only from the dedicated fields, mirroring
// net/http.Request.Write: Content-Length, Transfer-Encoding and Trailer
// entries in r.Header are ignored. The body size is taken from
// r.ContentLength, where zero and negative values mean the size is unknown,
// and a chunked r.TransferEncoding or a non-empty r.Trailer forces the
// unknown size too; a body of unknown size is streamed with chunked transfer
// encoding until EOF. Trailers force the chunked framing, unlike net/http
// which drops them on requests with a known size, because HTTP/2 requests
// carry a known length and trailers at the same time, and HTTP/1.x can
// transport trailers only after a chunked body.
//
// Derived state such as r.Form is not converted, and a body that was already
// consumed (e.g. by r.ParseForm) is attached as the drained stream it is.
//
// The host is taken from r.Host, or r.URL.Host if r.Host is empty. A Host
// entry in r.Header is ignored, mirroring net/http.
//
// Connection-level state is not converted, since a fasthttp.Request models
// only the wire message: r.RemoteAddr is ignored, and r.TLS is used only to
// derive the URI scheme, which is set from r.URL.Scheme if present, or to
// "https" if r.TLS is non-nil.
//
// Trailer names that are forbidden in trailers (see fasthttp.ErrBadTrailer)
// are skipped, and without a body all trailers are dropped, since trailers
// can only travel after a chunked body. Trailer values present in r.Trailer
// are copied at conversion time and synchronized again when the attached
// body reaches EOF, since for server requests net/http populates r.Trailer
// only while the body is being read. fasthttp writes trailers after draining
// the body stream, so the synchronized values are the ones written to the
// wire.
//
// URL credentials in r.URL.User are copied to the URI only when r.Header
// carries no Authorization entry, since fasthttp writes URI userinfo as a
// Basic Authorization header and an explicit header takes precedence over
// URL credentials, mirroring net/http.Client.
//
// HTTP/2 and newer protocols are normalized to HTTP/1.1, since fasthttp only
// models HTTP/1.x messages and the HTTP version is a hop-by-hop property.
func ConvertNetHTTPRequestToFastHTTPRequest(r *http.Request, req *fasthttp.Request) {
	req.Header.SetMethod(r.Method)

	if r.Proto != "" {
		proto := r.Proto
		if r.ProtoAtLeast(2, 0) {
			proto = "HTTP/1.1"
		}
		req.Header.SetProtocol(proto)
	}

	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	if host != "" {
		req.Header.SetHost(host)
	}

	if r.RequestURI != "" {
		req.SetRequestURI(r.RequestURI)
	} else if r.URL != nil {
		req.SetRequestURI(r.URL.RequestURI())
	}

	for k, values := range r.Header {
		if strings.EqualFold(k, fasthttp.HeaderHost) ||
			strings.EqualFold(k, fasthttp.HeaderContentLength) ||
			strings.EqualFold(k, fasthttp.HeaderTransferEncoding) ||
			strings.EqualFold(k, fasthttp.HeaderTrailer) {
			continue
		}
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}

	hasBody := r.Body != nil && r.Body != http.NoBody

	var trailerKeys []string
	if hasBody {
		for k, values := range r.Trailer {
			if req.Header.AddTrailer(k) != nil {
				continue
			}
			trailerKeys = append(trailerKeys, k)
			if len(values) > 0 {
				req.Header.Set(k, strings.Join(values, ", "))
			}
		}
	}

	if r.Close {
		req.Header.Del(fasthttp.HeaderConnection)
		req.SetConnectionClose()
	}

	if hasBody {
		var body io.Reader = r.Body
		if len(trailerKeys) > 0 {
			body = &trailerSyncReader{
				body:    r.Body,
				trailer: r.Trailer,
				keys:    trailerKeys,
				header:  &req.Header,
			}
		}
		contentLength := int(r.ContentLength)
		if r.ContentLength <= 0 || r.ContentLength >= int64(math.MaxInt) ||
			len(trailerKeys) > 0 ||
			(len(r.TransferEncoding) > 0 && r.TransferEncoding[0] == "chunked") {
			contentLength = -1
		}
		req.SetBodyStream(body, contentLength)
	}

	scheme := ""
	if r.URL != nil {
		scheme = r.URL.Scheme
	}
	if scheme == "" && r.TLS != nil {
		scheme = "https"
	}
	if scheme != "" && scheme != "http" {
		req.URI().SetScheme(scheme)
	}

	if r.URL != nil && r.URL.User != nil && r.Header.Get(fasthttp.HeaderAuthorization) == "" {
		uri := req.URI()
		uri.SetUsername(r.URL.User.Username())
		if password, hasPassword := r.URL.User.Password(); hasPassword {
			uri.SetPassword(password)
		}
	}
}

// trailerSyncReader passes reads through to the attached request body and,
// once the body reaches EOF, copies the trailer values from the net/http
// trailer map into the fasthttp request header. net/http completes the map
// right before a body read returns io.EOF, and fasthttp writes trailers only
// after the body stream is drained, so the copied values are the ones
// written to the wire. Only keys accepted by AddTrailer are synchronized: a
// forbidden name must not reach Set, where it would be treated as a special
// header carrying real state.
type trailerSyncReader struct {
	body    io.ReadCloser
	trailer http.Header
	header  *fasthttp.RequestHeader
	keys    []string
	synced  bool
}

func (t *trailerSyncReader) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	if err != nil && errors.Is(err, io.EOF) {
		t.sync()
	}
	return n, err
}

// Close closes the attached body, preserving the closing behavior the body
// would have when attached to the request unwrapped.
func (t *trailerSyncReader) Close() error {
	return t.body.Close()
}

func (t *trailerSyncReader) sync() {
	if t.synced {
		return
	}
	t.synced = true
	for _, k := range t.keys {
		if values := t.trailer[k]; len(values) > 0 {
			t.header.Set(k, strings.Join(values, ", "))
		}
	}
}

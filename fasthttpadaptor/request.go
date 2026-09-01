package fasthttpadaptor

import (
	"bytes"
	"encoding/base64"
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
// The conversion never reads the body: nil and http.NoBody are the explicit
// representations of an empty body, and any other body of unknown size is
// streamed, even if it turns out to be empty. net/http.Client instead probes
// such a body for some request methods to tell an empty body from an unknown
// one; callers that need the bodiless form should pass nil or http.NoBody.
//
// A body of known size is truncated to that size when it is written, like
// net/http does, so that a body longer than r.ContentLength cannot put bytes
// past the declared boundary, where a peer would parse them as the next
// request. Writing such a request body fails once the declared size has been
// written.
//
// Derived state such as r.Form is not converted, and a body that was already
// consumed (e.g. by r.ParseForm) is attached as the drained stream it is.
//
// The host is taken from r.Host, or r.URL.Host if r.Host is empty. A Host
// entry in r.Header is ignored, mirroring net/http.
//
// When r.Header carries no Content-Type entry, the default Content-Type
// fasthttp writes for requests (application/octet-stream) is disabled on req
// via SetNoDefaultContentType, so the conversion does not add a header the
// source request did not carry; net/http never generates a Content-Type.
// Recipients of a message without a Content-Type may assume
// application/octet-stream themselves (RFC 9110, Section 8.3). Note that
// req.Reset() restores the fasthttp default.
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
// Credentials follow the precedence of net/http.Client: an Authorization
// entry in r.Header takes precedence over URL credentials. Without such an
// entry, credentials in r.URL.User are written as a Basic Authorization
// header, including credentials with an empty username, just like
// net/http.Client sends them. With such an entry, userinfo embedded in an
// absolute-form r.RequestURI is dropped so it cannot displace the header.
// Either way a request carrying credentials is written with an origin-form
// request line, since a request target must not contain userinfo (RFC 9112,
// Section 3.2.4), just like net/http.Request.Write.
//
// HTTP/2 and newer protocols are normalized to HTTP/1.1, since fasthttp only
// models HTTP/1.x messages and the HTTP version is a hop-by-hop property. An
// older protocol is also normalized to HTTP/1.1 when the conversion requires
// chunked framing, since chunked transfer encoding does not exist in
// HTTP/1.0; net/http always writes request lines as HTTP/1.1. The implied
// close semantics of an HTTP/1.0 request survive the normalization when
// r.Close is set, which net/http derives from the version and the Connection
// header when it parses a request.
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

	if r.Header.Get(fasthttp.HeaderContentType) == "" {
		req.Header.SetNoDefaultContentType(true)
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
		contentLength := int(r.ContentLength)
		if r.ContentLength <= 0 || r.ContentLength >= int64(math.MaxInt) ||
			len(trailerKeys) > 0 ||
			(len(r.TransferEncoding) > 0 && r.TransferEncoding[0] == "chunked") {
			contentLength = -1
		}

		if contentLength == -1 && string(req.Header.Protocol()) != "HTTP/1.1" {
			req.Header.SetProtocol("HTTP/1.1")
		}

		var body io.Reader
		switch {
		case len(trailerKeys) > 0:
			body = &trailerSyncReader{
				body:    r.Body,
				trailer: r.Trailer,
				keys:    trailerKeys,
				header:  &req.Header,
			}
		case contentLength >= 0:
			body = &limitedBodyReader{body: r.Body, remaining: int64(contentLength)}
		default:
			body = r.Body
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

	if r.Header.Get(fasthttp.HeaderAuthorization) != "" {
		if strings.Contains(r.RequestURI, "://") {
			uri := req.URI()
			uri.SetUsername("")
			uri.SetPassword("")
		}
	} else if r.URL != nil && r.URL.User != nil {
		password, _ := r.URL.User.Password()
		req.Header.Set(fasthttp.HeaderAuthorization,
			"Basic "+base64.StdEncoding.EncodeToString(
				[]byte(r.URL.User.Username()+":"+password)))
		uri := req.URI()
		uri.SetUsername("")
		uri.SetPassword("")
	}
}

// errBodyTooLong is returned from a body read once the attached body turns
// out to hold more data than the declared content length.
var errBodyTooLong = errors.New("body is longer than the declared content length")

// limitedBodyReader caps the data read from the attached body at the declared
// content length. fasthttp copies the whole body stream to the wire before
// comparing the copied size with the content length, so without the cap the
// excess of a too long body is written past the framing boundary, where a
// peer parses it as the start of the next request. net/http instead writes
// the body through an io.LimitReader and reports the mismatch afterwards, so
// the cap keeps the written body within its declared boundary and turns the
// excess into an error.
type limitedBodyReader struct {
	body      io.ReadCloser
	remaining int64
}

func (l *limitedBodyReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		// The declared body was read in full, so this read may only
		// confirm the end of the body. Data beyond it must not be written,
		// since it would land past the framing boundary.
		var b [1]byte
		n, err := l.body.Read(b[:])
		switch {
		case n > 0:
			return 0, errBodyTooLong
		case err != nil:
			return 0, err
		default:
			return 0, nil
		}
	}

	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.body.Read(p)
	l.remaining -= int64(n)
	return n, err
}

// Close closes the attached body, preserving the closing behavior the body
// would have when attached to the request unwrapped.
func (l *limitedBodyReader) Close() error {
	return l.body.Close()
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

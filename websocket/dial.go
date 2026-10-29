package websocket

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

// DialOptions configures Dial. The zero value is valid.
type DialOptions struct {
	// Dial establishes the TCP connection. nil means net.Dialer, which honors
	// ctx; a custom dialer is bounded only by its own timeouts. The dialers of
	// the fasthttpproxy package can be used directly.
	Dial fasthttp.DialFunc

	// TLSConfig is used for wss URLs. nil means the default configuration.
	// ServerName is taken from the URL when empty.
	TLSConfig *tls.Config

	// Header holds extra request headers. A Host header overrides the URL
	// host; the headers of the handshake itself (Connection, Upgrade,
	// Sec-WebSocket-*, Content-Length, Transfer-Encoding, Trailer) are
	// rejected.
	Header *fasthttp.RequestHeader

	// Response, when not nil, receives the status and headers of the
	// handshake response, also when the handshake fails.
	Response *fasthttp.Response

	// Subprotocols lists the subprotocols to offer, in order of preference.
	// Names must be distinct HTTP tokens.
	Subprotocols []string

	// ReadBufferSize is the size of the buffer the handshake response and
	// then the frames are read through; the response headers must fit in it.
	// Zero means 4096.
	ReadBufferSize int

	// MaxMessageSize, ReadTimeout, WriteTimeout, CompressionMode and
	// CompressionThreshold work as in AcceptOptions.
	MaxMessageSize       int
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	CompressionMode      CompressionMode
	CompressionThreshold int
}

var defaultDialOptions DialOptions

// maxInterimResponses bounds the 1xx responses skipped before the final one,
// as net/http does.
const maxInterimResponses = 5

// Dial connects to the WebSocket server at u, a ws or wss URL (http and https
// are accepted as aliases) without a fragment, and completes the handshake.
// ctx bounds the whole of it, apart from a custom DialOptions.Dial. The path
// is sent as given; user information in the URL becomes a Basic Authorization
// header unless DialOptions.Header carries one. Redirects are not followed.
// Interim 1xx responses are skipped; a final response other than 101 is an
// error, and its status and headers are available through
// DialOptions.Response.
func Dial(ctx context.Context, u string, opts *DialOptions) (*Conn, error) {
	if opts == nil {
		opts = &defaultDialOptions
	}
	if opts.Response != nil {
		opts.Response.Reset()
	}
	if !validSubprotocols(opts.Subprotocols) {
		return nil, errors.New("websocket: invalid subprotocol name in DialOptions")
	}
	if strings.IndexByte(u, '#') >= 0 {
		return nil, errors.New("websocket: URL must not have a fragment")
	}
	uri := fasthttp.AcquireURI()
	defer fasthttp.ReleaseURI(uri)
	if err := uri.Parse(nil, []byte(u)); err != nil {
		return nil, err
	}
	uri.DisablePathNormalizing = true // Send the path as given.
	if !validRequestTarget(uri.RequestURI()) {
		return nil, errors.New("websocket: invalid URL path or query")
	}
	auth, err := basicAuth(u, uri)
	if err != nil {
		return nil, err
	}
	if opts.Header != nil {
		for k := range opts.Header.All() {
			if reservedHeader(k) {
				return nil, errors.New("websocket: DialOptions.Header must not set " + string(k))
			}
			if equalFoldASCII(k, fasthttp.HeaderAuthorization) {
				auth = ""
			}
		}
	}
	var isTLS bool
	switch string(uri.Scheme()) {
	case "ws", "http":
	case "wss", "https":
		isTLS = true
	default:
		return nil, errors.New("websocket: unsupported URL scheme")
	}
	if len(uri.Host()) == 0 {
		return nil, errors.New("websocket: URL has no host")
	}
	nc, err := dialConn(ctx, fasthttp.AddMissingPort(string(uri.Host()), isTLS), isTLS, opts)
	if err != nil {
		return nil, err
	}
	c, err := handshake(ctx, nc, uri, auth, opts)
	if err != nil {
		closeTransport(nc)
		if cerr := ctxCause(ctx); cerr != nil {
			err = cerr
		}
		return nil, err
	}
	return c, nil
}

func dialConn(ctx context.Context, addr string, isTLS bool, opts *DialOptions) (net.Conn, error) {
	var nc net.Conn
	var err error
	if opts.Dial != nil {
		nc, err = opts.Dial(addr)
	} else {
		var d net.Dialer
		nc, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil || !isTLS {
		return nc, err
	}
	cfg := opts.TLSConfig.Clone()
	if cfg == nil {
		cfg = &tls.Config{}
	}
	if cfg.ServerName == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			nc.Close()
			return nil, err
		}
		cfg.ServerName = host
	}
	tc := tls.Client(nc, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	return tc, nil
}

// validRequestTarget reports whether b can be sent as the request target: the
// characters RFC 3986 allows in a path and query, with well-formed percent
// escapes. Brackets are tolerated, as browsers send them.
func validRequestTarget(b []byte) bool {
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == '%':
			if i+2 >= len(b) || !isHex(b[i+1]) || !isHex(b[i+2]) {
				return false
			}
			i += 2
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("-._~!$&'()*+,;=:@/?[]", c) < 0:
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// basicAuth returns the Authorization value for the URL's user information,
// or "" when the URL has none. Empty user information still counts.
func basicAuth(rawURL string, uri *fasthttp.URI) (string, error) {
	authority := rawURL
	if _, rest, ok := strings.Cut(rawURL, "://"); ok {
		authority = rest
	}
	if i := strings.IndexAny(authority, "/?"); i >= 0 {
		authority = authority[:i]
	}
	if !strings.Contains(authority, "@") {
		return "", nil
	}
	u, err := url.PathUnescape(string(uri.Username()))
	if err != nil {
		return "", err
	}
	p, err := url.PathUnescape(string(uri.Password()))
	if err != nil {
		return "", err
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p)), nil
}

func handshake(ctx context.Context, nc net.Conn, uri *fasthttp.URI, auth string, opts *DialOptions) (*Conn, error) {
	if d, ok := ctx.Deadline(); ok {
		if err := nc.SetDeadline(d); err != nil {
			return nil, err
		}
	}
	stop := context.AfterFunc(ctx, func() {
		if nc.SetDeadline(aLongTimeAgo) != nil {
			nc.Close()
		}
	})
	defer stop()

	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		return nil, err
	}
	var key [24]byte
	base64.StdEncoding.Encode(key[:], keyBytes[:])

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.DisableNormalizing() // Send the field names as RFC 6455 spells them.
	req.Header.SetRequestURIBytes(uri.RequestURI())
	req.Header.SetHostBytes(uri.Host())
	req.Header.Set(fasthttp.HeaderUpgrade, "websocket")
	req.Header.Set(fasthttp.HeaderConnection, "Upgrade")
	req.Header.SetBytesV(fasthttp.HeaderSecWebSocketKey, key[:])
	req.Header.Set(fasthttp.HeaderSecWebSocketVersion, "13")
	if len(opts.Subprotocols) > 0 {
		req.Header.Set(fasthttp.HeaderSecWebSocketProtocol, strings.Join(opts.Subprotocols, ", "))
	}
	if ext := deflateOffer(opts.CompressionMode); ext != "" {
		req.Header.Set(fasthttp.HeaderSecWebSocketExtensions, ext)
	}
	if opts.Header != nil {
		for k, v := range opts.Header.All() {
			req.Header.AddBytesKV(k, v)
		}
	}
	if auth != "" {
		req.Header.Set(fasthttp.HeaderAuthorization, auth)
	}
	bw := bufio.NewWriter(nc)
	if err := req.Write(bw); err != nil {
		return nil, err
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}

	resp := opts.Response
	if resp == nil {
		resp = fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(resp)
	}
	size := opts.ReadBufferSize
	if size <= 0 {
		size = 4096
	}
	br := bufio.NewReaderSize(nc, size)
	// Interim responses, 1xx other than 101, precede the final one.
	for n := 0; ; n++ {
		if err := resp.Header.Read(br); err != nil {
			return nil, err
		}
		if code := resp.StatusCode(); code < 100 || code >= 200 || code == fasthttp.StatusSwitchingProtocols {
			break
		}
		if n == maxInterimResponses {
			return nil, errors.New("websocket: too many interim responses")
		}
	}
	rh := &resp.Header
	if resp.StatusCode() != fasthttp.StatusSwitchingProtocols {
		return nil, errors.New("websocket: handshake failed with status " + strconv.Itoa(resp.StatusCode()))
	}
	f := gather(rh.All())
	defer f.release()
	if !rh.IsHTTP11() || rh.ConnectionClose() || !hasToken(f.connection, "upgrade") || !onlyToken(f.upgrade, "websocket") {
		return nil, errors.New("websocket: handshake response is not an HTTP/1.1 upgrade")
	}
	var accept [28]byte
	if f.acceptN != 1 || !bytes.Equal(f.accept, appendAcceptKey(accept[:0], key[:])) {
		return nil, errors.New("websocket: bad Sec-WebSocket-Accept")
	}
	// The subprotocol field is absent or names one offered subprotocol.
	var subprotocol []byte
	if len(f.protocol) == 1 {
		subprotocol = trimHTTPSpace(f.protocol[0])
	}
	if len(f.protocol) > 1 || (len(f.protocol) == 1 && !slices.Contains(opts.Subprotocols, string(subprotocol))) {
		return nil, errors.New("websocket: server selected a subprotocol that was not offered")
	}
	if !validExtensions(f.extensions) {
		return nil, errBadExtension
	}
	deflate, err := acceptDeflate(opts.CompressionMode, opts.CompressionThreshold, f.extensions)
	if err != nil {
		return nil, err
	}
	if !stop() {
		return nil, ctx.Err()
	}
	if err := nc.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	cfg := connConfig{
		maxMessageSize: opts.MaxMessageSize,
		readTimeout:    opts.ReadTimeout,
		writeTimeout:   opts.WriteTimeout,
		deflate:        deflate,
	}
	return newConn(nc, br, true, string(subprotocol), cfg), nil
}

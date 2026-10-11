package websocket

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for Sec-WebSocket-Accept.
	"encoding/base64"
	"errors"
	"iter"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// AcceptOptions configures Accept. The zero value is valid.
type AcceptOptions struct {
	// CheckOrigin reports whether the request's Origin is acceptable. When
	// nil, requests without an Origin header are accepted and all others must
	// carry exactly one Origin of the form scheme://host[:port] whose host and
	// port equal the request Host, compared case-insensitively and without
	// regard to the scheme.
	CheckOrigin func(ctx *fasthttp.RequestCtx) bool

	// Subprotocols lists the supported subprotocols in order of preference.
	// The first one the client also offers is selected; with no match none is.
	// Names must be distinct HTTP tokens.
	Subprotocols []string

	// MaxMessageSize limits the size of a message, counting all its fragments.
	// A larger message closes the connection with StatusMessageTooBig.
	// Zero means DefaultMaxMessageSize.
	MaxMessageSize int

	// ReadTimeout bounds every Read, Reader and reader Read call, but not
	// CloseRead; WriteTimeout bounds every frame write, but not Ping's wait
	// for the pong. A call whose I/O exceeds its bound closes the connection;
	// one that times out while waiting for another call to release the
	// connection returns os.ErrDeadlineExceeded and leaves it open. Zero means
	// no bound.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

var (
	defaultAcceptOptions AcceptOptions

	acceptGUID = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	http11     = []byte("HTTP/1.1")

	// reservedHeaders are the fields the handshake owns.
	reservedHeaders = []string{
		fasthttp.HeaderConnection, fasthttp.HeaderUpgrade, fasthttp.HeaderContentLength, fasthttp.HeaderTransferEncoding,
		fasthttp.HeaderTrailer, fasthttp.HeaderSecWebSocketKey, fasthttp.HeaderSecWebSocketVersion, fasthttp.HeaderSecWebSocketProtocol,
		fasthttp.HeaderSecWebSocketExtensions, fasthttp.HeaderSecWebSocketAccept,
	}
)

// Accept upgrades the request to a WebSocket connection and arranges for
// handler to run on it once the request handler returns. The connection is
// closed when handler returns; calls still in flight on other goroutines fail.
// Headers set on ctx.Response before the call are sent with the 101 response,
// except those of the handshake itself and Trailer, which Accept removes or
// replaces; one stored under a spelling it cannot remove is a server error.
//
// If the request is not a valid WebSocket handshake, Accept writes an error
// response and returns the reason; the caller should then return without
// touching the response.
//
// fasthttp skips the upgrade while shutting down with Server.CloseOnShutdown.
// The Server's timeouts and Shutdown do not apply to the upgraded connection;
// use the options here instead. fasthttp closes the TCP connection when
// handler returns, or as soon as the WebSocket connection is closed with
// Server.KeepHijackedConns.
func Accept(ctx *fasthttp.RequestCtx, opts *AcceptOptions, handler func(*Conn)) error {
	if opts == nil {
		opts = &defaultAcceptOptions
	}
	if !validSubprotocols(opts.Subprotocols) {
		return reject(ctx, fasthttp.StatusInternalServerError, "invalid subprotocol name in AcceptOptions")
	}
	h := &ctx.Request.Header
	f := gather(h.All())
	defer f.release()
	switch {
	case !ctx.IsGet():
		err := reject(ctx, fasthttp.StatusMethodNotAllowed, "method must be GET")
		ctx.Response.Header.Set(fasthttp.HeaderAllow, fasthttp.MethodGet)
		return err
	case !h.IsHTTP11():
		return reject(ctx, fasthttp.StatusBadRequest, "HTTP/1.1 is required")
	case h.ConnectionClose() || !hasToken(f.connection, "upgrade"), !hasToken(f.upgrade, "websocket"):
		return reject(ctx, fasthttp.StatusBadRequest, "not a WebSocket upgrade request")
	case h.ContentLength() > 0 || h.ContentLength() == -1: // Bytes, or a chunked body still unread.
		return reject(ctx, fasthttp.StatusBadRequest, "handshake request has a body")
	case f.versionN != 1 || string(f.version) != "13":
		err := reject(ctx, fasthttp.StatusUpgradeRequired, "unsupported Sec-WebSocket-Version")
		ctx.Response.Header.Set(fasthttp.HeaderUpgrade, "websocket")
		ctx.Response.Header.Set(fasthttp.HeaderConnection, "Upgrade")
		ctx.Response.Header.Set(fasthttp.HeaderSecWebSocketVersion, "13")
		return err
	}
	if f.keyN != 1 || len(f.key) != 24 {
		return reject(ctx, fasthttp.StatusBadRequest, "bad Sec-WebSocket-Key")
	}
	var decoded [18]byte
	if n, err := base64.StdEncoding.Decode(decoded[:], f.key); err != nil || n != 16 {
		return reject(ctx, fasthttp.StatusBadRequest, "bad Sec-WebSocket-Key")
	}
	if !checkOrigin(ctx, opts, f) {
		return reject(ctx, fasthttp.StatusForbidden, "origin not allowed")
	}
	subprotocol, ok := selectSubprotocol(f, opts.Subprotocols)
	if !ok {
		return reject(ctx, fasthttp.StatusBadRequest, "bad Sec-WebSocket-Protocol")
	}
	if !validExtensions(f.extensions) {
		return reject(ctx, fasthttp.StatusBadRequest, "bad Sec-WebSocket-Extensions")
	}

	resp := &ctx.Response
	resp.ResetBody()
	if !delReservedHeaders(&resp.Header) {
		return reject(ctx, fasthttp.StatusInternalServerError, "a handshake header on the response cannot be replaced")
	}
	resp.Header.SetProtocol(http11)
	resp.SetStatusCode(fasthttp.StatusSwitchingProtocols)
	resp.Header.Set(fasthttp.HeaderUpgrade, "websocket")
	resp.Header.Set(fasthttp.HeaderConnection, "Upgrade")
	var accept [28]byte
	resp.Header.SetBytesV(fasthttp.HeaderSecWebSocketAccept, appendAcceptKey(accept[:0], f.key))
	if subprotocol != "" {
		resp.Header.Set(fasthttp.HeaderSecWebSocketProtocol, subprotocol)
	}
	cfg := connConfig{
		maxMessageSize: opts.MaxMessageSize,
		readTimeout:    opts.ReadTimeout,
		writeTimeout:   opts.WriteTimeout,
	}
	ctx.Hijack(func(nc net.Conn) {
		c := newConn(nc, nc, false, subprotocol, cfg)
		defer c.shutdown()
		handler(c)
	})
	return nil
}

// fields holds the header fields a handshake is judged by, gathered in one
// pass so that lookups do not depend on fasthttp's header name normalization.
// Single-valued fields keep their last value, trimmed, and a count.
type fields struct {
	key, version, origin, accept              []byte
	keyN, versionN, originN, acceptN          int
	connection, upgrade, protocol, extensions [][]byte
	names                                     [][]byte // Scratch for subprotocol names.
}

var fieldsPool sync.Pool

func gather(all iter.Seq2[[]byte, []byte]) *fields {
	f, _ := fieldsPool.Get().(*fields)
	if f == nil {
		f = new(fields)
	}
	for k, v := range all {
		switch {
		case equalFoldASCII(k, fasthttp.HeaderSecWebSocketKey):
			f.key, f.keyN = trimHTTPSpace(v), f.keyN+1
		case equalFoldASCII(k, fasthttp.HeaderSecWebSocketVersion):
			f.version, f.versionN = trimHTTPSpace(v), f.versionN+1
		case equalFoldASCII(k, fasthttp.HeaderOrigin):
			f.origin, f.originN = trimHTTPSpace(v), f.originN+1
		case equalFoldASCII(k, fasthttp.HeaderSecWebSocketAccept):
			f.accept, f.acceptN = trimHTTPSpace(v), f.acceptN+1
		case equalFoldASCII(k, fasthttp.HeaderConnection):
			f.connection = append(f.connection, v)
		case equalFoldASCII(k, fasthttp.HeaderUpgrade):
			f.upgrade = append(f.upgrade, v)
		case equalFoldASCII(k, fasthttp.HeaderSecWebSocketProtocol):
			f.protocol = append(f.protocol, v)
		case equalFoldASCII(k, fasthttp.HeaderSecWebSocketExtensions):
			f.extensions = append(f.extensions, v)
		}
	}
	return f
}

// release returns f to the pool, keeping the room its lists grew.
func (f *fields) release() {
	for _, l := range []*[][]byte{&f.connection, &f.upgrade, &f.protocol, &f.extensions, &f.names} {
		clear(*l)
		*l = (*l)[:0]
	}
	*f = fields{connection: f.connection, upgrade: f.upgrade, protocol: f.protocol, extensions: f.extensions, names: f.names}
	fieldsPool.Put(f)
}

// delReservedHeaders removes the fields the handshake owns, rescanning after
// each deletion since one may reveal another (Connection: close, Trailer). It
// reports false when a field is stored under a spelling Del does not reach.
func delReservedHeaders(h *fasthttp.ResponseHeader) bool {
	var kb [64]byte
	for {
		var key []byte
		for k := range h.All() {
			if reservedHeader(k) {
				key = append(kb[:0], k...)
				break
			}
		}
		if key == nil {
			return true
		}
		h.DelBytes(key)
		for k := range h.All() {
			if bytes.Equal(k, key) {
				return false
			}
		}
	}
}

func reservedHeader(key []byte) bool {
	return slices.ContainsFunc(reservedHeaders, func(name string) bool { return equalFoldASCII(key, name) })
}

func reject(ctx *fasthttp.RequestCtx, status int, msg string) error {
	ctx.Error(msg, status)
	return errors.New("websocket: " + msg)
}

func checkOrigin(ctx *fasthttp.RequestCtx, opts *AcceptOptions, f *fields) bool {
	if opts.CheckOrigin != nil {
		return opts.CheckOrigin(ctx)
	}
	if f.originN == 0 {
		return true
	}
	// An origin is scheme://host[:port]; "null" and anything else is refused.
	scheme, host, ok := bytes.Cut(f.origin, []byte("://"))
	if f.originN != 1 || !ok || !isToken(scheme) || len(host) == 0 || bytes.ContainsAny(host, "/?#@") {
		return false
	}
	return equalFoldASCII(host, string(ctx.Host()))
}

// validSubprotocols reports whether names are distinct tokens.
func validSubprotocols(names []string) bool {
	for i, name := range names {
		if !isToken(name) || slices.Contains(names[:i], name) {
			return false
		}
	}
	return true
}

// selectSubprotocol picks the first supported subprotocol the client offers.
// It reports false when the field is present without a name, or a name is
// not a token or repeats.
func selectSubprotocol(f *fields, supported []string) (string, bool) {
	best := len(supported)
	for _, v := range f.protocol {
		for name, rest, ok := nextToken(v); ok; name, rest, ok = nextToken(rest) {
			if !isToken(name) || slices.ContainsFunc(f.names, func(seen []byte) bool { return bytes.Equal(seen, name) }) {
				return "", false
			}
			f.names = append(f.names, name)
			if i := slices.IndexFunc(supported[:best], func(s string) bool { return s == string(name) }); i >= 0 {
				best = i
			}
		}
	}
	if len(f.protocol) > 0 && len(f.names) == 0 {
		return "", false
	}
	if best == len(supported) {
		return "", true
	}
	return supported[best], true
}

// validExtensions checks the Sec-WebSocket-Extensions syntax of RFC 6455
// section 9.1: when present, a non-empty list of tokens, each with optional
// ";"-separated token parameters whose values are tokens or quoted tokens.
// Values never contain separators, so quotes are ignored when splitting.
func validExtensions(values [][]byte) bool {
	n := 0
	for _, v := range values {
		for ext, rest, ok := nextToken(v); ok; ext, rest, ok = nextToken(rest) {
			n++
			name, params, more := bytes.Cut(ext, []byte{';'})
			if !isToken(trimHTTPSpace(name)) {
				return false
			}
			for more {
				var param []byte
				param, params, more = bytes.Cut(params, []byte{';'})
				key, val, hasVal := bytes.Cut(param, []byte{'='})
				if !isToken(trimHTTPSpace(key)) {
					return false
				}
				if val = trimHTTPSpace(val); hasVal && !isToken(val) && !isQuotedToken(val) {
					return false
				}
			}
		}
	}
	return len(values) == 0 || n > 0
}

// isQuotedToken reports whether b is a quoted-string whose unescaped value is
// a token.
func isQuotedToken(b []byte) bool {
	if len(b) < 3 || b[0] != '"' || b[len(b)-1] != '"' {
		return false
	}
	for i := 1; i < len(b)-1; i++ {
		if b[i] == '\\' {
			i++
		}
		if i == len(b)-1 || !isTokenByte(b[i]) {
			return false
		}
	}
	return true
}

// nextToken returns the first non-empty item of a comma-separated list and
// the rest of the list.
func nextToken(list []byte) (item, rest []byte, ok bool) {
	for len(list) > 0 {
		item, list, _ = bytes.Cut(list, []byte{','})
		if item = trimHTTPSpace(item); len(item) > 0 {
			return item, list, true
		}
	}
	return nil, nil, false
}

// hasToken reports whether any of the header values lists token, compared
// ASCII case-insensitively.
func hasToken(values [][]byte, token string) bool {
	for _, v := range values {
		for item, rest, ok := nextToken(v); ok; item, rest, ok = nextToken(rest) {
			if equalFoldASCII(item, token) {
				return true
			}
		}
	}
	return false
}

// onlyToken reports whether the header values list token, compared ASCII
// case-insensitively, and nothing else.
func onlyToken(values [][]byte, token string) bool {
	n := 0
	for _, v := range values {
		for item, rest, ok := nextToken(v); ok; item, rest, ok = nextToken(rest) {
			if !equalFoldASCII(item, token) {
				return false
			}
			n++
		}
	}
	return n > 0
}

func equalFoldASCII(a []byte, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// trimHTTPSpace removes the whitespace HTTP allows around field values.
func trimHTTPSpace(b []byte) []byte {
	return bytes.Trim(b, " \t")
}

// isToken reports whether s is a non-empty HTTP token (RFC 7230).
func isToken[T ~string | ~[]byte](s T) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isTokenByte(s[i]) {
			return false
		}
	}
	return true
}

func isTokenByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	default:
		return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
	}
}

func appendAcceptKey(dst, key []byte) []byte {
	var b [24 + 36]byte
	n := copy(b[:], key)
	n += copy(b[n:], acceptGUID)
	sum := sha1.Sum(b[:n]) //nolint:gosec // RFC 6455 mandates SHA-1 for Sec-WebSocket-Accept.
	return base64.StdEncoding.AppendEncode(dst, sum[:])
}

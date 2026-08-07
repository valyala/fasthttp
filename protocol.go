package fasthttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// ErrProtocolNotSupported is returned when the protocol serving a request
// doesn't implement an optional operation such as server push or a
// bidirectional request stream.
var (
	ErrProtocolNotSupported = errors.New("fasthttp: protocol operation not supported")
	// ErrHijackNotSupported is returned when connection hijacking is attempted
	// from a multiplexed protocol request.
	ErrHijackNotSupported = errors.New("fasthttp: connection hijacking isn't supported by this protocol")
	// ErrPushDisabled is returned when either endpoint disabled server push.
	ErrPushDisabled = errors.New("fasthttp: server push is disabled")
	// ErrPushLimit is returned when a protocol's promised-stream limit is full.
	ErrPushLimit = errors.New("fasthttp: server push limit reached")
	// ErrPushNotAllowed is returned when Push is called after the parent stream
	// can no longer initiate a promised request.
	ErrPushNotAllowed = errors.New("fasthttp: server push isn't allowed in this stream state")
)

// ProtocolHandler serves a connection selected by ALPN or a cleartext
// connection preface.
type ProtocolHandler interface {
	ServeConn(ctx *ProtocolServerContext, c net.Conn) error
}

// ProtocolRegistration describes a connection-oriented HTTP protocol that a
// Server can dispatch before its HTTP/1 parser runs.
//
// ALPN and CleartextPreface are copied by RegisterProtocol. At least one must
// be set. CleartextPreface is matched only on non-TLS connections.
//
// CleartextUpgradeToken additionally offers the protocol through the HTTP/1.1
// Upgrade handshake: a non-TLS HTTP/1 request whose Upgrade header equals the
// token is handed to the Handler, which must implement ProtocolUpgrader.
type ProtocolRegistration struct {
	ALPN                  []string
	FallbackALPN          []string
	CleartextPreface      []byte
	CleartextUpgradeToken string
	MinTLSVersion         uint16
	Handler               ProtocolHandler
}

// ProtocolUpgrader accepts HTTP/1.1 Upgrade handshakes for a registered
// protocol. UpgradeConn reports whether it took over the connection; when
// false the request is served as ordinary HTTP/1. upgraded is read-only and
// valid only for the duration of the call.
type ProtocolUpgrader interface {
	UpgradeConn(ctx *ProtocolServerContext, c net.Conn, upgraded *Request) (bool, error)
}

type tlsConn interface {
	Handshake() error
	ConnectionState() tls.ConnectionState
}

type registeredProtocol struct {
	alpn             []string
	cleartextPreface []byte
	upgradeToken     string
	handler          ProtocolHandler
}

// InformationalResponseWriter is implemented by protocols that can write an
// informational response without completing the request.
type InformationalResponseWriter interface {
	WriteInformational(statusCode int, header *ResponseHeader) error
}

// Pusher is implemented by protocols that support server push.
type Pusher interface {
	Push(target string, opts *PushOptions) error
}

// StreamAccepter is implemented by protocols that support accepting a
// bidirectional stream associated with a request.
type StreamAccepter interface {
	AcceptStream(handler StreamHandler) error
}

// ProtocolStream supplies request-scoped cancellation and optional protocol
// operations to RequestCtx.
//
// Implementations may additionally implement InformationalResponseWriter,
// Pusher, and StreamAccepter.
type ProtocolStream interface {
	context.Context
}

// PushOptions configures a server push request. Method defaults to GET. Header
// is copied before Push returns.
type PushOptions struct {
	Method string
	Header *RequestHeader
}

// StreamConn is a request-scoped, bidirectional stream. Closing or applying a
// deadline to a StreamConn must not affect other streams on the same physical
// connection.
type StreamConn interface {
	net.Conn
	CloseRead() error
	CloseWrite() error
}

// StreamHandler processes a bidirectional request stream.
type StreamHandler func(StreamConn)

// ProtocolServerContext connects a ProtocolHandler to one Server. A context is
// valid only for the duration of ProtocolHandler.ServeConn.
type ProtocolServerContext struct {
	server       *Server
	conn         net.Conn
	idleConnTime *atomic.Int64
	connTime     time.Time
	connID       uint64
	requestCount atomic.Uint64
	active       atomic.Int32
	prefaceRead  bool
	requestMu    sync.Mutex
	requestCache []cachedRequestCtx
	requestBytes int
}

type cachedRequestCtx struct {
	ctx           *RequestCtx
	retainedBytes int
}

const (
	maxProtocolRequestCtxCache             = 256
	defaultMaxProtocolRequestCtxCacheBytes = 128 << 20
)

// ServeProtocolConn serves c directly with handler while applying Server's
// connection limits, lifecycle accounting, and ConnState callbacks. It is
// intended for protocol packages that expose a dedicated prior-knowledge
// listener.
//
// ServeProtocolConn closes c before returning.
func (s *Server) ServeProtocolConn(c net.Conn, handler ProtocolHandler) error {
	if isNilInterfaceValue(handler) {
		return errors.New("fasthttp: protocol handler is nil")
	}
	if s.MaxConnsPerIP > 0 {
		perIPConn := wrapPerIPConn(s, c)
		if perIPConn == nil {
			return ErrPerIPConnLimit
		}
		c = perIPConn
	}
	if !s.tryAcquireConcurrency() {
		s.writeFastError(c, StatusServiceUnavailable, "The connection cannot be served because Server.Concurrency limit exceeded")
		_ = c.Close()
		return ErrConcurrencyLimit
	}
	defer s.releaseConcurrency()

	s.open.Add(1)
	defer s.open.Add(-1)
	err := s.serveProtocolConn(c, &registeredProtocol{handler: handler}, false)
	closeErr := c.Close()
	s.setState(c, StateClosed)
	if err != nil {
		return err
	}
	if errors.Is(closeErr, net.ErrClosed) {
		return nil
	}
	return closeErr
}

// Server returns the Server that dispatched this protocol connection.
func (ctx *ProtocolServerContext) Server() *Server {
	return ctx.server
}

// Done is closed when the Server starts shutting down. It may return nil when
// serving a standalone connection for which shutdown isn't configured.
func (ctx *ProtocolServerContext) Done() <-chan struct{} {
	return ctx.server.done
}

// ServerDate returns the Server's cached RFC 1123 Date header value. The
// returned bytes are read-only and remain valid until the next cache refresh.
func (ctx *ProtocolServerContext) ServerDate() []byte {
	serverDateOnce.Do(updateServerDate)
	return *serverDate.Load()
}

// CleartextPrefaceConsumed reports whether Server selected this protocol by
// consuming its registered cleartext preface. When true, ServeConn must start
// reading immediately after the preface. ALPN and ServeProtocolConn dispatches
// leave the preface for the protocol handler to read.
func (ctx *ProtocolServerContext) CleartextPrefaceConsumed() bool {
	return ctx.prefaceRead
}

// AcquireRequestCtx obtains a RequestCtx owned by the Server and binds it to a
// protocol stream. The caller must pair every successful call with
// ReleaseRequestCtx after all response and stream work is complete.
func (ctx *ProtocolServerContext) AcquireRequestCtx(c net.Conn, stream ProtocolStream) *RequestCtx {
	requestCtx := ctx.acquireRequestCtx(c)
	requestCtx.connTime = ctx.connTime
	requestCtx.time = time.Now()
	requestCtx.connID = ctx.connID
	requestCtx.connRequestNum = ctx.requestCount.Add(1)
	requestCtx.protocolStream = stream
	requestCtx.protocolOwner = ctx

	if ctx.active.Add(1) == 1 {
		ctx.idleConnTime.Store(0)
		ctx.server.setState(ctx.conn, StateActive)
	}

	return requestCtx
}

// ReleaseRequestCtx returns a context acquired with AcquireRequestCtx to the
// Server pool.
func (ctx *ProtocolServerContext) ReleaseRequestCtx(requestCtx *RequestCtx) {
	if requestCtx == nil || requestCtx.s != ctx.server || requestCtx.protocolOwner != ctx {
		panic("BUG: releasing a request context to the wrong protocol server")
	}

	if requestCtx.timeoutResponse == nil {
		requestCtx.protocolOwner = nil
		requestCtx.reset()
		retainedBytes := requestCtxRetainedBytes(requestCtx)
		ctx.requestMu.Lock()
		cacheBytes := ctx.requestCtxCacheBytes()
		if len(ctx.requestCache) < maxProtocolRequestCtxCache &&
			retainedBytes <= cacheBytes-ctx.requestBytes {
			ctx.requestCache = append(ctx.requestCache, cachedRequestCtx{requestCtx, retainedBytes})
			ctx.requestBytes += retainedBytes
			requestCtx = nil
		}
		ctx.requestMu.Unlock()
		if requestCtx != nil {
			requestCtx.Request.ReleaseBody(0)
			requestCtx.Response.ReleaseBody(0)
			ctx.server.ctxPool.Put(requestCtx)
		}
	}
	active := ctx.active.Add(-1)
	if active < 0 {
		panic("BUG: protocol request context released more than once")
	}
	if active == 0 {
		ctx.idleConnTime.Store(time.Now().Unix())
		ctx.server.setState(ctx.conn, StateIdle)
	}
}

func (ctx *ProtocolServerContext) requestCtxCacheBytes() int {
	if limit := ctx.server.MaxProtocolRequestCtxCacheBytes; limit != 0 {
		return limit
	}
	return defaultMaxProtocolRequestCtxCacheBytes
}

func (ctx *ProtocolServerContext) acquireRequestCtx(c net.Conn) *RequestCtx {
	ctx.requestMu.Lock()
	var requestCtx *RequestCtx
	if last := len(ctx.requestCache) - 1; last >= 0 {
		entry := ctx.requestCache[last]
		ctx.requestCache[last] = cachedRequestCtx{}
		ctx.requestCache = ctx.requestCache[:last]
		ctx.requestBytes -= entry.retainedBytes
		requestCtx = entry.ctx
	}
	ctx.requestMu.Unlock()
	if requestCtx == nil {
		return ctx.server.acquireCtx(c)
	}
	requestCtx.c = c
	if ctx.server.FormValueFunc != nil {
		requestCtx.formValueFunc = ctx.server.FormValueFunc
	}
	return requestCtx
}

func (ctx *ProtocolServerContext) releaseCachedRequestCtxs() {
	ctx.requestMu.Lock()
	cache := ctx.requestCache
	ctx.requestCache = nil
	ctx.requestBytes = 0
	ctx.requestMu.Unlock()
	for _, entry := range cache {
		entry.ctx.Request.ReleaseBody(0)
		entry.ctx.Response.ReleaseBody(0)
		ctx.server.ctxPool.Put(entry.ctx)
	}
}

func requestCtxRetainedBytes(requestCtx *RequestCtx) int {
	retained := 0
	if requestCtx.Request.body != nil {
		retained = cap(requestCtx.Request.body.B)
	}
	if requestCtx.Response.body != nil {
		retained += cap(requestCtx.Response.body.B)
	}
	retained += requestHeaderRetainedBytes(&requestCtx.Request.Header)
	retained += responseHeaderRetainedBytes(&requestCtx.Response.Header)
	return retained
}

func requestHeaderRetainedBytes(header *RequestHeader) int {
	return headerRetainedBytes(&header.header) +
		cap(header.method) + cap(header.requestURI) + cap(header.host) +
		cap(header.userAgent) + cap(header.connectProtocol) + cap(header.rawHeaders)
}

func responseHeaderRetainedBytes(header *ResponseHeader) int {
	return headerRetainedBytes(&header.header) + cap(header.statusMessage) +
		cap(header.contentEncoding) + cap(header.server)
}

func headerRetainedBytes(header *header) int {
	// Large field arrays are expensive to rescan after Reset has truncated
	// their lengths. They are also exactly the arenas a per-connection cache
	// should not retain; return a saturating estimate after O(1) checks.
	if cap(header.h) > 256 || cap(header.cookies) > 256 ||
		cap(header.mulHeader) > 256 || cap(header.trailer) > 256 {
		return math.MaxInt / 4
	}
	retained := cap(header.h)*int(unsafe.Sizeof(argsKV{})) +
		cap(header.cookies)*int(unsafe.Sizeof(argsKV{})) +
		cap(header.mulHeader)*int(unsafe.Sizeof([]byte{})) +
		cap(header.trailer)*int(unsafe.Sizeof([]byte{})) +
		cap(header.bufK) + cap(header.bufV) + cap(header.contentLengthBytes) +
		cap(header.contentType) + cap(header.protocol)
	fields := header.h[:cap(header.h)]
	for i := range fields {
		retained += cap(fields[i].key) + cap(fields[i].value)
	}
	cookies := header.cookies[:cap(header.cookies)]
	for i := range cookies {
		retained += cap(cookies[i].key) + cap(cookies[i].value)
	}
	multi := header.mulHeader[:cap(header.mulHeader)]
	for i := range multi {
		retained += cap(multi[i])
	}
	trailers := header.trailer[:cap(header.trailer)]
	for i := range trailers {
		retained += cap(trailers[i])
	}
	return retained
}

// RegisterProtocol registers a connection-oriented HTTP protocol. It must be
// called before the Server starts serving.
func (s *Server) RegisterProtocol(registration ProtocolRegistration) error { //nolint:gocritic
	if isNilInterfaceValue(registration.Handler) {
		return errors.New("fasthttp: protocol handler is nil")
	}
	if len(registration.ALPN) == 0 && len(registration.CleartextPreface) == 0 {
		return errors.New("fasthttp: protocol registration has no selector")
	}

	alpn := append([]string(nil), registration.ALPN...)
	fallbackALPN := append([]string(nil), registration.FallbackALPN...)
	preface := bytes.Clone(registration.CleartextPreface)
	seenALPN := make(map[string]struct{}, len(alpn))
	for _, protocol := range alpn {
		if protocol == "" {
			return errors.New("fasthttp: protocol alpn is empty")
		}
		if _, ok := seenALPN[protocol]; ok {
			return fmt.Errorf("fasthttp: protocol alpn %q is duplicated", protocol)
		}
		seenALPN[protocol] = struct{}{}
	}
	seenFallback := make(map[string]struct{}, len(fallbackALPN))
	for _, protocol := range fallbackALPN {
		if protocol == "" {
			return errors.New("fasthttp: protocol fallback alpn is empty")
		}
		if _, ok := seenALPN[protocol]; ok {
			return fmt.Errorf("fasthttp: protocol fallback alpn %q is also registered", protocol)
		}
		if _, ok := seenFallback[protocol]; ok {
			return fmt.Errorf("fasthttp: protocol fallback alpn %q is duplicated", protocol)
		}
		seenFallback[protocol] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ln) != 0 || s.open.Load() != 0 {
		return errors.New("fasthttp: cannot register a protocol after serving starts")
	}
	if s.protocol != nil {
		return errors.New("fasthttp: a protocol is already registered")
	}

	if registration.CleartextUpgradeToken != "" {
		if _, ok := registration.Handler.(ProtocolUpgrader); !ok {
			return errors.New("fasthttp: protocol upgrade token requires a ProtocolUpgrader handler")
		}
	}

	for _, protocol := range alpn {
		if _, ok := s.nextProtos[protocol]; ok {
			return fmt.Errorf("fasthttp: protocol alpn %q is already registered by NextProto", protocol)
		}
	}

	var tlsConfig *tls.Config
	if len(alpn) != 0 {
		var err error
		tlsConfig, err = prepareProtocolTLSConfig(
			s.TLSConfig,
			alpn,
			fallbackALPN,
			registration.MinTLSVersion,
		)
		if err != nil {
			return err
		}
	}

	protocol := &registeredProtocol{
		alpn:             alpn,
		cleartextPreface: preface,
		upgradeToken:     registration.CleartextUpgradeToken,
		handler:          registration.Handler,
	}
	s.protocol = protocol
	// ALPN dispatch goes through the same table as NextProto handlers; the
	// TLS NextProtos ordering is owned by prepareProtocolTLSConfig above.
	if len(alpn) != 0 && s.nextProtos == nil {
		s.nextProtos = make(map[string]ServeHandler)
	}
	for _, proto := range alpn {
		s.nextProtos[proto] = func(conn net.Conn) error {
			return s.serveProtocolConn(conn, protocol, false)
		}
	}
	if tlsConfig != nil {
		s.TLSConfig = tlsConfig
	}

	return nil
}

func prepareProtocolTLSConfig(
	current *tls.Config,
	alpn []string,
	fallback []string,
	minVersion uint16,
) (*tls.Config, error) {
	config := &tls.Config{} //nolint:gosec // MinVersion is enforced below from the registrations.
	if current != nil {
		config = current.Clone()
	}
	if minVersion != 0 {
		if config.MaxVersion != 0 && config.MaxVersion < minVersion {
			return nil, errors.New("fasthttp: protocol TLS maximum version is too low")
		}
		if config.MinVersion < minVersion {
			config.MinVersion = minVersion
		}
	}

	registered := make(map[string]struct{}, len(alpn))
	for _, protocol := range alpn {
		registered[protocol] = struct{}{}
	}
	fallbackSet := make(map[string]struct{}, len(fallback))
	for _, protocol := range fallback {
		fallbackSet[protocol] = struct{}{}
	}
	ordered := make([]string, 0, len(config.NextProtos)+len(alpn)+len(fallback))
	inserted := false
	for _, protocol := range config.NextProtos {
		_, isRegistered := registered[protocol]
		_, isFallback := fallbackSet[protocol]
		if !inserted && (isRegistered || isFallback) {
			ordered = append(ordered, alpn...)
			inserted = true
		}
		if !isRegistered {
			ordered = append(ordered, protocol)
		}
	}
	if !inserted {
		ordered = append(ordered, alpn...)
	}
	for _, protocol := range fallback {
		if !slices.Contains(ordered, protocol) {
			ordered = append(ordered, protocol)
		}
	}
	config.NextProtos = ordered
	if current != nil {
		// Preserve pointer identity for callers that already constructed a
		// tls.Listener from Server.TLSConfig. Validation and ordering happen on
		// the clone above, so a failure still leaves the caller's config intact.
		current.MinVersion = config.MinVersion
		current.NextProtos = config.NextProtos
		return current, nil
	}
	return config, nil
}

func isNilInterfaceValue(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (s *Server) serveProtocolConn(c net.Conn, protocol *registeredProtocol, prefaceRead bool) error {
	connTime := time.Now()
	idleConnTime := s.registerIdleConn(c, connTime.Add(5*time.Second))
	defer s.unregisterIdleConn(c, idleConnTime)
	s.registerProtocolConn(c)
	defer s.unregisterProtocolConn(c)

	ctx := &ProtocolServerContext{
		server:       s,
		conn:         c,
		idleConnTime: idleConnTime,
		connTime:     connTime,
		connID:       nextConnID(),
		prefaceRead:  prefaceRead,
	}
	defer ctx.releaseCachedRequestCtxs()
	return protocol.handler.ServeConn(ctx, c)
}

func (s *Server) protocolForUpgradeToken(token []byte) *registeredProtocol {
	if p := s.protocol; p != nil && p.upgradeToken != "" && string(token) == p.upgradeToken {
		return p
	}
	return nil
}

func (s *Server) serveUpgradedProtocolConn(
	c net.Conn,
	protocol *registeredProtocol,
	requestCtx *RequestCtx,
	idleConnTime *atomic.Int64,
) (bool, error) {
	s.registerProtocolConn(c)
	defer s.unregisterProtocolConn(c)
	// The HTTP/1 request is complete while the upgraded protocol waits for its
	// client preface. Mark that handshake wait idle immediately so Shutdown can
	// close it instead of waiting for the HTTP/1 initial-connection grace period.
	idleConnTime.Store(time.Now().Unix())
	ctx := &ProtocolServerContext{
		server:       s,
		conn:         c,
		idleConnTime: idleConnTime,
		connTime:     requestCtx.connTime,
		connID:       requestCtx.connID,
	}
	defer ctx.releaseCachedRequestCtxs()
	upgrader := protocol.handler.(ProtocolUpgrader) //nolint:forcetypeassert // enforced by RegisterProtocol
	return upgrader.UpgradeConn(ctx, c, &requestCtx.Request)
}

func (s *Server) registerProtocolConn(c net.Conn) {
	s.idleConnsMu.Lock()
	if s.protocolConns == nil {
		s.protocolConns = make(map[net.Conn]struct{})
	}
	s.protocolConns[c] = struct{}{}
	s.idleConnsMu.Unlock()
}

func (s *Server) unregisterProtocolConn(c net.Conn) {
	s.idleConnsMu.Lock()
	delete(s.protocolConns, c)
	s.idleConnsMu.Unlock()
}

func (s *Server) registerIdleConn(c net.Conn, idleAt time.Time) *atomic.Int64 {
	s.idleConnsMu.Lock()
	defer s.idleConnsMu.Unlock()
	if s.idleConns == nil {
		s.idleConns = make(map[net.Conn]*atomic.Int64)
	}
	if idleConnTime := s.idleConns[c]; idleConnTime != nil {
		idleConnTime.Store(idleAt.Unix())
		return idleConnTime
	}

	value := idleConnTimePool.Get()
	if value == nil {
		value = &atomic.Int64{}
	}
	idleConnTime := value.(*atomic.Int64) //nolint:forcetypeassert
	idleConnTime.Store(idleAt.Unix())
	s.idleConns[c] = idleConnTime
	return idleConnTime
}

func (s *Server) unregisterIdleConn(c net.Conn, idleConnTime *atomic.Int64) {
	s.idleConnsMu.Lock()
	delete(s.idleConns, c)
	s.idleConnsMu.Unlock()
	idleConnTimePool.Put(idleConnTime)
}

type protocolPrefaceReader struct {
	conn   net.Conn
	prefix []byte
}

func (r *protocolPrefaceReader) Read(p []byte) (int, error) {
	if len(r.prefix) != 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return r.conn.Read(p)
}

func (s *Server) detectCleartextProtocol(c net.Conn) (*registeredProtocol, []byte, time.Time, error) {
	if _, isTLS := c.(tlsConn); isTLS {
		return nil, nil, time.Time{}, nil
	}
	protocol := s.protocol
	if protocol == nil || len(protocol.cleartextPreface) == 0 {
		return nil, nil, time.Time{}, nil
	}
	preface := protocol.cleartextPreface

	var deadline time.Time
	if s.ReadTimeout > 0 {
		deadline = time.Now().Add(s.ReadTimeout)
		if err := c.SetReadDeadline(deadline); err != nil {
			return nil, nil, time.Time{}, err
		}
	}

	// Read at most the preface itself, so a match consumes it exactly and a
	// mismatch replays only the compared prefix to the HTTP/1 parser.
	prefix := make([]byte, 0, len(preface))
	emptyReads := 0
	for {
		n, err := c.Read(prefix[len(prefix):len(preface)])
		prefix = prefix[:len(prefix)+n]
		if n > 0 {
			emptyReads = 0
			if !bytes.Equal(prefix, preface[:len(prefix)]) {
				return nil, prefix, deadline, nil
			}
			if len(prefix) == len(preface) {
				return protocol, prefix, deadline, nil
			}
		} else if err == nil {
			emptyReads++
			if emptyReads >= 100 {
				return nil, prefix, deadline, io.ErrNoProgress
			}
		}
		if err != nil {
			return nil, prefix, deadline, err
		}
	}
}

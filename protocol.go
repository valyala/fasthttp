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
//
// Server.Shutdown waits for ServeConn to return without closing the
// connection, idle or not, so ServeConn must wind it down once
// ProtocolServerContext.Done is closed. ShutdownWithContext closes it when its
// context expires.
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
// valid only for the duration of the call; config is what HeaderReceived
// returned for it (zero when HeaderReceived is nil).
type ProtocolUpgrader interface {
	UpgradeConn(ctx *ProtocolServerContext, c net.Conn, upgraded *Request, config RequestConfig) (bool, error)
}

type registeredProtocol struct {
	alpn             []string
	cleartextPreface []byte
	upgradeToken     []byte
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

// HijackRejectionNotifier is implemented by protocol streams that want to be
// told a handler attempted connection hijacking, which multiplexed protocols
// cannot support. RequestCtx.Hijack notifies through it because that legacy
// API cannot return an error; TryHijack returns ErrHijackNotSupported directly.
// noResponse is the HijackSetNoResponse setting: the handler meant to take
// the connection without answering.
type HijackRejectionNotifier interface {
	HijackRejected(noResponse bool)
}

// ProtocolStream is the protocol stream behind a RequestCtx. Its context
// reports the stream's own lifetime to the protocol; RequestCtx's context
// methods stay server-scoped, as for HTTP/1.
//
// Implementations may additionally implement InformationalResponseWriter,
// Pusher, StreamAccepter, and HijackRejectionNotifier.
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
	done         <-chan struct{}
	idleConnTime *atomic.Int64
	connTime     time.Time
	connID       uint64
	requestCount atomic.Uint64
	isTLS        bool
	prefaceRead  bool
	// stateMu orders the active count with the idle time and ConnState it
	// implies; a protocol may acquire and release from several goroutines.
	stateMu        sync.Mutex
	active         int32
	activeReported bool
	// finished is set once ServeConn returned: the connection's idle time
	// and state belong to nobody then, but a late release still balances.
	finished     bool
	requestMu    sync.Mutex
	requestCache []cachedRequestCtx
	requestBytes int
	cacheClosed  bool
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
	if isNilOrTypedNil(handler) {
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
		s.rejectConn(c, StatusServiceUnavailable, "The connection cannot be served because Server.Concurrency limit exceeded")
		return ErrConcurrencyLimit
	}
	defer s.releaseConcurrency()

	s.open.Add(1)
	defer s.open.Add(-1)
	err := s.serveProtocolConn(c, &registeredProtocol{handler: handler}, protocolConnStart{})
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

// Done is closed when the Server starts shutting down.
func (ctx *ProtocolServerContext) Done() <-chan struct{} {
	return ctx.done
}

// HeaderTooLargeError returns the error HTTP/1 reports for a request header
// the server won't read in full, wrapping cause, so a protocol's oversized
// request reaches Server.ErrorHandler as the same *ErrSmallBuffer (default
// response 431).
func (ctx *ProtocolServerContext) HeaderTooLargeError(cause error) error {
	return &ErrSmallBuffer{error: cause}
}

// BodyStreamPanicError returns the error HTTP/1 reports for a response body
// stream whose Read or Close panicked, so a protocol's recovered panic reaches
// logs as the same *ErrBodyStreamWritePanic.
func (ctx *ProtocolServerContext) BodyStreamPanicError(recovered any) error {
	return newBodyStreamWritePanic(recovered)
}

// ParseTrailer records the names of a received Trailer field as the HTTP/1
// request parser does: names accumulate over repeated fields, values already
// in the header section stay there, and a forbidden or invalid name returns
// ErrBadTrailer while the valid names are still recorded.
func (ctx *ProtocolServerContext) ParseTrailer(h *RequestHeader, trailer []byte) error {
	var set trailerSet
	return h.addParsedTrailers(trailer, &set)
}

// ResponseBodyReader returns resp's body stream bounded to its declared
// Content-Length with Response.Write's rules: a shorter stream fails, bytes
// left over fail (a bare *os.File may have grown and stops at the length),
// and an error returned with the last bytes follows them. Without a declared
// length the stream itself is returned. The caller still closes the body
// stream; the returned reader never does.
func (ctx *ProtocolServerContext) ResponseBodyReader(resp *Response) io.Reader {
	return sizedBodyStream(resp.bodyStream, resp.Header.ContentLength())
}

// ServerDate returns the Server's cached RFC 1123 Date header value. The
// returned bytes are read-only and remain valid until the next cache refresh.
func (ctx *ProtocolServerContext) ServerDate() []byte {
	serverDateOnce.Do(updateServerDate)
	line := *serverDateLine.Load()
	return line[len(strDate)+len(": ") : len(line)-len(strCRLF)]
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
	return ctx.acquireStreamRequestCtx(c, stream, ctx.requestCount.Add(1))
}

// AcquireReplacementRequestCtx obtains a RequestCtx that stands in for
// replaced on stream, as the Server does for a timed-out handler: the same
// connection and request number, counted as the same request. replaced stays
// with whoever still uses it; AbandonRequestCtx it when that work ends.
func (ctx *ProtocolServerContext) AcquireReplacementRequestCtx(stream ProtocolStream, replaced *RequestCtx) *RequestCtx {
	return ctx.acquireStreamRequestCtx(replaced.c, stream, replaced.connRequestNum)
}

func (ctx *ProtocolServerContext) acquireStreamRequestCtx(c net.Conn, stream ProtocolStream, connRequestNum uint64) *RequestCtx {
	requestCtx := ctx.acquireRequestCtx(c)
	ctx.applyRequestDefaults(requestCtx)
	requestCtx.connTime = ctx.connTime
	requestCtx.time = time.Time{}
	if !ctx.server.LazyRequestTime {
		requestCtx.time = time.Now()
	}
	requestCtx.connID = ctx.connID
	requestCtx.connRequestNum = connRequestNum
	requestCtx.protocolStream = stream
	requestCtx.protocolOwner = ctx
	requestCtx.Response.lazyStreamWriter = true

	ctx.stateMu.Lock()
	ctx.active++
	if ctx.active == 1 && !ctx.finished {
		ctx.idleConnTime.Store(0)
		if !ctx.activeReported {
			ctx.activeReported = true
			ctx.server.setState(ctx.conn, StateActive)
		}
	}
	ctx.stateMu.Unlock()

	return requestCtx
}

// ReleaseRequestCtx returns a context acquired with AcquireRequestCtx to the
// Server pool, once its handler has returned. It reports whether the context
// was pooled: one that a timed-out handler or a response body writer may
// still use is left to the garbage collector.
//
// A SetBodyStreamWriter writer whose body was never read runs now, its writes
// failing.
func (ctx *ProtocolServerContext) ReleaseRequestCtx(requestCtx *RequestCtx) bool {
	ctx.detachRequestCtx(requestCtx)
	// A timed-out handler may still be setting its response.
	pooled := requestCtx.timeoutResponse == nil && !requestCtx.Response.releaseStreamWriters()
	if pooled {
		ctx.recycleRequestCtx(requestCtx)
	}
	ctx.releaseActive()
	return pooled
}

// AbandonRequestCtx releases a context acquired with AcquireRequestCtx that a
// goroutine outside the protocol's control may still use, such as a response
// body writer cut short by a stream reset. It is accounted for like a released
// context but is neither reset nor pooled. Writers whose bodies were never
// read run as for ReleaseRequestCtx.
func (ctx *ProtocolServerContext) AbandonRequestCtx(requestCtx *RequestCtx) {
	ctx.detachRequestCtx(requestCtx)
	if requestCtx.timeoutResponse == nil {
		requestCtx.Response.releaseStreamWriters()
	}
	ctx.releaseActive()
}

func (ctx *ProtocolServerContext) detachRequestCtx(requestCtx *RequestCtx) {
	if requestCtx == nil || requestCtx.s != ctx.server || requestCtx.protocolOwner != ctx {
		panic("BUG: releasing a request context to the wrong protocol server")
	}
	requestCtx.protocolOwner = nil
}

func (ctx *ProtocolServerContext) recycleRequestCtx(requestCtx *RequestCtx) {
	requestCtx.reset()
	requestCtx.Response.lazyStreamWriter = false
	retainedBytes := requestCtxRetainedBytes(requestCtx)
	ctx.requestMu.Lock()
	cacheBytes := ctx.requestCtxCacheBytes()
	if !ctx.cacheClosed && len(ctx.requestCache) < maxProtocolRequestCtxCache &&
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

func (ctx *ProtocolServerContext) releaseActive() {
	ctx.stateMu.Lock()
	defer ctx.stateMu.Unlock()
	ctx.active--
	if ctx.active < 0 {
		panic("BUG: protocol request context released more than once")
	}
	if ctx.active == 0 && !ctx.finished {
		ctx.idleConnTime.Store(time.Now().Unix())
		ctx.activeReported = false
		ctx.server.setState(ctx.conn, StateIdle)
	}
}

// finish marks ServeConn's return; the connection's state and idle time are
// off limits afterwards.
func (ctx *ProtocolServerContext) finish() {
	ctx.stateMu.Lock()
	ctx.finished = true
	ctx.stateMu.Unlock()
}

// applyRequestDefaults mirrors the per-request setup of the HTTP/1 serve loop.
func (ctx *ProtocolServerContext) applyRequestDefaults(requestCtx *RequestCtx) {
	server := ctx.server
	requestCtx.Request.isTLS = ctx.isTLS
	requestCtx.Request.logger = server.logger()
	requestCtx.Response.Header.noDefaultContentType = server.NoDefaultContentType
	requestCtx.Response.Header.noDefaultDate = server.NoDefaultDate
	if line := server.getServerNameLine(server.getServerName()); line != nil {
		requestCtx.Response.Header.setServerDefault(line)
	}
	requestCtx.Request.Header.secureErrorLogMessage = server.SecureErrorLogMessage
	requestCtx.Response.Header.secureErrorLogMessage = server.SecureErrorLogMessage
	requestCtx.Request.secureErrorLogMessage = server.SecureErrorLogMessage
	requestCtx.Response.secureErrorLogMessage = server.SecureErrorLogMessage
	if server.DisableHeaderNamesNormalizing {
		requestCtx.Request.Header.DisableNormalizing()
		requestCtx.Response.Header.DisableNormalizing()
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
	ctx.cacheClosed = true
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
//
//nolint:gocritic // registration by value is the public contract
func (s *Server) RegisterProtocol(registration ProtocolRegistration) error {
	if isNilOrTypedNil(registration.Handler) {
		return errors.New("fasthttp: protocol handler is nil")
	}
	if len(registration.ALPN) == 0 && len(registration.CleartextPreface) == 0 {
		return errors.New("fasthttp: protocol registration has no selector")
	}

	if err := validateProtocolALPN(registration.ALPN, registration.FallbackALPN); err != nil {
		return err
	}
	alpn := append([]string(nil), registration.ALPN...)
	fallbackALPN := append([]string(nil), registration.FallbackALPN...)
	preface := bytes.Clone(registration.CleartextPreface)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ln) != 0 || s.open.Load() != 0 {
		return errors.New("fasthttp: cannot register a protocol after serving starts")
	}

	if registration.CleartextUpgradeToken != "" {
		if _, ok := registration.Handler.(ProtocolUpgrader); !ok {
			return errors.New("fasthttp: protocol upgrade token requires a ProtocolUpgrader handler")
		}
	}

	// The shared nextProtos table covers ALPN clashes with both NextProto
	// handlers and previously registered protocols.
	for _, protocol := range alpn {
		if _, ok := s.nextProtos[protocol]; ok {
			return fmt.Errorf("fasthttp: protocol alpn %q is already registered", protocol)
		}
	}
	for _, existing := range s.protocols {
		if prefacesConflict(existing.cleartextPreface, preface) {
			return errors.New("fasthttp: protocol cleartext prefaces conflict")
		}
		if registration.CleartextUpgradeToken != "" &&
			upgradeTokenMatches([]byte(registration.CleartextUpgradeToken), existing.upgradeToken) {
			return fmt.Errorf("fasthttp: protocol upgrade token %q is already registered", registration.CleartextUpgradeToken)
		}
	}

	var tlsConfig *tls.Config
	tlsTable := s.protocolTLS
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
		tlsTable.alpn = append(slices.Clone(tlsTable.alpn), alpn...)
		tlsTable.fallback = slices.Clone(tlsTable.fallback)
		for _, protocol := range fallbackALPN {
			if !slices.Contains(tlsTable.fallback, protocol) {
				tlsTable.fallback = append(tlsTable.fallback, protocol)
			}
		}
		tlsTable.minVersion = max(tlsTable.minVersion, registration.MinTLSVersion)
	}

	protocol := &registeredProtocol{
		alpn:             alpn,
		cleartextPreface: preface,
		upgradeToken:     []byte(registration.CleartextUpgradeToken),
		handler:          registration.Handler,
	}
	s.protocols = append(s.protocols, protocol)
	// ALPN dispatch goes through the same table as NextProto handlers; the
	// TLS NextProtos ordering is owned by prepareProtocolTLSConfig above.
	if len(alpn) != 0 && s.nextProtos == nil {
		s.nextProtos = make(map[string]ServeHandler)
	}
	for _, proto := range alpn {
		s.nextProtos[proto] = func(conn net.Conn) error {
			return s.serveProtocolConn(conn, protocol, protocolConnStart{})
		}
	}
	if tlsConfig != nil {
		// Configs GetConfigForClient returns get the same preparation; the
		// wrapper is rebuilt around the user's callback for every registration.
		if !s.protocolTLSCallbackSet {
			s.protocolTLSCallback = tlsConfig.GetConfigForClient
			s.protocolTLSCallbackSet = true
		}
		if s.protocolTLSCallback != nil {
			tlsConfig.GetConfigForClient = tlsTable.wrap(s.protocolTLSCallback)
		}
		s.protocolTLS = tlsTable
		s.TLSConfig = tlsConfig
	}

	return nil
}

// validateProtocolALPN rejects empty, duplicated, or cross-listed names.
func validateProtocolALPN(alpn, fallbackALPN []string) error {
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
	return nil
}

func prefacesConflict(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return bytes.HasPrefix(a, b) || bytes.HasPrefix(b, a)
}

// orderNextProtos places the registered ALPN names before the first
// registered or fallback entry of existing (at the end when there is none),
// drops duplicates of the registered names and appends missing fallbacks.
func orderNextProtos(existing, alpn, fallback []string) []string {
	registered := make(map[string]struct{}, len(alpn))
	for _, protocol := range alpn {
		registered[protocol] = struct{}{}
	}
	fallbackSet := make(map[string]struct{}, len(fallback))
	for _, protocol := range fallback {
		fallbackSet[protocol] = struct{}{}
	}
	ordered := make([]string, 0, len(existing)+len(alpn)+len(fallback))
	inserted := false
	for _, protocol := range existing {
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
	return ordered
}

// applyProtocolTLSVersion raises MinVersion to the registrations' floor.
func applyProtocolTLSVersion(config *tls.Config, minVersion uint16) error {
	if minVersion == 0 {
		return nil
	}
	if config.MaxVersion != 0 && config.MaxVersion < minVersion {
		return errors.New("fasthttp: protocol TLS maximum version is too low")
	}
	if config.MinVersion < minVersion {
		config.MinVersion = minVersion
	}
	return nil
}

func prepareProtocolTLSConfig(
	current *tls.Config,
	alpn []string,
	fallback []string,
	minVersion uint16,
) (*tls.Config, error) {
	config := &tls.Config{}
	if current != nil {
		config = current.Clone()
	}
	if err := applyProtocolTLSVersion(config, minVersion); err != nil {
		return nil, err
	}
	config.NextProtos = orderNextProtos(config.NextProtos, alpn, fallback)
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

// protocolTLSTable is what the registered protocols require of a TLS config.
type protocolTLSTable struct {
	alpn       []string
	fallback   []string
	minVersion uint16
}

func (t protocolTLSTable) prepared(config *tls.Config) bool {
	if t.minVersion != 0 && config.MinVersion < t.minVersion {
		return false
	}
	return slices.Equal(config.NextProtos, orderNextProtos(config.NextProtos, t.alpn, t.fallback))
}

// prepareReturned readies a config GetConfigForClient returned the way
// RegisterProtocol readies Server.TLSConfig: a prepared one is used as is,
// any other is cloned, never mutated.
func (t protocolTLSTable) prepareReturned(returned *tls.Config) (*tls.Config, error) {
	if t.prepared(returned) {
		return returned, nil
	}
	config := returned.Clone()
	config.GetConfigForClient = nil
	if err := applyProtocolTLSVersion(config, t.minVersion); err != nil {
		return nil, err
	}
	config.NextProtos = orderNextProtos(config.NextProtos, t.alpn, t.fallback)
	return config, nil
}

func (t protocolTLSTable) wrap(
	original func(*tls.ClientHelloInfo) (*tls.Config, error),
) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		returned, err := original(hello)
		if err != nil || returned == nil {
			return returned, err
		}
		return t.prepareReturned(returned)
	}
}

func isNilOrTypedNil(v any) bool {
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

// protocolConnStart carries what the dispatcher already did for a
// connection: its idle registration and, after cleartext detection, the first
// byte's StateActive report.
type protocolConnStart struct {
	connTime     time.Time
	idleConnTime *atomic.Int64
	prefaceRead  bool
	active       bool
}

func (s *Server) serveProtocolConn(c net.Conn, protocol *registeredProtocol, start protocolConnStart) error {
	connTime := start.connTime
	if connTime.IsZero() {
		connTime = time.Now()
	}
	idleConnTime := start.idleConnTime
	if idleConnTime == nil {
		idleConnTime = s.registerIdleConn(c, connTime.Add(5*time.Second))
	}
	defer s.unregisterIdleConn(c, idleConnTime)
	done := s.registerProtocolConn(c)
	defer s.unregisterProtocolConn(c)

	ctx := &ProtocolServerContext{
		server:         s,
		conn:           c,
		done:           done,
		idleConnTime:   idleConnTime,
		connTime:       connTime,
		connID:         nextConnID(),
		isTLS:          isTLSConn(c),
		prefaceRead:    start.prefaceRead,
		activeReported: start.active,
	}
	defer ctx.releaseCachedRequestCtxs()
	defer ctx.finish()
	return protocolServeError(protocol.handler.ServeConn(ctx, c))
}

// protocolServeError maps the error of a connection the server closed itself,
// as Shutdown's deadline does, to nil, as the HTTP/1 path does.
func protocolServeError(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// protocolForALPN is the registered protocol serving the negotiated ALPN
// protocol; nil for a NextProto handler.
func (s *Server) protocolForALPN(proto string) *registeredProtocol {
	for _, candidate := range s.protocols {
		if slices.Contains(candidate.alpn, proto) {
			return candidate
		}
	}
	return nil
}

// protocolForUpgradeToken matches the Upgrade field's protocol list.
func (s *Server) protocolForUpgradeToken(upgrade []byte) *registeredProtocol {
	var values headerValueScanner
	values.b = upgrade
	for values.next() {
		for _, candidate := range s.protocols {
			if len(candidate.upgradeToken) != 0 && upgradeTokenMatches(values.value, candidate.upgradeToken) {
				return candidate
			}
		}
	}
	return nil
}

// upgradeTokenMatches compares protocol names case-insensitively and
// versions, when present, exactly (RFC 9110 §7.8, §16.7).
func upgradeTokenMatches(token, registered []byte) bool {
	tokenName, tokenVersion, _ := bytes.Cut(token, []byte{'/'})
	registeredName, registeredVersion, _ := bytes.Cut(registered, []byte{'/'})
	return caseInsensitiveCompare(tokenName, registeredName) && bytes.Equal(tokenVersion, registeredVersion)
}

func (s *Server) serveUpgradedProtocolConn(
	c net.Conn,
	protocol *registeredProtocol,
	requestCtx *RequestCtx,
	idleConnTime *atomic.Int64,
	config RequestConfig,
) (bool, error) {
	done := s.registerProtocolConn(c)
	defer s.unregisterProtocolConn(c)
	ctx := &ProtocolServerContext{
		server:         s,
		conn:           c,
		done:           done,
		idleConnTime:   idleConnTime,
		connTime:       requestCtx.connTime,
		connID:         requestCtx.connID,
		isTLS:          isTLSConn(c),
		activeReported: true,
	}
	// The Upgrade request becomes the protocol's first stream and keeps its
	// number; later streams continue the connection's numbering.
	ctx.requestCount.Store(requestCtx.connRequestNum - 1)
	defer ctx.releaseCachedRequestCtxs()
	defer ctx.finish()
	upgrader := protocol.handler.(ProtocolUpgrader) //nolint:forcetypeassert // enforced by RegisterProtocol
	handled, err := upgrader.UpgradeConn(ctx, c, &requestCtx.Request, config)
	return handled, protocolServeError(err)
}

// registerProtocolConn records c and returns the shutdown signal it must
// watch; both happen under protocolMu so a connection that registers while
// Shutdown runs sees a closed channel.
func (s *Server) registerProtocolConn(c net.Conn) <-chan struct{} {
	s.protocolMu.Lock()
	defer s.protocolMu.Unlock()
	s.idleConnsMu.Lock()
	if s.protocolConns == nil {
		s.protocolConns = make(map[net.Conn]struct{})
	}
	s.protocolConns[c] = struct{}{}
	s.idleConnsMu.Unlock()
	return s.protocolDoneLocked()
}

func (s *Server) protocolConnCount() int {
	s.idleConnsMu.Lock()
	defer s.idleConnsMu.Unlock()
	return len(s.protocolConns)
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

// isTLSConn answers RequestCtx.IsTLS for a connection without a RequestCtx.
func isTLSConn(c net.Conn) bool {
	if pic, ok := c.(*perIPConn); ok {
		c = pic.Conn
	}
	_, ok := c.(tlsConn)
	return ok
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

// cleartextDetection is what reading a connection's first bytes found: the
// protocol they matched, or the prefix to replay to the HTTP/1 parser.
type cleartextDetection struct {
	protocol *registeredProtocol
	prefix   []byte
	// deadline is the read deadline left armed on the connection.
	deadline time.Time
	// active reports whether the first byte was reported: idle time cleared
	// and StateActive delivered, as the HTTP/1 loop does for a new connection.
	active bool
	// deadlineFailed marks an error from SetReadDeadline, which ends the
	// connection as it does without protocols.
	deadlineFailed bool
	// claimed reports that Shutdown was closing the idle connection when its
	// first byte arrived; nothing on it is served.
	claimed bool
}

func (s *Server) detectCleartextProtocol(c net.Conn, idleConnTime *atomic.Int64) (cleartextDetection, error) {
	var result cleartextDetection
	if isTLSConn(c) {
		return result, nil
	}

	candidates := make([]*registeredProtocol, 0, len(s.protocols))
	maxPrefaceLen := 0
	for _, protocol := range s.protocols {
		preface := protocol.cleartextPreface
		if len(preface) == 0 {
			continue
		}
		candidates = append(candidates, protocol)
		if len(preface) > maxPrefaceLen {
			maxPrefaceLen = len(preface)
		}
	}
	if len(candidates) == 0 {
		return result, nil
	}

	if s.ReadTimeout > 0 {
		result.deadline = time.Now().Add(s.ReadTimeout)
		if err := c.SetReadDeadline(result.deadline); err != nil {
			result.deadlineFailed = true
			return result, err
		}
	}

	// Never read past the next candidate boundary: a mismatch must replay only
	// the compared prefix to the HTTP/1 parser.
	prefix := make([]byte, 0, maxPrefaceLen)
	emptyReads := 0
	for {
		nextLen := maxPrefaceLen
		for _, candidate := range candidates {
			if length := len(candidate.cleartextPreface); length < nextLen {
				nextLen = length
			}
		}
		oldLen := len(prefix)
		n, err := c.Read(prefix[oldLen:nextLen])
		prefix = prefix[:oldLen+n]
		result.prefix = prefix
		if n == 0 && err == nil {
			emptyReads++
			if emptyReads >= 100 {
				return result, io.ErrNoProgress
			}
			continue
		}
		if n > 0 {
			emptyReads = 0
			if !result.active {
				if idleConnTime.Swap(0) == idleConnClaimedByShutdown {
					result.claimed = true
					return result, nil
				}
				result.active = true
				s.setState(c, StateActive)
				// ReadTimeout restarts at the first byte, as the byte
				// reader's read does without protocols.
				if s.ReduceMemoryUsage && s.ReadTimeout > 0 {
					result.deadline = time.Now().Add(s.ReadTimeout)
					if err := c.SetReadDeadline(result.deadline); err != nil {
						result.deadlineFailed = true
						return result, err
					}
				}
			}
			matched, remaining := narrowPrefaceCandidates(candidates, prefix)
			if matched != nil {
				result.protocol = matched
				return result, nil
			}
			candidates = remaining
			if len(candidates) == 0 {
				return result, nil
			}
		}
		if err != nil {
			return result, err
		}
	}
}

// narrowPrefaceCandidates drops the candidates prefix has ruled out and
// returns the one it matched exactly, if any. It reuses candidates' storage.
func narrowPrefaceCandidates(
	candidates []*registeredProtocol,
	prefix []byte,
) (*registeredProtocol, []*registeredProtocol) {
	remaining := candidates[:0]
	for _, candidate := range candidates {
		preface := candidate.cleartextPreface
		if bytes.Equal(prefix, preface) {
			return candidate, remaining
		}
		if len(prefix) < len(preface) && bytes.Equal(prefix, preface[:len(prefix)]) {
			remaining = append(remaining, candidate)
		}
	}
	return nil, remaining
}

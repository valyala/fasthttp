package http2

import (
	"crypto/tls"
	"errors"
	"net"
	"reflect"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	defaultMaxConcurrentStreams    = 250
	defaultMaxHeaderListSize       = 64 << 10
	defaultHeaderTableSize         = 4096
	defaultMaxFrameSize            = 16 << 10
	defaultWriteBufferSize         = 64 << 10
	defaultReadBufferSize          = 16 << 10
	defaultConnectionWindowSize    = 4 << 20
	defaultStreamWindowSize        = 1 << 20
	defaultBufferedRequestBodySize = 128 << 20
	maxQueuedCommands              = 256
	maxPromisedStreams             = 16
	maxConfiguredConcurrentStreams = 1 << 20
	maxConfiguredHeaderListSize    = 64 << 20
	maxConfiguredHeaderTableSize   = 64 << 20
)

// defaultWriteByteTimeout bounds a write that makes no progress, not a whole
// write, so a slow but healthy peer is unaffected.
const defaultWriteByteTimeout = 15 * time.Second

// The HTTP/2 preface is a fixed handshake message that compliant peers send
// immediately. Bound it even when the HTTP/1-style server timeouts are zero so
// h2c upgrades cannot park a fully allocated protocol connection forever.
const defaultPrefaceTimeout = 10 * time.Second

// flushCoalesceTimeout bounds how long a flush waits for handlers started in
// the current batch to finish.
const flushCoalesceTimeout = time.Millisecond

// ClientMode selects how an HTTP/2 transport negotiates a connection.
type ClientMode uint8

const (
	// PreferHTTP2 advertises h2 and http/1.1 over TLS and reuses a connection
	// that selects HTTP/1.1 as the HostClient fallback connection.
	PreferHTTP2 ClientMode = iota
	// RequireHTTP2 fails when TLS ALPN doesn't select h2.
	RequireHTTP2
	// PriorKnowledge sends the HTTP/2 connection preface immediately on a
	// cleartext connection. It must only be used for known HTTP/2 origins.
	PriorKnowledge
)

// PushHandler decides whether promised requests are accepted and handles
// accepted push responses. Request and response values are only valid for the
// duration of the callback that receives them.
type PushHandler interface {
	Accept(parent, promised *fasthttp.Request) bool
	Handle(promised *fasthttp.Request, response *fasthttp.Response)
}

// ClientConfig controls HTTP/2 client negotiation, limits, and optional
// protocol extensions. The zero value prefers HTTP/2 for TLS origins and uses
// ordinary HTTP/1 for cleartext origins.
type ClientConfig struct {
	// Mode selects how connections negotiate HTTP/2. See the ClientMode
	// constants. The zero value is PreferHTTP2.
	Mode ClientMode

	// MaxConcurrentStreams caps the concurrent requests multiplexed on one
	// connection before an additional connection is dialed. The effective cap
	// is the smaller of this value and the peer's SETTINGS_MAX_CONCURRENT_STREAMS.
	// Zero means 250.
	MaxConcurrentStreams uint32

	// MaxHeaderListSize advertises SETTINGS_MAX_HEADER_LIST_SIZE: the maximum
	// decompressed size, in bytes, of a response header block, counting 32
	// bytes of overhead per field. Zero means 64 KiB, raised to
	// HostClient.ReadBufferSize when that is larger.
	MaxHeaderListSize uint32

	// MaxDecoderHeaderTableSize advertises SETTINGS_HEADER_TABLE_SIZE: the
	// HPACK dynamic table memory this client offers the server's encoder.
	// Zero means 4096 bytes.
	MaxDecoderHeaderTableSize uint32

	// MaxEncoderHeaderTableSize caps the HPACK dynamic table memory this
	// client's encoder uses, regardless of the larger value the server may
	// offer. Zero means 4096 bytes.
	MaxEncoderHeaderTableSize uint32

	// MaxReadFrameSize advertises SETTINGS_MAX_FRAME_SIZE: the largest frame
	// payload this client accepts. Values must stay within RFC 9113's
	// 16384..16777215 range. Zero means 16384.
	MaxReadFrameSize uint32

	// MaxResponseBufferPerConnection is the connection-level flow-control
	// window: the response bytes the server may send before the client
	// acknowledges consuming them. Zero means 4 MiB; values below 65535 are
	// rejected.
	MaxResponseBufferPerConnection int32

	// MaxResponseBufferPerStream is the per-stream flow-control window
	// advertised through SETTINGS_INITIAL_WINDOW_SIZE. Zero means 1 MiB.
	MaxResponseBufferPerStream int32

	// ReadIdleTimeout enables a health-check PING when no frame arrives on an
	// active connection for this long. Zero disables the health check.
	ReadIdleTimeout time.Duration

	// PingTimeout bounds how long a health-check PING may stay unanswered
	// before the connection is torn down. Zero means 15 seconds.
	PingTimeout time.Duration

	// WriteByteTimeout bounds how long a single physical write may make no
	// progress before the connection is torn down. Zero falls back to
	// HostClient.WriteTimeout, then to 15 seconds.
	WriteByteTimeout time.Duration

	// EnableExtendedConnect allows OpenStream to use RFC 8441 extended
	// CONNECT when the server advertises support for it.
	EnableExtendedConnect bool

	// PushHandler accepts and consumes RFC 9113 §8.4 server pushes. When nil,
	// push is disabled through SETTINGS_ENABLE_PUSH=0 and any PUSH_PROMISE is
	// a connection error.
	PushHandler PushHandler
}

type clientConfig struct {
	mode                  ClientMode
	maxConcurrentStreams  uint32
	maxHeaderListSize     uint32
	maxDecoderTableSize   uint32
	maxEncoderTableSize   uint32
	maxReadFrameSize      uint32
	connectionWindowSize  int32
	streamWindowSize      int32
	readIdleTimeout       time.Duration
	pingTimeout           time.Duration
	writeByteTimeout      time.Duration
	enableExtendedConnect bool
	pushHandler           PushHandler
}

func normalizeClientConfig(hc *fasthttp.HostClient, cfg *ClientConfig) (clientConfig, error) {
	if cfg.Mode > PriorKnowledge {
		return clientConfig{}, errors.New("http2: invalid client mode")
	}
	if isTypedNil(cfg.PushHandler) {
		return clientConfig{}, errors.New("http2: push handler is nil")
	}
	result := clientConfig{
		mode:                  cfg.Mode,
		maxConcurrentStreams:  cfg.MaxConcurrentStreams,
		maxHeaderListSize:     cfg.MaxHeaderListSize,
		maxDecoderTableSize:   cfg.MaxDecoderHeaderTableSize,
		maxEncoderTableSize:   cfg.MaxEncoderHeaderTableSize,
		maxReadFrameSize:      cfg.MaxReadFrameSize,
		connectionWindowSize:  cfg.MaxResponseBufferPerConnection,
		streamWindowSize:      cfg.MaxResponseBufferPerStream,
		readIdleTimeout:       cfg.ReadIdleTimeout,
		pingTimeout:           cfg.PingTimeout,
		writeByteTimeout:      cfg.WriteByteTimeout,
		enableExtendedConnect: cfg.EnableExtendedConnect,
		pushHandler:           cfg.PushHandler,
	}
	if result.maxConcurrentStreams == 0 {
		result.maxConcurrentStreams = defaultMaxConcurrentStreams
	}
	if result.maxConcurrentStreams > maxConfiguredConcurrentStreams {
		return clientConfig{}, errors.New("http2: max concurrent streams exceeds the safety limit")
	}
	if result.maxHeaderListSize == 0 {
		result.maxHeaderListSize = defaultMaxHeaderListSize
		if hc != nil && hc.ReadBufferSize > int(result.maxHeaderListSize) {
			result.maxHeaderListSize = uint32(hc.ReadBufferSize)
		}
	}
	if result.maxDecoderTableSize == 0 {
		result.maxDecoderTableSize = defaultHeaderTableSize
	}
	if result.maxHeaderListSize > maxConfiguredHeaderListSize ||
		result.maxDecoderTableSize > maxConfiguredHeaderTableSize ||
		result.maxEncoderTableSize > maxConfiguredHeaderTableSize {
		return clientConfig{}, errors.New("http2: configured header memory limit is too large")
	}
	if result.maxEncoderTableSize == 0 {
		result.maxEncoderTableSize = defaultHeaderTableSize
	}
	if result.maxReadFrameSize == 0 {
		result.maxReadFrameSize = defaultMaxFrameSize
	}
	if result.maxReadFrameSize < defaultMaxFrameSize || result.maxReadFrameSize > 1<<24-1 {
		return clientConfig{}, errors.New("http2: max read frame size must be between 16384 and 16777215")
	}
	if result.connectionWindowSize == 0 {
		result.connectionWindowSize = defaultConnectionWindowSize
	}
	if result.connectionWindowSize < 65535 {
		return clientConfig{}, errors.New("http2: connection response window must be at least 65535")
	}
	if result.streamWindowSize == 0 {
		result.streamWindowSize = defaultStreamWindowSize
	}
	if result.streamWindowSize < 1 {
		return clientConfig{}, errors.New("http2: stream response window must be positive")
	}
	if result.pingTimeout == 0 {
		result.pingTimeout = 15 * time.Second
	}
	if result.writeByteTimeout == 0 && hc != nil {
		result.writeByteTimeout = hc.WriteTimeout
	}
	if result.writeByteTimeout == 0 {
		result.writeByteTimeout = defaultWriteByteTimeout
	}
	return result, nil
}

func isTypedNil(value any) bool {
	if value == nil {
		return false
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// ServerConfig controls HTTP/2-specific server behavior. The zero value uses
// bounded production defaults and keeps server push and extended CONNECT
// disabled.
type ServerConfig struct {
	// MaxConcurrentStreams advertises SETTINGS_MAX_CONCURRENT_STREAMS: the
	// request streams one client connection may have in flight. Zero means 250.
	MaxConcurrentStreams uint32

	// MaxHeaderListSize advertises SETTINGS_MAX_HEADER_LIST_SIZE: the maximum
	// decompressed size, in bytes, of a request header block, counting 32
	// bytes of overhead per field. Zero means 64 KiB, raised to
	// Server.ReadBufferSize when that is larger.
	MaxHeaderListSize uint32

	// MaxDecoderHeaderTableSize advertises SETTINGS_HEADER_TABLE_SIZE: the
	// HPACK dynamic table memory this server offers the client's encoder.
	// Zero means 4096 bytes.
	MaxDecoderHeaderTableSize uint32

	// MaxEncoderHeaderTableSize caps the HPACK dynamic table memory this
	// server's encoder uses, regardless of the larger value the client may
	// offer. Zero means 4096 bytes.
	MaxEncoderHeaderTableSize uint32

	// MaxReadFrameSize advertises SETTINGS_MAX_FRAME_SIZE: the largest frame
	// payload this server accepts. Values must stay within RFC 9113's
	// 16384..16777215 range. Zero means 16384.
	MaxReadFrameSize uint32

	// MaxRapidResetsPerSecond caps peer RST_STREAM frames plus RST_STREAM
	// responses induced by invalid peer input per second before the connection
	// is closed with ENHANCE_YOUR_CALM, mitigating rapid-cancellation and
	// reset-oracle attacks. Zero means 1000.
	MaxRapidResetsPerSecond uint32

	// MaxUploadBufferPerConnection is the connection-level flow-control
	// window advertised to the peer for request DATA. It limits the amount of
	// unconsumed DATA in flight, not the total size of ordinary request bodies
	// retained while handlers are waiting to run. Zero means 4 MiB; values
	// below 65535 are rejected.
	MaxUploadBufferPerConnection int32

	// MaxUploadBufferPerStream is the per-stream flow-control window
	// advertised through SETTINGS_INITIAL_WINDOW_SIZE. Zero means 1 MiB.
	MaxUploadBufferPerStream int32

	// MaxBufferedRequestBodyPerConnection is the hard aggregate limit for
	// ordinary request bodies that fasthttp buffers before invoking their
	// handlers. Once this separate memory budget is occupied, additional
	// buffered-body streams are reset until earlier handlers return. Streaming
	// bodies return budget as the handler reads them. Zero means 128 MiB.
	MaxBufferedRequestBodyPerConnection int32

	// IdleTimeout closes a connection gracefully after it has had no active
	// streams for this long. Zero falls back to Server.IdleTimeout, then to
	// Server.ReadTimeout; if all are zero, idle connections are kept open.
	IdleTimeout time.Duration

	// PingTimeout bounds how long a shutdown PING may stay unanswered before
	// the connection is torn down. Zero means 15 seconds.
	PingTimeout time.Duration

	// WriteByteTimeout bounds how long a physical write or an individual
	// response stream blocked by peer flow control may make no progress before
	// the connection or stream is torn down. Zero falls back to
	// Server.WriteTimeout, then to 15 seconds.
	WriteByteTimeout time.Duration

	// EnablePush allows handlers to use RequestCtx.Push toward clients that
	// accept server push. When false, push attempts report
	// ErrProtocolNotSupported.
	EnablePush bool

	// EnableExtendedConnect advertises RFC 8441 extended CONNECT so clients
	// can open bidirectional streams accepted through RequestCtx.AcceptStream.
	EnableExtendedConnect bool
}

type serverConfig struct {
	maxConcurrentStreams    uint32
	maxHeaderListSize       uint32
	maxDecoderTableSize     uint32
	maxEncoderTableSize     uint32
	maxReadFrameSize        uint32
	maxRapidResetsPerSecond uint32
	connectionWindowSize    int32
	streamWindowSize        int32
	maxBufferedRequestBody  int32
	idleTimeout             time.Duration
	pingTimeout             time.Duration
	writeByteTimeout        time.Duration
	enablePush              bool
	enableExtendedConnect   bool
}

func normalizeServerConfig(s *fasthttp.Server, cfg *ServerConfig) (serverConfig, error) {
	result := serverConfig{
		maxConcurrentStreams:    cfg.MaxConcurrentStreams,
		maxHeaderListSize:       cfg.MaxHeaderListSize,
		maxDecoderTableSize:     cfg.MaxDecoderHeaderTableSize,
		maxEncoderTableSize:     cfg.MaxEncoderHeaderTableSize,
		maxReadFrameSize:        cfg.MaxReadFrameSize,
		maxRapidResetsPerSecond: cfg.MaxRapidResetsPerSecond,
		connectionWindowSize:    cfg.MaxUploadBufferPerConnection,
		streamWindowSize:        cfg.MaxUploadBufferPerStream,
		maxBufferedRequestBody:  cfg.MaxBufferedRequestBodyPerConnection,
		idleTimeout:             cfg.IdleTimeout,
		pingTimeout:             cfg.PingTimeout,
		writeByteTimeout:        cfg.WriteByteTimeout,
		enablePush:              cfg.EnablePush,
		enableExtendedConnect:   cfg.EnableExtendedConnect,
	}
	if result.maxConcurrentStreams == 0 {
		result.maxConcurrentStreams = defaultMaxConcurrentStreams
	}
	if result.maxConcurrentStreams > maxConfiguredConcurrentStreams {
		return serverConfig{}, errors.New("http2: max concurrent streams exceeds the safety limit")
	}
	if result.maxHeaderListSize == 0 {
		result.maxHeaderListSize = defaultMaxHeaderListSize
		if s.ReadBufferSize > int(result.maxHeaderListSize) {
			result.maxHeaderListSize = uint32(s.ReadBufferSize)
		}
	}
	if result.maxDecoderTableSize == 0 {
		result.maxDecoderTableSize = defaultHeaderTableSize
	}
	if result.maxEncoderTableSize == 0 {
		result.maxEncoderTableSize = defaultHeaderTableSize
	}
	if result.maxHeaderListSize > maxConfiguredHeaderListSize ||
		result.maxDecoderTableSize > maxConfiguredHeaderTableSize ||
		result.maxEncoderTableSize > maxConfiguredHeaderTableSize {
		return serverConfig{}, errors.New("http2: configured header memory limit is too large")
	}
	if result.maxReadFrameSize == 0 {
		result.maxReadFrameSize = defaultMaxFrameSize
	}
	if result.maxReadFrameSize < defaultMaxFrameSize || result.maxReadFrameSize > 1<<24-1 {
		return serverConfig{}, errors.New("http2: max read frame size must be between 16384 and 16777215")
	}
	if result.maxRapidResetsPerSecond == 0 {
		result.maxRapidResetsPerSecond = 1000
	}
	if result.connectionWindowSize == 0 {
		result.connectionWindowSize = defaultConnectionWindowSize
	}
	if result.connectionWindowSize < 65535 {
		return serverConfig{}, errors.New("http2: connection upload window must be at least 65535")
	}
	if result.maxBufferedRequestBody == 0 {
		result.maxBufferedRequestBody = defaultBufferedRequestBodySize
	}
	if result.maxBufferedRequestBody < 1 {
		return serverConfig{}, errors.New("http2: buffered request body limit must be positive")
	}
	if result.streamWindowSize == 0 {
		result.streamWindowSize = defaultStreamWindowSize
	}
	if result.streamWindowSize < 1 {
		return serverConfig{}, errors.New("http2: stream upload window must be positive")
	}
	if result.pingTimeout == 0 {
		result.pingTimeout = 15 * time.Second
	}
	if result.idleTimeout == 0 {
		result.idleTimeout = s.IdleTimeout
		if result.idleTimeout == 0 {
			result.idleTimeout = s.ReadTimeout
		}
	}
	if result.writeByteTimeout == 0 {
		result.writeByteTimeout = s.WriteTimeout
	}
	if result.writeByteTimeout == 0 {
		result.writeByteTimeout = defaultWriteByteTimeout
	}
	return result, nil
}

type serverHandler struct {
	config serverConfig
}

// ConfigureServer enables HTTP/2 on s through TLS ALPN, cleartext prior
// knowledge, and the HTTP/1.1 h2c Upgrade handshake. Existing TLS settings
// are preserved.
func ConfigureServer(s *fasthttp.Server, cfg ServerConfig) error {
	if s == nil {
		return errors.New("http2: server is nil")
	}
	normalized, err := normalizeServerConfig(s, &cfg)
	if err != nil {
		return err
	}
	return s.RegisterProtocol(fasthttp.ProtocolRegistration{
		ALPN:                  []string{"h2"},
		FallbackALPN:          []string{"http/1.1"},
		CleartextPreface:      []byte(clientPreface),
		CleartextUpgradeToken: "h2c",
		MinTLSVersion:         tls.VersionTLS12,
		Handler:               &serverHandler{config: normalized},
	})
}

// ServeConn serves one HTTP/2 prior-knowledge connection. It doesn't accept
// the obsolete h2c Upgrade handshake.
func ServeConn(s *fasthttp.Server, c net.Conn, cfg ServerConfig) error {
	if s == nil {
		return errors.New("http2: server is nil")
	}
	if c == nil {
		return errors.New("http2: connection is nil")
	}
	normalized, err := normalizeServerConfig(s, &cfg)
	if err != nil {
		return err
	}
	return s.ServeProtocolConn(c, &serverHandler{config: normalized})
}

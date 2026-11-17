package http2

import (
	"crypto/tls"
	"errors"
	"net"
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
	defaultInitialWindowSize       = 65535 // RFC 9113, Section 6.9.2
	defaultBufferedRequestBodySize = 128 << 20
	defaultMaxRapidResetsPerSecond = 1000
	maxQueuedCommands              = 256
	maxPromisedStreams             = 16
	maxConfiguredConcurrentStreams = 1 << 20
	maxConfiguredHeaderListSize    = 64 << 20
	maxConfiguredHeaderTableSize   = 64 << 20
	maxFrameSizeLimit              = 1<<24 - 1 // RFC 9113, Section 6.5.2
)

// defaultWriteByteTimeout bounds one write to the connection, not a whole
// message: a peer has this long to take in about a write buffer.
const defaultWriteByteTimeout = 15 * time.Second

// defaultPingTimeout is how long a health-check or shutdown PING may stay
// unanswered.
const defaultPingTimeout = 15 * time.Second

// defaultPrefaceTimeout bounds the wait for the client preface when the
// server's read and idle timeouts are zero.
const defaultPrefaceTimeout = 10 * time.Second

// flushCoalesceTimeout is the silence that ends a flush's wait for the
// handlers started in its batch.
const flushCoalesceTimeout = time.Millisecond

// normalizeSharedLimits applies the defaults and bounds of the limits a client
// and a server configure alike. readBufferSize raises the default header list
// size.
func normalizeSharedLimits(
	maxConcurrentStreams, maxHeaderListSize, maxDecoderTableSize, maxEncoderTableSize, maxReadFrameSize *uint32,
	readBufferSize int,
) error {
	if *maxConcurrentStreams == 0 {
		*maxConcurrentStreams = defaultMaxConcurrentStreams
	}
	if *maxConcurrentStreams > maxConfiguredConcurrentStreams {
		return errors.New("http2: max concurrent streams exceeds the safety limit")
	}
	if *maxHeaderListSize == 0 {
		*maxHeaderListSize = defaultMaxHeaderListSize
		if readBufferSize > int(*maxHeaderListSize) {
			// Clamped before the conversion, so an oversized buffer still fails the limit check.
			*maxHeaderListSize = uint32(min(readBufferSize, maxConfiguredHeaderListSize+1)) // #nosec G115
		}
	}
	if *maxDecoderTableSize == 0 {
		*maxDecoderTableSize = defaultHeaderTableSize
	}
	if *maxEncoderTableSize == 0 {
		*maxEncoderTableSize = defaultHeaderTableSize
	}
	if *maxHeaderListSize > maxConfiguredHeaderListSize ||
		*maxDecoderTableSize > maxConfiguredHeaderTableSize ||
		*maxEncoderTableSize > maxConfiguredHeaderTableSize {
		return errors.New("http2: configured header memory limit is too large")
	}
	if *maxReadFrameSize == 0 {
		*maxReadFrameSize = defaultMaxFrameSize
	}
	if *maxReadFrameSize < defaultMaxFrameSize || *maxReadFrameSize > maxFrameSizeLimit {
		return errors.New("http2: max read frame size must be between 16384 and 16777215")
	}
	return nil
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
	// buffered-body streams are reset until earlier streams are released.
	// Streamed bodies don't count; flow control bounds them. Zero means
	// 128 MiB.
	MaxBufferedRequestBodyPerConnection int32

	// IdleTimeout closes a connection gracefully after it has had no active
	// streams for this long: GOAWAY, then up to PingTimeout for the peer to
	// acknowledge the shutdown PING. A peer that never sent its SETTINGS is
	// closed without the PING. Zero falls back to Server.IdleTimeout, then
	// to Server.ReadTimeout; if all are zero, idle connections are kept open.
	IdleTimeout time.Duration

	// PingTimeout bounds how long a shutdown PING may stay unanswered before
	// the connection is torn down, and so how long Server.Shutdown and the
	// idle close wait on a connection whose client doesn't answer. Zero means
	// 15 seconds.
	PingTimeout time.Duration

	// WriteByteTimeout bounds each write to the connection, of at most two
	// write buffers; one that doesn't complete in time tears the connection
	// down. A response's own deadline is Server.WriteTimeout. Zero falls back
	// to Server.WriteTimeout, then to 15 seconds.
	WriteByteTimeout time.Duration

	// EnablePush allows handlers to use RequestCtx.Push toward clients that
	// accept server push, at most 16 pushes at a time per connection. When
	// false, Push returns fasthttp.ErrPushDisabled.
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
	// workerIdleLimit replaces maxIdleStreamWorkerDuration when nonzero.
	workerIdleLimit       time.Duration
	enablePush            bool
	enableExtendedConnect bool
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
	if cfg.IdleTimeout < 0 || cfg.PingTimeout < 0 || cfg.WriteByteTimeout < 0 {
		return serverConfig{}, errors.New("http2: timeouts must not be negative")
	}
	if err := normalizeSharedLimits(
		&result.maxConcurrentStreams, &result.maxHeaderListSize, &result.maxDecoderTableSize,
		&result.maxEncoderTableSize, &result.maxReadFrameSize, s.ReadBufferSize,
	); err != nil {
		return serverConfig{}, err
	}
	if result.maxRapidResetsPerSecond == 0 {
		result.maxRapidResetsPerSecond = defaultMaxRapidResetsPerSecond
	}
	if result.connectionWindowSize == 0 {
		result.connectionWindowSize = defaultConnectionWindowSize
	}
	if result.connectionWindowSize < defaultInitialWindowSize {
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
		result.pingTimeout = defaultPingTimeout
	}
	// A negative Server timeout is none, as for HTTP/1.
	if result.idleTimeout == 0 {
		result.idleTimeout = s.IdleTimeout
		if result.idleTimeout == 0 {
			result.idleTimeout = s.ReadTimeout
		}
		result.idleTimeout = max(result.idleTimeout, 0)
	}
	if result.writeByteTimeout == 0 {
		result.writeByteTimeout = s.WriteTimeout
	}
	if result.writeByteTimeout <= 0 {
		result.writeByteTimeout = defaultWriteByteTimeout
	}
	return result, nil
}

type serverHandler struct {
	config serverConfig
}

// ConfigureServer enables HTTP/2 on s through TLS ALPN, cleartext prior
// knowledge, and the HTTP/1.1 h2c Upgrade handshake. Existing TLS settings
// are preserved. The Server fields cfg falls back to are read now: set them
// before the call.
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

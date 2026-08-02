package http2

import (
	"errors"
	"net"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	defaultMaxConcurrentStreams   = 250
	defaultHeaderTableSize        = 4096
	defaultMaxFrameSize           = 16 << 10
	defaultConnectionWindowSize   = 1 << 20
	defaultStreamWindowSize       = 1 << 20
	defaultMaxQueuedControlFrames = 10_000
)

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
	Mode                           ClientMode
	MaxConcurrentStreams           uint32
	MaxHeaderListSize              uint32
	MaxDecoderHeaderTableSize      uint32
	MaxEncoderHeaderTableSize      uint32
	MaxReadFrameSize               uint32
	MaxUploadBufferPerConnection   int32
	MaxUploadBufferPerStream       int32
	MaxResponseBufferPerConnection int32
	MaxResponseBufferPerStream     int32

	ReadIdleTimeout time.Duration
	PingTimeout     time.Duration

	EnableExtendedConnect bool
	PushHandler           PushHandler
	CountError            func(errorType string)
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
	enableExtendedConnect bool
	pushHandler           PushHandler
	countError            func(string)
}

func normalizeClientConfig(hc *fasthttp.HostClient, cfg ClientConfig) (clientConfig, error) {
	if cfg.Mode > PriorKnowledge {
		return clientConfig{}, errors.New("http2: invalid client mode")
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
		enableExtendedConnect: cfg.EnableExtendedConnect,
		pushHandler:           cfg.PushHandler,
		countError:            cfg.CountError,
	}
	if result.maxConcurrentStreams == 0 {
		result.maxConcurrentStreams = defaultMaxConcurrentStreams
	}
	if result.maxHeaderListSize == 0 {
		if hc != nil && hc.ReadBufferSize > 0 {
			result.maxHeaderListSize = uint32(hc.ReadBufferSize)
		} else {
			result.maxHeaderListSize = 4096
		}
	}
	if result.maxDecoderTableSize == 0 {
		result.maxDecoderTableSize = defaultHeaderTableSize
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
	return result, nil
}

// ServerConfig controls HTTP/2-specific server behavior. The zero value uses
// bounded production defaults and keeps server push and extended CONNECT
// disabled.
type ServerConfig struct {
	MaxConcurrentStreams         uint32
	MaxHeaderListSize            uint32
	MaxDecoderHeaderTableSize    uint32
	MaxEncoderHeaderTableSize    uint32
	MaxReadFrameSize             uint32
	MaxQueuedControlFrames       int
	MaxPromisedStreams           uint32
	MaxPushDepth                 uint8
	MaxUploadBufferPerConnection int32
	MaxUploadBufferPerStream     int32

	IdleTimeout      time.Duration
	ReadIdleTimeout  time.Duration
	PingTimeout      time.Duration
	WriteByteTimeout time.Duration

	EnablePush            bool
	EnableExtendedConnect bool
	CountError            func(errorType string)
}

type serverConfig struct {
	maxConcurrentStreams   uint32
	maxHeaderListSize      uint32
	maxDecoderTableSize    uint32
	maxEncoderTableSize    uint32
	maxReadFrameSize       uint32
	maxQueuedControlFrames int
	maxPromisedStreams     uint32
	maxPushDepth           uint8
	connectionWindowSize   int32
	streamWindowSize       int32
	idleTimeout            time.Duration
	readIdleTimeout        time.Duration
	pingTimeout            time.Duration
	writeByteTimeout       time.Duration
	enablePush             bool
	enableExtendedConnect  bool
	countError             func(string)
}

func normalizeServerConfig(s *fasthttp.Server, cfg ServerConfig) (serverConfig, error) {
	result := serverConfig{
		maxConcurrentStreams:   cfg.MaxConcurrentStreams,
		maxHeaderListSize:      cfg.MaxHeaderListSize,
		maxDecoderTableSize:    cfg.MaxDecoderHeaderTableSize,
		maxEncoderTableSize:    cfg.MaxEncoderHeaderTableSize,
		maxReadFrameSize:       cfg.MaxReadFrameSize,
		maxQueuedControlFrames: cfg.MaxQueuedControlFrames,
		maxPromisedStreams:     cfg.MaxPromisedStreams,
		maxPushDepth:           cfg.MaxPushDepth,
		connectionWindowSize:   cfg.MaxUploadBufferPerConnection,
		streamWindowSize:       cfg.MaxUploadBufferPerStream,
		idleTimeout:            cfg.IdleTimeout,
		readIdleTimeout:        cfg.ReadIdleTimeout,
		pingTimeout:            cfg.PingTimeout,
		writeByteTimeout:       cfg.WriteByteTimeout,
		enablePush:             cfg.EnablePush,
		enableExtendedConnect:  cfg.EnableExtendedConnect,
		countError:             cfg.CountError,
	}
	if result.maxConcurrentStreams == 0 {
		result.maxConcurrentStreams = defaultMaxConcurrentStreams
	}
	if result.maxHeaderListSize == 0 {
		result.maxHeaderListSize = uint32(s.ReadBufferSize)
		if result.maxHeaderListSize == 0 {
			result.maxHeaderListSize = 4096
		}
	}
	if result.maxDecoderTableSize == 0 {
		result.maxDecoderTableSize = defaultHeaderTableSize
	}
	if result.maxEncoderTableSize == 0 {
		result.maxEncoderTableSize = defaultHeaderTableSize
	}
	if result.maxReadFrameSize == 0 {
		result.maxReadFrameSize = defaultMaxFrameSize
	}
	if result.maxReadFrameSize < defaultMaxFrameSize || result.maxReadFrameSize > 1<<24-1 {
		return serverConfig{}, errors.New("http2: max read frame size must be between 16384 and 16777215")
	}
	if result.maxQueuedControlFrames == 0 {
		result.maxQueuedControlFrames = defaultMaxQueuedControlFrames
	}
	if result.maxQueuedControlFrames < 1 {
		return serverConfig{}, errors.New("http2: max queued control frames must be positive")
	}
	if result.maxPromisedStreams == 0 {
		result.maxPromisedStreams = 16
	}
	if result.maxPushDepth == 0 {
		result.maxPushDepth = 1
	}
	if result.connectionWindowSize == 0 {
		result.connectionWindowSize = defaultConnectionWindowSize
	}
	if result.connectionWindowSize < 65535 {
		return serverConfig{}, errors.New("http2: connection upload window must be at least 65535")
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
	return result, nil
}

type serverHandler struct {
	config serverConfig
}

// ConfigureServer enables HTTP/2 on s through TLS ALPN and cleartext prior
// knowledge. Existing TLS settings are preserved.
func ConfigureServer(s *fasthttp.Server, cfg ServerConfig) error {
	if s == nil {
		return errors.New("http2: server is nil")
	}
	normalized, err := normalizeServerConfig(s, cfg)
	if err != nil {
		return err
	}
	if s.TLSConfig != nil {
		s.TLSConfig = s.TLSConfig.Clone()
	}
	return s.RegisterProtocol(fasthttp.ProtocolRegistration{
		ALPN:             []string{"h2"},
		CleartextPreface: []byte(clientPreface),
		Handler:          &serverHandler{config: normalized},
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
	normalized, err := normalizeServerConfig(s, cfg)
	if err != nil {
		return err
	}
	return s.ServeProtocolConn(c, &serverHandler{config: normalized})
}

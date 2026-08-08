package fasthttp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// ProtocolRoundTripper is an optional extension to RoundTripper for transports
// that own multiplexed or otherwise protocol-specific connections.
type ProtocolRoundTripper interface {
	RoundTripWithContext(
		ctx *ProtocolClientContext,
		hc *HostClient,
		req *Request,
		resp *Response,
	) (retry bool, err error)
}

// StreamRoundTripper is implemented by transports that can open a
// bidirectional request stream.
type StreamRoundTripper interface {
	OpenStreamWithContext(
		ctx *ProtocolClientContext,
		hc *HostClient,
		req *Request,
		resp *Response,
	) (StreamConn, error)
}

// ProtocolTransportCloser is implemented by protocol transports that retain
// idle connections outside HostClient's HTTP/1 connection pool.
type ProtocolTransportCloser interface {
	CloseIdleConnections(hc *HostClient)
}

// RegisterProtocolTransport installs a protocol transport while preserving
// HostClient.Transport as its fallback. It must be called before the
// HostClient is used.
//
// A transport may additionally implement interface{ MinTLSVersion() uint16 }
// to enforce a minimum TLS version on the connections dialed for it.
func (c *HostClient) RegisterProtocolTransport(transport ProtocolRoundTripper) error {
	if isNilOrTypedNil(transport) {
		return errors.New("fasthttp: protocol transport is nil")
	}
	if c.protocolTransport != nil {
		return errors.New("fasthttp: protocol transport is already registered")
	}
	if atomic.LoadInt32(&c.pendingRequests) != 0 || c.ConnsCount() != 0 {
		return errors.New("fasthttp: cannot register a protocol transport after use")
	}
	c.protocolTransport = transport
	return nil
}

// ProtocolClientContext supplies one request attempt with its deadline and a
// HostClient-aware connection dialer.
type ProtocolClientContext struct {
	hostClient   *HostClient
	deadline     time.Time
	readTimeout  time.Duration
	writeTimeout time.Duration
}

// Deadline returns the absolute request deadline, if one was configured.
func (ctx *ProtocolClientContext) Deadline() (time.Time, bool) {
	return ctx.deadline, !ctx.deadline.IsZero()
}

// ReadTimeout returns the HostClient read timeout for this request attempt.
func (ctx *ProtocolClientContext) ReadTimeout() time.Duration {
	return ctx.readTimeout
}

// WriteTimeout returns the HostClient write timeout for this request attempt.
func (ctx *ProtocolClientContext) WriteTimeout() time.Duration {
	return ctx.writeTimeout
}

// RoundTripHTTP1 executes the request with HostClient's configured HTTP/1
// transport. Protocol transports use it after a host has previously selected
// HTTP/1 with ALPN.
func (ctx *ProtocolClientContext) RoundTripHTTP1(req *Request, resp *Response) (bool, error) {
	return roundTripHTTP1Fallback(ctx.hostClient, req, resp, ctx.deadline)
}

// AcquireConn reserves one of HostClient's physical connection slots and
// dials a connection. nextProtos is advertised with ALPN for TLS clients.
// The caller owns the returned connection until it calls Close or
// RoundTripHTTP1.
func (ctx *ProtocolClientContext) AcquireConn(nextProtos []string) (*ProtocolClientConn, error) {
	timeout := time.Duration(0)
	if !ctx.deadline.IsZero() {
		timeout = time.Until(ctx.deadline)
		if timeout <= 0 {
			return nil, ErrTimeout
		}
	}

	if err := ctx.hostClient.reserveProtocolConn(timeout); err != nil {
		return nil, err
	}
	conn, negotiatedProtocol, err := ctx.hostClient.dialHostHardWithALPN(timeout, nextProtos)
	if err != nil {
		ctx.hostClient.releaseProtocolConnSlot()
		return nil, err
	}

	return &ProtocolClientConn{
		hostClient:         ctx.hostClient,
		conn:               conn,
		negotiatedProtocol: negotiatedProtocol,
		createdTime:        time.Now(),
		deadline:           ctx.deadline,
	}, nil
}

// ProtocolClientConn is a physical connection leased from a HostClient.
type ProtocolClientConn struct {
	hostClient         *HostClient
	conn               net.Conn
	negotiatedProtocol string
	createdTime        time.Time
	deadline           time.Time
	isReleased         atomic.Bool
}

// Conn returns the physical connection.
func (c *ProtocolClientConn) Conn() net.Conn {
	return c.conn
}

// NegotiatedProtocol returns the TLS ALPN result. It is empty for cleartext
// connections.
func (c *ProtocolClientConn) NegotiatedProtocol() string {
	return c.negotiatedProtocol
}

// Close closes the physical connection and releases its HostClient slot.
func (c *ProtocolClientConn) Close() error {
	if !c.isReleased.CompareAndSwap(false, true) {
		return nil
	}
	err := c.conn.Close()
	c.hostClient.releaseProtocolConnSlot()
	return err
}

// RoundTripHTTP1 transfers the connection to HostClient's HTTP/1 transport.
// The ProtocolClientConn must not be used after this method returns.
func (c *ProtocolClientConn) RoundTripHTTP1(req *Request, resp *Response) (bool, error) {
	if !c.isReleased.CompareAndSwap(false, true) {
		return false, errors.New("fasthttp: protocol client connection is already released")
	}
	if builtIn, ok := c.hostClient.transport().(*transport); ok {
		cc := acquireClientConn(c.conn)
		cc.createdTime = c.createdTime
		return builtIn.roundTripConn(c.hostClient, cc, req, resp, c.deadline)
	}
	// A custom transport cannot adopt an already-dialed connection.
	_ = c.conn.Close()
	c.hostClient.releaseProtocolConnSlot()
	return roundTripHTTP1Fallback(c.hostClient, req, resp, c.deadline)
}

// roundTripHTTP1Fallback runs the request on the HTTP/1 transport with the
// deadline's remaining time as the request timeout.
func roundTripHTTP1Fallback(hc *HostClient, req *Request, resp *Response, deadline time.Time) (bool, error) {
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, ErrTimeout
		}
		original := req.timeout
		req.timeout = remaining
		defer func() { req.timeout = original }()
	}
	return hc.transport().RoundTrip(hc, req, resp)
}

var protocolClientContextPool sync.Pool

// acquireProtocolClientContext pools the context, which escapes because it is
// only reached through an interface method. Transports must not retain it.
func (c *HostClient) acquireProtocolClientContext(req *Request) *ProtocolClientContext {
	ctx, _ := protocolClientContextPool.Get().(*ProtocolClientContext)
	if ctx == nil {
		ctx = &ProtocolClientContext{}
	}
	*ctx = ProtocolClientContext{
		hostClient:   c,
		readTimeout:  c.ReadTimeout,
		writeTimeout: c.WriteTimeout,
	}
	if req.timeout > 0 {
		ctx.deadline = time.Now().Add(req.timeout)
	}
	return ctx
}

func releaseProtocolClientContext(ctx *ProtocolClientContext) {
	*ctx = ProtocolClientContext{}
	protocolClientContextPool.Put(ctx)
}

func (c *HostClient) tryReserveConnSlot() bool {
	c.connsLock.Lock()
	defer c.connsLock.Unlock()
	if c.connsCount >= c.maxConnsLocked() {
		return false
	}
	c.connsCount++
	return true
}

func (c *HostClient) reserveProtocolConn(reqTimeout time.Duration) error {
	if c.tryReserveConnSlot() {
		return nil
	}
	if c.MaxConnWaitTimeout <= 0 {
		return ErrNoFreeConns
	}

	timeout := c.MaxConnWaitTimeout
	timeoutErr := ErrNoFreeConns
	if reqTimeout > 0 && reqTimeout < timeout {
		timeout = reqTimeout
		timeoutErr = ErrTimeout
	}
	tc := AcquireTimer(timeout)
	defer ReleaseTimer(tc)

	// Wait in the same FIFO as HTTP/1 requests.
	w := &wantConn{ready: make(chan struct{}, 1), slotOnly: true}
	c.queueForIdle(w)

	select {
	case <-w.ready:
		return w.err
	case <-tc.C:
		w.cancel(c, ErrTimeout)
		return timeoutErr
	}
}

func (c *HostClient) releaseProtocolConnSlot() {
	// Protocol and HTTP/1 connections share connsCount, so freeing a protocol
	// slot must serve queued waiters too.
	c.decConnsCount()
}

func (c *HostClient) dialHostHardWithALPN(
	dialTimeout time.Duration,
	nextProtos []string,
) (net.Conn, string, error) {
	c.addrsLock.Lock()
	n := len(c.addrs)
	c.addrsLock.Unlock()
	if n == 0 {
		n = 1
	}

	timeout := c.ReadTimeout + c.WriteTimeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	if dialTimeout > 0 {
		timeout = min(timeout, dialTimeout)
	}
	deadline := time.Now().Add(timeout)

	var lastErr error
	for range n {
		addr := c.nextAddr()
		var tlsConfig *tls.Config
		if c.IsTLS {
			baseConfig, err := c.cachedTLSConfig(addr)
			if err != nil {
				lastErr = err
				continue
			}
			tlsConfig = baseConfig.Clone()
			tlsConfig.NextProtos = mergeNextProtos(nextProtos, tlsConfig.NextProtos)
			if minVersion := protocolMinTLSVersion(c.protocolTransport); tlsConfig.MinVersion < minVersion {
				tlsConfig.MinVersion = minVersion
			}
		}

		conn, err := dialAddr(
			addr,
			c.Dial,
			c.DialTimeout,
			c.DialDualStack,
			c.IsTLS,
			tlsConfig,
			dialTimeout,
			c.WriteTimeout,
		)
		if err != nil {
			lastErr = err
			if time.Now().After(deadline) {
				break
			}
			continue
		}
		if !c.IsTLS {
			return conn, "", nil
		}

		negotiated, err := finishTLSHandshake(conn, deadline)
		if err != nil {
			_ = conn.Close()
			lastErr = err
			continue
		}
		return conn, negotiated, nil
	}

	return nil, "", fmt.Errorf("dialling protocol connection: %w", lastErr)
}

// finishTLSHandshake completes the handshake on an already-dialed TLS
// connection and returns the negotiated ALPN protocol.
func finishTLSHandshake(conn net.Conn, deadline time.Time) (string, error) {
	tlsConnection, ok := conn.(tlsConn)
	if !ok {
		return "", errors.New("fasthttp: tls dial returned a connection without handshake support")
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if err := tlsConnection.Handshake(); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "", ErrTLSHandshakeTimeout
		}
		return "", err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return "", err
	}
	return tlsConnection.ConnectionState().NegotiatedProtocol, nil
}

// protocolMinTLSVersion reports the minimum TLS version a protocol transport
// requires for its dialed connections, or zero when it doesn't declare one
// through the optional interface{ MinTLSVersion() uint16 } interface.
func protocolMinTLSVersion(transport ProtocolRoundTripper) uint16 {
	if versioner, ok := transport.(interface{ MinTLSVersion() uint16 }); ok {
		return versioner.MinTLSVersion()
	}
	return 0
}

func mergeNextProtos(preferred, existing []string) []string {
	merged := make([]string, 0, len(preferred)+len(existing))
	for _, protocol := range preferred {
		if protocol != "" && !slices.Contains(merged, protocol) {
			merged = append(merged, protocol)
		}
	}
	for _, protocol := range existing {
		if protocol != "" && !slices.Contains(merged, protocol) {
			merged = append(merged, protocol)
		}
	}
	return merged
}

// OpenStream opens a bidirectional request stream using the configured
// transport. resp must be non-nil and remains owned by the caller for the
// stream lifetime.
func (c *HostClient) OpenStream(req *Request, resp *Response) (StreamConn, error) {
	if resp == nil {
		return nil, errors.New("fasthttp: OpenStream response cannot be nil")
	}
	if err := c.prepareRequestResponse(req, resp); err != nil {
		return nil, err
	}

	transport, ok := c.protocolTransport.(StreamRoundTripper)
	if !ok {
		return nil, ErrProtocolNotSupported
	}
	atomic.AddInt32(&c.pendingRequests, 1)
	defer atomic.AddInt32(&c.pendingRequests, -1)
	ctx := c.acquireProtocolClientContext(req)
	stream, err := transport.OpenStreamWithContext(ctx, c, req, resp)
	releaseProtocolClientContext(ctx)
	return stream, err
}

// OpenStream opens a bidirectional request stream using the HostClient selected
// from req's URI.
//
// Per-host clients are created lazily, so the Client.ConfigureClient hook that
// installs a protocol transport on them must be set before the first request.
func (c *Client) OpenStream(req *Request, resp *Response) (StreamConn, error) {
	if req == nil {
		panic("BUG: req cannot be nil")
	}
	if len(req.URI().Host()) == 0 {
		return nil, ErrorInvalidURI
	}
	hc, err := c.hostClientForRequest(req)
	if err != nil {
		return nil, err
	}

	atomic.AddInt32(&hc.pendingClientRequests, 1)
	defer atomic.AddInt32(&hc.pendingClientRequests, -1)
	return hc.OpenStream(req, resp)
}

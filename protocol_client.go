package fasthttp

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
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
	if isNilProtocolTransport(transport) {
		return errors.New("fasthttp: protocol transport is nil")
	}
	if c.protocolTransport != nil {
		return errors.New("fasthttp: protocol transport is already registered")
	}
	http1Transport := c.Transport
	if http1Transport == nil {
		http1Transport = DefaultTransport
	}
	if !isDefaultRoundTripper(http1Transport) {
		return errors.New("fasthttp: protocol transport cannot preserve the configured HTTP/1 transport")
	}
	if atomic.LoadInt32(&c.pendingRequests) != 0 || c.ConnsCount() != 0 {
		return errors.New("fasthttp: cannot register a protocol transport after use")
	}
	c.protocolTransport = transport
	return nil
}

func isDefaultRoundTripper(roundTripper RoundTripper) bool {
	builtIn, ok := roundTripper.(*transport)
	return ok && builtIn == defaultTransport
}

func isNilProtocolTransport(transport ProtocolRoundTripper) bool {
	if transport == nil {
		return true
	}
	value := reflect.ValueOf(transport)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
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
	acquireTimeout := req.timeout
	if !ctx.deadline.IsZero() {
		// The h2 attempt already spent part of the budget.
		acquireTimeout = time.Until(ctx.deadline)
	}
	return defaultTransport.roundTripWithDeadline(ctx.hostClient, req, resp, ctx.deadline, acquireTimeout)
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
		raddr:              conn.RemoteAddr(),
		laddr:              conn.LocalAddr(),
		createdTime:        time.Now(),
		deadline:           ctx.deadline,
	}, nil
}

// ProtocolClientConn is a physical connection leased from a HostClient.
type ProtocolClientConn struct {
	hostClient         *HostClient
	conn               net.Conn
	negotiatedProtocol string
	raddr              net.Addr
	laddr              net.Addr
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

// ApplyResponseMetadata records physical-connection metadata on resp. The
// addresses are captured once at dial time: they never change for a leased
// connection, and a multiplexed transport applies them to every response.
func (c *ProtocolClientConn) ApplyResponseMetadata(resp *Response) {
	resp.raddr = c.raddr
	resp.laddr = c.laddr
}

// PrepareResponseBody ensures resp can buffer at least size body bytes without
// growing its backing buffer. Protocol transports may call it after validating
// a response Content-Length.
func (c *ProtocolClientConn) PrepareResponseBody(resp *Response, size int) {
	if size <= 0 {
		return
	}
	body := resp.bodyBuffer()
	body.B = slices.Grow(body.B, size)
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
	cc := acquireClientConn(c.conn)
	cc.createdTime = c.createdTime
	return defaultTransport.roundTripConn(c.hostClient, cc, req, resp, c.deadline)
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

func (c *HostClient) reserveProtocolConn(reqTimeout time.Duration) error {
	waitTimeout := c.MaxConnWaitTimeout
	if reqTimeout > 0 && (waitTimeout <= 0 || reqTimeout < waitTimeout) {
		waitTimeout = reqTimeout
	}

	var timer *time.Timer
	if c.MaxConnWaitTimeout > 0 {
		timer = AcquireTimer(waitTimeout)
		defer ReleaseTimer(timer)
	}

	for {
		c.connsLock.Lock()
		maxConns := c.MaxConns
		if maxConns <= 0 {
			maxConns = DefaultMaxConnsPerHost
		}
		if c.connsCount < maxConns {
			c.connsCount++
			c.connsLock.Unlock()
			return nil
		}
		if c.MaxConnWaitTimeout <= 0 {
			c.connsLock.Unlock()
			return ErrNoFreeConns
		}
		if c.connSlotAvailable == nil {
			c.connSlotAvailable = make(chan struct{})
		}
		available := c.connSlotAvailable
		c.connsLock.Unlock()

		select {
		case <-available:
		case <-timer.C:
			if reqTimeout > 0 && reqTimeout <= c.MaxConnWaitTimeout {
				return ErrTimeout
			}
			return ErrNoFreeConns
		}
	}
}

func (c *HostClient) releaseProtocolConnSlot() {
	// Protocol and HTTP/1 connections share connsCount, so a freed protocol
	// slot must serve a queued HTTP/1 waiter exactly like a freed HTTP/1
	// connection would; otherwise a waiter under MaxConnWaitTimeout sleeps
	// until timeout even though a slot is available.
	c.decConnsCount()
}

func (c *HostClient) signalConnSlotAvailableLocked() {
	if c.connSlotAvailable == nil {
		return
	}
	close(c.connSlotAvailable)
	c.connSlotAvailable = nil
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
	deadline := time.Now().Add(timeout)
	if dialTimeout > 0 && time.Now().Add(dialTimeout).Before(deadline) {
		deadline = time.Now().Add(dialTimeout)
	}

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

		tlsConnection, ok := conn.(tlsConn)
		if !ok {
			_ = conn.Close()
			return nil, "", errors.New("fasthttp: tls dial returned a connection without handshake support")
		}
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, "", err
		}
		if err := tlsConnection.Handshake(); err != nil {
			_ = conn.Close()
			lastErr = err
			continue
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			_ = conn.Close()
			return nil, "", err
		}
		return conn, tlsConnection.ConnectionState().NegotiatedProtocol, nil
	}

	return nil, "", fmt.Errorf("dialling protocol connection: %w", lastErr)
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
		if protocol != "" && !containsString(merged, protocol) {
			merged = append(merged, protocol)
		}
	}
	for _, protocol := range existing {
		if protocol != "" && !containsString(merged, protocol) {
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
	uri := req.URI()
	host := uri.Host()
	if len(host) == 0 {
		return nil, ErrorInvalidURI
	}
	if bytes.IndexByte(host, ',') >= 0 {
		return nil, fmt.Errorf("invalid host %q: use a host client for multiple hosts", host)
	}

	isTLS := uri.isHTTPS()
	if !isTLS && !uri.isHTTP() {
		return nil, fmt.Errorf("unsupported protocol %q. http and https are supported", uri.Scheme())
	}
	c.mOnce.Do(func() {
		c.m = make(map[string]*HostClient)
		c.ms = make(map[string]*HostClient)
	})
	hc, err := c.hostClient(host, isTLS)
	if err != nil {
		return nil, err
	}

	atomic.AddInt32(&hc.pendingClientRequests, 1)
	defer atomic.AddInt32(&hc.pendingClientRequests, -1)
	return hc.OpenStream(req, resp)
}

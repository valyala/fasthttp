package http2

import (
	"crypto/tls"
	"errors"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

type clientPool struct {
	transport *Transport
	hc        *fasthttp.HostClient
	config    clientConfig

	mu        sync.Mutex
	conns     []*clientConn
	dialing   bool
	h1Only    bool
	notify    chan struct{}
	waiters   int
	available chan struct{}
	closed    bool
}

// Transport is a native fasthttp HTTP/2 client transport. A Transport may be
// shared by multiple HostClient values.
type Transport struct {
	config      ClientConfig
	configError error

	// mu serialises pool creation and removal; lookups go through the
	// lock-free fast path of pools.
	mu    sync.Mutex
	pools sync.Map // map[*fasthttp.HostClient]*clientPool
}

// NewTransport constructs an HTTP/2 transport. ConfigureHostClient or
// HostClient.RegisterProtocolTransport installs it without replacing the
// existing HTTP/1 transport.
func NewTransport(config ClientConfig) *Transport { //nolint:gocritic // config by value is the public contract
	_, err := normalizeClientConfig(nil, &config)
	return &Transport{
		config:      config,
		configError: err,
	}
}

// ConfigureHostClient enables HTTP/2 on hc while preserving the built-in
// HTTP/1 transport as its fallback.
func ConfigureHostClient(hc *fasthttp.HostClient, config ClientConfig) error { //nolint:gocritic // config by value is the public contract
	if hc == nil {
		return errors.New("http2: host client is nil")
	}
	transport := NewTransport(config)
	if transport.configError != nil {
		return transport.configError
	}
	return configureHostClientTransport(hc, transport)
}

// ConfigureClient arranges for every HostClient lazily created by c to use a
// shared HTTP/2 transport. Existing ConfigureClient behavior runs first.
func ConfigureClient(c *fasthttp.Client, config ClientConfig) error { //nolint:gocritic // config by value is the public contract
	if c == nil {
		return errors.New("http2: client is nil")
	}
	transport := NewTransport(config)
	if transport.configError != nil {
		return transport.configError
	}
	previous := c.ConfigureClient
	c.ConfigureClient = func(hc *fasthttp.HostClient) error {
		if previous != nil {
			if err := previous(hc); err != nil {
				return err
			}
		}
		return configureHostClientTransport(hc, transport)
	}
	return nil
}

func configureHostClientTransport(hc *fasthttp.HostClient, transport *Transport) error {
	if transport.config.Mode == PriorKnowledge && hc.IsTLS {
		return errors.New("http2: prior knowledge mode requires a cleartext HostClient")
	}
	if transport.config.Mode == RequireHTTP2 && !hc.IsTLS {
		return errors.New("http2: require HTTP/2 mode requires a TLS HostClient")
	}
	if _, err := normalizeClientConfig(hc, &transport.config); err != nil {
		return err
	}
	return hc.RegisterProtocolTransport(transport)
}

func (t *Transport) poolFor(hc *fasthttp.HostClient) (*clientPool, error) {
	if t.configError != nil {
		return nil, t.configError
	}
	if pool, ok := t.pools.Load(hc); ok {
		return pool.(*clientPool), nil //nolint:forcetypeassert
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if pool, ok := t.pools.Load(hc); ok {
		return pool.(*clientPool), nil //nolint:forcetypeassert
	}
	config, err := normalizeClientConfig(hc, &t.config)
	if err != nil {
		return nil, err
	}
	pool := &clientPool{
		transport: t,
		hc:        hc,
		config:    config,
		notify:    make(chan struct{}),
		available: make(chan struct{}, 1),
	}
	t.pools.Store(hc, pool)
	return pool, nil
}

// RoundTripWithContext implements fasthttp.ProtocolRoundTripper.
func (t *Transport) RoundTripWithContext(
	ctx *fasthttp.ProtocolClientContext,
	hc *fasthttp.HostClient,
	req *fasthttp.Request,
	resp *fasthttp.Response,
) (bool, error) {
	for {
		pool, err := t.poolFor(hc)
		if err != nil {
			return false, err
		}
		if !hc.IsTLS && pool.config.mode != PriorKnowledge {
			return ctx.RoundTripHTTP1(req, resp)
		}
		stream, fallback, err := pool.acquireStream(ctx, req, resp, false)
		if errors.Is(err, errClientPoolClosed) {
			continue
		}
		if err != nil {
			return false, err
		}
		if fallback != nil {
			return fallback.RoundTripHTTP1(req, resp)
		}
		if stream == nil {
			return ctx.RoundTripHTTP1(req, resp)
		}
		return stream.conn.roundTrip(ctx, stream)
	}
}

// OpenStreamWithContext implements fasthttp.StreamRoundTripper.
func (t *Transport) OpenStreamWithContext(
	ctx *fasthttp.ProtocolClientContext,
	hc *fasthttp.HostClient,
	req *fasthttp.Request,
	resp *fasthttp.Response,
) (fasthttp.StreamConn, error) {
	for {
		pool, err := t.poolFor(hc)
		if err != nil {
			return nil, err
		}
		if !pool.config.enableExtendedConnect {
			return nil, fasthttp.ErrProtocolNotSupported
		}
		if !hc.IsTLS && pool.config.mode != PriorKnowledge {
			return nil, fasthttp.ErrProtocolNotSupported
		}
		stream, fallback, err := pool.acquireStream(ctx, req, resp, true)
		if errors.Is(err, errClientPoolClosed) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if fallback != nil {
			_ = fallback.Close()
			return nil, fasthttp.ErrProtocolNotSupported
		}
		if stream == nil {
			return nil, fasthttp.ErrProtocolNotSupported
		}
		result := stream.conn.openStream(ctx, stream)
		return result.streamConn, result.err
	}
}

// MinTLSVersion reports the floor RFC 9113 §9.2 requires for HTTP/2 over TLS.
// The root-package dialer consults it through an optional interface so the
// generic protocol bridge doesn't need to know about h2.
func (t *Transport) MinTLSVersion() uint16 {
	return tls.VersionTLS12
}

// CloseIdleConnections closes idle HTTP/2 connections owned for hc.
func (t *Transport) CloseIdleConnections(hc *fasthttp.HostClient) {
	if pool, ok := t.pools.Load(hc); ok {
		pool.(*clientPool).closeIdle() //nolint:forcetypeassert
		t.removePoolIfEmpty(hc, pool.(*clientPool))
	}
}

func (t *Transport) removePoolIfEmpty(hc *fasthttp.HostClient, pool *clientPool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	current, ok := t.pools.Load(hc)
	if !ok {
		return
	}
	if stored, ok := current.(*clientPool); !ok || stored != pool {
		return
	}
	pool.mu.Lock()
	empty := len(pool.conns) == 0 && !pool.dialing && pool.waiters == 0
	if empty {
		pool.closed = true
	}
	pool.mu.Unlock()
	if empty {
		t.pools.Delete(hc)
	}
}

func (p *clientPool) acquireStream(
	ctx *fasthttp.ProtocolClientContext,
	req *fasthttp.Request,
	resp *fasthttp.Response,
	openStream bool,
) (*clientStream, *fasthttp.ProtocolClientConn, error) {
	deadline, _ := ctx.Deadline()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, errClientPoolClosed
		}
		if p.h1Only {
			p.mu.Unlock()
			return nil, nil, nil
		}
		now := time.Now()
		for _, conn := range p.conns {
			if stream := conn.reserveStream(req, resp, openStream, deadline, now); stream != nil {
				p.mu.Unlock()
				return stream, nil, nil
			}
		}
		if p.dialing {
			notify := p.notify
			p.waiters++
			p.mu.Unlock()
			err := waitForClientEvent(notify, deadline, 0)
			p.mu.Lock()
			p.waiters--
			p.mu.Unlock()
			if err != nil {
				return nil, nil, err
			}
			continue
		}

		maxConns := p.hc.MaxConns
		if maxConns <= 0 {
			maxConns = fasthttp.DefaultMaxConnsPerHost
		}
		if p.hc.ConnsCount() >= maxConns {
			if p.hc.MaxConnWaitTimeout <= 0 {
				p.mu.Unlock()
				return nil, nil, fasthttp.ErrNoFreeConns
			}
			p.mu.Unlock()
			err := waitForClientEvent(p.available, deadline, p.hc.MaxConnWaitTimeout)
			if err != nil {
				return nil, nil, err
			}
			continue
		}
		p.dialing = true
		p.mu.Unlock()

		conn, fallback, err := p.dial(ctx)
		p.mu.Lock()
		p.dialing = false
		if fallback != nil {
			p.h1Only = true
		}
		if conn != nil {
			p.conns = append(p.conns, conn)
		}
		p.signalLocked()
		p.mu.Unlock()
		if conn != nil {
			go conn.readLoop()
		}
		if fallback != nil || err != nil {
			return nil, fallback, err
		}
	}
}

func (p *clientPool) dial(ctx *fasthttp.ProtocolClientContext) (*clientConn, *fasthttp.ProtocolClientConn, error) {
	nextProtos := []string(nil)
	if p.hc.IsTLS {
		nextProtos = []string{"h2", "http/1.1"}
	}
	lease, err := ctx.AcquireConn(nextProtos)
	if err != nil {
		return nil, nil, err
	}
	if p.hc.IsTLS && lease.NegotiatedProtocol() != "h2" {
		if p.config.mode == RequireHTTP2 {
			_ = lease.Close()
			return nil, nil, ErrHTTP2Required
		}
		return nil, lease, nil
	}
	conn, err := newClientConn(p, lease)
	if err != nil {
		_ = lease.Close()
		return nil, nil, err
	}
	return conn, nil, nil
}

func (p *clientPool) signalLocked() {
	if p.waiters == 0 {
		return
	}
	close(p.notify)
	p.notify = make(chan struct{})
}

func (p *clientPool) streamAvailable() {
	select {
	case p.available <- struct{}{}:
	default:
	}
}

func (p *clientPool) remove(conn *clientConn) {
	p.mu.Lock()
	for i, candidate := range p.conns {
		if candidate == conn {
			copy(p.conns[i:], p.conns[i+1:])
			p.conns = p.conns[:len(p.conns)-1]
			break
		}
	}
	p.signalLocked()
	// The freed physical slot also wakes waiters blocked on MaxConns.
	p.streamAvailable()
	p.mu.Unlock()
	p.transport.removePoolIfEmpty(p.hc, p)
}

func (p *clientPool) closeIdle() {
	p.mu.Lock()
	connections := append([]*clientConn(nil), p.conns...)
	p.mu.Unlock()
	for _, conn := range connections {
		conn.closeIfIdle()
	}
}

func waitForClientEvent(notify <-chan struct{}, deadline time.Time, maxWait time.Duration) error {
	wait := maxWait
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fasthttp.ErrTimeout
		}
		if wait <= 0 || remaining < wait {
			wait = remaining
		}
	}
	if wait <= 0 {
		<-notify
		return nil
	}
	timer := fasthttp.AcquireTimer(wait)
	defer fasthttp.ReleaseTimer(timer)
	select {
	case <-notify:
		return nil
	case <-timer.C:
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fasthttp.ErrTimeout
		}
		return fasthttp.ErrNoFreeConns
	}
}

var (
	_ fasthttp.ProtocolRoundTripper       = (*Transport)(nil)
	_ fasthttp.StreamRoundTripper         = (*Transport)(nil)
	_ fasthttp.ProtocolTransportCloser    = (*Transport)(nil)
	_ interface{ MinTLSVersion() uint16 } = (*Transport)(nil)
)

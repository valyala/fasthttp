package http2

import (
	"crypto/tls"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

type clientPool struct {
	transport *Transport
	hc        *fasthttp.HostClient
	config    clientConfig

	// idleSeq numbers the moments connections turned idle; ConnPoolStrategy
	// orders them by it.
	idleSeq atomic.Uint64

	mu      sync.Mutex
	conns   []*clientConn
	dialing bool
	// retiring counts connections closing outside mu; their HostClient slots
	// free once they finish.
	retiring int
	h1Only   bool
	closed   bool
	// closeRequested records a CloseIdleConnections that found the pool in
	// use; the cleanup that empties it then removes it, h1Only included.
	closeRequested bool

	// FIFO of requests waiting for a dial or MaxConns capacity. wakeMu is a
	// leaf lock: connections signal waiters under their own lock.
	wakeMu  sync.Mutex
	queue   []*poolWaiter
	waiters atomic.Int32 // len(queue)
}

type poolWaiter struct {
	ready chan struct{}
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
//
//nolint:gocritic // config by value is the public contract
func ConfigureHostClient(hc *fasthttp.HostClient, config ClientConfig) error {
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

// IdleConnsCount reports the open HTTP/2 connections owned for hc with no
// stream in flight; HostClient.IdleConnsCount adds it to its HTTP/1 count.
func (t *Transport) IdleConnsCount(hc *fasthttp.HostClient) int {
	value, ok := t.pools.Load(hc)
	if !ok {
		return 0
	}
	pool := value.(*clientPool) //nolint:forcetypeassert
	pool.mu.Lock()
	defer pool.mu.Unlock()
	idle := 0
	for _, conn := range pool.conns {
		if conn.isIdle() {
			idle++
		}
	}
	return idle
}

// CloseIdleConnections closes idle HTTP/2 connections owned for hc.
func (t *Transport) CloseIdleConnections(hc *fasthttp.HostClient) {
	if pool, ok := t.pools.Load(hc); ok {
		pool.(*clientPool).closeIdle() //nolint:forcetypeassert
		t.removePoolIfEmpty(hc, pool.(*clientPool), true)
	}
}

// removePoolIfEmpty unregisters pool once nothing uses or waits for it. An
// automatic cleanup keeps a pool that recorded the ALPN fallback to HTTP/1
// for as long as the HostClient lives; an explicit one (CloseIdleConnections)
// removes it, at once or, while the pool is still in use, through the cleanup
// that empties it. Lock order: t.mu, then pool.mu.
func (t *Transport) removePoolIfEmpty(hc *fasthttp.HostClient, pool *clientPool, explicit bool) {
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
	if explicit {
		pool.closeRequested = true
	}
	empty := len(pool.conns) == 0 && !pool.dialing && pool.retiring == 0 && pool.waiters.Load() == 0 &&
		(!pool.h1Only || pool.closeRequested)
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
	var waiter *poolWaiter
	queued := false
	defer func() {
		if queued {
			p.stopWaiting(waiter)
		}
	}()
	var connWaitDeadline time.Time
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
		var expired []*clientConn
		if len(p.conns) == 1 {
			conn := p.conns[0]
			if stream := conn.reserveStream(req, resp, openStream, deadline, now); stream != nil {
				p.mu.Unlock()
				return stream, nil, nil
			}
			if conn.retiredIdle() {
				expired = append(expired, conn)
			}
		} else if len(p.conns) > 1 {
			var chosen *clientConn
			chosen, expired = p.pickConnLocked(now)
			if chosen != nil {
				stream := chosen.reserveStream(req, resp, openStream, deadline, now)
				p.mu.Unlock()
				if stream != nil {
					return stream, nil, nil
				}
				continue // changed since it was picked: scan again
			}
		}
		if len(expired) != 0 {
			// Their slots count against MaxConns until they close, which
			// happens off this goroutine: the request deadline still applies.
			for _, conn := range expired {
				if conn.retireIdleLocked(true) {
					go conn.finishRetire()
				}
			}
			p.mu.Unlock()
			continue
		}

		atLimit := !p.dialing && p.hc.ConnsCount() >= ctx.MaxConns()
		// A retiring connection frees its slot, like a dial in progress.
		waitForSlot := atLimit && p.retiring == 0
		if !p.dialing && !atLimit {
			p.dialing = true
			p.mu.Unlock()
			if queued {
				p.stopWaiting(waiter)
				queued = false
			}
			conn, fallback, err := p.dial(ctx)
			p.mu.Lock()
			p.dialing = false
			if fallback != nil {
				p.h1Only = true
			}
			if conn != nil {
				p.conns = append(p.conns, conn)
			}
			p.mu.Unlock()
			p.capacityChanged()
			if conn != nil {
				go conn.readLoop()
			}
			if err != nil {
				// A pool that holds nothing after its dial failed is gone;
				// the next request registers a new one.
				p.transport.removePoolIfEmpty(p.hc, p, false)
			}
			if fallback != nil || err != nil {
				return nil, fallback, err
			}
			continue
		}
		if waitForSlot && p.hc.MaxConnWaitTimeout <= 0 {
			p.mu.Unlock()
			return nil, nil, fasthttp.ErrNoFreeConns
		}
		if !queued {
			// Queued before the state is checked again, so a change in
			// between can't be missed.
			if waiter == nil {
				waiter = &poolWaiter{ready: make(chan struct{}, 1)}
			}
			p.startWaiting(waiter)
			queued = true
			p.mu.Unlock()
			continue
		}
		p.mu.Unlock()

		maxWait := time.Duration(0) // a dial or a retirement always finishes
		if waitForSlot {
			if connWaitDeadline.IsZero() {
				connWaitDeadline = now.Add(p.hc.MaxConnWaitTimeout)
			}
			if maxWait = time.Until(connWaitDeadline); maxWait <= 0 {
				return nil, nil, fasthttp.ErrNoFreeConns
			}
		}
		if err := waitForClientEvent(waiter.ready, deadline, maxWait); err != nil {
			return nil, nil, err
		}
		// The wakeup dequeued the waiter.
		queued = false
	}
}

// pickConnLocked chooses the connection to reserve on as HTTP/1's idle list
// does: among idle connections FIFO takes the one released earliest and LIFO
// the one released latest; without one, the first created with capacity. It
// also reports the connections found retired and idle. p.mu is held.
func (p *clientPool) pickConnLocked(now time.Time) (chosen *clientConn, expired []*clientConn) {
	lifo := p.hc.ConnPoolStrategy == fasthttp.LIFO
	var chosenSeq uint64
	var active *clientConn
	for _, conn := range p.conns {
		seq, ok := conn.reservable(now)
		switch {
		case !ok:
			if conn.retiredIdle() {
				expired = append(expired, conn)
			}
		case seq == 0:
			if active == nil {
				active = conn
			}
		case chosen == nil || lifo && seq > chosenSeq || !lifo && seq < chosenSeq:
			chosen, chosenSeq = conn, seq
		}
	}
	if chosen == nil {
		chosen = active
	}
	return chosen, expired
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
	if p.hc.IsTLS {
		switch lease.NegotiatedProtocol() {
		case "h2":
		case "", "http/1.1":
			if p.config.mode == RequireHTTP2 {
				_ = lease.Close()
				return nil, nil, ErrHTTP2Required
			}
			return nil, lease, nil
		default:
			// Any other protocol came from the caller's NextProtos, and we don't speak it.
			_ = lease.Close()
			return nil, nil, errUnsupportedProtocol
		}
	}
	conn, err := newClientConn(p, lease)
	if err != nil {
		_ = lease.Close()
		return nil, nil, err
	}
	return conn, nil, nil
}

func (p *clientPool) startWaiting(waiter *poolWaiter) {
	p.wakeMu.Lock()
	p.queue = append(p.queue, waiter)
	p.waiters.Store(int32(len(p.queue))) // #nosec G115
	p.wakeMu.Unlock()
}

// stopWaiting dequeues a waiter that stops waiting. A wakeup it received
// but didn't take passes to the next waiter. The last waiter to leave retries
// the cleanup a failed dial couldn't finish while requests were queued.
func (p *clientPool) stopWaiting(waiter *poolWaiter) {
	p.wakeMu.Lock()
	if i := slices.Index(p.queue, waiter); i >= 0 {
		p.queue = slices.Delete(p.queue, i, i+1)
		p.waiters.Store(int32(len(p.queue))) // #nosec G115
	} else {
		// Dequeued by a wakeup it didn't take.
		select {
		case <-waiter.ready:
		default:
		}
		p.wakeLocked(1)
	}
	last := len(p.queue) == 0
	p.wakeMu.Unlock()
	if last {
		p.transport.removePoolIfEmpty(p.hc, p, false)
	}
}

func (p *clientPool) wakeLocked(n int) {
	n = min(n, len(p.queue))
	for _, waiter := range p.queue[:n] {
		waiter.ready <- struct{}{}
	}
	p.queue = slices.Delete(p.queue, 0, n)
	p.waiters.Store(int32(len(p.queue))) // #nosec G115
}

// streamFreed wakes the first waiter: a stream finished.
func (p *clientPool) streamFreed() {
	if p.waiters.Load() == 0 {
		return
	}
	p.wakeMu.Lock()
	p.wakeLocked(1)
	p.wakeMu.Unlock()
}

// capacityChanged wakes every waiter: a connection came or went, or a peer
// changed its stream limit.
func (p *clientPool) capacityChanged() {
	if p.waiters.Load() == 0 {
		return
	}
	p.wakeMu.Lock()
	p.wakeLocked(len(p.queue))
	p.wakeMu.Unlock()
}

// retireLocked takes conn out of the pool while it still holds its slot;
// retired follows once the slot is released. p.mu is held.
func (p *clientPool) retireLocked(conn *clientConn) {
	if i := slices.Index(p.conns, conn); i >= 0 {
		p.conns = slices.Delete(p.conns, i, i+1)
	}
	p.retiring++
}

func (p *clientPool) retired() {
	p.mu.Lock()
	p.retiring--
	p.mu.Unlock()
	// Also wakes waiters blocked on MaxConns: the physical slot is free.
	p.capacityChanged()
	p.transport.removePoolIfEmpty(p.hc, p, false)
}

func (p *clientPool) closeIdle() {
	p.mu.Lock()
	connections := append([]*clientConn(nil), p.conns...)
	p.mu.Unlock()
	for _, conn := range connections {
		conn.cancelUnansweredPushes()
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
	_ interface {
		IdleConnsCount(*fasthttp.HostClient) int
	} = (*Transport)(nil)
)

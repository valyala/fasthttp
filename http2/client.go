package http2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	errHTTP2Required        = errors.New("http2: server didn't negotiate h2")
	errClientConnClosed     = errors.New("http2: client connection closed")
	errClientStreamClosed   = errors.New("http2: client stream closed")
	errResponseBodyTooLarge = errors.New("http2: response body too large")
)

type clientResult struct {
	streamConn fasthttp.StreamConn
	retry      bool
	err        error
}

type clientStream struct {
	id   uint32
	conn *clientConn
	req  *fasthttp.Request
	resp *fasthttp.Response

	result     chan clientResult
	done       chan struct{}
	resultSent bool
	doneClosed bool
	timer      *time.Timer

	requestStarted bool
	requestBytes   int64
	localClosed    bool
	remoteClosed   bool
	responseHeader bool
	bodyDone       bool
	isOpenStream   bool
	isStreaming    bool

	sendWindow int64
	recvWindow int64

	statusCode            int
	expectedResponseBytes int64
	responseBytes         int64
	maxResponseBodySize   int
	responseBody          *responseBody
	err                   error
	isPush                bool
	pushComplete          bool
	promisedRequest       *fasthttp.Request
}

type pendingPushBlock struct {
	parentID   uint32
	promisedID uint32
	block      []byte
}

type clientConn struct {
	pool    *clientPool
	hc      *fasthttp.HostClient
	config  clientConfig
	lease   *fasthttp.ProtocolClientConn
	conn    net.Conn
	framer  *xhttp2.Framer
	decoder *hpack.Decoder

	writeMu      sync.Mutex
	encoder      *hpack.Encoder
	headerBuffer bytes.Buffer

	mu                       sync.Mutex
	streams                  map[uint32]*clientStream
	nextStreamID             uint32
	nextHeaderStreamID       uint32
	activeStreams            uint32
	peerMaxConcurrentStreams uint32
	peerInitialStreamWindow  int64
	peerConnectionWindow     int64
	peerMaxFrameSize         int
	peerMaxHeaderListSize    uint64
	receiveConnectionWindow  int64
	receivedSettings         bool
	goAway                   bool
	goAwayLastStreamID       uint32
	closed                   bool
	err                      error
	notify                   chan struct{}
	created                  time.Time
	lastIdle                 time.Time
	idleTimer                *time.Timer
	lastPromisedStreamID     uint32
	pendingPush              *pendingPushBlock
}

type clientPool struct {
	transport *Transport
	hc        *fasthttp.HostClient
	config    clientConfig

	mu      sync.Mutex
	conns   []*clientConn
	dialing bool
	h1Only  bool
	notify  chan struct{}
	closed  bool
}

// Transport is a native fasthttp HTTP/2 client transport. A Transport may be
// shared by multiple HostClient values.
type Transport struct {
	config      ClientConfig
	configError error

	mu    sync.Mutex
	pools map[*fasthttp.HostClient]*clientPool
}

// NewTransport constructs an HTTP/2 transport. ConfigureHostClient or
// HostClient.RegisterProtocolTransport installs it without replacing the
// existing HTTP/1 transport.
func NewTransport(config ClientConfig) *Transport {
	_, err := normalizeClientConfig(nil, config)
	return &Transport{
		config:      config,
		configError: err,
		pools:       make(map[*fasthttp.HostClient]*clientPool),
	}
}

// ConfigureHostClient enables HTTP/2 on hc while preserving the built-in
// HTTP/1 transport as its fallback.
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
func ConfigureClient(c *fasthttp.Client, config ClientConfig) error {
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
	if hc.Transport != nil {
		return errors.New("http2: cannot safely preserve a custom HostClient transport as an HTTP/1 fallback")
	}
	if transport.config.Mode == PriorKnowledge && hc.IsTLS {
		return errors.New("http2: prior knowledge mode requires a cleartext HostClient")
	}
	if transport.config.Mode == RequireHTTP2 && !hc.IsTLS {
		return errors.New("http2: require HTTP/2 mode requires a TLS HostClient")
	}
	if _, err := normalizeClientConfig(hc, transport.config); err != nil {
		return err
	}
	return hc.RegisterProtocolTransport(transport)
}

func (t *Transport) poolFor(hc *fasthttp.HostClient) (*clientPool, error) {
	if t.configError != nil {
		return nil, t.configError
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if pool := t.pools[hc]; pool != nil {
		return pool, nil
	}
	config, err := normalizeClientConfig(hc, t.config)
	if err != nil {
		return nil, err
	}
	pool := &clientPool{
		transport: t,
		hc:        hc,
		config:    config,
		notify:    make(chan struct{}),
	}
	t.pools[hc] = pool
	return pool, nil
}

// RoundTripWithContext implements fasthttp.ProtocolRoundTripper.
func (t *Transport) RoundTripWithContext(
	ctx *fasthttp.ProtocolClientContext,
	hc *fasthttp.HostClient,
	req *fasthttp.Request,
	resp *fasthttp.Response,
) (bool, error) {
	pool, err := t.poolFor(hc)
	if err != nil {
		return false, err
	}
	if !hc.IsTLS && pool.config.mode != PriorKnowledge {
		return ctx.RoundTripHTTP1(req, resp)
	}
	stream, fallback, err := pool.acquireStream(ctx, req, resp, false)
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

// OpenStreamWithContext implements fasthttp.StreamRoundTripper.
func (t *Transport) OpenStreamWithContext(
	ctx *fasthttp.ProtocolClientContext,
	hc *fasthttp.HostClient,
	req *fasthttp.Request,
	resp *fasthttp.Response,
) (fasthttp.StreamConn, error) {
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

// CloseIdleConnections closes idle HTTP/2 connections owned for hc.
func (t *Transport) CloseIdleConnections(hc *fasthttp.HostClient) {
	t.mu.Lock()
	pool := t.pools[hc]
	t.mu.Unlock()
	if pool != nil {
		pool.closeIdle()
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
			return nil, nil, errClientConnClosed
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
			p.mu.Unlock()
			if err := waitForClientEvent(notify, deadline, 0); err != nil {
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
			notify := p.notify
			p.mu.Unlock()
			if err := waitForClientEvent(notify, deadline, p.hc.MaxConnWaitTimeout); err != nil {
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
			return nil, nil, errHTTP2Required
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
	close(p.notify)
	p.notify = make(chan struct{})
}

func (p *clientPool) streamAvailable() {
	p.mu.Lock()
	p.signalLocked()
	p.mu.Unlock()
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
	p.mu.Unlock()
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
	timer := time.NewTimer(wait)
	defer timer.Stop()
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

func newClientConn(pool *clientPool, lease *fasthttp.ProtocolClientConn) (*clientConn, error) {
	conn := &clientConn{
		pool:                     pool,
		hc:                       pool.hc,
		config:                   pool.config,
		lease:                    lease,
		conn:                     lease.Conn(),
		streams:                  make(map[uint32]*clientStream),
		nextStreamID:             1,
		nextHeaderStreamID:       1,
		peerMaxConcurrentStreams: 100,
		peerInitialStreamWindow:  65535,
		peerConnectionWindow:     65535,
		peerMaxFrameSize:         defaultMaxFrameSize,
		peerMaxHeaderListSize:    math.MaxUint32,
		receiveConnectionWindow:  int64(pool.config.connectionWindowSize),
		notify:                   make(chan struct{}),
		created:                  time.Now(),
	}
	conn.framer = xhttp2.NewFramer(conn.conn, conn.conn)
	conn.decoder = hpack.NewDecoder(pool.config.maxDecoderTableSize, nil)
	conn.decoder.SetAllowedMaxDynamicTableSize(pool.config.maxDecoderTableSize)
	conn.decoder.SetMaxStringLength(int(pool.config.maxHeaderListSize))
	conn.framer.ReadMetaHeaders = conn.decoder
	conn.framer.MaxHeaderListSize = pool.config.maxHeaderListSize
	conn.framer.SetMaxReadFrameSize(pool.config.maxReadFrameSize)
	conn.encoder = hpack.NewEncoder(&conn.headerBuffer)
	conn.encoder.SetMaxDynamicTableSizeLimit(pool.config.maxEncoderTableSize)
	if err := conn.writePrefaceAndSettings(); err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *clientConn) writePrefaceAndSettings() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := io.WriteString(c.conn, clientPreface); err != nil {
		return err
	}
	settings := []xhttp2.Setting{
		{ID: xhttp2.SettingEnablePush, Val: boolSetting(c.config.pushHandler != nil)},
		{ID: xhttp2.SettingMaxConcurrentStreams, Val: c.config.maxConcurrentStreams},
		{ID: xhttp2.SettingInitialWindowSize, Val: uint32(c.config.streamWindowSize)},
		{ID: xhttp2.SettingMaxFrameSize, Val: c.config.maxReadFrameSize},
		{ID: xhttp2.SettingMaxHeaderListSize, Val: c.config.maxHeaderListSize},
		{ID: xhttp2.SettingHeaderTableSize, Val: c.config.maxDecoderTableSize},
		{ID: xhttp2.SettingNoRFC7540Priorities, Val: 1},
	}
	if c.config.enableExtendedConnect {
		settings = append(settings, xhttp2.Setting{ID: xhttp2.SettingEnableConnectProtocol, Val: 1})
	}
	if err := c.framer.WriteSettings(settings...); err != nil {
		return err
	}
	increment := uint32(c.config.connectionWindowSize - 65535)
	if increment != 0 {
		return c.framer.WriteWindowUpdate(0, increment)
	}
	return nil
}

func boolSetting(value bool) uint32 {
	if value {
		return 1
	}
	return 0
}

func (c *clientConn) reserveStream(
	req *fasthttp.Request,
	resp *fasthttp.Response,
	openStream bool,
	deadline time.Time,
	now time.Time,
) *clientStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.goAway || c.nextStreamID > math.MaxInt32 || c.activeStreams >= c.peerMaxConcurrentStreams {
		return nil
	}
	if c.hc.MaxConnDuration > 0 && now.Sub(c.created) >= c.hc.MaxConnDuration {
		return nil
	}
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
	stream := &clientStream{
		id:                    c.nextStreamID,
		conn:                  c,
		req:                   req,
		resp:                  resp,
		result:                make(chan clientResult, 1),
		done:                  make(chan struct{}),
		isOpenStream:          openStream,
		isStreaming:           openStream || resp.StreamBody,
		sendWindow:            c.peerInitialStreamWindow,
		recvWindow:            int64(c.config.streamWindowSize),
		expectedResponseBytes: -1,
		maxResponseBodySize:   c.hc.MaxResponseBodySize,
	}
	c.streams[stream.id] = stream
	c.nextStreamID += 2
	c.activeStreams++
	if !deadline.IsZero() {
		stream.timer = time.AfterFunc(time.Until(deadline), func() {
			c.resetStream(stream.id, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, false)
		})
	}
	return stream
}

func (c *clientConn) roundTrip(
	ctx *fasthttp.ProtocolClientContext,
	stream *clientStream,
) (bool, error) {
	deadline, _ := ctx.Deadline()
	if err := c.writeRequest(stream, false, deadline); err != nil {
		c.resetStream(stream.id, xhttp2.ErrCodeCancel, err, false)
	}
	result := c.waitResult(stream, deadline)
	return result.retry, result.err
}

func (c *clientConn) openStream(
	ctx *fasthttp.ProtocolClientContext,
	stream *clientStream,
) clientResult {
	deadline, _ := ctx.Deadline()
	if len(stream.req.Header.ConnectProtocol()) == 0 || string(stream.req.Header.Method()) != fasthttp.MethodConnect {
		c.resetStream(stream.id, xhttp2.ErrCodeCancel, errors.New("http2: OpenStream requires an extended CONNECT request"), false)
		return c.waitResult(stream, deadline)
	}
	if stream.req.IsBodyStream() || len(stream.req.Body()) != 0 {
		c.resetStream(stream.id, xhttp2.ErrCodeCancel, errors.New("http2: OpenStream request must not have a body"), false)
		return c.waitResult(stream, deadline)
	}
	if err := c.writeRequest(stream, true, deadline); err != nil {
		c.resetStream(stream.id, xhttp2.ErrCodeCancel, err, false)
	}
	return c.waitResult(stream, deadline)
}

func (c *clientConn) waitResult(stream *clientStream, deadline time.Time) clientResult {
	if deadline.IsZero() {
		return <-stream.result
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case result := <-stream.result:
		return result
	case <-timer.C:
		c.resetStream(stream.id, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, false)
		return <-stream.result
	}
}

func (c *clientConn) writeRequest(stream *clientStream, keepOpen bool, deadline time.Time) error {
	req := stream.req
	var body []byte
	var reader io.Reader
	if req.IsBodyStream() {
		reader = req.BodyStream()
	} else {
		body = req.Body()
	}
	hasBody := reader != nil || len(body) != 0
	if declared := requestContentLength(&req.Header); declared >= 0 && !req.IsBodyStream() && declared != int64(len(body)) {
		return errors.New("http2: request body length doesn't match content-length")
	}
	if err := c.writeRequestHeaders(stream, !keepOpen && !hasBody, req, deadline); err != nil {
		c.fail(err)
		return err
	}
	c.mu.Lock()
	if current := c.streams[stream.id]; current == stream {
		if !keepOpen && !hasBody {
			stream.localClosed = true
			c.maybeFinishStreamLocked(stream)
		}
	}
	c.mu.Unlock()
	if keepOpen || !hasBody {
		return nil
	}
	if reader == nil {
		return c.sendData(stream, body, true, deadline)
	}
	defer req.CloseBodyStream() //nolint:errcheck
	return c.sendRequestStream(stream, reader, requestContentLength(&req.Header), deadline)
}

func requestContentLength(header *fasthttp.RequestHeader) int64 {
	if len(header.Peek(fasthttp.HeaderContentLength)) == 0 {
		return -1
	}
	return int64(header.ContentLength())
}

func (c *clientConn) writeRequestHeaders(
	stream *clientStream,
	endStream bool,
	req *fasthttp.Request,
	deadline time.Time,
) error {
	for {
		c.mu.Lock()
		if c.streams[stream.id] != stream || stream.err != nil {
			err := stream.err
			if err == nil {
				err = errClientStreamClosed
			}
			c.mu.Unlock()
			return err
		}
		if c.nextHeaderStreamID == stream.id {
			break
		}
		notify := c.notify
		done := stream.done
		c.mu.Unlock()
		if err := waitForStreamEvent(notify, done, deadline); err != nil {
			return err
		}
	}
	maxHeaderListSize := c.peerMaxHeaderListSize
	maxFrameSize := c.peerMaxFrameSize
	c.mu.Unlock()

	c.writeMu.Lock()
	block, err := encodeRequestHeaders(
		c.encoder,
		&c.headerBuffer,
		req,
		maxHeaderListSize,
		c.config.enableExtendedConnect,
	)
	if err != nil {
		c.writeMu.Unlock()
		c.advanceHeaderStream(stream, false)
		return err
	}
	first := min(len(block), maxFrameSize)
	if err := c.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      stream.id,
		BlockFragment: block[:first],
		EndStream:     endStream,
		EndHeaders:    first == len(block),
	}); err != nil {
		c.writeMu.Unlock()
		c.advanceHeaderStream(stream, false)
		return err
	}
	block = block[first:]
	for len(block) != 0 {
		length := min(len(block), maxFrameSize)
		if err := c.framer.WriteContinuation(stream.id, length == len(block), block[:length]); err != nil {
			c.writeMu.Unlock()
			c.advanceHeaderStream(stream, false)
			return err
		}
		block = block[length:]
	}
	c.writeMu.Unlock()
	c.advanceHeaderStream(stream, true)
	return nil
}

func (c *clientConn) advanceHeaderStream(stream *clientStream, started bool) {
	c.mu.Lock()
	if c.streams[stream.id] == stream && started {
		stream.requestStarted = true
	}
	if c.nextHeaderStreamID == stream.id {
		c.nextHeaderStreamID += 2
		c.skipUnavailableHeaderStreamsLocked()
	}
	c.signalLocked()
	c.mu.Unlock()
}

func (c *clientConn) skipUnavailableHeaderStreamsLocked() {
	for c.nextHeaderStreamID < c.nextStreamID {
		stream := c.streams[c.nextHeaderStreamID]
		if stream != nil && !stream.requestStarted {
			return
		}
		c.nextHeaderStreamID += 2
	}
}

func (c *clientConn) sendRequestStream(
	stream *clientStream,
	reader io.Reader,
	expected int64,
	deadline time.Time,
) error {
	buffer := make([]byte, defaultMaxFrameSize)
	var sent int64
	for {
		c.mu.Lock()
		responseStarted := stream.responseHeader || stream.remoteClosed
		c.mu.Unlock()
		if responseStarted {
			return c.sendData(stream, nil, true, deadline)
		}
		n, readErr := reader.Read(buffer)
		if n > 0 {
			sent += int64(n)
			if expected >= 0 && sent > expected {
				return errors.New("http2: request body exceeds content-length")
			}
			end := errors.Is(readErr, io.EOF)
			if end && expected >= 0 && sent != expected {
				return errors.New("http2: request body length doesn't match content-length")
			}
			if err := c.sendData(stream, buffer[:n], end, deadline); err != nil {
				return err
			}
			if end {
				return nil
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			if expected >= 0 && sent != expected {
				return errors.New("http2: request body length doesn't match content-length")
			}
			return c.sendData(stream, nil, true, deadline)
		}
	}
}

func (c *clientConn) sendData(stream *clientStream, data []byte, endStream bool, deadline time.Time) error {
	for len(data) != 0 || endStream {
		c.mu.Lock()
		current := c.streams[stream.id]
		if current != stream || stream.err != nil {
			err := stream.err
			if err == nil {
				err = errClientStreamClosed
			}
			c.mu.Unlock()
			return err
		}
		if stream.localClosed {
			c.mu.Unlock()
			return errClientStreamClosed
		}
		amount := 0
		if len(data) != 0 && c.peerConnectionWindow > 0 && stream.sendWindow > 0 {
			amount = min(len(data), c.peerMaxFrameSize, int(c.peerConnectionWindow), int(stream.sendWindow))
		}
		if len(data) != 0 && amount == 0 {
			notify := c.notify
			done := stream.done
			c.mu.Unlock()
			if err := waitForStreamEvent(notify, done, deadline); err != nil {
				return err
			}
			continue
		}
		last := amount == len(data) && endStream
		c.peerConnectionWindow -= int64(amount)
		stream.sendWindow -= int64(amount)
		stream.requestBytes += int64(amount)
		if last {
			stream.localClosed = true
		}
		c.mu.Unlock()

		c.writeMu.Lock()
		err := c.framer.WriteData(stream.id, last, data[:amount])
		c.writeMu.Unlock()
		if err != nil {
			c.fail(err)
			return err
		}
		data = data[amount:]
		if last {
			c.mu.Lock()
			c.maybeFinishStreamLocked(stream)
			c.mu.Unlock()
			return nil
		}
	}
	return nil
}

func waitForStreamEvent(notify, done <-chan struct{}, deadline time.Time) error {
	if deadline.IsZero() {
		select {
		case <-notify:
			return nil
		case <-done:
			return errClientStreamClosed
		}
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-notify:
		return nil
	case <-done:
		return errClientStreamClosed
	case <-timer.C:
		return fasthttp.ErrTimeout
	}
}

func (c *clientConn) readLoop() {
	for {
		frame, err := c.framer.ReadFrame()
		if err != nil {
			c.fail(err)
			return
		}
		if err := c.processFrame(frame); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *clientConn) processFrame(frame xhttp2.Frame) error {
	c.mu.Lock()
	firstFrame := !c.receivedSettings
	c.mu.Unlock()
	if firstFrame {
		settings, ok := frame.(*xhttp2.SettingsFrame)
		if !ok || settings.IsAck() {
			return errors.New("http2: server's first frame isn't settings")
		}
	}
	switch frame := frame.(type) {
	case *xhttp2.SettingsFrame:
		return c.processSettings(frame)
	case *xhttp2.MetaHeadersFrame:
		return c.processResponseHeaders(frame)
	case *xhttp2.DataFrame:
		return c.processResponseData(frame)
	case *xhttp2.RSTStreamFrame:
		retry := frame.ErrCode == xhttp2.ErrCodeRefusedStream
		c.failStream(frame.StreamID, fmt.Errorf("http2: peer reset stream: %s", frame.ErrCode), retry)
		return nil
	case *xhttp2.WindowUpdateFrame:
		return c.processWindowUpdate(frame)
	case *xhttp2.PingFrame:
		if !frame.IsAck() {
			return c.writeControl(func() error { return c.framer.WritePing(true, frame.Data) })
		}
		return nil
	case *xhttp2.GoAwayFrame:
		c.processGoAway(frame)
		return nil
	case *xhttp2.PushPromiseFrame:
		return c.processPushPromise(frame)
	case *xhttp2.ContinuationFrame:
		return c.processPushContinuation(frame)
	default:
		return nil
	}
}

func (c *clientConn) processSettings(frame *xhttp2.SettingsFrame) error {
	if frame.IsAck() {
		return nil
	}
	var encoderTableSize uint32
	var updateEncoderTable bool
	c.mu.Lock()
	for i := range frame.NumSettings() {
		setting := frame.Setting(i)
		switch setting.ID {
		case xhttp2.SettingHeaderTableSize:
			value := setting.Val
			if value > c.config.maxEncoderTableSize {
				value = c.config.maxEncoderTableSize
			}
			encoderTableSize = value
			updateEncoderTable = true
		case xhttp2.SettingMaxConcurrentStreams:
			c.peerMaxConcurrentStreams = setting.Val
		case xhttp2.SettingInitialWindowSize:
			delta := int64(setting.Val) - c.peerInitialStreamWindow
			for _, stream := range c.streams {
				stream.sendWindow += delta
				if stream.sendWindow > math.MaxInt32 {
					c.mu.Unlock()
					return errors.New("http2: stream send window overflow")
				}
			}
			c.peerInitialStreamWindow = int64(setting.Val)
		case xhttp2.SettingMaxFrameSize:
			c.peerMaxFrameSize = int(setting.Val)
		case xhttp2.SettingMaxHeaderListSize:
			c.peerMaxHeaderListSize = uint64(setting.Val)
		case xhttp2.SettingEnablePush:
			c.mu.Unlock()
			return errors.New("http2: server sent SETTINGS_ENABLE_PUSH")
		}
	}
	c.receivedSettings = true
	c.signalLocked()
	c.mu.Unlock()
	c.writeMu.Lock()
	if updateEncoderTable {
		c.encoder.SetMaxDynamicTableSize(encoderTableSize)
	}
	err := c.framer.WriteSettingsAck()
	c.writeMu.Unlock()
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *clientConn) processResponseHeaders(frame *xhttp2.MetaHeadersFrame) error {
	if frame.Truncated {
		c.resetStream(frame.StreamID, xhttp2.ErrCodeEnhanceYourCalm, errInvalidResponseHeaders, false)
		return nil
	}
	c.mu.Lock()
	stream := c.streams[frame.StreamID]
	if stream == nil {
		c.mu.Unlock()
		return nil
	}
	if stream.responseHeader {
		if stream.remoteClosed || !frame.StreamEnded() {
			c.mu.Unlock()
			return errors.New("http2: invalid response trailers")
		}
		if err := populateResponseTrailers(stream.resp, frame.Fields); err != nil {
			c.mu.Unlock()
			return err
		}
		c.endResponseLocked(stream, nil)
		c.mu.Unlock()
		return nil
	}
	status, err := responseStatus(frame.Fields)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if status >= 100 && status < 200 {
		if status == 101 || frame.StreamEnded() {
			c.mu.Unlock()
			return errors.New("http2: invalid informational response")
		}
		c.mu.Unlock()
		return nil
	}
	status, contentLength, err := populateResponse(
		stream.resp,
		frame.Fields,
		c.hc.DisableHeaderNamesNormalizing,
	)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	stream.statusCode = status
	stream.expectedResponseBytes = contentLength
	stream.responseHeader = true
	c.lease.ApplyResponseMetadata(stream.resp)
	if responseHasNoBody(stream) && contentLength > 0 && !stream.req.Header.IsHead() {
		c.mu.Unlock()
		return errors.New("http2: body-forbidden response has a positive content-length")
	}
	if stream.isStreaming {
		body := newResponseBody(
			func(consumed int) { c.consumeResponseBytes(stream.id, consumed) },
			func(discarded int) { c.closeResponseBody(stream.id, discarded) },
			func() { c.responseBodyDone(stream.id) },
		)
		stream.responseBody = body
		if !stream.isOpenStream {
			bodySize := -1
			if contentLength >= 0 {
				bodySize = int(contentLength)
			}
			stream.resp.SetBodyStream(body, bodySize)
			stream.resp.Header.Del(fasthttp.HeaderTransferEncoding)
		}
	}
	if stream.isOpenStream {
		if status < 200 || status >= 300 {
			c.sendResultLocked(stream, clientResult{err: fmt.Errorf("http2: extended connect failed with status %d", status)})
		} else {
			streamConn := &clientStreamConn{stream: stream, read: stream.responseBody}
			c.sendResultLocked(stream, clientResult{streamConn: streamConn})
		}
	} else if stream.isStreaming {
		c.sendResultLocked(stream, clientResult{})
	}
	if frame.StreamEnded() {
		c.endResponseLocked(stream, nil)
	}
	c.mu.Unlock()
	return nil
}

func responseStatus(fields []hpack.HeaderField) (int, error) {
	for _, field := range fields {
		if field.Name == ":status" {
			if len(field.Value) != 3 {
				return 0, errInvalidResponseHeaders
			}
			value, err := strconv.Atoi(field.Value)
			if err != nil {
				return 0, errInvalidResponseHeaders
			}
			return value, nil
		}
	}
	return 0, errInvalidResponseHeaders
}

func responseHasNoBody(stream *clientStream) bool {
	return stream.req.Header.IsHead() || stream.statusCode == 204 || stream.statusCode == 304
}

func (c *clientConn) processResponseData(frame *xhttp2.DataFrame) error {
	flowLength := int64(frame.Header().Length)
	data := frame.Data()
	c.mu.Lock()
	stream := c.streams[frame.StreamID]
	if stream == nil || !stream.responseHeader || stream.remoteClosed {
		c.mu.Unlock()
		return errors.New("http2: data on an invalid response stream")
	}
	c.receiveConnectionWindow -= flowLength
	stream.recvWindow -= flowLength
	if c.receiveConnectionWindow < 0 || stream.recvWindow < 0 {
		c.mu.Unlock()
		return errors.New("http2: response flow-control window exceeded")
	}
	if responseHasNoBody(stream) && len(data) != 0 {
		c.mu.Unlock()
		return errors.New("http2: response body isn't permitted")
	}
	stream.responseBytes += int64(len(data))
	if stream.expectedResponseBytes >= 0 && stream.responseBytes > stream.expectedResponseBytes {
		c.mu.Unlock()
		c.resetStream(frame.StreamID, xhttp2.ErrCodeProtocol, errors.New("http2: response body exceeds content-length"), false)
		return nil
	}
	if stream.maxResponseBodySize > 0 && stream.responseBytes > int64(stream.maxResponseBodySize) {
		c.mu.Unlock()
		c.resetStream(frame.StreamID, xhttp2.ErrCodeCancel, errResponseBodyTooLarge, false)
		return nil
	}
	body := stream.responseBody
	if body == nil {
		stream.resp.AppendBody(data)
	}
	ended := frame.StreamEnded()
	c.mu.Unlock()

	if body != nil && len(data) != 0 {
		if err := body.write(data); err != nil {
			c.closeResponseBody(frame.StreamID, len(data))
			return nil
		}
	} else if len(data) != 0 {
		c.consumeResponseBytes(frame.StreamID, len(data))
	}
	padding := int(flowLength) - len(data)
	if padding > 0 {
		c.consumeResponseBytes(frame.StreamID, padding)
	}
	if ended {
		c.mu.Lock()
		if current := c.streams[frame.StreamID]; current == stream {
			c.endResponseLocked(stream, nil)
		}
		c.mu.Unlock()
	}
	return nil
}

func (c *clientConn) endResponseLocked(stream *clientStream, responseErr error) {
	if stream.expectedResponseBytes >= 0 && stream.responseBytes != stream.expectedResponseBytes {
		responseErr = errors.New("http2: response body length doesn't match content-length")
	}
	stream.remoteClosed = true
	if stream.responseBody != nil {
		stream.responseBody.closeWithError(responseErr)
	}
	if responseErr != nil {
		stream.err = responseErr
		if !stream.isPush {
			c.sendResultLocked(stream, clientResult{err: responseErr})
		}
	} else if stream.isPush {
		stream.pushComplete = true
	} else if !stream.isStreaming {
		c.sendResultLocked(stream, clientResult{})
	}
	c.maybeFinishStreamLocked(stream)
}

func (c *clientConn) consumeResponseBytes(streamID uint32, amount int) {
	if amount <= 0 {
		return
	}
	c.mu.Lock()
	stream := c.streams[streamID]
	c.receiveConnectionWindow += int64(amount)
	if stream != nil {
		stream.recvWindow += int64(amount)
	}
	c.mu.Unlock()
	_ = c.writeControl(func() error {
		if err := c.framer.WriteWindowUpdate(0, uint32(amount)); err != nil {
			return err
		}
		if stream != nil {
			return c.framer.WriteWindowUpdate(streamID, uint32(amount))
		}
		return nil
	})
}

func (c *clientConn) closeResponseBody(streamID uint32, discarded int) {
	if discarded > 0 {
		c.consumeResponseBytes(streamID, discarded)
	}
	c.mu.Lock()
	stream := c.streams[streamID]
	remoteOpen := stream != nil && !stream.remoteClosed
	c.mu.Unlock()
	if remoteOpen {
		c.resetStream(streamID, xhttp2.ErrCodeCancel, errClientStreamClosed, false)
	}
}

func (c *clientConn) responseBodyDone(streamID uint32) {
	c.mu.Lock()
	if stream := c.streams[streamID]; stream != nil {
		stream.bodyDone = true
		c.maybeFinishStreamLocked(stream)
	}
	c.mu.Unlock()
}

func (c *clientConn) processWindowUpdate(frame *xhttp2.WindowUpdateFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if frame.StreamID == 0 {
		if c.peerConnectionWindow+int64(frame.Increment) > math.MaxInt32 {
			return errors.New("http2: connection send window overflow")
		}
		c.peerConnectionWindow += int64(frame.Increment)
		c.signalLocked()
		return nil
	}
	if stream := c.streams[frame.StreamID]; stream != nil {
		if stream.sendWindow+int64(frame.Increment) > math.MaxInt32 {
			return errors.New("http2: stream send window overflow")
		}
		stream.sendWindow += int64(frame.Increment)
		c.signalLocked()
	}
	return nil
}

func (c *clientConn) processGoAway(frame *xhttp2.GoAwayFrame) {
	c.mu.Lock()
	c.goAway = true
	c.goAwayLastStreamID = frame.LastStreamID
	for _, stream := range c.streams {
		if stream.id > frame.LastStreamID && !stream.responseHeader {
			c.failStreamLocked(stream, errors.New("http2: stream wasn't processed before GOAWAY"), true)
		}
	}
	c.signalLocked()
	c.mu.Unlock()
	c.pool.streamAvailable()
}

func (c *clientConn) processPushPromise(frame *xhttp2.PushPromiseFrame) error {
	c.mu.Lock()
	if c.pendingPush != nil {
		c.mu.Unlock()
		return errors.New("http2: push promise interrupted a header block")
	}
	parent := c.streams[frame.StreamID]
	validID := frame.PromiseID != 0 && frame.PromiseID&1 == 0 && frame.PromiseID > c.lastPromisedStreamID
	if !validID {
		c.mu.Unlock()
		return errors.New("http2: invalid promised stream id")
	}
	c.lastPromisedStreamID = frame.PromiseID
	if parent == nil || c.config.pushHandler == nil || c.activeStreams >= c.config.maxConcurrentStreams {
		c.mu.Unlock()
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(frame.PromiseID, xhttp2.ErrCodeCancel)
		})
	}
	pending := &pendingPushBlock{
		parentID:   frame.StreamID,
		promisedID: frame.PromiseID,
		block:      bytes.Clone(frame.HeaderBlockFragment()),
	}
	if len(pending.block) > int(c.config.maxHeaderListSize) {
		c.mu.Unlock()
		return errors.New("http2: compressed push header block is too large")
	}
	if !frame.HeadersEnded() {
		c.pendingPush = pending
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	return c.finishPushPromise(pending)
}

func (c *clientConn) processPushContinuation(frame *xhttp2.ContinuationFrame) error {
	c.mu.Lock()
	pending := c.pendingPush
	if pending == nil || frame.StreamID != pending.parentID {
		c.mu.Unlock()
		return errors.New("http2: unexpected push continuation")
	}
	pending.block = append(pending.block, frame.HeaderBlockFragment()...)
	if len(pending.block) > int(c.config.maxHeaderListSize) {
		c.pendingPush = nil
		c.mu.Unlock()
		return errors.New("http2: compressed push header block is too large")
	}
	if !frame.HeadersEnded() {
		c.mu.Unlock()
		return nil
	}
	c.pendingPush = nil
	c.mu.Unlock()
	return c.finishPushPromise(pending)
}

func (c *clientConn) finishPushPromise(pending *pendingPushBlock) error {
	fields := make([]hpack.HeaderField, 0, 8)
	headerSize := uint64(0)
	c.decoder.SetEmitFunc(func(field hpack.HeaderField) {
		headerSize += uint64(len(field.Name) + len(field.Value) + 32)
		fields = append(fields, hpack.HeaderField{
			Name:      strings.Clone(field.Name),
			Value:     strings.Clone(field.Value),
			Sensitive: field.Sensitive,
		})
	})
	if _, err := c.decoder.Write(pending.block); err != nil {
		return err
	}
	if err := c.decoder.Close(); err != nil {
		return err
	}
	if headerSize > uint64(c.config.maxHeaderListSize) {
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(pending.promisedID, xhttp2.ErrCodeEnhanceYourCalm)
		})
	}

	promised := fasthttp.AcquireRequest()
	if err := populatePromisedRequest(promised, fields); err != nil {
		fasthttp.ReleaseRequest(promised)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(pending.promisedID, xhttp2.ErrCodeProtocol)
		})
	}
	c.mu.Lock()
	parent := c.streams[pending.parentID]
	if parent == nil {
		c.mu.Unlock()
		fasthttp.ReleaseRequest(promised)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(pending.promisedID, xhttp2.ErrCodeCancel)
		})
	}
	parentCopy := fasthttp.AcquireRequest()
	parent.req.CopyTo(parentCopy)
	c.mu.Unlock()
	accepted := c.config.pushHandler.Accept(parentCopy, promised)
	fasthttp.ReleaseRequest(parentCopy)
	if !accepted {
		fasthttp.ReleaseRequest(promised)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(pending.promisedID, xhttp2.ErrCodeCancel)
		})
	}

	response := fasthttp.AcquireResponse()
	c.mu.Lock()
	if c.closed || c.streams[pending.promisedID] != nil || c.activeStreams >= c.config.maxConcurrentStreams {
		c.mu.Unlock()
		fasthttp.ReleaseRequest(promised)
		fasthttp.ReleaseResponse(response)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(pending.promisedID, xhttp2.ErrCodeCancel)
		})
	}
	stream := &clientStream{
		id:                    pending.promisedID,
		conn:                  c,
		req:                   promised,
		resp:                  response,
		result:                make(chan clientResult, 1),
		done:                  make(chan struct{}),
		requestStarted:        true,
		localClosed:           true,
		bodyDone:              true,
		sendWindow:            c.peerInitialStreamWindow,
		recvWindow:            int64(c.config.streamWindowSize),
		expectedResponseBytes: -1,
		maxResponseBodySize:   c.hc.MaxResponseBodySize,
		isPush:                true,
		promisedRequest:       promised,
	}
	c.streams[stream.id] = stream
	c.activeStreams++
	c.mu.Unlock()
	return nil
}

func (c *clientConn) writeControl(write func() error) error {
	c.writeMu.Lock()
	err := write()
	c.writeMu.Unlock()
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *clientConn) resetStream(streamID uint32, code xhttp2.ErrCode, cause error, retry bool) {
	c.mu.Lock()
	stream := c.streams[streamID]
	if stream == nil {
		c.mu.Unlock()
		return
	}
	c.failStreamLocked(stream, cause, retry)
	c.mu.Unlock()
	_ = c.writeControl(func() error { return c.framer.WriteRSTStream(streamID, code) })
}

func (c *clientConn) failStream(streamID uint32, cause error, retry bool) {
	c.mu.Lock()
	if stream := c.streams[streamID]; stream != nil {
		c.failStreamLocked(stream, cause, retry)
	}
	c.mu.Unlock()
}

func (c *clientConn) failStreamLocked(stream *clientStream, cause error, retry bool) {
	stream.err = cause
	stream.localClosed = true
	stream.remoteClosed = true
	stream.bodyDone = true
	if stream.responseBody != nil {
		stream.responseBody.closeWithError(cause)
	}
	if !stream.isPush {
		c.sendResultLocked(stream, clientResult{retry: retry && !stream.responseHeader, err: cause})
	}
	c.releaseStreamLocked(stream)
	c.skipUnavailableHeaderStreamsLocked()
}

func (c *clientConn) sendResultLocked(stream *clientStream, result clientResult) {
	if stream.resultSent {
		return
	}
	stream.resultSent = true
	stream.result <- result
}

func (c *clientConn) maybeFinishStreamLocked(stream *clientStream) {
	if !stream.localClosed || !stream.remoteClosed {
		return
	}
	if stream.isStreaming && !stream.bodyDone {
		return
	}
	c.releaseStreamLocked(stream)
}

func (c *clientConn) releaseStreamLocked(stream *clientStream) {
	if c.streams[stream.id] != stream {
		return
	}
	delete(c.streams, stream.id)
	if stream.timer != nil {
		stream.timer.Stop()
	}
	if !stream.doneClosed {
		close(stream.done)
		stream.doneClosed = true
	}
	if c.activeStreams > 0 {
		c.activeStreams--
	}
	if stream.isPush {
		request := stream.promisedRequest
		response := stream.resp
		if stream.pushComplete {
			handler := c.config.pushHandler
			go func() {
				handler.Handle(request, response)
				fasthttp.ReleaseRequest(request)
				fasthttp.ReleaseResponse(response)
			}()
		} else {
			fasthttp.ReleaseRequest(request)
			fasthttp.ReleaseResponse(response)
		}
		stream.promisedRequest = nil
		stream.req = nil
		stream.resp = nil
	}
	c.lastIdle = time.Now()
	c.signalLocked()
	if c.activeStreams == 0 {
		idleTimeout := c.hc.MaxIdleConnDuration
		if idleTimeout <= 0 {
			idleTimeout = fasthttp.DefaultMaxIdleConnDuration
		}
		c.idleTimer = time.AfterFunc(idleTimeout, c.closeIfIdle)
	}
	go c.pool.streamAvailable()
}

func (c *clientConn) signalLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

func (c *clientConn) fail(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = cause
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	for _, stream := range c.streams {
		c.failStreamLocked(stream, cause, false)
	}
	c.signalLocked()
	c.mu.Unlock()
	_ = c.lease.Close()
	c.pool.remove(c)
}

func (c *clientConn) closeIfIdle() {
	c.mu.Lock()
	if c.closed || c.activeStreams != 0 {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.lease.Close()
	c.pool.remove(c)
}

var _ fasthttp.ProtocolRoundTripper = (*Transport)(nil)
var _ fasthttp.StreamRoundTripper = (*Transport)(nil)
var _ fasthttp.ProtocolTransportCloser = (*Transport)(nil)

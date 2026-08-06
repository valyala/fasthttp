package http2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	// ErrHTTP2Required is returned when RequireHTTP2 is configured and TLS
	// negotiation doesn't select h2.
	ErrHTTP2Required      = errors.New("http2: server didn't negotiate h2")
	errClientConnClosed   = errors.New("http2: client connection closed")
	errClientPoolClosed   = errors.New("http2: client connection pool closed")
	errClientStreamClosed = errors.New("http2: client stream closed")
	// ErrRefusedStream matches a StreamError carrying REFUSED_STREAM.
	ErrRefusedStream = errors.New("http2: stream refused")
	// ErrConnectionDraining matches streams rejected by a received GOAWAY.
	ErrConnectionDraining = errors.New("http2: connection is draining")
)

// gracefulWriterDrainTimeout is short because CloseIdleConnections holds a lock
// covering every host: a stalled peer must not stall unrelated hosts.
const gracefulWriterDrainTimeout = 250 * time.Millisecond

// StreamError reports a peer-generated RST_STREAM. ErrCode is the HTTP/2
// error code encoded on the wire.
type StreamError struct {
	StreamID uint32
	ErrCode  uint32
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("http2: peer reset stream %d: %s", e.StreamID, xhttp2.ErrCode(e.ErrCode))
}

func (e *StreamError) Unwrap() error {
	if xhttp2.ErrCode(e.ErrCode) == xhttp2.ErrCodeRefusedStream {
		return ErrRefusedStream
	}
	return nil
}

// GoAwayError reports that a stream was above the last stream ID accepted by
// a peer. ErrCode is the HTTP/2 error code encoded on the wire.
type GoAwayError struct {
	LastStreamID uint32
	ErrCode      uint32
}

func (e *GoAwayError) Error() string {
	return fmt.Sprintf(
		"http2: peer is draining after stream %d: %s",
		e.LastStreamID,
		xhttp2.ErrCode(e.ErrCode),
	)
}

func (e *GoAwayError) Unwrap() error { return ErrConnectionDraining }

type clientConnectionWriteError struct {
	err error
}

func (e *clientConnectionWriteError) Error() string { return e.err.Error() }
func (e *clientConnectionWriteError) Unwrap() error { return e.err }

var (
	clientResultChannelPool sync.Pool
	clientStreamPool        sync.Pool
)

func acquireClientResultChannel() chan clientResult {
	if value := clientResultChannelPool.Get(); value != nil {
		return value.(chan clientResult) //nolint:forcetypeassert
	}
	return make(chan clientResult, 1)
}

func releaseClientResultChannel(resultChannel chan clientResult) {
	clientResultChannelPool.Put(resultChannel)
}

func acquireClientStream() *clientStream {
	if value := clientStreamPool.Get(); value != nil {
		return value.(*clientStream) //nolint:forcetypeassert
	}
	return &clientStream{}
}

func releaseClientStream(stream *clientStream) {
	*stream = clientStream{}
	clientStreamPool.Put(stream)
}

type clientResult struct {
	streamConn fasthttp.StreamConn
	retry      bool
	err        error
}

type clientStream struct {
	// id is assigned in writeRequestHeaders under the connection write slot,
	// which makes wire order of request HEADERS equal ID order.
	id   uint32
	conn *clientConn
	req  *fasthttp.Request
	resp *fasthttp.Response

	result     chan clientResult
	resultSent bool
	timer      *time.Timer
	readTimer  *time.Timer

	requestStarted         bool
	requestBytes           int64
	localClosed            bool
	remoteClosed           bool
	responseHeader         bool
	informationalResponses uint8
	bodyDone               bool
	isOpenStream           bool
	isStreaming            bool
	// Snapshots: with a streaming response body Do returns at the headers,
	// after which the caller owns req again and may release it.
	isHead        bool
	isConnect     bool
	parentRequest *fasthttp.Request

	sendWindow          int64
	recvWindow          int64
	pendingWindowUpdate int64

	statusCode            int
	expectedResponseBytes int64
	responseBytes         int64
	maxResponseBodySize   int
	responseBody          *responseBody
	discardResponseBody   bool
	err                   error
	isPush                bool
	pushComplete          bool
	promisedRequest       *fasthttp.Request
	lifecycleReleased     bool
	callerDone            bool
	poolable              bool
}

type clientConn struct {
	pool          *clientPool
	hc            *fasthttp.HostClient
	config        clientConfig
	lease         *fasthttp.ProtocolClientConn
	conn          net.Conn
	framer        *xhttp2.Framer
	frames        *frameReader
	headerDecoder *headerCodec

	// writeSem is a channel, not a Mutex, so a waiter can abandon it at its
	// deadline: the holder may be parked in asyncFrameWriter.enqueue.
	writeSem       chan struct{}
	bufferedWriter flushWriter
	writer         *asyncFrameWriter
	encoder        *hpack.Encoder
	headerBuffer   bytes.Buffer
	headerStrings  headerStringCache

	mu                       sync.Mutex
	streams                  map[uint32]*clientStream
	nextStreamID             uint32
	activeStreams            uint32
	peerMaxConcurrentStreams uint32
	peerInitialStreamWindow  int64
	peerConnectionWindow     int64
	peerMaxFrameSize         int
	peerMaxHeaderListSize    uint64
	receiveConnectionWindow  int64
	pendingConnectionUpdate  int64
	receivedSettings         bool
	peerExtendedConnect      bool
	goAway                   bool
	goAwayLastStreamID       uint32
	closed                   bool
	err                      error
	notify                   chan struct{}
	waiters                  int
	created                  time.Time
	lastIdle                 time.Time
	idleTimer                *time.Timer
	lastPromisedStreamID     uint32
}

func newClientConn(pool *clientPool, lease *fasthttp.ProtocolClientConn) (*clientConn, error) {
	if err := validateTLSConnection(lease.Conn()); err != nil {
		return nil, err
	}
	conn := &clientConn{
		pool:                     pool,
		hc:                       pool.hc,
		config:                   pool.config,
		lease:                    lease,
		conn:                     lease.Conn(),
		streams:                  make(map[uint32]*clientStream),
		nextStreamID:             1,
		peerMaxConcurrentStreams: 100,
		peerInitialStreamWindow:  65535,
		peerConnectionWindow:     65535,
		peerMaxFrameSize:         defaultMaxFrameSize,
		peerMaxHeaderListSize:    math.MaxUint32,
		receiveConnectionWindow:  int64(pool.config.connectionWindowSize),
		notify:                   make(chan struct{}),
		writeSem:                 make(chan struct{}, 1),
		created:                  time.Now(),
	}
	writeBufferSize := pool.hc.WriteBufferSize
	if writeBufferSize <= 0 {
		writeBufferSize = defaultWriteBufferSize
	}
	conn.writer = newAsyncFrameWriter(
		conn.conn,
		writeBufferSize,
		defaultWriteQueueBatches,
		conn.config.writeByteTimeout,
	)
	conn.bufferedWriter = conn.writer
	readBufferSize := pool.hc.ReadBufferSize
	if readBufferSize <= 0 {
		readBufferSize = defaultReadBufferSize
	}
	reader := bufio.NewReaderSize(conn.conn, readBufferSize)
	conn.framer = xhttp2.NewFramer(conn.bufferedWriter, reader)
	conn.frames = newFrameReader(conn.framer, reader)
	conn.framer.SetReuseFrames()
	conn.headerDecoder = newHeaderCodec(pool.config.maxDecoderTableSize, pool.config.maxHeaderListSize)
	conn.framer.SetMaxReadFrameSize(pool.config.maxReadFrameSize)
	conn.encoder = hpack.NewEncoder(&conn.headerBuffer)
	conn.encoder.SetMaxDynamicTableSizeLimit(pool.config.maxEncoderTableSize)
	if err := conn.writePrefaceAndSettings(); err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *clientConn) writePrefaceAndSettings() (err error) {
	if err = c.lockWrite(time.Time{}); err != nil {
		return err
	}
	defer func() { err = c.unlockWrite(err) }()
	if _, err := io.WriteString(c.bufferedWriter, clientPreface); err != nil {
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
		if err := c.framer.WriteWindowUpdate(0, increment); err != nil {
			return err
		}
	}
	return c.bufferedWriter.Flush()
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
	}
	stream := acquireClientStream()
	isHead := req.Header.IsHead()
	discardResponseBody := resp.SkipBody || isHead
	*stream = clientStream{
		conn:                  c,
		req:                   req,
		resp:                  resp,
		result:                acquireClientResultChannel(),
		isOpenStream:          openStream,
		isHead:                isHead,
		isConnect:             req.Header.IsConnect(),
		isStreaming:           openStream || resp.StreamBody && !discardResponseBody,
		recvWindow:            int64(c.config.streamWindowSize),
		expectedResponseBytes: -1,
		maxResponseBodySize:   c.hc.MaxResponseBodySize,
		discardResponseBody:   discardResponseBody,
		poolable:              !openStream,
	}
	if c.config.pushHandler != nil && !openStream {
		// PUSH_PROMISE handling runs on the read loop and needs the parent
		// request, which the caller may already have released by then.
		parentRequest := fasthttp.AcquireRequest()
		req.Header.CopyTo(&parentRequest.Header)
		parentRequest.SetRequestURIBytes(req.URI().FullURI())
		stream.parentRequest = parentRequest
	}
	c.activeStreams++
	if !deadline.IsZero() {
		// No ID yet, so the timer holds the pointer; maybeFinalizeStreamLocked
		// keeps a late callback off a pooled stream.
		stream.timer = time.AfterFunc(time.Until(deadline), func() {
			c.expireRequestDeadline(stream)
		})
	}
	return stream
}

func (c *clientConn) roundTrip(
	ctx *fasthttp.ProtocolClientContext,
	stream *clientStream,
) (bool, error) {
	deadline, _ := ctx.Deadline()
	writeDeadline := phaseDeadline(deadline, ctx.WriteTimeout())
	if err := c.writeRequest(stream, false, writeDeadline); err != nil {
		c.cancelStream(stream, xhttp2.ErrCodeCancel, err, false)
	}
	readDeadline := phaseDeadline(deadline, ctx.ReadTimeout())
	if ctx.ReadTimeout() > 0 {
		c.armResponseReadTimeout(stream, readDeadline)
	}
	result := c.waitResult(stream, readDeadline)
	return result.retry, result.err
}

func (c *clientConn) expireRequestDeadline(stream *clientStream) {
	c.mu.Lock()
	ignore := stream.lifecycleReleased || stream.err != nil ||
		stream.isOpenStream && stream.responseHeader
	c.mu.Unlock()
	if !ignore {
		c.cancelStream(stream, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, false)
	}
}

func (c *clientConn) armResponseReadTimeout(stream *clientStream, deadline time.Time) {
	streamID := stream.id
	c.mu.Lock()
	if c.streams[streamID] == stream {
		stream.readTimer = time.AfterFunc(time.Until(deadline), func() {
			c.resetStream(streamID, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, false)
		})
	}
	c.mu.Unlock()
}

func (c *clientConn) openStream(
	ctx *fasthttp.ProtocolClientContext,
	stream *clientStream,
) clientResult {
	deadline, _ := ctx.Deadline()
	writeDeadline := phaseDeadline(deadline, ctx.WriteTimeout())
	if len(stream.req.Header.ConnectProtocol()) == 0 || string(stream.req.Header.Method()) != fasthttp.MethodConnect {
		c.cancelStream(stream, xhttp2.ErrCodeCancel, errors.New("http2: OpenStream requires an extended CONNECT request"), false)
		return c.waitResult(stream, deadline)
	}
	if stream.req.IsBodyStream() || len(stream.req.Body()) != 0 {
		c.cancelStream(stream, xhttp2.ErrCodeCancel, errors.New("http2: OpenStream request must not have a body"), false)
		return c.waitResult(stream, deadline)
	}
	if err := c.writeRequest(stream, true, writeDeadline); err != nil {
		c.cancelStream(stream, xhttp2.ErrCodeCancel, err, false)
	}
	return c.waitResult(stream, phaseDeadline(deadline, ctx.ReadTimeout()))
}

func phaseDeadline(requestDeadline time.Time, timeout time.Duration) time.Time {
	if timeout <= 0 {
		return requestDeadline
	}
	deadline := time.Now().Add(timeout)
	if !requestDeadline.IsZero() && requestDeadline.Before(deadline) {
		return requestDeadline
	}
	return deadline
}

func (c *clientConn) waitResult(stream *clientStream, deadline time.Time) clientResult {
	resultChannel := stream.result
	if deadline.IsZero() {
		result := <-resultChannel
		releaseClientResultChannel(resultChannel)
		if !stream.isOpenStream {
			c.finishClientStreamUse(stream)
		}
		return result
	}
	timer := fasthttp.AcquireTimer(time.Until(deadline))
	defer fasthttp.ReleaseTimer(timer)
	select {
	case result := <-resultChannel:
		releaseClientResultChannel(resultChannel)
		if !stream.isOpenStream {
			c.finishClientStreamUse(stream)
		}
		return result
	case <-timer.C:
		c.cancelStream(stream, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, false)
		result := <-resultChannel
		releaseClientResultChannel(resultChannel)
		if !stream.isOpenStream {
			c.finishClientStreamUse(stream)
		}
		return result
	}
}

func (c *clientConn) resetStream(streamID uint32, code xhttp2.ErrCode, cause error, retry bool) {
	c.mu.Lock()
	stream := c.streams[streamID]
	c.mu.Unlock()
	if stream == nil {
		return
	}
	c.cancelStream(stream, code, cause, retry)
}

// cancelStream fails stream, emitting RST_STREAM only if its HEADERS reached
// the wire. An uncommitted stream just abandons its ID, which RFC 9113 §5.1.1
// permits.
func (c *clientConn) cancelStream(stream *clientStream, code xhttp2.ErrCode, cause error, retry bool) {
	if c.config.countError != nil {
		// Guarded at the call site: building the tag allocates twice, and every
		// deadline-driven cancel lands here, so a disabled counter must be free.
		c.countError("stream_" + strings.ToLower(code.String()))
	}
	c.mu.Lock()
	streamID := stream.id
	requestStarted := stream.requestStarted
	c.failStreamLocked(stream, cause, retry)
	c.mu.Unlock()
	if requestStarted {
		_ = c.writeControl(func() error { return c.framer.WriteRSTStream(streamID, code) })
	}
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
	c.maybeFinalizeStreamLocked(stream)
}

func (c *clientConn) sendResultLocked(stream *clientStream, result clientResult) {
	if stream.resultSent {
		return
	}
	stream.resultSent = true
	stream.result <- result
}

func (c *clientConn) maybeFinalizeStreamLocked(stream *clientStream) {
	// lifecycleReleased makes finalization idempotent: a stream that never
	// received an ID has no map entry to guard it, and a registered stream
	// may reach this point from both its writer and a concurrent cancel.
	if stream.lifecycleReleased {
		return
	}
	if stream.id != 0 && c.streams[stream.id] != stream {
		return
	}
	if !stream.localClosed || !stream.remoteClosed {
		return
	}
	if stream.isStreaming && !stream.bodyDone {
		return
	}
	delete(c.streams, stream.id)
	if stream.timer != nil {
		// Stop reports false once the callback has begun; it may be waiting
		// for c.mu. It relies on lifecycleReleased, so the object must keep
		// its state instead of being zeroed by the pool.
		if !stream.timer.Stop() {
			stream.poolable = false
		}
		stream.timer = nil
	}
	if stream.readTimer != nil {
		stream.readTimer.Stop()
		stream.readTimer = nil
	}
	if stream.parentRequest != nil {
		fasthttp.ReleaseRequest(stream.parentRequest)
		stream.parentRequest = nil
	}
	c.activeStreams--
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
	stream.lifecycleReleased = true
	if stream.callerDone && stream.poolable {
		releaseClientStream(stream)
	}
	c.lastIdle = time.Now()
	c.signalLocked()
	if c.activeStreams == 0 {
		if c.goAway {
			go c.closeIfIdle()
			return
		}
		idleTimeout := c.hc.MaxIdleConnDuration
		if idleTimeout <= 0 {
			idleTimeout = fasthttp.DefaultMaxIdleConnDuration
		}
		// Re-armed, not replaced: one-at-a-time requests idle after every one.
		if c.idleTimer == nil {
			c.idleTimer = time.AfterFunc(idleTimeout, c.closeIfIdle)
		} else {
			c.idleTimer.Reset(idleTimeout)
		}
	}
	c.pool.streamAvailable()
}

func (c *clientConn) finishClientStreamUse(stream *clientStream) {
	c.mu.Lock()
	stream.callerDone = true
	if stream.lifecycleReleased && stream.poolable {
		releaseClientStream(stream)
	}
	c.mu.Unlock()
}

func (c *clientConn) signalLocked() {
	if c.waiters == 0 {
		return
	}
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
	c.countError("connection_error")
	c.shutdownWriter(cause, false)
	_ = c.lease.Close()
	c.pool.remove(c)
}

func (c *clientConn) countError(errorType string) {
	if c.config.countError != nil {
		c.config.countError(errorType)
	}
}

func (c *clientConn) closeIfIdle() {
	c.mu.Lock()
	if c.closed || c.activeStreams != 0 {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.shutdownWriter(nil, true)
	_ = c.lease.Close()
	c.pool.remove(c)
}

func (c *clientConn) shutdownWriter(cause error, graceful bool) {
	if !graceful {
		// Must not take the write slot: the producer to unblock is holding
		// it. abort synchronizes through the writer's own sendMu.
		c.writer.abort(cause)
		return
	}
	// A graceful drain is best-effort. If the write slot cannot be taken
	// promptly the connection is already wedged, so abort instead of waiting.
	if err := c.acquireWrite(time.Now().Add(gracefulWriterDrainTimeout)); err != nil {
		c.writer.abort(errClientConnClosed)
		return
	}
	defer c.releaseWrite()
	_ = c.writer.closeAndWait(gracefulWriterDrainTimeout)
}

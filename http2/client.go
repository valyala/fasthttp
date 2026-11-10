package http2

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
)

var (
	// ErrHTTP2Required is returned when RequireHTTP2 is configured and TLS
	// negotiation doesn't select h2.
	ErrHTTP2Required      = errors.New("http2: server didn't negotiate h2")
	errClientConnClosed   = errors.New("http2: client connection closed")
	errPingTimeout        = errors.New("http2: health check PING timed out")
	errClientPoolClosed   = errors.New("http2: client connection pool closed")
	errClientStreamClosed = errors.New("http2: client stream closed")
	// errTooManyControlFrames fails a connection whose peer keeps sending
	// frames that need answers while the transport is stalled.
	errTooManyControlFrames = errors.New("http2: too many queued control frames")
	errResponseComplete     = errors.New("http2: response completed before the request body")
	errUnsupportedProtocol  = errors.New("http2: server negotiated a protocol other than h2 or http/1.1")
	// ErrRefusedStream matches a StreamError carrying REFUSED_STREAM. The
	// server didn't process the request, so HostClient.RetryIfErr may retry
	// it whatever its method.
	ErrRefusedStream = errors.New("http2: stream refused")
	// ErrConnectionDraining matches requests a received GOAWAY kept from
	// being processed, which HostClient.RetryIfErr may retry likewise.
	ErrConnectionDraining = errors.New("http2: connection is draining")
)

// gracefulWriterDrainTimeout is short because CloseIdleConnections holds a lock
// covering every host: a stalled peer must not stall unrelated hosts.
const gracefulWriterDrainTimeout = 250 * time.Millisecond

// openStreamCloseGrace is how long a closed OpenStream stream waits for the
// peer's END_STREAM before it is reset.
const openStreamCloseGrace = time.Second

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

// Is reports a reset stream as a closed one, as the server's StreamConn
// reports the peer's reset.
func (e *StreamError) Is(target error) bool {
	return target == errStreamClosed
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

// peerConnectionError is a connection error the peer caused; GOAWAY carries
// its code.
type peerConnectionError struct {
	code xhttp2.ErrCode
	err  error
}

func (e *peerConnectionError) Error() string { return e.err.Error() }
func (e *peerConnectionError) Unwrap() error { return e.err }

func peerError(code xhttp2.ErrCode, message string) error {
	return &peerConnectionError{code: code, err: errors.New(message)}
}

// goAwayCode reports whether err is a connection error the peer caused, and
// the code to report it with.
func goAwayCode(err error) (xhttp2.ErrCode, bool) {
	if peerErr, ok := errors.AsType[*peerConnectionError](err); ok {
		return peerErr.code, true
	}
	if errors.Is(err, errTooManyControlFrames) {
		return xhttp2.ErrCodeEnhanceYourCalm, true
	}
	if errors.Is(err, xhttp2.ErrFrameTooLarge) {
		return xhttp2.ErrCodeFrameSize, true
	}
	if connectionError, ok := errors.AsType[xhttp2.ConnectionError](err); ok {
		return xhttp2.ErrCode(connectionError), true
	}
	if streamError, ok := errors.AsType[xhttp2.StreamError](err); ok {
		return streamError.Code, true
	}
	return 0, false
}

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

// failureKind is where a stream failure originated. Every kind but a local
// one is one HostClient's policy may retry, as it may retry every HTTP/1
// write and read error; failStreamLocked applies the common exclusions.
type failureKind uint8

const (
	// failureLocal is the caller's: a cancellation, a closed body or
	// StreamConn, a request that can't be prepared or encoded.
	failureLocal failureKind = iota
	// failureWriteTimeout is a write deadline passed waiting for the write
	// slot or the peer's window.
	failureWriteTimeout
	// failureReadTimeout is a read deadline passed before the response, or
	// the rest of it, arrived.
	failureReadTimeout
	// failurePeer is a reset, or a response the client rejects.
	failurePeer
	// failureConnection is a failed, draining or closed connection.
	failureConnection
)

type clientStream struct {
	streamFlowState

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
	// closeTimer resets a closed OpenStream stream the peer doesn't end.
	closeTimer *time.Timer

	requestStarted         bool
	localClosed            bool
	remoteClosed           bool
	responseHeader         bool
	informationalResponses uint8
	bodyDone               bool
	isOpenStream           bool
	// callerClosed marks an OpenStream stream its caller closed: data the
	// peer still sends resets it.
	callerClosed bool
	isStreaming  bool
	// Snapshots: with a streaming response body Do returns at the headers,
	// after which the caller owns req again and may release it.
	isHead        bool
	isConnect     bool
	hasBodyStream bool
	parentRequest *fasthttp.Request

	statusCode            int
	expectedResponseBytes int64
	responseBytes         int64
	maxResponseBodySize   int
	responseBody          *responseBody
	discardResponseBody   bool
	err                   error
	isPush                bool
	// reserved marks a promised stream before its first HEADERS (RFC 9113
	// §5.1 reserved (remote)): only HEADERS, RST_STREAM and PRIORITY may
	// arrive.
	reserved          bool
	pushComplete      bool
	promisedRequest   *fasthttp.Request
	lifecycleReleased bool
	callerDone        bool
	poolable          bool
}

// retryable reports whether HostClient's policy may replay the request after
// a failure of kind with cause: not a body stream whose HEADERS were
// committed, and not ErrBodyTooLarge, which would recur.
func (s *clientStream) retryable(kind failureKind, cause error) bool {
	return kind != failureLocal && (!s.hasBodyStream || !s.requestStarted) &&
		!errors.Is(cause, fasthttp.ErrBodyTooLarge)
}

type clientConn struct {
	connFlowState
	headerEncoder

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

	mu                sync.Mutex
	streams           map[uint32]*clientStream
	nextStreamID      uint32
	activeStreams     uint32
	activePushStreams uint32

	peerExtendedConnect bool
	goAway              bool
	goAwayLastStreamID  uint32
	expired             bool // past MaxConnDuration; closes once idle
	// drainOnRetire has the retirement send GOAWAY(NO_ERROR) first: the
	// connection expired by Connection: close intent or MaxConnDuration.
	drainOnRetire bool
	// idleSeq orders idle connections for ConnPoolStrategy: the pool counter's
	// value when the last stream ended, 0 while streams are active.
	idleSeq              uint64
	closed               bool
	err                  error
	notify               chan struct{}
	waiters              int
	created              time.Time
	idleTimer            *time.Timer
	lastPromisedStreamID uint32

	// ReadIdleTimeout health check. Times are offsets from created.
	healthTimer     *time.Timer
	lastFrameAt     atomic.Int64
	pingSentAt      time.Duration
	pingOutstanding bool

	// settingsAcked is set once the peer acknowledged this client's SETTINGS;
	// only then does a smaller HPACK table apply (RFC 9113 §6.5.3).
	settingsAcked bool

	// Frames owed to the peer, recorded under mu and framed by sendControl,
	// so neither a body reader nor the read loop waits for the write slot:
	// flow-control credit, and the control frames the read loop answers
	// with, in FIFO order.
	creditConn    int64
	creditStreams map[uint32]int64
	controls      []controlFrame
	// controlsInFlight counts the batch sendControl detached and hasn't
	// written yet; controlOverflow marks a connection that exceeded
	// maxQueuedControlFrames and is failing.
	controlsInFlight int
	controlOverflow  bool
	// maxControls and closeGrace replace maxQueuedControlFrames and
	// openStreamCloseGrace when nonzero.
	maxControls  int
	closeGrace   time.Duration
	senderSignal chan struct{}
	// goAwayOnExit is the GOAWAY sendControl writes when done closes.
	goAwayOnExit *goAwayFrame
	// done is closed once the connection fails or retires; senderDone once
	// sendControl returned.
	done       chan struct{}
	senderDone chan struct{}
}

type goAwayFrame struct {
	lastStreamID uint32
	code         xhttp2.ErrCode
}

func newClientConn(pool *clientPool, lease *fasthttp.ProtocolClientConn) (*clientConn, error) {
	if err := validateTLSConnection(lease.Conn()); err != nil {
		return nil, err
	}
	conn := &clientConn{
		pool:          pool,
		hc:            pool.hc,
		config:        pool.config,
		lease:         lease,
		conn:          lease.Conn(),
		streams:       make(map[uint32]*clientStream),
		nextStreamID:  1,
		connFlowState: newConnFlowState(int64(pool.config.connectionWindowSize)),
		notify:        make(chan struct{}),
		writeSem:      make(chan struct{}, 1),
		senderSignal:  make(chan struct{}, 1),
		done:          make(chan struct{}),
		senderDone:    make(chan struct{}),
		created:       time.Now(),
		idleSeq:       pool.idleSeq.Add(1),
	}
	// x/net's pre-SETTINGS assumption for new client connections.
	conn.peerMaxConcurrentStreams = 100
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
	// A smaller table applies once the peer acknowledges SETTINGS.
	conn.headerDecoder = newHeaderCodec(
		max(pool.config.maxDecoderTableSize, defaultHeaderTableSize),
		pool.config.maxHeaderListSize,
	)
	conn.framer.SetMaxReadFrameSize(pool.config.maxReadFrameSize)
	conn.initHeaderEncoder(pool.config.maxEncoderTableSize)
	if err := conn.writePrefaceAndSettings(); err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *clientConn) writePrefaceAndSettings() (err error) {
	if err = c.lockWrite(time.Time{}, nil); err != nil {
		return err
	}
	defer func() { err = c.unlockWrite(err) }()
	if _, err := io.WriteString(c.bufferedWriter, clientPreface); err != nil {
		return err
	}
	enablePush := uint32(0)
	if c.config.pushHandler != nil {
		enablePush = 1
	}
	settings := []xhttp2.Setting{
		{ID: xhttp2.SettingEnablePush, Val: enablePush},
		{ID: xhttp2.SettingMaxConcurrentStreams, Val: c.config.maxConcurrentStreams},
		{ID: xhttp2.SettingInitialWindowSize, Val: uint32(c.config.streamWindowSize)}, // #nosec G115
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
	increment := uint32(c.config.connectionWindowSize - 65535) // #nosec G115
	if increment != 0 {
		if err := c.framer.WriteWindowUpdate(0, increment); err != nil {
			return err
		}
	}
	return c.bufferedWriter.Flush()
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
	if _, ok := c.reservableLocked(now); !ok {
		return nil
	}
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	stream := acquireClientStream()
	isHead := req.Header.IsHead()
	discardResponseBody := resp.SkipBody || isHead
	isStreaming := openStream || resp.StreamBody && !discardResponseBody
	*stream = clientStream{
		conn:                  c,
		req:                   req,
		resp:                  resp,
		result:                acquireClientResultChannel(),
		isOpenStream:          openStream,
		isHead:                isHead,
		isConnect:             req.Header.IsConnect(),
		hasBodyStream:         req.IsBodyStream(),
		isStreaming:           isStreaming,
		streamFlowState:       streamFlowState{recv: recvWindow{window: int64(c.config.streamWindowSize)}},
		expectedResponseBytes: -1,
		maxResponseBodySize:   c.responseBodyLimit(isStreaming),
		discardResponseBody:   discardResponseBody,
		poolable:              !openStream,
	}
	c.activeStreams++
	c.idleSeq = 0
	// Connection: close retires the connection as HTTP/1 closes it after the
	// response: no further reservations; it closes once its streams end.
	if req.ConnectionClose() {
		c.expired = true
		c.drainOnRetire = true
	}
	// waitResult's pooled timer already covers buffered exchanges; only phases
	// that outlive it need a standing timer.
	if !deadline.IsZero() && (stream.isStreaming || req.IsBodyStream()) {
		// No ID yet, so the timer holds the pointer; maybeFinalizeStreamLocked
		// keeps a late callback off a pooled stream.
		stream.timer = time.AfterFunc(time.Until(deadline), func() {
			c.expireRequestDeadline(stream)
		})
	}
	return stream
}

// reservableLocked reports whether a stream may be reserved on c, and c's
// idle sequence; an over-age or exhausted connection is marked expired, so
// that it frees its MaxConns slot once idle. c.mu is held.
func (c *clientConn) reservableLocked(now time.Time) (idleSeq uint64, ok bool) {
	if c.closed || c.goAway {
		return 0, false
	}
	if c.expired || c.nextStreamID > math.MaxInt32 {
		c.expired = true
		return 0, false
	}
	if c.hc.MaxConnDuration > 0 && now.Sub(c.created) >= c.hc.MaxConnDuration {
		c.expired = true
		c.drainOnRetire = true
		return 0, false
	}
	// Pushes are peer-initiated; neither limit applies to them here.
	streamLimit := min(c.config.maxConcurrentStreams, c.peerMaxConcurrentStreams)
	if c.activeStreams-c.activePushStreams >= streamLimit {
		return 0, false
	}
	return c.idleSeq, true
}

func (c *clientConn) reservable(now time.Time) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reservableLocked(now)
}

// responseBodyLimit is HostClient.MaxResponseBodySize for a buffered
// response; a streamed one is unbounded, as over HTTP/1.
func (c *clientConn) responseBodyLimit(streaming bool) int {
	if streaming {
		return 0
	}
	return c.hc.MaxResponseBodySize
}

func (c *clientConn) roundTrip(
	ctx *fasthttp.ProtocolClientContext,
	stream *clientStream,
) (bool, error) {
	deadline, _ := ctx.Deadline()
	// Read before the stream is registered: processResponseHeaders clears it
	// for a bodyless status.
	streaming := stream.isStreaming
	writeDeadline, callerWriteDeadline := phaseDeadline(deadline, ctx.WriteTimeout())
	if err := c.sendRequest(ctx, stream, writeDeadline); err != nil {
		if closeErr, ok := errors.AsType[*requestBodyCloseError](err); ok {
			c.failRequestBodyClose(stream, closeErr.err)
		} else {
			// ErrTimeout here is the write deadline passing at the slot or
			// the peer's window: WriteTimeout's, or the caller's own.
			// Anything else is the request's own.
			kind := failureLocal
			if errors.Is(err, fasthttp.ErrTimeout) && !callerWriteDeadline {
				kind = failureWriteTimeout
			}
			c.cancelStream(stream, xhttp2.ErrCodeCancel, err, kind)
		}
	}
	readDeadline, callerReadDeadline := phaseDeadline(deadline, ctx.ReadTimeout())
	timeoutKind := failureReadTimeout
	if callerReadDeadline {
		timeoutKind = failureLocal
	}
	if ctx.ReadTimeout() > 0 && streaming {
		c.armResponseReadTimeout(stream, readDeadline, timeoutKind)
	}
	result := c.waitResult(stream, readDeadline, timeoutKind)
	return result.retry, result.err
}

// sendRequest settles the request as Request.Write does, keeps a copy for
// push handling, and sends it. It runs on the caller's goroutine while ctx is
// valid.
func (c *clientConn) sendRequest(
	ctx *fasthttp.ProtocolClientContext,
	stream *clientStream,
	deadline time.Time,
) error {
	body, defaultContentType, err := ctx.PrepareRequest(stream.req)
	if err != nil {
		return err
	}
	// Before the write, as HTTP/1 does: RetryIfErrUpstream names the peer
	// for a failure that precedes the response.
	stream.resp.ParseNetConn(c.lease.Conn())
	if c.config.pushHandler != nil {
		c.attachParentRequest(stream)
	}
	var reader io.Reader
	if stream.req.IsBodyStream() {
		// Bounded to the declared length with Request.Write's rules; the
		// stream itself is closed by writeRequest.
		reader = ctx.RequestBodyReader(stream.req)
	}
	return c.writeRequest(stream, body, defaultContentType, reader, false, deadline)
}

// attachParentRequest copies the request for PUSH_PROMISE handling, which
// runs on the read loop after the caller may have released it.
func (c *clientConn) attachParentRequest(stream *clientStream) {
	parent := fasthttp.AcquireRequest()
	stream.req.Header.CopyTo(&parent.Header)
	parent.SetRequestURIBytes(stream.req.URI().FullURI())
	c.mu.Lock()
	if stream.lifecycleReleased {
		c.mu.Unlock()
		fasthttp.ReleaseRequest(parent)
		return
	}
	stream.parentRequest = parent
	c.mu.Unlock()
}

func (c *clientConn) expireRequestDeadline(stream *clientStream) {
	c.mu.Lock()
	ignore := stream.lifecycleReleased || stream.err != nil ||
		stream.isOpenStream && stream.responseHeader
	if ignore {
		c.mu.Unlock()
		return
	}
	// The caller's deadline: not a phase timeout.
	c.cancelStreamAndUnlock(stream, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, failureLocal)
}

// armResponseReadTimeout bounds a streaming response by deadline; kind says
// whose deadline it is.
func (c *clientConn) armResponseReadTimeout(stream *clientStream, deadline time.Time, kind failureKind) {
	streamID := stream.id
	c.mu.Lock()
	if c.streams[streamID] == stream {
		stream.readTimer = time.AfterFunc(time.Until(deadline), func() {
			c.resetStream(streamID, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, kind)
		})
	}
	c.mu.Unlock()
}

func (c *clientConn) openStream(
	ctx *fasthttp.ProtocolClientContext,
	stream *clientStream,
) clientResult {
	deadline, _ := ctx.Deadline()
	writeDeadline, _ := phaseDeadline(deadline, ctx.WriteTimeout())
	if len(stream.req.Header.ConnectProtocol()) == 0 || string(stream.req.Header.Method()) != fasthttp.MethodConnect {
		c.cancelStream(stream, xhttp2.ErrCodeCancel, errors.New("http2: OpenStream requires an extended CONNECT request"), failureLocal)
		return c.waitResult(stream, deadline, failureLocal)
	}
	if stream.req.IsBodyStream() || len(stream.req.Body()) != 0 {
		c.cancelStream(stream, xhttp2.ErrCodeCancel, errors.New("http2: OpenStream request must not have a body"), failureLocal)
		return c.waitResult(stream, deadline, failureLocal)
	}
	// Not prepared as a Request.Write: that adds Content-Length: 0, which a
	// CONNECT must not carry.
	if err := c.writeRequest(stream, nil, nil, nil, true, writeDeadline); err != nil {
		c.cancelStream(stream, xhttp2.ErrCodeCancel, err, failureLocal)
	}
	readDeadline, _ := phaseDeadline(deadline, ctx.ReadTimeout())
	return c.waitResult(stream, readDeadline, failureLocal)
}

// waitResult waits for stream's result until deadline; timeoutKind is the
// failure a passed deadline is: the caller's, or ReadTimeout's.
func (c *clientConn) waitResult(stream *clientStream, deadline time.Time, timeoutKind failureKind) clientResult {
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
		c.mu.Lock()
		if stream.resultSent {
			// The result won the race; it stands.
			c.mu.Unlock()
		} else {
			c.cancelStreamAndUnlock(stream, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, timeoutKind)
		}
		result := <-resultChannel
		releaseClientResultChannel(resultChannel)
		if !stream.isOpenStream {
			c.finishClientStreamUse(stream)
		}
		return result
	}
}

func (c *clientConn) resetStream(streamID uint32, code xhttp2.ErrCode, cause error, kind failureKind) {
	c.mu.Lock()
	stream := c.streams[streamID]
	if stream == nil {
		c.mu.Unlock()
		return
	}
	// Under the lookup's lock: once it is released the stream may be
	// finalized and pooled.
	c.cancelStreamAndUnlock(stream, code, cause, kind)
}

// cancelStream fails stream, emitting RST_STREAM only while the stream is
// open on the wire. A stream whose HEADERS weren't committed just abandons
// its ID, which RFC 9113 §5.1.1 permits.
func (c *clientConn) cancelStream(stream *clientStream, code xhttp2.ErrCode, cause error, kind failureKind) {
	c.mu.Lock()
	c.cancelStreamAndUnlock(stream, code, cause, kind)
}

func (c *clientConn) cancelStreamAndUnlock(stream *clientStream, code xhttp2.ErrCode, cause error, kind failureKind) {
	streamID := stream.id
	sendReset := c.resetAllowedLocked(stream)
	c.failStreamLocked(stream, cause, kind)
	if sendReset {
		// Queued, never written here: the read loop is among the callers.
		c.queueControlLocked(controlFrame{kind: controlReset, streamID: streamID, code: code})
	}
	c.mu.Unlock()
}

// failRequestBodyClose fails stream with its request body's Close error. The
// body went out whole, so the response may already have queued a success;
// the error replaces it, as HTTP/1 reports a Close error over the response.
// roundTrip hasn't entered waitResult, so nothing else reads the result.
func (c *clientConn) failRequestBodyClose(stream *clientStream, cause error) {
	c.mu.Lock()
	if stream.resultSent {
		<-stream.result
		stream.resultSent = false
	}
	body := stream.responseBody
	c.cancelStreamAndUnlock(stream, xhttp2.ErrCodeCancel, cause, failureLocal)
	// Outside c.mu, which Close takes; unread bytes return their credit.
	if body != nil {
		_ = body.Close()
	}
}

// resetAllowedLocked reports whether RST_STREAM may follow the committed
// HEADERS: every path that closes a stream on the wire closes both halves.
func (c *clientConn) resetAllowedLocked(stream *clientStream) bool {
	return stream.requestStarted && !c.closed && (!stream.localClosed || !stream.remoteClosed)
}

// closeOpenStream records that the caller closed an OpenStream stream.
func (c *clientConn) closeOpenStream(stream *clientStream) {
	c.mu.Lock()
	stream.callerClosed = true
	c.mu.Unlock()
}

// armCloseGrace bounds how long a fully closed OpenStream stream, its
// END_STREAM sent, waits for the peer to end its side: a silent peer must
// not pin the stream slot. The timer resolves the stream by ID.
func (c *clientConn) armCloseGrace(stream *clientStream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stream.lifecycleReleased || stream.err != nil || !stream.localClosed ||
		stream.remoteClosed || stream.closeTimer != nil {
		return
	}
	streamID := stream.id
	grace := openStreamCloseGrace
	if c.closeGrace != 0 {
		grace = c.closeGrace
	}
	stream.closeTimer = time.AfterFunc(grace, func() {
		c.resetStream(streamID, xhttp2.ErrCodeCancel, net.ErrClosed, failureLocal)
	})
}

// failStreamLocked fails stream with cause, a failure of kind.
func (c *clientConn) failStreamLocked(stream *clientStream, cause error, kind failureKind) {
	// A connection's errors are reported as HTTP/1 reports them: an EOF is
	// the connection closed before the response or the body cut short, and a
	// request reports a transport timeout as ErrTimeout; a StreamConn reports
	// the transport's error. A local or peer error keeps its identity.
	if kind == failureConnection {
		switch {
		case errors.Is(cause, io.EOF) && stream.responseHeader:
			cause = errBodyTruncated
		case errors.Is(cause, io.EOF):
			cause = fasthttp.ErrConnectionClosed
		case !stream.isOpenStream && isTimeout(cause):
			cause = fasthttp.ErrTimeout
		}
	}
	if stream.responseBody != nil {
		stream.responseBody.fail(cause)
	}
	stream.err = cause
	stream.localClosed = true
	stream.remoteClosed = true
	stream.bodyDone = true
	if !stream.isPush {
		c.sendResultLocked(stream, clientResult{retry: stream.retryable(kind, cause), err: cause})
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
	if stream.closeTimer != nil {
		stream.closeTimer.Stop()
		stream.closeTimer = nil
	}
	if stream.parentRequest != nil {
		fasthttp.ReleaseRequest(stream.parentRequest)
		stream.parentRequest = nil
	}
	c.activeStreams--
	if stream.isPush {
		c.activePushStreams--
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
	c.signalLocked()
	// A closed connection is being torn down: no idle timer may retain it.
	if c.activeStreams == 0 && !c.closed {
		c.idleSeq = c.pool.idleSeq.Add(1)
		// A retired connection closes now, freeing its MaxConns slot; an
		// over-age one retires here, as HTTP/1 closes it after its response.
		if !c.expired && c.hc.MaxConnDuration > 0 && time.Since(c.created) >= c.hc.MaxConnDuration {
			c.expired = true
			c.drainOnRetire = true
		}
		if c.goAway || c.expired {
			go c.retireIdle(true)
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
	c.pool.streamFreed()
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

// fail closes c with cause. Lock order: pool.mu, then c.mu; the socket is
// closed with neither held, since a TLS close writes.
func (c *clientConn) fail(cause error) {
	c.pool.mu.Lock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.pool.mu.Unlock()
		return
	}
	goAway := false
	if code, ok := goAwayCode(cause); ok {
		// Written by sendControl on its way out, never by the read loop.
		c.goAwayOnExit = &goAwayFrame{lastStreamID: c.lastPromisedStreamID, code: code}
		goAway = true
	}
	c.closeLocked()
	c.err = cause
	for _, stream := range c.streams {
		c.failStreamLocked(stream, cause, failureConnection)
	}
	c.signalLocked()
	c.mu.Unlock()
	c.pool.retireLocked(c)
	c.pool.mu.Unlock()
	if goAway {
		c.awaitSender(2 * gracefulWriterDrainTimeout)
	}
	c.shutdownWriter(cause, false)
	_ = c.lease.Close()
	c.pool.retired()
}

// awaitSender waits, bounded, for sendControl to return: it may be writing
// the GOAWAY. The abort that follows unblocks it in any case.
func (c *clientConn) awaitSender(bound time.Duration) {
	timer := fasthttp.AcquireTimer(bound)
	defer fasthttp.ReleaseTimer(timer)
	select {
	case <-c.senderDone:
	case <-timer.C:
	}
}

// closeLocked marks c closed and stops its timers; c.mu is held.
func (c *clientConn) closeLocked() {
	c.closed = true
	close(c.done)
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	if c.healthTimer != nil {
		c.healthTimer.Stop()
	}
}

// writeGoAway reports a connection error to the peer (RFC 9113 §5.4.1). It
// is best effort and bounded like a graceful drain; sendControl writes it
// on its way out.
func (c *clientConn) writeGoAway(lastStreamID uint32, code xhttp2.ErrCode) {
	if err := c.acquireWrite(time.Now().Add(gracefulWriterDrainTimeout), nil); err != nil {
		return
	}
	defer c.releaseWrite()
	c.writer.setDeadline(time.Now().Add(gracefulWriterDrainTimeout))
	err := c.framer.WriteGoAway(lastStreamID, code, nil)
	c.writer.setDeadline(time.Time{})
	if err == nil {
		_ = c.writer.closeAndWait(gracefulWriterDrainTimeout)
	}
}

// isIdle reports an open connection, neither retiring nor draining, with no
// stream in flight.
func (c *clientConn) isIdle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && !c.expired && !c.goAway && c.activeStreams == 0
}

// retiredIdle reports whether c is expired or draining with no stream left.
func (c *clientConn) retiredIdle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return (c.expired || c.goAway) && c.activeStreams == 0 && !c.closed
}

// closeIfIdle closes c if no stream is active, without telling the peer: the
// idle timer, CloseIdleConnections and a peer GOAWAY retire this way.
func (c *clientConn) closeIfIdle() {
	c.retireIdle(false)
}

// retireIdle closes c if no stream is active; with allowDrain, a connection
// marked drainOnRetire sends GOAWAY(NO_ERROR) first.
func (c *clientConn) retireIdle(allowDrain bool) {
	c.pool.mu.Lock()
	retired := c.retireIdleLocked(allowDrain)
	c.pool.mu.Unlock()
	if retired {
		c.finishRetire()
	}
}

// retireIdleLocked takes an idle c out of the pool and reports whether it
// did; finishRetire must follow. With allowDrain and drainOnRetire, the
// GOAWAY sendControl writes on its way out is recorded. pool.mu is held.
func (c *clientConn) retireIdleLocked(allowDrain bool) bool {
	c.mu.Lock()
	if c.closed || c.activeStreams != 0 {
		c.mu.Unlock()
		return false
	}
	if allowDrain && c.drainOnRetire {
		c.goAwayOnExit = &goAwayFrame{lastStreamID: c.lastPromisedStreamID, code: xhttp2.ErrCodeNo}
	}
	c.closeLocked()
	c.mu.Unlock()
	c.pool.retireLocked(c)
	return true
}

// finishRetire closes a retired connection and frees its slot. A recorded
// GOAWAY gets the bounded wait fail gives it; otherwise an idle connection
// has nothing worth draining and the writer is aborted at once.
func (c *clientConn) finishRetire() {
	c.mu.Lock()
	drain := c.goAwayOnExit != nil
	c.mu.Unlock()
	if drain {
		c.awaitSender(2 * gracefulWriterDrainTimeout)
	}
	c.writer.abort(errClientConnClosed)
	_ = c.lease.Close()
	c.pool.retired()
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
	if err := c.acquireWrite(time.Now().Add(gracefulWriterDrainTimeout), nil); err != nil {
		c.writer.abort(errClientConnClosed)
		return
	}
	defer c.releaseWrite()
	_ = c.writer.closeAndWait(gracefulWriterDrainTimeout)
}

// phaseDeadline bounds a phase by timeout within the request's deadline, and
// reports whether the bound is the caller's deadline rather than the phase's
// timeout.
func phaseDeadline(requestDeadline time.Time, timeout time.Duration) (deadline time.Time, callers bool) {
	if timeout <= 0 {
		return requestDeadline, true
	}
	deadline = time.Now().Add(timeout)
	if !requestDeadline.IsZero() && requestDeadline.Before(deadline) {
		return requestDeadline, true
	}
	return deadline, false
}

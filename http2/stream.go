package http2

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

var errStreamClosed = errors.New("http2: stream closed")

type requestBody struct {
	mu           sync.Mutex
	ready        *sync.Cond
	chunks       []requestBodyChunk
	chunkHead    int
	chunkOffset  int
	buffered     int
	err          error
	consume      func(int)
	eofCommit    func() error
	eofCommitted bool
	isClosed     bool
}

type requestBodyChunk struct {
	data       []byte
	release    func([]byte)
	dataBuffer *incomingDataBuffer
}

const maxRequestBodyChunks = 128

func newRequestBody(consume func(int)) *requestBody {
	body := &requestBody{consume: consume}
	body.ready = sync.NewCond(&body.mu)
	return body
}

func (b *requestBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	for b.buffered == 0 && !b.isClosed {
		b.ready.Wait()
	}
	if b.buffered == 0 {
		err := b.err
		var commit func() error
		if err == nil && !b.eofCommitted {
			b.eofCommitted = true
			commit = b.eofCommit
		}
		b.mu.Unlock()
		if commit != nil {
			if err := commit(); err != nil {
				return 0, err
			}
		}
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	n := 0
	for n < len(p) && b.chunkHead < len(b.chunks) {
		chunk := &b.chunks[b.chunkHead]
		copied := copy(p[n:], chunk.data[b.chunkOffset:])
		n += copied
		b.buffered -= copied
		b.chunkOffset += copied
		if b.chunkOffset != len(chunk.data) {
			break
		}
		releaseRequestBodyChunk(chunk)
		*chunk = requestBodyChunk{}
		b.chunkHead++
		b.chunkOffset = 0
	}
	if b.chunkHead == len(b.chunks) {
		b.chunks = b.chunks[:0]
		b.chunkHead = 0
	}
	b.mu.Unlock()
	if n > 0 && b.consume != nil {
		b.consume(n)
	}
	return n, nil
}

func (b *requestBody) setEOFCommit(commit func() error) {
	b.mu.Lock()
	b.eofCommit = commit
	b.mu.Unlock()
}

func (b *requestBody) Close() error {
	b.discardWithError(errStreamClosed)
	return nil
}

func (b *requestBody) writeOwned(p []byte, release func([]byte)) error {
	return b.writeChunk(requestBodyChunk{data: p, release: release})
}

func (b *requestBody) writeIncoming(buffer *incomingDataBuffer) error {
	return b.writeChunk(requestBodyChunk{data: buffer.data, dataBuffer: buffer})
}

func (b *requestBody) writeChunk(incoming requestBodyChunk) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.isClosed {
		releaseRequestBodyChunk(&incoming)
		return errStreamClosed
	}
	if len(incoming.data) == 0 {
		releaseRequestBodyChunk(&incoming)
		return nil
	}
	if len(b.chunks)-b.chunkHead >= maxRequestBodyChunks {
		b.compactLocked(incoming)
	} else {
		if b.chunkHead > 0 && len(b.chunks) == cap(b.chunks) {
			remaining := copy(b.chunks, b.chunks[b.chunkHead:])
			clear(b.chunks[remaining:])
			b.chunks = b.chunks[:remaining]
			b.chunkHead = 0
		}
		b.chunks = append(b.chunks, incoming)
		b.buffered += len(incoming.data)
	}
	b.ready.Broadcast()
	return nil
}

func (b *requestBody) compactLocked(incoming requestBodyChunk) {
	targetLen := b.buffered + len(incoming.data)
	var data []byte
	startChunk := b.chunkHead
	if startChunk < len(b.chunks) {
		first := &b.chunks[startChunk]
		// A previous compaction leaves one body-owned chunk at the front.
		// Grow that chunk geometrically instead of copying the entire buffered
		// body into an exact-sized allocation every 128 small DATA frames.
		if first.release == nil && first.dataBuffer == nil {
			unread := first.data[b.chunkOffset:]
			if b.chunkOffset != 0 {
				copy(first.data, unread)
				unread = first.data[:len(unread)]
			}
			data = slices.Grow(unread, targetLen-len(unread))
			*first = requestBodyChunk{}
			startChunk++
		}
	}
	if data == nil {
		data = make([]byte, 0, targetLen)
	}
	for i := startChunk; i < len(b.chunks); i++ {
		chunk := &b.chunks[i]
		chunkStart := 0
		if i == b.chunkHead {
			chunkStart = b.chunkOffset
		}
		data = append(data, chunk.data[chunkStart:]...)
		releaseRequestBodyChunk(chunk)
		*chunk = requestBodyChunk{}
	}
	data = append(data, incoming.data...)
	releaseRequestBodyChunk(&incoming)
	if len(data) != targetLen {
		panic("BUG: compacted HTTP/2 request body has the wrong length")
	}
	b.chunks = append(b.chunks[:0], requestBodyChunk{data: data})
	b.chunkHead = 0
	b.chunkOffset = 0
	b.buffered = len(data)
}

func (b *requestBody) closeWithError(err error) {
	b.mu.Lock()
	if !b.isClosed {
		b.isClosed = true
		b.err = err
		b.ready.Broadcast()
	}
	b.mu.Unlock()
}

func (b *requestBody) discardWithError(err error) {
	b.mu.Lock()
	b.isClosed = true
	b.err = err
	for i := b.chunkHead; i < len(b.chunks); i++ {
		chunk := &b.chunks[i]
		releaseRequestBodyChunk(chunk)
		*chunk = requestBodyChunk{}
	}
	b.chunks = b.chunks[:0]
	b.chunkHead = 0
	b.chunkOffset = 0
	b.buffered = 0
	b.ready.Broadcast()
	b.mu.Unlock()
}

func releaseRequestBodyChunk(chunk *requestBodyChunk) {
	if chunk.dataBuffer != nil {
		releaseIncomingData(chunk.dataBuffer)
		return
	}
	if chunk.release != nil {
		chunk.release(chunk.data)
	}
}

type serverStream struct {
	streamFlowState

	id             uint32
	conn           *serverConn
	readTimer      *time.Timer
	writeTimer     *time.Timer
	cancelMu       sync.Mutex
	done           chan struct{}
	cancelCause    error
	request        *fasthttp.RequestCtx
	body           *requestBody
	maxBody        int
	bodyBytes      int64
	bufferedBytes  int64
	expectedBody   int64
	unconsumedFlow int64

	remoteClosed   bool
	localClosed    bool
	isReset        bool
	handlerStarted bool
	// hijackRejected is written by the handler goroutine and read by the
	// event loop after the handler-done command establishes ordering.
	hijackRejected     bool
	flushQueued        bool
	handlerGen         uint32
	writeTimeoutGen    uint32
	worker             *streamWorker
	handlerDone        bool
	isPush             bool
	priority           priority
	discardRequestBody bool

	pendingData         []byte
	pendingAck          chan error
	pendingWrite        *streamWrite
	responseEOF         bool
	responseHasTrailers bool
	responseHeaderSent  bool
	responseBytes       int64
	expectedResponse    int64
	responsePumpStarted bool
	responsePumpDone    bool
	hasAbandonedRequest bool
	// hasStreamConn marks a stream handed to a stream handler. Its StreamConn
	// may be used past the handler from another goroutine, so the stream
	// must stay intact once finalized rather than return to the pool.
	hasStreamConn bool

	acceptMu      sync.Mutex
	streamHandler fasthttp.StreamHandler
}

var serverStreamPool sync.Pool

var closedStreamDone = func() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()

func newServerStream(conn *serverConn, id uint32) *serverStream {
	var stream *serverStream
	if value := serverStreamPool.Get(); value != nil {
		stream = value.(*serverStream) //nolint:forcetypeassert
	} else {
		stream = &serverStream{}
	}
	*stream = serverStream{
		id:   id,
		conn: conn,
		streamFlowState: streamFlowState{
			send: sendWindow{window: conn.peerInitialStreamWindow},
			recv: recvWindow{window: int64(conn.config.streamWindowSize)},
		},
		expectedBody:     -1,
		expectedResponse: -1,
	}
	return stream
}

func releaseServerStream(stream *serverStream) {
	*stream = serverStream{}
	serverStreamPool.Put(stream)
}

// HijackRejected records a handler's hijack attempt so the response can explain
// that multiplexed protocols don't support it.
func (s *serverStream) HijackRejected() {
	s.hijackRejected = true
}

func (s *serverStream) Deadline() (time.Time, bool) {
	// Server.ReadTimeout is a transport deadline, and RequestCtx promises
	// successive Deadline calls agree, so it must not be exposed here.
	return time.Time{}, false
}

func (s *serverStream) Done() <-chan struct{} {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.cancelCause != nil {
		return closedStreamDone
	}
	if s.done == nil {
		s.done = make(chan struct{})
	}
	return s.done
}

func (s *serverStream) Err() error {
	s.cancelMu.Lock()
	cause := s.cancelCause
	s.cancelMu.Unlock()
	switch {
	case cause == nil:
		return nil
	case errors.Is(cause, fasthttp.ErrTimeout), errors.Is(cause, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return context.Canceled
	}
}

func (s *serverStream) Value(key any) any {
	return s.conn.ctx.Value(key)
}

func (s *serverStream) cancel(cause error) {
	if cause == nil {
		cause = context.Canceled
	}
	s.cancelMu.Lock()
	if s.cancelCause == nil {
		s.cancelCause = cause
		if s.done != nil {
			close(s.done)
		}
	}
	s.cancelMu.Unlock()
}

func (s *serverStream) cause() error {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	return s.cancelCause
}

func (s *serverStream) WriteInformational(
	statusCode int,
	header *fasthttp.ResponseHeader,
) error {
	if statusCode < 100 || statusCode >= 200 {
		return errors.New("http2: informational status must be between 100 and 199")
	}
	copyHeader := &fasthttp.ResponseHeader{}
	header.CopyTo(copyHeader)
	result := make(chan error, 1)
	command := serverCommand{
		kind:       serverCommandInformational,
		streamID:   s.id,
		statusCode: statusCode,
		header:     copyHeader,
		result:     result,
	}
	select {
	case s.conn.commands <- command:
	case <-s.Done():
		return s.cause()
	}
	select {
	case err := <-result:
		return err
	case <-s.Done():
		return s.cause()
	}
}

func (s *serverStream) Push(target string, opts *fasthttp.PushOptions) error {
	var copiedOpts *fasthttp.PushOptions
	if opts != nil {
		copiedOpts = &fasthttp.PushOptions{Method: opts.Method}
		if opts.Header != nil {
			copiedOpts.Header = &fasthttp.RequestHeader{}
			opts.Header.CopyTo(copiedOpts.Header)
		}
	}
	result := make(chan error, 1)
	command := serverCommand{
		kind:     serverCommandPush,
		streamID: s.id,
		target:   strings.Clone(target),
		pushOpts: copiedOpts,
		result:   result,
	}
	select {
	case s.conn.commands <- command:
	case <-s.Done():
		return s.cause()
	}
	select {
	case err := <-result:
		return err
	case <-s.Done():
		return s.cause()
	}
}

func (s *serverStream) AcceptStream(handler fasthttp.StreamHandler) error {
	if handler == nil {
		return errors.New("http2: stream handler is nil")
	}
	if !s.conn.config.enableExtendedConnect {
		return fasthttp.ErrProtocolNotSupported
	}
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	// A handler TimeoutHandler gave up on may still call this after its
	// request was replaced and released.
	if s.hasAbandonedRequest {
		return errStreamClosed
	}
	if len(s.request.Request.Header.ConnectProtocol()) == 0 {
		return errors.New("http2: request isn't an extended connect")
	}
	if s.streamHandler != nil {
		return errors.New("http2: stream is already accepted")
	}
	s.streamHandler = handler
	return nil
}

// streamConnState carries the deadline and shutdown state shared by the server
// and client StreamConn implementations.
type streamConnState struct {
	netConn net.Conn
	cancel  func() // ends the stream once a deadline passes

	writeMu       sync.Mutex
	mu            sync.Mutex
	readDeadline  streamDeadline
	writeDeadline streamDeadline
	isClosed      bool
	readClosed    bool
	writeClosed   bool
}

// streamDeadline is one direction's deadline. Its timer runs only while an
// operation in that direction is in flight and cancels the stream when the
// deadline passes, so moving the deadline also reaches a blocked call.
type streamDeadline struct {
	at      time.Time
	timer   *time.Timer
	pending int
	expired bool
}

func (c *streamConnState) LocalAddr() net.Addr {
	return c.netConn.LocalAddr()
}

func (c *streamConnState) RemoteAddr() net.Addr {
	return c.netConn.RemoteAddr()
}

func (c *streamConnState) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.setDeadlineLocked(&c.readDeadline, deadline)
	c.setDeadlineLocked(&c.writeDeadline, deadline)
	c.mu.Unlock()
	return nil
}

func (c *streamConnState) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.setDeadlineLocked(&c.readDeadline, deadline)
	c.mu.Unlock()
	return nil
}

func (c *streamConnState) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.setDeadlineLocked(&c.writeDeadline, deadline)
	c.mu.Unlock()
	return nil
}

func (c *streamConnState) setDeadlineLocked(d *streamDeadline, at time.Time) {
	d.at = at
	if d.pending != 0 {
		c.armLocked(d)
	}
}

// armLocked schedules the timer for d.at, or parks it for a zero deadline.
// The timer is kept across operations; expire tolerates a stale run.
func (c *streamConnState) armLocked(d *streamDeadline) {
	switch {
	case d.at.IsZero():
		if d.timer != nil {
			d.timer.Stop()
		}
	case d.timer == nil:
		d.timer = time.AfterFunc(time.Until(d.at), func() { c.expire(d) })
	default:
		d.timer.Reset(time.Until(d.at))
	}
}

// readUnderDeadline reads once. If the read deadline passes while the read
// waits, including a deadline set after it began, the stream is cancelled.
func (c *streamConnState) readUnderDeadline(read io.Reader, p []byte) (int, error) {
	c.mu.Lock()
	if c.isClosed || c.readClosed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	if err := c.beginLocked(&c.readDeadline); err != nil {
		c.mu.Unlock()
		return 0, err
	}
	c.mu.Unlock()
	n, err := read.Read(p)
	c.mu.Lock()
	expired := c.endLocked(&c.readDeadline)
	c.mu.Unlock()
	if expired {
		return n, errStreamTimeout
	}
	return n, err
}

// beginLocked registers an operation, failing it at once past the deadline.
func (c *streamConnState) beginLocked(d *streamDeadline) error {
	if !d.at.IsZero() && !time.Now().Before(d.at) {
		return errStreamTimeout
	}
	d.pending++
	if d.pending == 1 {
		c.armLocked(d)
	}
	return nil
}

// endLocked unregisters an operation and reports whether the deadline
// cancelled the stream under it.
func (c *streamConnState) endLocked(d *streamDeadline) bool {
	d.pending--
	if d.pending == 0 && d.timer != nil {
		d.timer.Stop()
	}
	return d.expired
}

func (c *streamConnState) expire(d *streamDeadline) {
	c.mu.Lock()
	if d.pending == 0 || d.at.IsZero() || time.Now().Before(d.at) {
		c.mu.Unlock()
		return
	}
	d.expired = true
	c.mu.Unlock()
	c.cancel()
}

type streamConn struct {
	streamConnState

	stream *serverStream
	read   io.Reader
}

func (c *streamConn) Read(p []byte) (int, error) {
	return c.readUnderDeadline(c.read, p)
}

func (c *streamConn) cancelOnDeadline() {
	c.stream.conn.cancelStream(c.stream.id, errStreamTimeout)
}

func (c *streamConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	if c.isClosed || c.writeClosed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	if err := c.beginLocked(&c.writeDeadline); err != nil {
		c.mu.Unlock()
		return 0, err
	}
	deadline := c.writeDeadline.at
	c.mu.Unlock()
	n, err := c.write(p, deadline)
	c.mu.Lock()
	expired := c.endLocked(&c.writeDeadline)
	c.mu.Unlock()
	if expired && err == nil {
		err = errStreamTimeout
	}
	return n, err
}

func (c *streamConn) write(p []byte, deadline time.Time) (int, error) {
	write := &streamWrite{result: make(chan streamWriteResult, 1)}
	// p is shared, not cloned: the owner completes a write only after its
	// bytes are copied out or dropped, and Write blocks on that completion.
	command := serverCommand{
		kind:     serverCommandResponseData,
		streamID: c.stream.id,
		data:     p,
		write:    write,
	}
	var expired <-chan time.Time
	if !deadline.IsZero() {
		timer := fasthttp.AcquireTimer(time.Until(deadline))
		defer fasthttp.ReleaseTimer(timer)
		expired = timer.C
	}
	select {
	case c.stream.conn.commands <- command:
	case <-c.stream.Done():
		return 0, c.stream.cause()
	case <-expired:
		return 0, errStreamTimeout
	}
	select {
	case result := <-write.result:
		return result.n, result.err
	case <-c.stream.conn.ownerDone:
		return c.abandonedWriteResult(write)
	case <-expired:
		// An accepted write cannot be abandoned: its bytes may still reach the
		// peer, so only the owner can report the authoritative byte count.
		select {
		case c.stream.conn.commands <- serverCommand{
			kind:     serverCommandCancelWrite,
			streamID: c.stream.id,
			write:    write,
			err:      errStreamTimeout,
		}:
		case <-c.stream.conn.ctx.Done():
		}
		select {
		case result := <-write.result:
			return result.n, result.err
		case <-c.stream.conn.ownerDone:
			return c.abandonedWriteResult(write)
		}
	}
}

// abandonedWriteResult reports a write whose owner stopped. A result the owner
// posted before stopping is authoritative; otherwise no byte was framed.
func (c *streamConn) abandonedWriteResult(write *streamWrite) (int, error) {
	select {
	case result := <-write.result:
		return result.n, result.err
	default:
		return 0, c.stream.cause()
	}
}

func (c *streamConn) Close() error {
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil
	}
	c.isClosed = true
	readClosed := c.readClosed
	writeClosed := c.writeClosed
	c.mu.Unlock()
	var readErr, writeErr error
	if !readClosed {
		readErr = c.CloseRead()
	}
	if !writeClosed {
		writeErr = c.CloseWrite()
	}
	return errors.Join(readErr, writeErr)
}

func (c *streamConn) CloseRead() error {
	c.mu.Lock()
	if c.readClosed {
		c.mu.Unlock()
		return nil
	}
	c.readClosed = true
	c.mu.Unlock()
	result := make(chan error, 1)
	select {
	case c.stream.conn.commands <- serverCommand{
		kind:     serverCommandCloseRead,
		streamID: c.stream.id,
		result:   result,
	}:
	case <-c.stream.Done():
		return c.stream.cause()
	}
	select {
	case err := <-result:
		return err
	case <-c.stream.Done():
		return c.stream.cause()
	}
}

func (c *streamConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.writeClosed {
		c.mu.Unlock()
		return nil
	}
	c.writeClosed = true
	c.mu.Unlock()
	result := make(chan error, 1)
	command := serverCommand{
		kind:     serverCommandResponseEOF,
		streamID: c.stream.id,
		result:   result,
	}
	select {
	case c.stream.conn.commands <- command:
	case <-c.stream.Done():
		return c.stream.cause()
	}
	select {
	case err := <-result:
		return err
	case <-c.stream.Done():
		return c.stream.cause()
	}
}

var (
	_ fasthttp.ProtocolStream              = (*serverStream)(nil)
	_ fasthttp.InformationalResponseWriter = (*serverStream)(nil)
	_ fasthttp.Pusher                      = (*serverStream)(nil)
	_ fasthttp.StreamAccepter              = (*serverStream)(nil)
	_ fasthttp.HijackRejectionNotifier     = (*serverStream)(nil)
	_ fasthttp.StreamConn                  = (*streamConn)(nil)
)

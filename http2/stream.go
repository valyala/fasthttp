package http2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
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

type responseBody struct {
	mu           sync.Mutex
	ready        *sync.Cond
	buffer       bytes.Buffer
	err          error
	eofCommit    func() error
	isClosed     bool
	isDone       bool
	eofCommitted bool
	consume      func(int)
	closeBody    func(int)
	done         func()
}

func newResponseBody(consume, closeBody func(int), done func()) *responseBody {
	body := &responseBody{consume: consume, closeBody: closeBody, done: done}
	body.ready = sync.NewCond(&body.mu)
	return body
}

func (b *responseBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	for b.buffer.Len() == 0 && !b.isClosed {
		b.ready.Wait()
	}
	if b.buffer.Len() == 0 {
		err := b.err
		var commit func() error
		if err == nil && !b.eofCommitted {
			b.eofCommitted = true
			commit = b.eofCommit
		}
		done := b.markDoneLocked()
		b.mu.Unlock()
		if commit != nil {
			err = commit()
		}
		if done != nil {
			done()
		}
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	// Non-empty buffer: Read cannot fail.
	n, _ := b.buffer.Read(p)
	b.mu.Unlock()
	if n > 0 {
		b.consume(n)
	}
	return n, nil
}

func (b *responseBody) setEOFCommit(commit func() error) {
	b.mu.Lock()
	b.eofCommit = commit
	b.mu.Unlock()
}

func (b *responseBody) Close() error {
	b.mu.Lock()
	if b.isDone {
		b.mu.Unlock()
		return nil
	}
	discarded := b.buffer.Len()
	b.buffer.Reset()
	b.isClosed = true
	b.err = net.ErrClosed
	b.ready.Broadcast()
	done := b.markDoneLocked()
	b.mu.Unlock()
	b.closeBody(discarded)
	if done != nil {
		done()
	}
	return nil
}

func (b *responseBody) write(p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.isClosed {
		return errStreamClosed
	}
	// bytes.Buffer.Write never fails.
	b.buffer.Write(p)
	b.ready.Broadcast()
	return nil
}

func (b *responseBody) closeWithError(err error) {
	b.mu.Lock()
	if !b.isClosed {
		b.isClosed = true
		b.err = err
		b.ready.Broadcast()
	}
	b.mu.Unlock()
}

func (b *responseBody) markDoneLocked() func() {
	if b.isDone {
		return nil
	}
	b.isDone = true
	return b.done
}

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

type serverStream struct {
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

	remoteClosed       bool
	localClosed        bool
	isReset            bool
	handlerStarted     bool
	flushQueued        bool
	handlerGen         uint32
	worker             *streamWorker
	handlerDone        bool
	isPush             bool
	pushDepth          uint8
	priority           priority
	discardRequestBody bool

	sendWindow          int64
	recvWindow          int64
	pendingWindowUpdate int64

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
		id:               id,
		conn:             conn,
		sendWindow:       conn.peerInitialStreamWindow,
		recvWindow:       int64(conn.config.streamWindowSize),
		expectedBody:     -1,
		expectedResponse: -1,
	}
	return stream
}

func releaseServerStream(stream *serverStream) {
	*stream = serverStream{}
	serverStreamPool.Put(stream)
}

func (s *serverStream) Deadline() (time.Time, bool) {
	// Server.ReadTimeout bounds receipt of the request body; it is not an
	// application deadline. RequestCtx promises that successive Deadline calls
	// return the same result, so exposing and later clearing that transport
	// deadline would violate context.Context and race with streaming handlers.
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
	if len(s.request.Request.Header.ConnectProtocol()) == 0 {
		return errors.New("http2: request isn't an extended connect")
	}
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	if s.streamHandler != nil {
		return errors.New("http2: stream is already accepted")
	}
	s.streamHandler = handler
	return nil
}

type streamConn struct {
	stream *serverStream
	read   io.Reader

	writeMu       sync.Mutex
	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	isClosed      bool
	readClosed    bool
	writeClosed   bool
}

func (c *streamConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.isClosed || c.readClosed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	deadline := c.readDeadline
	c.mu.Unlock()
	if deadline.IsZero() {
		return c.read.Read(p)
	}
	conn := c.stream.conn
	streamID := c.stream.id
	return readWithStreamDeadline(c.read, p, deadline, func() {
		conn.cancelStream(streamID, timeoutError{})
	})
}

func (c *streamConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	isClosed := c.isClosed
	writeClosed := c.writeClosed
	deadline := c.writeDeadline
	c.mu.Unlock()
	if isClosed || writeClosed {
		return 0, net.ErrClosed
	}
	if !deadline.IsZero() && time.Until(deadline) <= 0 {
		return 0, timeoutError{}
	}

	write := &streamWrite{result: make(chan streamWriteResult, 1)}
	data := bytes.Clone(p)
	command := serverCommand{
		kind:     serverCommandResponseData,
		streamID: c.stream.id,
		data:     data,
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
		return 0, timeoutError{}
	}
	select {
	case result := <-write.result:
		return result.n, result.err
	case <-expired:
		// Once accepted by the connection owner, a write cannot simply be
		// abandoned: its bytes might be flow-control blocked and delivered
		// later. Ask the same owner to cancel this exact write, then wait for
		// its authoritative partial-byte count before returning.
		select {
		case c.stream.conn.commands <- serverCommand{
			kind:     serverCommandCancelWrite,
			streamID: c.stream.id,
			write:    write,
			err:      timeoutError{},
		}:
		case <-c.stream.conn.ctx.Done():
		}
		result := <-write.result
		return result.n, result.err
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

func (c *streamConn) LocalAddr() net.Addr {
	return c.stream.conn.conn.LocalAddr()
}

func (c *streamConn) RemoteAddr() net.Addr {
	return c.stream.conn.conn.RemoteAddr()
}

func (c *streamConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.writeDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *streamConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *streamConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadline = deadline
	c.mu.Unlock()
	return nil
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "http2: stream deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// readWithStreamDeadline blocks in one read, cancelling the stream at deadline
// to unblock it. A read woken that way reports a timeout rather than the
// closed-stream error the cancellation itself produced. The read and the
// deadline race to claim the outcome, so a read that already holds data leaves
// the stream alive instead of reporting success on a cancelled one.
func readWithStreamDeadline(read io.Reader, p []byte, deadline time.Time, cancel func()) (int, error) {
	if time.Until(deadline) <= 0 {
		return 0, timeoutError{}
	}
	var claimed atomic.Bool
	timer := time.AfterFunc(time.Until(deadline), func() {
		if claimed.CompareAndSwap(false, true) {
			cancel()
		}
	})
	n, err := read.Read(p)
	timer.Stop()
	if !claimed.CompareAndSwap(false, true) {
		return n, timeoutError{}
	}
	return n, err
}

type clientStreamConn struct {
	stream *clientStream
	read   *responseBody

	writeMu       sync.Mutex
	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	readClosed    bool
	writeClosed   bool
	closed        bool
}

func (c *clientStreamConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.readClosed || c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	deadline := c.readDeadline
	c.mu.Unlock()
	if deadline.IsZero() {
		return c.read.Read(p)
	}
	conn := c.stream.conn
	streamID := c.stream.id
	return readWithStreamDeadline(c.read, p, deadline, func() {
		conn.resetStream(streamID, xhttp2.ErrCodeCancel, timeoutError{}, false)
	})
}

func (c *clientStreamConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.writeClosed || c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	deadline := c.writeDeadline
	c.mu.Unlock()
	if err := c.stream.conn.sendData(c.stream, p, false, deadline); err != nil {
		if errors.Is(err, fasthttp.ErrTimeout) {
			c.stream.conn.resetStream(c.stream.id, xhttp2.ErrCodeCancel, timeoutError{}, false)
			return 0, timeoutError{}
		}
		return 0, err
	}
	return len(p), nil
}

func (c *clientStreamConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	readClosed := c.readClosed
	writeClosed := c.writeClosed
	c.readClosed = true
	c.writeClosed = true
	deadline := c.writeDeadline
	c.mu.Unlock()
	if !readClosed {
		_ = c.read.Close()
	}
	if !writeClosed {
		c.writeMu.Lock()
		err := c.stream.conn.sendData(c.stream, nil, true, deadline)
		c.writeMu.Unlock()
		return err
	}
	return nil
}

func (c *clientStreamConn) CloseRead() error {
	c.mu.Lock()
	if c.readClosed {
		c.mu.Unlock()
		return nil
	}
	c.readClosed = true
	c.mu.Unlock()
	return c.read.Close()
}

func (c *clientStreamConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.writeClosed {
		c.mu.Unlock()
		return nil
	}
	c.writeClosed = true
	deadline := c.writeDeadline
	c.mu.Unlock()
	return c.stream.conn.sendData(c.stream, nil, true, deadline)
}

func (c *clientStreamConn) LocalAddr() net.Addr {
	return c.stream.conn.conn.LocalAddr()
}

func (c *clientStreamConn) RemoteAddr() net.Addr {
	return c.stream.conn.conn.RemoteAddr()
}

func (c *clientStreamConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.writeDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *clientStreamConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *clientStreamConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadline = deadline
	c.mu.Unlock()
	return nil
}

var (
	_ fasthttp.ProtocolStream              = (*serverStream)(nil)
	_ fasthttp.InformationalResponseWriter = (*serverStream)(nil)
	_ fasthttp.Pusher                      = (*serverStream)(nil)
	_ fasthttp.StreamAccepter              = (*serverStream)(nil)
	_ fasthttp.StreamConn                  = (*streamConn)(nil)
	_ fasthttp.StreamConn                  = (*clientStreamConn)(nil)
)

func releaseRequestBodyChunk(chunk *requestBodyChunk) {
	if chunk.dataBuffer != nil {
		releaseIncomingData(chunk.dataBuffer)
		return
	}
	if chunk.release != nil {
		chunk.release(chunk.data)
	}
}

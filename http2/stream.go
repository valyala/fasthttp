package http2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
)

var errStreamClosed = errors.New("http2: stream closed")

type requestBody struct {
	mu       sync.Mutex
	ready    *sync.Cond
	buffer   bytes.Buffer
	err      error
	consume  func(int)
	isClosed bool
}

type responseBody struct {
	mu        sync.Mutex
	ready     *sync.Cond
	buffer    bytes.Buffer
	err       error
	isClosed  bool
	isDone    bool
	consume   func(int)
	closeBody func(int)
	done      func()
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
		if err == nil {
			err = io.EOF
		}
		done := b.markDoneLocked()
		b.mu.Unlock()
		if done != nil {
			done()
		}
		return 0, err
	}
	n, err := b.buffer.Read(p)
	b.mu.Unlock()
	if n > 0 && b.consume != nil {
		b.consume(n)
	}
	return n, err
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
	if b.closeBody != nil {
		b.closeBody(discarded)
	}
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
	_, err := b.buffer.Write(p)
	if err == nil {
		b.ready.Broadcast()
	}
	return err
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
	for b.buffer.Len() == 0 && !b.isClosed {
		b.ready.Wait()
	}
	if b.buffer.Len() == 0 {
		err := b.err
		if err == nil {
			err = io.EOF
		}
		b.mu.Unlock()
		return 0, err
	}
	n, err := b.buffer.Read(p)
	b.mu.Unlock()
	if n > 0 && b.consume != nil {
		b.consume(n)
	}
	return n, err
}

func (b *requestBody) Close() error {
	b.closeWithError(errStreamClosed)
	return nil
}

func (b *requestBody) write(p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.isClosed {
		return errStreamClosed
	}
	_, err := b.buffer.Write(p)
	if err == nil {
		b.ready.Broadcast()
	}
	return err
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

type serverStream struct {
	id             uint32
	conn           *serverConn
	ctx            context.Context
	cancel         context.CancelCauseFunc
	request        *fasthttp.RequestCtx
	body           *requestBody
	maxBody        int
	bodyBytes      int64
	expectedBody   int64
	unconsumedFlow int64

	remoteClosed   bool
	localClosed    bool
	isReset        bool
	handlerStarted bool
	handlerDone    bool
	isPush         bool
	pushDepth      uint8
	priority       priority

	sendWindow int64
	recvWindow int64

	pendingData         []byte
	pendingAck          chan error
	responseEOF         bool
	responseHasTrailers bool
	responseHeaderSent  bool
	responseBytes       int64
	expectedResponse    int64

	acceptMu      sync.Mutex
	streamHandler fasthttp.StreamHandler
}

func newServerStream(conn *serverConn, id uint32) *serverStream {
	ctx, cancel := context.WithCancelCause(conn.ctx)
	return &serverStream{
		id:               id,
		conn:             conn,
		ctx:              ctx,
		cancel:           cancel,
		sendWindow:       conn.peerInitialStreamWindow,
		recvWindow:       int64(conn.config.streamWindowSize),
		expectedBody:     -1,
		expectedResponse: -1,
	}
}

func (s *serverStream) Deadline() (time.Time, bool) {
	return s.ctx.Deadline()
}

func (s *serverStream) Done() <-chan struct{} {
	return s.ctx.Done()
}

func (s *serverStream) Err() error {
	return s.ctx.Err()
}

func (s *serverStream) Value(key any) any {
	return s.ctx.Value(key)
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
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	}
	select {
	case err := <-result:
		return err
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
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
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	}
	select {
	case err := <-result:
		return err
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	}
}

func (s *serverStream) AcceptStream(handler fasthttp.StreamHandler) error {
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

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	isClosed      bool
}

func (c *streamConn) Read(p []byte) (int, error) {
	return c.read.Read(p)
}

func (c *streamConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	isClosed := c.isClosed
	deadline := c.writeDeadline
	c.mu.Unlock()
	if isClosed {
		return 0, net.ErrClosed
	}

	result := make(chan error, 1)
	data := bytes.Clone(p)
	command := serverCommand{
		kind:     serverCommandResponseData,
		streamID: c.stream.id,
		data:     data,
		result:   result,
	}
	var timer <-chan time.Time
	if !deadline.IsZero() {
		timer = time.After(time.Until(deadline))
	}
	select {
	case c.stream.conn.commands <- command:
	case <-c.stream.ctx.Done():
		return 0, context.Cause(c.stream.ctx)
	case <-timer:
		return 0, timeoutError{}
	}
	select {
	case err := <-result:
		if err != nil {
			return 0, err
		}
		return len(p), nil
	case <-c.stream.ctx.Done():
		return 0, context.Cause(c.stream.ctx)
	case <-timer:
		return 0, timeoutError{}
	}
}

func (c *streamConn) Close() error {
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil
	}
	c.isClosed = true
	c.mu.Unlock()
	return c.CloseWrite()
}

func (c *streamConn) CloseRead() error {
	if c.stream.body != nil {
		return c.stream.body.Close()
	}
	return nil
}

func (c *streamConn) CloseWrite() error {
	result := make(chan error, 1)
	command := serverCommand{
		kind:     serverCommandResponseEOF,
		streamID: c.stream.id,
		result:   result,
	}
	select {
	case c.stream.conn.commands <- command:
	case <-c.stream.ctx.Done():
		return context.Cause(c.stream.ctx)
	}
	select {
	case err := <-result:
		return err
	case <-c.stream.ctx.Done():
		return context.Cause(c.stream.ctx)
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

type clientStreamConn struct {
	stream *clientStream
	read   *responseBody

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
	if !deadline.IsZero() && time.Until(deadline) <= 0 {
		return 0, timeoutError{}
	}

	var expired chan struct{}
	var timer *time.Timer
	if !deadline.IsZero() {
		expired = make(chan struct{})
		timer = time.AfterFunc(time.Until(deadline), func() {
			close(expired)
			c.stream.conn.resetStream(c.stream.id, xhttp2.ErrCodeCancel, timeoutError{}, false)
		})
	}
	n, err := c.read.Read(p)
	if timer != nil && !timer.Stop() {
		select {
		case <-expired:
			return n, timeoutError{}
		default:
		}
	}
	return n, err
}

func (c *clientStreamConn) Write(p []byte) (int, error) {
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
		return c.stream.conn.sendData(c.stream, nil, true, deadline)
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

var _ fasthttp.ProtocolStream = (*serverStream)(nil)
var _ fasthttp.InformationalResponseWriter = (*serverStream)(nil)
var _ fasthttp.Pusher = (*serverStream)(nil)
var _ fasthttp.StreamAccepter = (*serverStream)(nil)
var _ fasthttp.StreamConn = (*streamConn)(nil)
var _ fasthttp.StreamConn = (*clientStreamConn)(nil)

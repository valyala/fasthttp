package http2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

var errStreamClosed = errors.New("http2: stream closed")

var errBodyTruncated = fmt.Errorf("http2: connection closed before the body completed: %w", io.ErrUnexpectedEOF)

// truncationCause is the error a body reader gets when its connection ended
// with cause before END_STREAM: a clean EOF from the transport means the body
// was cut short, not completed.
func truncationCause(cause error) error {
	if cause == nil || errors.Is(cause, io.EOF) {
		return errBodyTruncated
	}
	return cause
}

type requestBody struct {
	mu           sync.Mutex
	ready        *sync.Cond
	chunks       []requestBodyChunk
	chunkHead    int
	chunkOffset  int
	buffered     int
	err          error
	eofCommit    func() error
	eofCommitted bool
	isClosed     bool
	// conn is the owner the body's credit goes to, by streamID: the stream
	// may be back in the pool when the owner applies it. Nil for a body
	// without an owner.
	conn     *serverConn
	streamID uint32
	// pendingCredit and closeRequested are what the owner hasn't applied
	// yet: bytes the reader consumed and the reader's Close, once; a body
	// the owner dropped itself (ownerDiscarded) reports no close.
	// creditQueued marks the body as sitting in the owner's credit queue.
	// All under mu; a reader never waits on the owner to record them.
	pendingCredit  int
	closeRequested bool
	creditQueued   bool
	closePosted    bool
	ownerDiscarded bool
}

type requestBodyChunk struct {
	data       []byte
	release    func([]byte)
	dataBuffer *incomingDataBuffer
}

const maxRequestBodyChunks = 128

func newRequestBody(conn *serverConn, streamID uint32) *requestBody {
	body := &requestBody{conn: conn, streamID: streamID}
	body.ready = sync.NewCond(&body.mu)
	return body
}

func (b *requestBody) Read(p []byte) (int, error) {
	return b.readUntil(p, nil)
}

// readUntil is Read that gives up with errInterrupted once cut is closed while
// it waits; buffered data stays for a later read.
func (b *requestBody) readUntil(p []byte, cut <-chan struct{}) (int, error) {
	b.mu.Lock()
	// With nothing buffered the cut comes first: a Close that reset the
	// stream reports its own cause, not the stream's end.
	for b.buffered == 0 {
		if isClosedChan(cut) {
			b.mu.Unlock()
			return 0, errInterrupted
		}
		if b.isClosed {
			break
		}
		b.ready.Wait()
	}
	if b.buffered == 0 {
		err := b.err
		if err == nil && !b.eofCommitted {
			b.eofCommitted = true
			// Under b.mu: discardWithError, which precedes releasing the
			// request, must not overtake the commit.
			if b.eofCommit != nil {
				err = b.eofCommit()
				b.eofCommit = nil
			}
		}
		b.mu.Unlock()
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
	wake := false
	if n > 0 && b.conn != nil {
		b.pendingCredit += n
		wake = b.queueCreditLocked()
	}
	b.mu.Unlock()
	if wake {
		b.conn.wakeCredit()
	}
	return n, nil
}

// queueCreditLocked hands the body to the owner's credit queue unless it is
// there already, and reports whether the owner needs waking. mu is held; the
// queue mutex is taken inside it, never the reverse.
func (b *requestBody) queueCreditLocked() bool {
	if b.creditQueued {
		return false
	}
	b.creditQueued = true
	b.conn.queueCredit(b)
	return true
}

// takeCredit is the owner's snapshot of what the reader recorded: the bytes
// consumed and whether it closed the body, cleared together with the queued
// mark so the next record queues the body again.
func (b *requestBody) takeCredit() (consumed int, closeRequested bool) {
	b.mu.Lock()
	consumed, closeRequested = b.pendingCredit, b.closeRequested
	b.pendingCredit, b.closeRequested, b.creditQueued = 0, false, false
	b.mu.Unlock()
	return consumed, closeRequested
}

// failure is the error the body ended with, as its readers report it.
func (b *requestBody) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *requestBody) wake() {
	b.mu.Lock()
	b.ready.Broadcast()
	b.mu.Unlock()
}

func (b *requestBody) setEOFCommit(commit func() error) {
	b.mu.Lock()
	b.eofCommit = commit
	b.mu.Unlock()
}

// Close drops the body for its reader and tells the owner, which returns the
// retained flow credit; the owner's own discards post nothing.
func (b *requestBody) Close() error {
	b.mu.Lock()
	post := b.conn != nil && !b.ownerDiscarded && !b.closePosted
	b.closePosted = true
	b.isClosed = true
	b.err = errStreamClosed
	b.dropChunksLocked()
	b.ready.Broadcast()
	wake := false
	if post {
		b.closeRequested = true
		wake = b.queueCreditLocked()
	}
	b.mu.Unlock()
	if wake {
		b.conn.wakeCredit()
	}
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

// complete ends the body cleanly: the peer sent END_STREAM.
func (b *requestBody) complete() {
	b.end(nil)
}

// fail ends the body with cause; a transport EOF is a truncated body.
func (b *requestBody) fail(cause error) {
	b.end(truncationCause(cause))
}

func (b *requestBody) end(err error) {
	b.mu.Lock()
	if !b.isClosed {
		b.isClosed = true
		b.err = err
		b.ready.Broadcast()
	}
	b.mu.Unlock()
}

// discardWithError is the owner's discard: the body ends with err for its
// reader and the retained chunks are freed.
func (b *requestBody) discardWithError(err error) {
	b.mu.Lock()
	b.ownerDiscarded = true
	b.isClosed = true
	b.err = err
	b.dropChunksLocked()
	b.ready.Broadcast()
	b.mu.Unlock()
}

// dropChunks is the owner's discard of the retained chunks and the trailer
// commit; whether the body has ended is unchanged.
func (b *requestBody) dropChunks() {
	b.mu.Lock()
	b.ownerDiscarded = true
	b.dropChunksLocked()
	b.mu.Unlock()
}

func (b *requestBody) dropChunksLocked() {
	b.eofCommit = nil
	for i := b.chunkHead; i < len(b.chunks); i++ {
		chunk := &b.chunks[i]
		releaseRequestBodyChunk(chunk)
		*chunk = requestBodyChunk{}
	}
	b.chunks = b.chunks[:0]
	b.chunkHead = 0
	b.chunkOffset = 0
	b.buffered = 0
}

// staged reports the bytes retained for a reader.
func (b *requestBody) staged() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffered
}

// drain hands the retained bytes to fn in order, frees the chunks and
// returns the trailer commit, if any, for the caller to apply; the owner
// takes the body over.
func (b *requestBody) drain(fn func([]byte)) func() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ownerDiscarded = true
	for i := b.chunkHead; i < len(b.chunks); i++ {
		chunk := &b.chunks[i]
		start := 0
		if i == b.chunkHead {
			start = b.chunkOffset
		}
		fn(chunk.data[start:])
	}
	commit := b.eofCommit
	b.dropChunksLocked()
	return commit
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
	handlerGen     uint32
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

	remoteClosed bool
	localClosed  bool
	isReset      bool
	// wireClosed marks the stream closed in both directions on the wire; it
	// then no longer counts toward SETTINGS_MAX_CONCURRENT_STREAMS.
	wireClosed bool
	// handlerStarted means a job owns the RequestCtx or the response was
	// produced; handlerDone that nothing owns it any more.
	handlerStarted bool
	headRequest    bool
	connectRequest bool
	job            streamJob
	jobState       streamJobState
	// jobErr is the error a jobError reports; refusedDuringJob asks the
	// pending Expect job's completion to run that job instead.
	jobErr           error
	refusedDuringJob bool
	// holdsJobSlot counts the stream toward the running-job bound.
	holdsJobSlot bool
	// stagedBuffered marks a body staging a buffered request's upload for an
	// Expect job; its bytes count toward the buffered budget.
	stagedBuffered bool
	// readTimeout bounds the request body from the end of the header block;
	// writeTimeout bounds queuing the whole response after the handler
	// returns. HeaderReceived may set either per request.
	readTimeout  time.Duration
	writeTimeout time.Duration
	// bodyFramed records that DATA arrived: the request had body framing.
	bodyFramed bool
	// hijackRejected is written by the handler goroutine and read by the
	// event loop only for a handler that wasn't abandoned, which the
	// handler-done command orders.
	hijackRejected     bool
	hijackNoResponse   bool
	flushQueued        bool
	writeTimeoutGen    uint32
	worker             *streamWorker
	handlerDone        bool
	isPush             bool
	priority           priority
	discardRequestBody bool
	// resetSent marks a stream this server sent RST_STREAM for; frames that
	// arrive on it afterwards are ignored.
	resetSent bool

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
	// pumpReader is the response body stream the pump reads, captured when
	// it starts; pumpCanceled marks a pump the connection cut short, whose
	// RequestCtx a body writer may still use.
	pumpReader   io.Reader
	pumpCanceled bool
	// pumpMu orders the reader's close between the pump and the connection:
	// pumpCloseOwned gives it to the connection, pumpClosedReader records
	// the pump's own close.
	pumpMu           sync.Mutex
	pumpCloseOwned   bool
	pumpClosedReader bool
	// hasAbandonedRequest marks a timed-out handler that may still run; it
	// is set under acceptMu.
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
// that multiplexed protocols don't support it; noResponse records that the
// handler meant not to answer.
func (s *serverStream) HijackRejected(noResponse bool) {
	s.hijackRejected = true
	s.hijackNoResponse = noResponse
}

// closePumpReader closes the response body stream from the pump, once, unless
// the connection took the reader over when it canceled the pump. It returns
// the Close error, a recovered panic's included; a Close that already ran,
// or panicked, isn't retried.
func (s *serverStream) closePumpReader(request *fasthttp.RequestCtx) error {
	s.pumpMu.Lock()
	skip := s.pumpCloseOwned || s.pumpClosedReader
	if !skip {
		s.pumpClosedReader = true
	}
	s.pumpMu.Unlock()
	if skip {
		return nil
	}
	return s.conn.closeBodyStream(request)
}

// precedesFinalResponse reports whether the handler may still send frames
// that must come before the final response.
func (s *serverStream) precedesFinalResponse() bool {
	return !s.isReset && !s.localClosed && !s.responseHeaderSent && !s.handlerDone
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

// closedError is the error an operation on the stream gets once it is reset
// or gone: its terminal cause, else errStreamClosed.
func (s *serverStream) closedError() error {
	if cause := s.cause(); cause != nil {
		return cause
	}
	return errStreamClosed
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

// isTunnelRequest reports a CONNECT request, extended or not: the stream is
// the tunnel, with no END_STREAM from the peer to wait for. The
// extended-CONNECT setting is enforced where a request carrying :protocol is
// parsed.
func (s *serverStream) isTunnelRequest() bool {
	return s.request.Request.Header.IsConnect()
}

func (s *serverStream) AcceptStream(handler fasthttp.StreamHandler) error {
	if handler == nil {
		return errors.New("http2: stream handler is nil")
	}
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	// A handler TimeoutHandler gave up on may still call this after its
	// request was replaced and released.
	if s.hasAbandonedRequest {
		return errStreamClosed
	}
	if !s.isTunnelRequest() {
		return errors.New("http2: request isn't a connect")
	}
	if s.streamHandler != nil {
		return errors.New("http2: stream is already accepted")
	}
	s.streamHandler = handler
	return nil
}

// errInterrupted reports an operation cut short by its StreamConn's deadline
// or Close; the StreamConn reports the cut's cause instead.
var errInterrupted = errors.New("http2: stream operation interrupted")

// streamConnState carries the deadline and close state shared by the server
// and client StreamConn implementations.
type streamConnState struct {
	netConn net.Conn
	// wakeRead wakes a Read waiting on the body, wakeWrite a write the cut
	// alone doesn't reach. Both run without mu.
	wakeRead  func()
	wakeWrite func()

	// writeGate serializes writes and END_STREAM; unlike a mutex, its
	// waiters can be cut.
	writeGate     chan struct{}
	mu            sync.Mutex
	readDeadline  streamDeadline
	writeDeadline streamDeadline
	isClosed      bool
	readClosed    bool
	writeClosed   bool
}

// streamDeadline is one direction's deadline. Its timer runs only while an
// operation in that direction is in flight. A passing deadline cuts the
// current generation, which interrupts those operations but leaves the stream
// open; a deadline that no longer lies in the past starts a new generation.
type streamDeadline struct {
	at      time.Time
	timer   *time.Timer
	pending int
	gen     *streamCut
}

// streamCut interrupts the operations begun under it once done is closed;
// cause is set first.
type streamCut struct {
	done  chan struct{}
	cause error
}

func newStreamCut() *streamCut {
	return &streamCut{done: make(chan struct{})}
}

func (g *streamCut) isCut() bool {
	return isClosedChan(g.done)
}

// cut runs under the owning streamConnState's mu.
func (g *streamCut) cut(cause error) {
	if !g.isCut() {
		g.cause = cause
		close(g.done)
	}
}

func isClosedChan(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// interruptedBy reports err, or the cause of gen's cut when err says the
// operation was interrupted.
func interruptedBy(err error, gen *streamCut) error {
	if errors.Is(err, errInterrupted) && gen.isCut() {
		return gen.cause
	}
	return err
}

func (c *streamConnState) init(netConn net.Conn) {
	c.netConn = netConn
	c.writeGate = make(chan struct{}, 1)
	c.readDeadline.gen = newStreamCut()
	c.writeDeadline.gen = newStreamCut()
}

func (c *streamConnState) LocalAddr() net.Addr {
	return c.netConn.LocalAddr()
}

func (c *streamConnState) RemoteAddr() net.Addr {
	return c.netConn.RemoteAddr()
}

func (c *streamConnState) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return net.ErrClosed
	}
	c.setDeadlineLocked(&c.readDeadline, deadline)
	c.setDeadlineLocked(&c.writeDeadline, deadline)
	return nil
}

func (c *streamConnState) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return net.ErrClosed
	}
	c.setDeadlineLocked(&c.readDeadline, deadline)
	return nil
}

func (c *streamConnState) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return net.ErrClosed
	}
	c.setDeadlineLocked(&c.writeDeadline, deadline)
	return nil
}

func (c *streamConnState) setDeadlineLocked(d *streamDeadline, at time.Time) {
	d.at = at
	if d.gen.isCut() && (at.IsZero() || time.Now().Before(at)) {
		d.gen = newStreamCut()
	}
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

// beginLocked registers an operation in d and returns the generation that
// interrupts it, failing at once past the deadline.
func (c *streamConnState) beginLocked(d *streamDeadline) (*streamCut, error) {
	if !d.at.IsZero() && !time.Now().Before(d.at) {
		return nil, errStreamTimeout
	}
	d.pending++
	if d.pending == 1 {
		c.armLocked(d)
	}
	return d.gen, nil
}

func (c *streamConnState) endLocked(d *streamDeadline) {
	d.pending--
	if d.pending == 0 && d.timer != nil {
		d.timer.Stop()
	}
}

func (c *streamConnState) expire(d *streamDeadline) {
	c.mu.Lock()
	if d.pending == 0 || d.at.IsZero() || time.Now().Before(d.at) {
		c.mu.Unlock()
		return
	}
	d.gen.cut(errStreamTimeout)
	c.mu.Unlock()
	if d == &c.readDeadline {
		c.wakeRead()
	} else if c.wakeWrite != nil {
		c.wakeWrite()
	}
}

// closeLocked marks the StreamConn closed and cuts every operation. It reports
// which halves were still open and whether a write was in flight.
func (c *streamConnState) closeLocked() (readOpen, writeOpen, writeActive bool) {
	c.isClosed = true
	readOpen, writeOpen = !c.readClosed, !c.writeClosed
	c.readClosed, c.writeClosed = true, true
	writeActive = c.writeDeadline.pending != 0
	for _, d := range [...]*streamDeadline{&c.readDeadline, &c.writeDeadline} {
		d.gen.cut(net.ErrClosed)
		if d.timer != nil {
			d.timer.Stop()
		}
	}
	return readOpen, writeOpen, writeActive
}

// deadlineChan is closed when at passes; it is nil without a deadline. stop
// releases its timer.
func deadlineChan(at time.Time) (ch <-chan struct{}, stop func()) {
	if at.IsZero() {
		return nil, func() {}
	}
	if !time.Now().Before(at) {
		return closedStreamDone, func() {}
	}
	done := make(chan struct{})
	timer := time.AfterFunc(time.Until(at), func() { close(done) })
	return done, func() { timer.Stop() }
}

// cutReader is a body whose blocked Read gives up once cut is closed; failure
// is the error that ended it, nil while it is open or ended cleanly.
type cutReader interface {
	readUntil(p []byte, cut <-chan struct{}) (int, error)
	failure() error
}

type emptyBody struct{}

func (emptyBody) readUntil([]byte, <-chan struct{}) (int, error) {
	return 0, io.EOF
}

func (emptyBody) failure() error { return nil }

func (c *streamConnState) readUnderDeadline(body cutReader, p []byte) (int, error) {
	c.mu.Lock()
	if c.isClosed || c.readClosed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	if len(p) == 0 {
		// At once, as a TCP connection's: the stream's failure, if any;
		// buffered data stays for the next read.
		c.mu.Unlock()
		return 0, body.failure()
	}
	gen, err := c.beginLocked(&c.readDeadline)
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	n, err := body.readUntil(p, gen.done)
	c.mu.Lock()
	c.endLocked(&c.readDeadline)
	c.mu.Unlock()
	return n, interruptedBy(err, gen)
}

// beginWrite registers a write-side operation, writes or END_STREAM.
func (c *streamConnState) beginWrite() (*streamCut, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed || c.writeClosed {
		return nil, net.ErrClosed
	}
	return c.beginLocked(&c.writeDeadline)
}

func (c *streamConnState) endWrite() {
	c.mu.Lock()
	c.endLocked(&c.writeDeadline)
	c.mu.Unlock()
}

// enterGate waits for the write gate unless gen is cut first, and reports
// whether the write side closed meanwhile.
func (c *streamConnState) enterGate(gen *streamCut) error {
	select {
	case c.writeGate <- struct{}{}:
	case <-gen.done:
		return errInterrupted
	}
	c.mu.Lock()
	closed := c.isClosed || c.writeClosed
	c.mu.Unlock()
	if closed {
		<-c.writeGate
		return net.ErrClosed
	}
	return nil
}

func (c *streamConnState) leaveGate() {
	<-c.writeGate
}

type streamConn struct {
	streamConnState

	stream *serverStream
	body   cutReader
	// inflight is the write the owner may hold, which wakeWrite asks it to
	// end. Guarded by mu.
	inflight *streamWrite
}

func newStreamConn(c *serverConn, stream *serverStream) *streamConn {
	conn := &streamConn{stream: stream, body: emptyBody{}}
	conn.init(c.conn)
	body := stream.body
	if body != nil {
		conn.body = body
	}
	conn.wakeRead = func() {
		if body != nil {
			body.wake()
		}
	}
	conn.wakeWrite = conn.interruptWrite
	return conn
}

func (c *streamConn) Read(p []byte) (int, error) {
	return c.readUnderDeadline(c.body, p)
}

// interruptWrite asks the owner to end the write it holds; one it hasn't taken
// yet sees the cut when it does.
func (c *streamConn) interruptWrite() {
	c.mu.Lock()
	write := c.inflight
	c.mu.Unlock()
	if write == nil {
		return
	}
	select {
	case c.stream.conn.commands <- serverCommand{
		kind:     serverCommandInterruptWrite,
		streamID: c.stream.id,
		write:    write,
	}:
	case <-c.stream.conn.ctx.Done():
	}
}

func (c *streamConn) Write(p []byte) (int, error) {
	gen, err := c.beginWrite()
	if err != nil {
		return 0, err
	}
	n, err := c.writeUnderGate(p, gen)
	c.endWrite()
	return n, interruptedBy(err, gen)
}

func (c *streamConn) writeUnderGate(p []byte, gen *streamCut) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := c.enterGate(gen); err != nil {
		return 0, err
	}
	defer c.leaveGate()
	return c.write(p, gen)
}

func (c *streamConn) write(p []byte, gen *streamCut) (int, error) {
	write := &streamWrite{result: make(chan streamWriteResult, 1), cut: gen.done}
	// p is shared, not cloned: the owner completes a write only after its
	// bytes are copied out or dropped, and Write blocks on that completion.
	command := serverCommand{
		kind:     serverCommandResponseData,
		streamID: c.stream.id,
		data:     p,
		write:    write,
	}
	c.mu.Lock()
	c.inflight = write
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inflight = nil
		c.mu.Unlock()
	}()
	select {
	case c.stream.conn.commands <- command:
	case <-gen.done:
		return 0, errInterrupted
	case <-c.stream.Done():
		return 0, c.stream.cause()
	}
	// An accepted write cannot be abandoned: its bytes may still reach the
	// peer, so only the owner can report the authoritative byte count.
	select {
	case result := <-write.result:
		return result.n, c.streamError(result.err)
	case <-c.stream.conn.ownerDone:
		return c.abandonedWriteResult(write)
	}
}

// streamError reports the owner's answer for a stream that is gone as the
// stream's terminal cause.
func (c *streamConn) streamError(err error) error {
	if errors.Is(err, errStreamClosed) {
		return c.stream.closedError()
	}
	return err
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

// Close ends both halves. A write in flight can't be followed by END_STREAM,
// and an END_STREAM can't be sent past the write deadline, so either resets
// the stream instead.
func (c *streamConn) Close() error {
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil
	}
	writeAt := c.writeDeadline.at
	readOpen, writeOpen, writeActive := c.closeLocked()
	c.mu.Unlock()
	c.wakeRead()
	if writeActive {
		c.reset()
		return nil
	}
	if readOpen {
		_ = c.closeReadOnOwner()
	}
	if writeOpen {
		cut, stop := deadlineChan(writeAt)
		err := c.endStream(cut)
		stop()
		if err != nil {
			c.reset()
		}
	}
	return nil
}

func (c *streamConn) reset() {
	c.stream.cancel(net.ErrClosed)
	c.stream.conn.cancelStream(c.stream.id, net.ErrClosed)
}

func (c *streamConn) CloseRead() error {
	c.mu.Lock()
	if c.readClosed {
		c.mu.Unlock()
		return nil
	}
	c.readClosed = true
	c.readDeadline.gen.cut(net.ErrClosed)
	c.mu.Unlock()
	c.wakeRead()
	return c.closeReadOnOwner()
}

// closeReadOnOwner has the owner drop unread body bytes and return their flow
// credit.
func (c *streamConn) closeReadOnOwner() error {
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
		return c.streamError(err)
	case <-c.stream.Done():
		return c.stream.cause()
	}
}

// CloseWrite sends END_STREAM as a write: the write deadline bounds it, and a
// CloseWrite cut short leaves the write side open.
func (c *streamConn) CloseWrite() error {
	gen, err := c.beginWrite()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	if err != nil {
		return err
	}
	err = c.closeWriteUnderGate(gen)
	c.endWrite()
	return interruptedBy(err, gen)
}

func (c *streamConn) closeWriteUnderGate(gen *streamCut) error {
	if err := c.enterGate(gen); err != nil {
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
	defer c.leaveGate()
	if err := c.endStream(gen.done); err != nil {
		return err
	}
	c.mu.Lock()
	c.writeClosed = true
	c.mu.Unlock()
	return nil
}

// endStream hands END_STREAM to the owner. Once it has it the write side is
// closed for good, so only the handoff can be cut; a cut that already
// happened wins over the handoff.
func (c *streamConn) endStream(cut <-chan struct{}) error {
	if isClosedChan(cut) {
		return errInterrupted
	}
	result := make(chan error, 1)
	select {
	case c.stream.conn.commands <- serverCommand{
		kind:     serverCommandResponseEOF,
		streamID: c.stream.id,
		result:   result,
	}:
	case <-cut:
		return errInterrupted
	case <-c.stream.Done():
		return c.stream.cause()
	}
	select {
	case err := <-result:
		return c.streamError(err)
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

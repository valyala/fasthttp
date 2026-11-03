package http2

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type incomingFrameKind uint8

const (
	incomingFrameUnknown incomingFrameKind = iota
	incomingFrameSettings
	incomingFrameHeaders
	incomingFrameData
	incomingFrameRST
	incomingFrameWindowUpdate
	incomingFramePing
	incomingFrameGoAway
	incomingFramePriorityUpdate
	incomingFramePriority
	incomingFrameInvalidPushPromise
	incomingFrameStreamError
)

type incomingFrame struct {
	kind         incomingFrameKind
	streamID     uint32
	endStream    bool
	ack          bool
	truncated    bool
	flowLength   int
	data         []byte
	fields       []hpack.HeaderField
	settings     []xhttp2.Setting
	increment    uint32
	errCode      xhttp2.ErrCode
	lastStreamID uint32
	pingData     [8]byte
	priority     string
	dependency   uint32
	hasPriority  bool
	morePending  bool
	frameType    xhttp2.FrameType
	err          error
	fieldStorage *incomingHeaderFieldStorage
	dataBuffer   *incomingDataBuffer
}

type incomingHeaderFieldStorage struct {
	fields []hpack.HeaderField
}

type incomingDataBuffer struct {
	data []byte
}

type responsePumpBuffer struct {
	data []byte
}

var (
	incomingHeaderFieldsPool sync.Pool
	incomingDataPool         sync.Pool
	responsePumpBufferPool   sync.Pool
)

var shutdownPingData = [8]byte{'f', 'a', 's', 't', 'h', '2', 'g', 'o'}

var (
	errDataOnHalfClosedStream    = errors.New("http2: data on half-closed remote stream")
	errStreamRecvWindowExceeded  = errors.New("http2: stream receive window exceeded")
	errRequestBodyLengthMismatch = errors.New("http2: request body exceeds content-length")
	errBufferedBodyLimit         = errors.New("http2: connection buffered request body limit exceeded")
	errShutdownBeforePreface     = errors.New("http2: server shut down before the client preface")
	errPeerClosedBeforePreface   = errors.New("http2: peer closed before the client preface")
	errTooManyPrioritizedStreams = errors.New("http2: prioritized idle streams and open streams exceed the concurrent stream limit")
	errFrameOnReservedStream     = errors.New("http2: client frame on a reserved stream")
	errRequestHeaderTooLarge     = errors.New("http2: request header list exceeds MaxHeaderListSize")
	errHijackWithoutResponse     = errors.New("http2: handler hijacked the request without a response")
)

type serverCommandKind uint8

const (
	serverCommandUnknown serverCommandKind = iota
	serverCommandHandlerDone
	serverCommandInformational
	serverCommandResponseData
	serverCommandResponseEOF
	serverCommandResponsePumpDone
	serverCommandStreamHandlerDone
	serverCommandPush
	serverCommandCloseRead
	serverCommandRequestReadTimeout
	serverCommandCancelStream
	serverCommandResponseWriteTimeout
	serverCommandInterruptWrite
	serverCommandJobDone
	// serverCommandInspect runs inspect on the owner; tests read owner state
	// through it.
	serverCommandInspect
)

type streamWriteResult struct {
	n   int
	err error
}

// streamWrite is owned by the connection goroutine. A StreamConn.Write waits
// on result after its command is accepted, so the owner can report exactly how
// many bytes were framed before a deadline or reset ended the remainder.
type streamWrite struct {
	result  chan streamWriteResult
	cut     <-chan struct{}
	written int
	done    bool
}

func (w *streamWrite) isCut() bool {
	return isClosedChan(w.cut)
}

func (w *streamWrite) complete(err error) {
	if w == nil || w.done {
		return
	}
	w.done = true
	w.result <- streamWriteResult{n: w.written, err: err}
}

type serverCommand struct {
	kind       serverCommandKind
	streamID   uint32
	generation uint32
	requestCtx *fasthttp.RequestCtx
	statusCode int
	header     *fasthttp.ResponseHeader
	data       []byte
	write      *streamWrite
	err        error
	result     chan error
	target     string
	pushOpts   *fasthttp.PushOptions
	inspect    func(*serverConn)
}

// fail reports err through whichever completion path the command carries.
func (c *serverCommand) fail(err error) {
	switch {
	case c.write != nil:
		c.write.complete(err)
	case c.result != nil:
		c.result <- err
	}
}

type serverError struct {
	code xhttp2.ErrCode
	err  error
}

func (e *serverError) Error() string {
	return e.err.Error()
}

func (e *serverError) Unwrap() error {
	return e.err
}

type serverConn struct {
	connFlowState
	headerEncoder

	protocolContext *fasthttp.ProtocolServerContext
	server          *fasthttp.Server
	config          serverConfig
	conn            net.Conn
	ctx             context.Context //nolint:containedctx
	cancel          context.CancelCauseFunc
	framer          *xhttp2.Framer
	frames          *frameReader
	idleWorkers     []*streamWorker
	allWorkers      []*streamWorker
	headerDecoder   *headerCodec
	writer          *asyncFrameWriter

	events    chan incomingFrame
	commands  chan serverCommand
	ownerDone chan struct{}
	// creditQueue holds the request bodies with credit the event loop hasn't
	// applied (consumed bytes, a reader's Close); a body is queued once until
	// the loop takes its record. creditWake is sent to without blocking. The
	// loop detaches the queue under creditMu and releases it before any body
	// mutex or the writer; producers take a body's mutex, then creditMu.
	creditMu    sync.Mutex
	creditQueue []*requestBody
	creditSpare []*requestBody
	creditWake  chan struct{}
	// readDone is closed when readLoop exits; readFailed records a read error
	// the event loop acted on, goAwaySent a GOAWAY it wrote. Both decide
	// whether the exit drains the peer's input before Close.
	readDone   chan struct{}
	readFailed bool
	goAwaySent bool
	workers    sync.WaitGroup
	// readDeadlineMu orders the reader's header-block deadline against the
	// serving goroutine's interruption of the reader, which readInterrupted
	// makes final.
	readDeadlineMu  sync.Mutex
	readInterrupted bool

	streams map[uint32]*serverStream
	// openStreams counts client streams open on the wire, the quantity
	// SETTINGS_MAX_CONCURRENT_STREAMS bounds; runningJobs and jobQueue bound
	// the workers, as a stream the peer reset keeps its job.
	openStreams     uint32
	runningJobs     uint32
	jobQueue        []*serverStream
	jobQueueHead    int
	flushQueue      []uint32
	flushBuckets    [8][]uint32
	pendingHandlers int
	handlerGen      uint32
	cycleTime       time.Time
	// idleSince is when the last stream closed; idleRearm asks the event
	// loop to measure the idle timeout from it. lastWorkerReap bounds how
	// often parked workers are reaped on a busy connection; workerReapTimer
	// reaps them on a silent one, armed for the oldest parked worker.
	idleSince          time.Time
	idleRearm          bool
	lastWorkerReap     time.Time
	workerReapTimer    *time.Timer
	workerReapArmed    bool
	lastClientStreamID uint32
	lastProcessedID    uint32
	isGoingAway        bool
	peerGoingAway      bool
	peerGoAwayLastID   uint32
	isDraining         bool
	// shutdownTimer bounds the wait for the drain PING's ACK by pingTimeout.
	shutdownTimer *time.Timer

	peerAllowsPush bool
	// settingsAcked reports the peer acknowledged this server's SETTINGS;
	// until then it may still use the defaults (RFC 9113 §6.5.3).
	settingsAcked bool

	bufferedRequestBytes    int64
	nextPushStreamID        uint32
	activePushes            uint32
	priorityUpdates         map[uint32]priority
	priorityPrunedThroughID uint32
	// closedClientStreams records, per recently closed stream, whether this
	// server reset it; later frames on such a stream are ignored (RFC 9113
	// §5.1).
	closedClientStreams      map[uint32]bool
	closedClientStreamOrder  []uint32
	closedClientStreamCursor int
	rapidResetWindowStart    time.Time
	rapidResetCount          uint32
}

func (h *serverHandler) ServeConn(ctx *fasthttp.ProtocolServerContext, c net.Conn) error {
	conn := newServerConn(ctx, c, &h.config)
	return conn.serve(nil)
}

func newServerConn(
	protocolContext *fasthttp.ProtocolServerContext,
	c net.Conn,
	config *serverConfig,
) *serverConn {
	ctx, cancel := context.WithCancelCause(context.Background())
	conn := &serverConn{
		protocolContext:     protocolContext,
		server:              protocolContext.Server(),
		config:              *config,
		conn:                c,
		ctx:                 ctx,
		cancel:              cancel,
		ownerDone:           make(chan struct{}),
		streams:             make(map[uint32]*serverStream),
		connFlowState:       newConnFlowState(int64(config.connectionWindowSize)),
		peerAllowsPush:      true,
		nextPushStreamID:    2,
		priorityUpdates:     make(map[uint32]priority),
		closedClientStreams: make(map[uint32]bool),
	}
	conn.initHeaderEncoder(config.maxEncoderTableSize)
	return conn
}

func (c *serverConn) initQueues() {
	if c.events != nil {
		return
	}
	maxCommands := min(int(c.config.maxConcurrentStreams)*2+32, maxQueuedCommands)
	c.events = make(chan incomingFrame, 64)
	c.commands = make(chan serverCommand, maxCommands)
	c.creditWake = make(chan struct{}, 1)
}

func (c *serverConn) serve(upgrade *serverUpgrade) (retErr error) {
	defer func() {
		c.cancel(retErr)
		c.failPendingStreams(retErr)
		c.stopStreamWorkers()
		c.workers.Wait()
		c.releaseAllStreams()
		c.dropQueuedCredit()
		graceful := c.goAwaySent && !c.readFailed
		if c.writer != nil {
			if err := c.writer.closeAndWait(c.config.pingTimeout); err != nil {
				graceful = false
				if retErr == nil {
					retErr = err
				}
			}
		}
		if graceful {
			c.drainBeforeClose()
		}
		_ = c.conn.Close()
	}()

	var err error
	if upgrade != nil {
		err = c.startUpgraded(upgrade)
	} else {
		err = c.startDirect()
	}
	// Neither is an error of the connection: HTTP/1's ServeConn reports a
	// peer that connects and leaves with nil too.
	if errors.Is(err, errShutdownBeforePreface) || errors.Is(err, errPeerClosedBeforePreface) {
		return nil
	}
	if err != nil {
		return err
	}

	c.readDone = make(chan struct{})
	go c.readLoop()
	return c.eventLoop()
}

// rstAvoidanceDelay bounds the input drain before Close, as in net/http.
const rstAvoidanceDelay = 500 * time.Millisecond

// drainBeforeClose ends a connection whose final GOAWAY went out over a
// working transport: it stops the read loop, half-closes when the connection
// offers it, and reads the peer's remaining input for a bounded time. A
// close with unread input sends RST on Linux, which drops the response bytes
// still queued.
func (c *serverConn) drainBeforeClose() {
	if c.readDone == nil {
		return
	}
	// The read loop blocks in readFrame; its event send no longer blocks
	// with the context canceled.
	c.interruptReader()
	<-c.readDone
	if closeWriter, ok := c.conn.(interface{ CloseWrite() error }); ok {
		if err := closeWriter.CloseWrite(); err != nil {
			return
		}
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(rstAvoidanceDelay)); err != nil {
		return
	}
	discard := make([]byte, 4<<10)
	for {
		if _, err := c.conn.Read(discard); err != nil {
			return
		}
	}
}

// failPendingStreams unblocks every goroutine waiting on a stream once the
// event loop has stopped. Closing ownerDone afterwards tells StreamConn writers
// that a command still queued will never run.
func (c *serverConn) failPendingStreams(cause error) {
	for _, stream := range c.streams {
		stream.cancel(cause)
		c.cancelResponsePump(stream)
		if stream.body != nil {
			stream.body.fail(cause)
		}
		if stream.pendingAck != nil {
			stream.pendingAck <- cause
			stream.pendingAck = nil
		}
		if stream.pendingWrite != nil {
			stream.pendingWrite.complete(cause)
			stream.pendingWrite = nil
		}
	}
	close(c.ownerDone)
}

// startDirect prepares a connection that speaks HTTP/2 from its first byte.
// Half-open connections parked before the preface are cheap for a peer to
// create, so nothing is allocated until readClientPreface returns.
func (c *serverConn) startDirect() error {
	if err := validateTLSConnection(c.conn); err != nil {
		// RFC 9113 §9.2.2: the close is announced. No framer exists yet, and
		// the write is best effort.
		direct := directFrameWriter{conn: c.conn, timeout: c.config.writeByteTimeout}
		_ = xhttp2.NewFramer(direct, nil).WriteGoAway(0, xhttp2.ErrCodeInadequateSecurity, []byte(err.Error()))
		return err
	}
	if err := c.readClientPreface(true); err != nil {
		if errors.Is(err, errShutdownBeforePreface) || errors.Is(err, errPeerClosedBeforePreface) {
			return err
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, _ = io.CopyN(io.Discard, c.conn, 64<<10)
		return err
	}
	c.installFramer()
	if err := c.writeInitialSettings(); err != nil {
		return fmt.Errorf("http2: writing initial settings: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("http2: flushing initial settings: %w", err)
	}
	c.initQueues()
	return nil
}

// startUpgraded answers an h2c upgrade on the raw connection, then joins the
// same post-preface setup. The upgraded request is owed a response, so a
// shutdown doesn't cut the preface read short; the preface timeout bounds it.
func (c *serverConn) startUpgraded(upgrade *serverUpgrade) error {
	direct := directFrameWriter{conn: c.conn, timeout: c.config.writeByteTimeout}
	if _, err := direct.Write(upgradeResponse); err != nil {
		return fmt.Errorf("http2: writing upgrade response: %w", err)
	}
	c.framer = xhttp2.NewFramer(direct, nil)
	if err := c.applySettings(upgrade.settings); err != nil {
		return fmt.Errorf("http2: applying upgrade settings: %w", err)
	}
	if err := c.writeInitialSettings(); err != nil {
		return fmt.Errorf("http2: writing initial settings: %w", err)
	}
	if err := c.readClientPreface(false); err != nil {
		return err
	}
	c.installFramer()
	c.initQueues()
	c.bootstrapUpgradedStream(upgrade.request, upgrade.config)
	return nil
}

// installFramer builds the async writer, buffered reader, and framer the
// connection uses once the peer is known to speak HTTP/2.
func (c *serverConn) installFramer() {
	writeBufferSize := c.server.WriteBufferSize
	if writeBufferSize <= 0 {
		writeBufferSize = defaultWriteBufferSize
	}
	c.writer = newAsyncFrameWriter(
		c.conn,
		writeBufferSize,
		defaultWriteQueueBatches,
		c.config.writeByteTimeout,
	)
	readBufferSize := c.server.ReadBufferSize
	if readBufferSize <= 0 {
		readBufferSize = defaultReadBufferSize
	}
	reader := bufio.NewReaderSize(c.conn, readBufferSize)
	c.framer = xhttp2.NewFramer(c.writer, reader)
	c.framer.SetReuseFrames()
	c.frames = newFrameReader(c.framer, reader)
	if c.server.ReadTimeout > 0 {
		c.frames.headerBlockStarted = c.armHeaderBlockDeadline
	}
	// A smaller table applies once the peer acknowledges SETTINGS.
	c.headerDecoder = newHeaderCodec(
		max(c.config.maxDecoderTableSize, defaultHeaderTableSize),
		c.config.maxHeaderListSize,
	)
	c.framer.SetMaxReadFrameSize(c.config.maxReadFrameSize)
}

func (c *serverConn) eventLoop() error {
	serverDone := c.protocolContext.Done()
	c.idleSince = time.Now()
	c.lastWorkerReap = c.idleSince
	var idleTimer *time.Timer
	if c.config.idleTimeout > 0 {
		idleTimer = time.NewTimer(c.config.idleTimeout)
		defer idleTimer.Stop()
	}
	idleArmed := idleTimer != nil
	// Owned by this goroutine and never pooled: a pooled 1ms timer expires so
	// often that releasing it mid-expiry trips initTimer's sanity check.
	var coalesceTimer *time.Timer
	coalesceArmed := false
	defer func() {
		if c.shutdownTimer != nil {
			c.shutdownTimer.Stop()
		}
		if coalesceTimer != nil {
			coalesceTimer.Stop()
		}
		if c.workerReapTimer != nil {
			c.workerReapTimer.Stop()
		}
	}()
	c.cycleTime = c.idleSince
	for {
		if c.isGoingAway && len(c.streams) == 0 {
			return nil
		}
		expectMore := false
		// cycleTime is taken before each event: a worker parked after a long
		// silence gets a current timestamp.
		select {
		case event := <-c.events:
			c.cycleTime = time.Now()
			expectMore = event.morePending
			if closeConnection, err := c.handleIncomingEvent(&event); closeConnection {
				return err
			}
		case <-timerChannel(c.workerReapTimer):
			c.cycleTime = time.Now()
			c.workerReapArmed = false
			c.reapIdleStreamWorkers()
			c.armWorkerReap()
		case <-timerChannel(idleTimer):
			c.cycleTime = time.Now()
			c.reapIdleStreamWorkers()
			switch {
			case len(c.streams) != 0:
				idleArmed = false
			case !c.receivedSettings:
				// A peer that never completed the handshake gets the final
				// GOAWAY at once, not a PING round trip it may never answer.
				if err := c.finishGracefulShutdown(); err != nil {
					return err
				}
			default:
				if err := c.beginDrain(); err != nil {
					return err
				}
			}
		case <-timerChannel(c.shutdownTimer):
			if err := c.finishGracefulShutdown(); err != nil {
				return err
			}
		case command := <-c.commands:
			c.cycleTime = time.Now()
			if err := c.processCommand(&command); err != nil {
				return c.failCommand(err)
			}
		case <-c.creditWake:
			c.cycleTime = time.Now()
			if err := c.applyQueuedCredit(); err != nil {
				return c.failCommand(err)
			}
		case <-serverDone:
			serverDone = nil
			if err := c.beginDrain(); err != nil {
				return err
			}
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
		// One burst becomes one write. Dry channels don't end the batch while
		// frames are buffered or started handlers haven't reported back.
		coalesceProgress := false
	drain:
		for range 63 {
			select {
			case event := <-c.events:
				expectMore = event.morePending
				coalesceProgress = true
				if closeConnection, err := c.handleIncomingEvent(&event); closeConnection {
					return err
				}
				continue
			case command := <-c.commands:
				coalesceProgress = true
				if err := c.processCommand(&command); err != nil {
					return c.failCommand(err)
				}
				continue
			case <-c.creditWake:
				// Credit joins the batch: a WINDOW_UPDATE held back to the
				// flush stalls the upload.
				coalesceProgress = true
				if err := c.applyQueuedCredit(); err != nil {
					return c.failCommand(err)
				}
				continue
			default:
			}
			if !expectMore && c.pendingHandlers == 0 {
				break
			}
			if !coalesceArmed {
				if coalesceTimer == nil {
					coalesceTimer = time.NewTimer(flushCoalesceTimeout)
				} else {
					coalesceTimer.Reset(flushCoalesceTimeout)
				}
				coalesceArmed = true
				coalesceProgress = false
			}
			select {
			case event := <-c.events:
				expectMore = event.morePending
				coalesceProgress = true
				if closeConnection, err := c.handleIncomingEvent(&event); closeConnection {
					return err
				}
			case command := <-c.commands:
				coalesceProgress = true
				if err := c.processCommand(&command); err != nil {
					return c.failCommand(err)
				}
			case <-c.creditWake:
				coalesceProgress = true
				if err := c.applyQueuedCredit(); err != nil {
					return c.failCommand(err)
				}
			case <-coalesceTimer.C:
				// The bound is on silence, not on the batch: while handlers
				// keep reporting back, keep collecting them. Silence writes
				// off the stragglers so they cannot tax later flushes.
				if coalesceProgress {
					coalesceProgress = false
					coalesceTimer.Reset(flushCoalesceTimeout)
					continue
				}
				c.pendingHandlers = 0
				c.handlerGen++
				coalesceArmed = false
				break drain
			case <-c.ctx.Done():
				return context.Cause(c.ctx)
			}
		}
		if coalesceArmed {
			coalesceTimer.Stop()
			coalesceArmed = false
		}
		if err := c.flushResponses(); err != nil {
			return err
		}
		if err := c.writer.Flush(); err != nil {
			return err
		}
		if c.cycleTime.Sub(c.lastWorkerReap) >= maxIdleStreamWorkerDuration {
			c.lastWorkerReap = c.cycleTime
			c.reapIdleStreamWorkers()
			c.armWorkerReap()
		}
		if idleTimer != nil {
			// The timeout runs from the moment the last stream closed, also
			// when a stream came and went within this round.
			switch {
			case len(c.streams) != 0:
				if idleArmed {
					idleTimer.Stop()
					idleArmed = false
				}
			case !idleArmed || c.idleRearm:
				idleTimer.Reset(max(c.config.idleTimeout-time.Since(c.idleSince), 0))
				idleArmed = true
			}
			c.idleRearm = false
		}
	}
}

func (c *serverConn) handleIncomingEvent(event *incomingFrame) (bool, error) {
	defer releaseIncomingFrame(event)
	if event.err != nil && event.kind != incomingFrameStreamError {
		if !isPeerFrameError(event.err) {
			c.readFailed = true
		}
		if writerErr := c.writer.err(); writerErr != nil {
			return true, writerErr
		}
		if errors.Is(event.err, io.EOF) {
			if len(c.streams) == 0 {
				return true, nil
			}
			// A peer gone with requests in flight reads as HTTP/1's partial
			// request followed by FIN; the worker pool doesn't log it. No
			// GOAWAY, as for the idle peer's EOF.
			return true, io.ErrUnexpectedEOF
		}
		return true, c.failConnection(errorCode(event.err), event.err)
	}
	if err := c.processFrame(event); err != nil {
		if protocolErr, ok := errors.AsType[*serverError](err); ok {
			return true, c.failConnection(protocolErr.code, protocolErr.err)
		}
		return true, c.failConnection(xhttp2.ErrCodeInternal, err)
	}
	return false, nil
}

// isPeerFrameError reports a read-loop error the peer's frames caused rather
// than the transport; the GOAWAY it earns can still be drained before Close.
func isPeerFrameError(err error) bool {
	if errors.Is(err, xhttp2.ErrFrameTooLarge) {
		return true
	}
	if _, ok := errors.AsType[xhttp2.ConnectionError](err); ok {
		return true
	}
	_, ok := errors.AsType[xhttp2.StreamError](err)
	return ok
}

func (c *serverConn) failCommand(err error) error {
	if protocolErr, ok := errors.AsType[*serverError](err); ok {
		return c.failConnection(protocolErr.code, protocolErr.err)
	}
	return c.failConnection(xhttp2.ErrCodeInternal, err)
}

// readClientPreface reads the connection preface. With interruptOnShutdown a
// server shutdown ends the wait with errShutdownBeforePreface.
func (c *serverConn) readClientPreface(interruptOnShutdown bool) error {
	if c.protocolContext.CleartextPrefaceConsumed() {
		return nil
	}
	prefaceTimeout := c.server.ReadTimeout
	if prefaceTimeout <= 0 {
		prefaceTimeout = c.config.idleTimeout
	}
	if prefaceTimeout <= 0 {
		prefaceTimeout = defaultPrefaceTimeout
	}
	if prefaceTimeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(prefaceTimeout)); err != nil {
			return err
		}
	}
	preface := make([]byte, len(clientPreface))
	stopWatch := func() bool { return false }
	if interruptOnShutdown {
		stopWatch = c.interruptReadOnShutdown()
	}
	_, err := io.ReadFull(c.conn, preface)
	if interrupted := stopWatch(); err != nil {
		if interrupted {
			return errShutdownBeforePreface
		}
		// io.ReadFull's EOF is zero bytes; a partial preface is
		// io.ErrUnexpectedEOF.
		if errors.Is(err, io.EOF) {
			return errPeerClosedBeforePreface
		}
		return fmt.Errorf("http2: reading client preface: %w", err)
	}
	if string(preface) != clientPreface {
		return errors.New("http2: invalid client preface")
	}
	if prefaceTimeout > 0 {
		if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
			return err
		}
	}
	return nil
}

// applyDecoderTableSize enforces the advertised HPACK table size once the
// peer acknowledges it. The decoder is readLoop's, and the size applies from
// this point in the frame stream.
func (c *serverConn) applyDecoderTableSize() {
	c.headerDecoder.setMaxTableSize(c.config.maxDecoderTableSize)
}

// interruptReadOnShutdown expires the read deadline once the server starts
// shutting down; until the preface arrives nothing else watches for that.
// stop waits for the watcher and reports whether it fired.
func (c *serverConn) interruptReadOnShutdown() (stop func() bool) {
	done := c.protocolContext.Done()
	if done == nil {
		return func() bool { return false }
	}
	stopped := make(chan struct{})
	fired := make(chan bool, 1)
	go func() {
		select {
		case <-done:
			_ = c.conn.SetReadDeadline(time.Unix(1, 0))
			fired <- true
		case <-stopped:
			fired <- false
		}
	}()
	return func() bool {
		close(stopped)
		return <-fired
	}
}

func (c *serverConn) writeInitialSettings() error {
	settings := []xhttp2.Setting{
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
	windowIncrement := uint32(c.config.connectionWindowSize - 65535) // #nosec G115
	if windowIncrement != 0 {
		return c.framer.WriteWindowUpdate(0, windowIncrement)
	}
	return nil
}

func (c *serverConn) readLoop() {
	defer close(c.readDone)
	for {
		frame, err := c.frames.readFrame()
		event := incomingFrame{err: err}
		if err != nil {
			if readErr, ok := errors.AsType[*frameReadError](err); ok {
				event.frameType = readErr.frameType
			}
			if streamError, ok := errors.AsType[xhttp2.StreamError](err); ok {
				event.kind = incomingFrameStreamError
				event.streamID = streamError.StreamID
				event.errCode = streamError.Code
			}
		} else {
			if headers, ok := frame.(*headersFrame); ok {
				event = c.decodeIncomingHeaders(headers)
				if c.frames.headerBlockStarted != nil {
					c.clearHeaderBlockDeadline()
				}
			} else {
				event = incomingFrameFromWire(frame.(xhttp2.Frame)) //nolint:forcetypeassert
				if event.kind == incomingFrameSettings && event.ack {
					c.applyDecoderTableSize()
				}
			}
			// decodeIncomingHeaders may consume CONTINUATIONs, so check last.
			event.morePending = c.frames.completeFrameBuffered()
		}
		select {
		case c.events <- event:
		case <-c.ctx.Done():
			return
		}
		if event.err != nil && event.kind != incomingFrameStreamError {
			return
		}
	}
}

// armHeaderBlockDeadline bounds one header block, HEADERS payload and
// CONTINUATIONs, by Server.ReadTimeout, as HTTP/1 bounds its request header
// read; waiting for the next frame header is outside it. Reader goroutine.
func (c *serverConn) armHeaderBlockDeadline() {
	c.setReaderDeadline(time.Now().Add(c.server.ReadTimeout))
}

func (c *serverConn) clearHeaderBlockDeadline() {
	c.setReaderDeadline(time.Time{})
}

func (c *serverConn) setReaderDeadline(deadline time.Time) {
	c.readDeadlineMu.Lock()
	if !c.readInterrupted {
		_ = c.conn.SetReadDeadline(deadline)
	}
	c.readDeadlineMu.Unlock()
}

// interruptReader expires the reader's deadline for good; the reader's own
// arming and clearing never overwrite it.
func (c *serverConn) interruptReader() {
	c.readDeadlineMu.Lock()
	c.readInterrupted = true
	_ = c.conn.SetReadDeadline(time.Unix(1, 0))
	c.readDeadlineMu.Unlock()
}

func (c *serverConn) decodeIncomingHeaders(frame *headersFrame) incomingFrame {
	event := incomingFrame{
		kind:        incomingFrameHeaders,
		streamID:    frame.streamID,
		endStream:   frame.StreamEnded(),
		hasPriority: frame.hasPriority,
	}
	if event.hasPriority {
		event.dependency = frame.streamDep
	}
	fieldStorage := acquireIncomingHeaderFields(8)
	decoded, truncated, invalid, err := c.headerDecoder.decode(
		c.framer,
		frame.streamID,
		frame,
		fieldStorage.fields,
	)
	event.fields = decoded
	event.fieldStorage = fieldStorage
	event.truncated = truncated
	switch {
	case err != nil:
		event.err = err
	case invalid != nil:
		event.kind = incomingFrameStreamError
		event.err = invalid
		event.errCode = xhttp2.ErrCodeProtocol
	}
	return event
}

// incomingFrameFromWire copies reused x/net frame slices into pooled event
// storage. HEADERS never arrive here: frameReader decodes them itself.
func incomingFrameFromWire(frame xhttp2.Frame) incomingFrame {
	header := frame.Header()
	event := incomingFrame{streamID: header.StreamID}
	switch frame := frame.(type) {
	case *xhttp2.SettingsFrame:
		event.kind = incomingFrameSettings
		event.ack = frame.IsAck()
		if !event.ack {
			// SETTINGS arrives once or twice per connection; pooling it is noise.
			event.settings = make([]xhttp2.Setting, frame.NumSettings())
			for i := range frame.NumSettings() {
				event.settings[i] = frame.Setting(i)
			}
		}
	case *xhttp2.DataFrame:
		event.kind = incomingFrameData
		event.endStream = frame.StreamEnded()
		event.flowLength = int(frame.Header().Length)
		event.dataBuffer = copyIncomingData(frame.Data())
		if event.dataBuffer != nil {
			event.data = event.dataBuffer.data
		}
	case *xhttp2.RSTStreamFrame:
		event.kind = incomingFrameRST
		event.errCode = frame.ErrCode
	case *xhttp2.WindowUpdateFrame:
		event.kind = incomingFrameWindowUpdate
		event.increment = frame.Increment
	case *xhttp2.PingFrame:
		event.kind = incomingFramePing
		event.ack = frame.IsAck()
		event.pingData = frame.Data
	case *xhttp2.GoAwayFrame:
		event.kind = incomingFrameGoAway
		event.lastStreamID = frame.LastStreamID
		event.errCode = frame.ErrCode
	case *xhttp2.PriorityUpdateFrame:
		event.kind = incomingFramePriorityUpdate
		event.streamID = frame.PrioritizedStreamID
		event.priority = strings.Clone(frame.Priority)
	case *xhttp2.PriorityFrame:
		event.kind = incomingFramePriority
		event.dependency = frame.StreamDep
	case *xhttp2.PushPromiseFrame:
		event.kind = incomingFrameInvalidPushPromise
	default:
		event.kind = incomingFrameUnknown
	}
	return event
}

func acquireIncomingHeaderFields(length int) *incomingHeaderFieldStorage {
	if value := incomingHeaderFieldsPool.Get(); value != nil {
		storage := value.(*incomingHeaderFieldStorage) //nolint:forcetypeassert
		if cap(storage.fields) >= length {
			storage.fields = storage.fields[:0]
			return storage
		}
	}
	return &incomingHeaderFieldStorage{fields: make([]hpack.HeaderField, 0, length)}
}

func copyIncomingData(source []byte) *incomingDataBuffer {
	if len(source) == 0 {
		return nil
	}
	var buffer *incomingDataBuffer
	if value := incomingDataPool.Get(); value != nil {
		buffer = value.(*incomingDataBuffer) //nolint:forcetypeassert
	} else {
		buffer = &incomingDataBuffer{}
	}
	if cap(buffer.data) < len(source) {
		buffer.data = make([]byte, len(source))
	} else {
		buffer.data = buffer.data[:len(source)]
	}
	copy(buffer.data, source)
	return buffer
}

func releaseIncomingFrame(event *incomingFrame) {
	if event.fieldStorage != nil {
		for i := range event.fields {
			event.fields[i] = hpack.HeaderField{}
		}
		if cap(event.fields) <= 128 {
			event.fieldStorage.fields = event.fields[:0]
			incomingHeaderFieldsPool.Put(event.fieldStorage)
		}
	}
	if event.dataBuffer != nil {
		releaseIncomingData(event.dataBuffer)
	}
}

func releaseIncomingData(buffer *incomingDataBuffer) {
	if cap(buffer.data) <= 1<<20 {
		buffer.data = buffer.data[:0]
		incomingDataPool.Put(buffer)
	}
}

func (c *serverConn) processFrame(event *incomingFrame) error {
	if !c.receivedSettings {
		if event.kind != incomingFrameSettings || event.ack {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: first frame isn't settings")}
		}
		c.receivedSettings = true
	}

	switch event.kind {
	case incomingFrameUnknown:
		return nil
	case incomingFrameSettings:
		return c.processSettings(event)
	case incomingFrameHeaders:
		return c.processHeaders(event)
	case incomingFrameData:
		return c.processData(event)
	case incomingFrameRST:
		return c.processRST(event)
	case incomingFrameWindowUpdate:
		return c.processWindowUpdate(event)
	case incomingFramePing:
		if event.ack && c.isDraining && event.pingData == shutdownPingData {
			return c.finishGracefulShutdown()
		}
		if !event.ack {
			return c.framer.WritePing(true, event.pingData)
		}
		return nil
	case incomingFrameGoAway:
		if c.peerGoingAway && event.lastStreamID > c.peerGoAwayLastID {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: peer increased GOAWAY last stream id")}
		}
		c.peerGoingAway = true
		c.peerGoAwayLastID = event.lastStreamID
		return nil
	case incomingFramePriorityUpdate:
		return c.processPriorityUpdate(event)
	case incomingFramePriority:
		if event.streamID == event.dependency && !c.closedClientStreams[event.streamID] {
			return c.resetStream(event.streamID, xhttp2.ErrCodeProtocol, errors.New("http2: stream depends on itself"))
		}
		return nil
	case incomingFrameInvalidPushPromise:
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: client sent push promise")}
	case incomingFrameStreamError:
		return c.processHeaderStreamError(event)
	default:
		return nil
	}
}

func (c *serverConn) processHeaderStreamError(event *incomingFrame) error {
	if stream := c.streams[event.streamID]; stream != nil {
		return c.resetStream(event.streamID, event.errCode, event.err)
	}
	if c.closedClientStreams[event.streamID] {
		return nil
	}
	if event.frameType == xhttp2.FrameWindowUpdate && c.streamIsIdle(event.streamID) {
		return &serverError{
			code: xhttp2.ErrCodeProtocol,
			err:  errors.New("http2: invalid window update on idle stream"),
		}
	}
	if event.streamID == 0 || event.streamID&1 == 0 || event.streamID <= c.lastClientStreamID {
		return &serverError{
			code: xhttp2.ErrCodeProtocol,
			err:  errors.New("http2: malformed headers used an invalid client stream id"),
		}
	}
	c.lastClientStreamID = event.streamID
	delete(c.priorityUpdates, event.streamID)
	c.trackClosedClientStream(event.streamID, true)
	if err := c.recordRapidReset(); err != nil {
		return err
	}
	return c.framer.WriteRSTStream(event.streamID, event.errCode)
}

func (c *serverConn) processSettings(event *incomingFrame) error {
	if event.ack {
		c.processSettingsAck()
		return nil
	}
	if err := c.applySettings(event.settings); err != nil {
		return err
	}
	return c.framer.WriteSettingsAck()
}

func (c *serverConn) applySettings(settings []xhttp2.Setting) error {
	type finalSettings struct {
		headerTableSize, enablePush, maxConcurrentStreams  uint32
		initialWindowSize, maxFrameSize, maxHeaderListSize uint32
		minHeaderTableSize                                 uint32
		hasHeaderTableSize, hasEnablePush                  bool
		hasMaxConcurrentStreams, hasInitialWindowSize      bool
		hasMaxFrameSize, hasMaxHeaderListSize              bool
	}
	var final finalSettings
	maxInitialWindowDelta := int64(math.MinInt64)
	for _, setting := range settings {
		if err := validateSettingValue(setting); err != nil {
			return err
		}
		switch setting.ID {
		case xhttp2.SettingHeaderTableSize:
			if !final.hasHeaderTableSize || setting.Val < final.minHeaderTableSize {
				final.minHeaderTableSize = setting.Val
			}
			final.headerTableSize = setting.Val
			final.hasHeaderTableSize = true
		case xhttp2.SettingEnablePush:
			final.enablePush = setting.Val
			final.hasEnablePush = true
		case xhttp2.SettingMaxConcurrentStreams:
			final.maxConcurrentStreams = setting.Val
			final.hasMaxConcurrentStreams = true
		case xhttp2.SettingInitialWindowSize:
			delta := int64(setting.Val) - c.peerInitialStreamWindow
			if delta > maxInitialWindowDelta {
				maxInitialWindowDelta = delta
			}
			final.initialWindowSize = setting.Val
			final.hasInitialWindowSize = true
		case xhttp2.SettingMaxFrameSize:
			final.maxFrameSize = setting.Val
			final.hasMaxFrameSize = true
		case xhttp2.SettingMaxHeaderListSize:
			final.maxHeaderListSize = setting.Val
			final.hasMaxHeaderListSize = true
		}
	}

	// Repeated INITIAL_WINDOW_SIZE deltas telescope from the value at frame
	// start, so checking the largest one preserves overflow detection.
	if final.hasInitialWindowSize {
		for _, stream := range c.streams {
			if stream.send.window+maxInitialWindowDelta > math.MaxInt32 {
				return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: stream send window overflow")}
			}
		}
		finalDelta := int64(final.initialWindowSize) - c.peerInitialStreamWindow
		for _, stream := range c.streams {
			stream.send.window += finalDelta
		}
		c.peerInitialStreamWindow = int64(final.initialWindowSize)
	}
	if final.hasHeaderTableSize {
		// The encoder signals the smallest size as well as the last one
		// (RFC 7541 §4.2).
		c.encoder.SetMaxDynamicTableSize(min(final.minHeaderTableSize, c.config.maxEncoderTableSize))
		c.encoder.SetMaxDynamicTableSize(min(final.headerTableSize, c.config.maxEncoderTableSize))
	}
	if final.hasEnablePush {
		c.peerAllowsPush = final.enablePush == 1
	}
	if final.hasMaxConcurrentStreams {
		c.peerMaxConcurrentStreams = final.maxConcurrentStreams
	}
	if final.hasMaxFrameSize {
		c.peerMaxFrameSize = int(final.maxFrameSize)
	}
	if final.hasMaxHeaderListSize {
		c.peerMaxHeaderListSize = uint64(final.maxHeaderListSize)
	}
	// ENABLE_CONNECT_PROTOCOL and NO_RFC7540_PRIORITIES affect features
	// initiated by the peer; validation happens above and their values are
	// checked at the corresponding use sites.
	return nil
}

// validateSettingValue is the part of a SETTINGS check that depends on the
// value alone; applySettings adds the connection-dependent checks.
func validateSettingValue(setting xhttp2.Setting) error {
	if err := setting.Valid(); err != nil {
		return &serverError{code: errorCode(err), err: err}
	}
	switch setting.ID {
	case xhttp2.SettingEnablePush:
		if setting.Val > 1 {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid SETTINGS_ENABLE_PUSH")}
		}
	case xhttp2.SettingMaxFrameSize:
		if setting.Val < defaultMaxFrameSize || setting.Val > 1<<24-1 {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid SETTINGS_MAX_FRAME_SIZE")}
		}
	case xhttp2.SettingEnableConnectProtocol, xhttp2.SettingNoRFC7540Priorities:
		if setting.Val > 1 {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid boolean setting")}
		}
	}
	return nil
}

// processSettingsAck enforces the stream window that was relaxed to the RFC
// 9113 default until the peer acknowledged it. readLoop does the same for the
// HPACK table.
func (c *serverConn) processSettingsAck() {
	if c.settingsAcked {
		return
	}
	delta := int64(c.config.streamWindowSize) - c.initialStreamRecvWindow()
	c.settingsAcked = true
	if delta != 0 {
		for _, stream := range c.streams {
			stream.recv.window += delta
		}
	}
}

// initialStreamRecvWindow is the window the peer assumes for a new stream.
func (c *serverConn) initialStreamRecvWindow() int64 {
	window := int64(c.config.streamWindowSize)
	if !c.settingsAcked {
		window = max(window, 65535)
	}
	return window
}

func (c *serverConn) processHeaders(event *incomingFrame) error {
	if existing := c.streams[event.streamID]; existing != nil {
		if existing.resetSent {
			return nil
		}
		// RFC 9113 §5.1: reserved (local) until the push response HEADERS go
		// out, half-closed (remote) after.
		if existing.isPush && !existing.responseHeaderSent {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errFrameOnReservedStream}
		}
		if event.hasPriority && event.streamID == event.dependency {
			return c.resetStream(event.streamID, xhttp2.ErrCodeProtocol, errors.New("http2: stream depends on itself"))
		}
		if event.truncated {
			return c.resetStream(event.streamID, xhttp2.ErrCodeEnhanceYourCalm, errInvalidRequestHeaders)
		}
		return c.processTrailers(existing, event)
	}
	if event.streamID == 0 || event.streamID&1 == 0 {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid client stream id")}
	}
	if event.streamID <= c.lastClientStreamID {
		if resetSent, wasClosed := c.closedClientStreams[event.streamID]; wasClosed {
			if resetSent {
				return nil
			}
			return &serverError{code: xhttp2.ErrCodeStreamClosed, err: errors.New("http2: headers on closed stream")}
		}
		// A request on a lower ID reuses or skips a stream: a connection
		// error (RFC 9113 §5.1.1). Trailers name a stream that is closed,
		// implicitly or with its tombstone aged out of the ring.
		if len(event.fields) != 0 && event.fields[0].IsPseudo() {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: reused or skipped client stream id")}
		}
		return c.resetClosedStream(event.streamID)
	}
	c.lastClientStreamID = event.streamID
	if event.hasPriority && event.streamID == event.dependency {
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeProtocol, true)
	}
	if c.isGoingAway {
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeRefusedStream, false)
	}
	// Pushes are server-initiated; SETTINGS_MAX_CONCURRENT_STREAMS limits
	// only the peer's.
	if c.openStreams >= c.config.maxConcurrentStreams {
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeRefusedStream, false)
	}
	// The stream's own hint applies to it; RFC 9218 §7.1 bounds the hints
	// left for idle streams plus the open streams, this one included.
	hint, hasHint := c.priorityUpdates[event.streamID]
	c.prunePriorityUpdates()
	if c.prioritizedStreamsExceed(1) {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errTooManyPrioritizedStreams}
	}

	stream := newServerStream(c, event.streamID)
	stream.priority = priority{urgency: 3}
	if hasHint {
		stream.priority = hint
	}
	stream.recv.window = c.initialStreamRecvWindow()
	requestCtx := c.protocolContext.AcquireRequestCtx(c.conn, stream)
	stream.request = requestCtx
	if event.truncated {
		return c.refuseOversizedRequest(stream, event)
	}
	parsed, err := populateRequest(requestCtx, event.fields, c.protocolContext, c.config.enableExtendedConnect)
	if err != nil {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(err)
		releaseServerStream(stream)
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeProtocol, true)
	}
	expectedBody := parsed.contentLength
	var requestConfig fasthttp.RequestConfig
	if c.server.HeaderReceived != nil {
		requestConfig = c.server.HeaderReceived(&requestCtx.Request.Header)
	}
	isTunnel := stream.isTunnelRequest()
	c.applyRequestConfig(stream, &requestConfig, isTunnel)
	stream.expectedBody = expectedBody
	if parsed.priority != "" && !hasHint {
		if hint, parseErr := parsePriority(parsed.priority); parseErr == nil {
			stream.priority = hint
		}
	}
	c.streams[event.streamID] = stream
	c.openStreams++
	c.lastProcessedID = event.streamID
	// CONNECT streams are tunnels: they never send END_STREAM, so a request
	// read timeout would kill them mid-conversation. applyRequestConfig
	// cleared maxBody for the same reason.
	if !event.endStream && !isTunnel && stream.readTimeout > 0 {
		c.armRequestReadTimeout(stream)
	}
	// Refused as HTTP/1 refuses them: ErrorHandler answers, the upload is
	// dropped. HTTP/1's parser fails on the declaration before GetOnly
	// applies.
	refusal := parsed.refusal
	switch {
	case refusal != nil:
	case c.server.GetOnly && !requestCtx.Request.Header.IsGet() && !requestCtx.Request.Header.IsHead():
		refusal = fasthttp.ErrGetOnly
	case stream.maxBody > 0 && expectedBody > int64(stream.maxBody):
		refusal = fasthttp.ErrBodyTooLarge
	}
	if refusal != nil {
		c.refuseRequest(stream, refusal)
		if event.endStream {
			return c.finishRequestBody(stream)
		}
		return nil
	}
	switch {
	case expectsContinue(&requestCtx.Request.Header):
		// The Expect job answers first, as HTTP/1 consults the callbacks
		// for every request carrying the expectation; the upload is staged
		// in the body until it does. An outstanding job keeps
		// finishRequestBody from starting the handler.
		c.newRequestBodyStream(stream)
		stream.stagedBuffered = !c.server.StreamRequestBody && !isTunnel
		c.startJob(stream, jobExpect)
		if event.endStream {
			return c.finishRequestBody(stream)
		}
	case event.endStream:
		// A declared length means body framing: the stream is empty rather
		// than absent, as HTTP/1's ContinueReadBodyStream has it.
		if (c.server.StreamRequestBody || isTunnel) && expectedBody >= 0 {
			c.newRequestBodyStream(stream)
			c.attachRequestBodyStream(stream)
		}
		return c.finishRequestBody(stream)
	case c.server.StreamRequestBody || isTunnel:
		// A tunnel's handler runs at the headers, as HTTP/1 runs a CONNECT's;
		// the body stream is the tunnel's read side.
		c.newRequestBodyStream(stream)
		c.attachRequestBodyStream(stream)
		c.startJob(stream, jobHandler)
	}
	return nil
}

// applyRequestConfig sets a stream's body limit and timeouts: the Server's
// values, a HeaderReceived override where positive, and no body cap for a
// streamed body or a CONNECT tunnel, as HTTP/1 has it. readTimeout bounds
// the request body from the end of the header block, writeTimeout the
// response from the handler's return.
func (c *serverConn) applyRequestConfig(stream *serverStream, config *fasthttp.RequestConfig, isTunnel bool) {
	stream.maxBody = c.server.MaxRequestBodySize
	if stream.maxBody <= 0 {
		stream.maxBody = fasthttp.DefaultMaxRequestBodySize
	}
	stream.readTimeout = c.server.ReadTimeout
	stream.writeTimeout = c.server.WriteTimeout
	if config.MaxRequestBodySize > 0 {
		stream.maxBody = config.MaxRequestBodySize
	}
	if config.ReadTimeout > 0 {
		stream.readTimeout = config.ReadTimeout
	}
	if config.WriteTimeout > 0 {
		stream.writeTimeout = config.WriteTimeout
	}
	if isTunnel || c.server.StreamRequestBody {
		stream.maxBody = 0
	}
}

// refuseOversizedRequest registers a stream whose header block exceeded
// MaxHeaderListSize and answers it as HTTP/1 answers a header it won't read
// in full: through Server.ErrorHandler with its *fasthttp.ErrSmallBuffer,
// 431 by default. The request holds the pseudo-headers that decoded;
// HeaderReceived doesn't see the partial header, and the upload is dropped.
func (c *serverConn) refuseOversizedRequest(stream *serverStream, event *incomingFrame) error {
	populateTruncatedRequest(stream.request, event.fields)
	c.applyRequestConfig(stream, &fasthttp.RequestConfig{}, false)
	c.streams[event.streamID] = stream
	c.openStreams++
	c.lastProcessedID = event.streamID
	c.refuseRequest(stream, c.protocolContext.HeaderTooLargeError(errRequestHeaderTooLarge))
	if event.endStream {
		return c.finishRequestBody(stream)
	}
	return nil
}

// refuseRequest answers a request its handler never gets through
// Server.ErrorHandler on a worker, as HTTP/1's error response does. DATA is
// credited and dropped from here on.
func (c *serverConn) refuseRequest(stream *serverStream, cause error) {
	c.stopRequestReadTimeout(stream)
	stream.discardRequestBody = true
	stream.jobErr = cause
	c.startJob(stream, jobError)
}

// handlerPending reports that no job owns the request yet: a refusal can
// still answer in the handler's place.
func (s *serverStream) handlerPending() bool {
	return s.jobState == jobStateNone && !s.handlerStarted || s.jobState == jobStateAwaitingBody
}

// newRequestBodyStream creates the stream's body, whose credit reaches the
// event loop through the credit queue by stream ID: the stream may be back in
// the pool when it is applied.
func (c *serverConn) newRequestBodyStream(stream *serverStream) {
	stream.body = newRequestBody(c, stream.id)
}

// queueCredit adds body to the credit queue; the body's mutex is held.
func (c *serverConn) queueCredit(body *requestBody) {
	c.creditMu.Lock()
	c.creditQueue = append(c.creditQueue, body)
	c.creditMu.Unlock()
}

// wakeCredit tells the event loop the credit queue has entries; a wake
// already pending covers them.
func (c *serverConn) wakeCredit() {
	select {
	case c.creditWake <- struct{}{}:
	default:
	}
}

// applyQueuedCredit applies every queued body's record: a reader's Close
// drops the rest of the upload and returns all retained credit, as HTTP/1
// drains an unread body, which covers the bytes it consumed; consumed bytes
// alone return their credit. A record for a stream that is gone, reset,
// already discarded, or that owns another body is dropped: the credit was
// returned when the stream ended.
func (c *serverConn) applyQueuedCredit() error {
	c.creditMu.Lock()
	queue := c.creditQueue
	c.creditQueue = c.creditSpare[:0]
	c.creditMu.Unlock()
	defer func() {
		clear(queue)
		c.creditSpare = queue[:0]
	}()
	for i, body := range queue {
		queue[i] = nil
		consumed, closeRequested := body.takeCredit()
		stream := c.streams[body.streamID]
		if stream == nil || stream.body != body || stream.isReset {
			continue
		}
		if closeRequested {
			if err := c.discardRequestBody(stream); err != nil {
				return err
			}
			continue
		}
		if stream.discardRequestBody {
			continue
		}
		stream.unconsumedFlow -= int64(consumed)
		if stream.unconsumedFlow < 0 {
			return errors.New("http2: request body flow accounting underflow")
		}
		if err := c.consumeRequestBytes(stream, int64(consumed)); err != nil {
			return err
		}
	}
	return nil
}

// dropQueuedCredit forgets the queued records once the event loop is gone,
// so finished bodies aren't retained.
func (c *serverConn) dropQueuedCredit() {
	c.creditMu.Lock()
	clear(c.creditQueue)
	c.creditQueue = nil
	c.creditMu.Unlock()
	clear(c.creditSpare)
	c.creditSpare = nil
}

func (c *serverConn) attachRequestBodyStream(stream *serverStream) {
	bodySize := -1
	if stream.expectedBody >= 0 {
		bodySize = int(stream.expectedBody)
	}
	stream.request.Request.SetBodyStream(stream.body, bodySize)
}

// expectStatus answers Expect: 100-continue as the HTTP/1 serve loop does.
func (c *serverConn) expectStatus(requestCtx *fasthttp.RequestCtx) int {
	switch {
	case c.server.ExpectHandler != nil:
		return c.server.ExpectHandler(requestCtx)
	case c.server.ContinueHandler != nil && !c.server.ContinueHandler(&requestCtx.Request.Header):
		return fasthttp.StatusExpectationFailed
	default:
		return fasthttp.StatusContinue
	}
}

func (c *serverConn) runExpectJob(stream *serverStream) {
	statusCode := c.expectStatus(stream.request)
	c.markAbandonedIfTimedOut(stream, stream.request)
	c.postJobDone(stream, statusCode)
}

func (c *serverConn) runErrorJob(stream *serverStream) {
	c.writeErrorResponse(stream.request, stream.jobErr)
	c.markAbandonedIfTimedOut(stream, stream.request)
	c.postJobDone(stream, 0)
}

// markAbandonedIfTimedOut keeps a stream whose job set a timeout response out
// of the pool: the job's goroutine may keep using requestCtx, which still
// points at the stream. Marked by the job rather than the event loop, which
// may never process the completion.
func (c *serverConn) markAbandonedIfTimedOut(stream *serverStream, requestCtx *fasthttp.RequestCtx) {
	if requestCtx.LastTimeoutErrorResponse() == nil {
		return
	}
	stream.acceptMu.Lock()
	stream.hasAbandonedRequest = true
	stream.acceptMu.Unlock()
}

// writeErrorResponse builds the response of a request answered without its
// handler, as HTTP/1's error path does: Server.ErrorHandler, else 431 for an
// oversized header, 408 for a timeout and 400 for anything else.
func (c *serverConn) writeErrorResponse(requestCtx *fasthttp.RequestCtx, err error) {
	_, headerTooLarge := errors.AsType[*fasthttp.ErrSmallBuffer](err)
	switch {
	case c.server.ErrorHandler != nil:
		c.server.ErrorHandler(requestCtx, err)
	case headerTooLarge:
		requestCtx.Error("Too big request header", fasthttp.StatusRequestHeaderFieldsTooLarge)
	case isTimeout(err):
		requestCtx.Error("Request timeout", fasthttp.StatusRequestTimeout)
	default:
		requestCtx.Error("Error when parsing request", fasthttp.StatusBadRequest)
	}
}

func (c *serverConn) postJobDone(stream *serverStream, statusCode int) {
	select {
	case c.commands <- serverCommand{
		kind:       serverCommandJobDone,
		streamID:   stream.id,
		generation: stream.handlerGen,
		statusCode: statusCode,
	}:
	case <-c.ctx.Done():
	}
}

// handleJobDone continues a stream whose Expect or error job returned; the
// job's worker and slot were released. A reset stream only finalizes.
func (c *serverConn) handleJobDone(stream *serverStream, command *serverCommand) error {
	kind := stream.job
	stream.job = jobNone
	stream.jobState = jobStateFinished
	stream.handlerDone = true
	if stream.isReset {
		c.maybeFinalizeStream(stream)
		return nil
	}
	if kind == jobError {
		return c.respondWithoutHandler(stream)
	}
	if stream.refusedDuringJob {
		// Refused while the Expect job ran: ErrorHandler answers instead.
		c.startJob(stream, jobError)
		return nil
	}
	if command.statusCode != fasthttp.StatusContinue {
		stream.request.Response.SetStatusCode(command.statusCode)
		return c.respondWithoutHandler(stream)
	}
	// No 100 for a request the peer already completed (RFC 9110 §10.1.1).
	if !stream.remoteClosed {
		block, err := c.encodeContinueHeaders()
		if err != nil {
			return err
		}
		if err := c.writeHeaderBlock(stream.id, false, block); err != nil {
			return err
		}
	}
	if c.server.StreamRequestBody || stream.isTunnelRequest() {
		// A request complete without body framing gets what processHeaders
		// gives one without the expectation: an empty stream for a declared
		// length, none otherwise; its trailers, if any, apply at once.
		if !stream.remoteClosed || stream.bodyFramed || stream.expectedBody >= 0 {
			c.attachRequestBodyStream(stream)
		} else {
			commit := stream.body.drain(func([]byte) {})
			stream.body = nil
			if commit != nil {
				if err := commit(); err != nil {
					return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
				}
			}
			reportUndeclaredLength(stream)
		}
		c.startJob(stream, jobHandler)
		return nil
	}
	if err := c.drainStagedBody(stream); err != nil {
		return err
	}
	if stream.remoteClosed {
		reportUndeclaredLength(stream)
		c.startJob(stream, jobHandler)
		return nil
	}
	stream.jobState = jobStateAwaitingBody
	return nil
}

// drainStagedBody moves the upload staged during an Expect job, already
// counted toward the buffered budget, into the buffered request and returns
// its flow credit.
func (c *serverConn) drainStagedBody(stream *serverStream) error {
	body := stream.body
	staged := int64(body.staged())
	request := &stream.request.Request
	commit := body.drain(func(chunk []byte) { request.AppendBody(chunk) })
	stream.body = nil
	stream.stagedBuffered = false
	stream.unconsumedFlow -= staged
	if stream.unconsumedFlow < 0 {
		return errors.New("http2: request body flow accounting underflow")
	}
	if err := c.consumeRequestBytes(stream, staged); err != nil {
		return err
	}
	if commit != nil {
		if err := commit(); err != nil {
			return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
		}
	}
	return nil
}

// respondWithoutHandler sends what stream.request holds as the response of a
// request no handler runs for. The rest of the upload is credited and
// dropped; finishResponse resets a request still open.
func (c *serverConn) respondWithoutHandler(stream *serverStream) error {
	stream.handlerStarted = true
	stream.handlerDone = true
	stream.jobState = jobStateFinished
	if err := c.discardRequestBody(stream); err != nil {
		return err
	}
	return c.handleHandlerDone(stream, stream.request)
}

// discardRequestBody switches the stream to dropping its upload: retained
// chunks are freed and their credit returned, later DATA is credited only.
func (c *serverConn) discardRequestBody(stream *serverStream) error {
	stream.discardRequestBody = true
	if stream.body != nil {
		stream.body.dropChunks()
	}
	amount := stream.unconsumedFlow
	stream.unconsumedFlow = 0
	return c.consumeRequestBytes(stream, amount)
}

func expectsContinue(header *fasthttp.RequestHeader) bool {
	return bytes.EqualFold(header.Peek("expect"), strContinue)
}

var strContinue = []byte("100-continue")

// rejectNewStream refuses a stream before it is registered. Only a refusal
// the peer's input forced counts toward MaxRapidResetsPerSecond; one for
// capacity or draining doesn't.
func (c *serverConn) rejectNewStream(streamID uint32, code xhttp2.ErrCode, peerInduced bool) error {
	delete(c.priorityUpdates, streamID)
	c.trackClosedClientStream(streamID, true)
	if peerInduced {
		if err := c.recordRapidReset(); err != nil {
			return err
		}
	}
	return c.framer.WriteRSTStream(streamID, code)
}

func (c *serverConn) processTrailers(stream *serverStream, event *incomingFrame) error {
	if stream.remoteClosed {
		return c.resetStream(stream.id, xhttp2.ErrCodeStreamClosed, errors.New("http2: headers on half-closed remote stream"))
	}
	if !event.endStream {
		return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, errors.New("http2: invalid request trailers"))
	}
	for _, field := range event.fields {
		if field.IsPseudo() || isConnectionSpecificHeader(field.Name) {
			return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, errors.New("http2: invalid trailer field"))
		}
	}
	if stream.discardRequestBody {
		// Validated and dropped: a job may own the request.
		var discarded fasthttp.RequestHeader
		if err := applyRequestTrailers(c.protocolContext, &discarded, event.fields); err != nil {
			return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
		}
		return c.finishRequestBody(stream)
	}
	requestHeader := &stream.request.Request.Header
	if stream.body == nil {
		// A partial trailer set on error is unobservable: the stream resets and
		// the RequestCtx is recycled first.
		if err := applyRequestTrailers(c.protocolContext, requestHeader, event.fields); err != nil {
			return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
		}
		return c.finishRequestBody(stream)
	}
	// A streaming body applies trailers only when its reader drains, too late
	// to reject the frame.
	var validationHeader fasthttp.RequestHeader
	if err := applyRequestTrailers(c.protocolContext, &validationHeader, event.fields); err != nil {
		return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
	}
	fields := append([]hpack.HeaderField(nil), event.fields...)
	stream.body.setEOFCommit(func() error {
		return applyRequestTrailers(c.protocolContext, requestHeader, fields)
	})
	return c.finishRequestBody(stream)
}

// rejectData returns the frame's flow to the connection and resets the stream.
func (c *serverConn) rejectData(stream *serverStream, flowLength int64, code xhttp2.ErrCode, cause error) error {
	c.restoreConnectionWindow(flowLength)
	return c.resetStream(stream.id, code, cause)
}

func (c *serverConn) processData(event *incomingFrame) error {
	stream := c.streams[event.streamID]
	if stream == nil && c.streamIsIdle(event.streamID) {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: data on idle stream")}
	}
	flowLength := int64(event.flowLength)
	payloadLength := len(event.data)
	if !c.recv.debit(flowLength) {
		return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: connection receive window exceeded")}
	}
	if stream == nil {
		if err := c.consumeConnectionBytes(flowLength); err != nil {
			return err
		}
		if c.closedClientStreams[event.streamID] {
			return nil
		}
		return c.resetClosedStream(event.streamID)
	}
	if stream.resetSent {
		return c.consumeConnectionBytes(flowLength)
	}
	if stream.isPush && !stream.responseHeaderSent {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errFrameOnReservedStream}
	}
	if stream.remoteClosed {
		return c.rejectData(stream, flowLength, xhttp2.ErrCodeStreamClosed, errDataOnHalfClosedStream)
	}
	if !stream.recv.debit(flowLength) {
		return c.rejectData(stream, flowLength, xhttp2.ErrCodeFlowControl, errStreamRecvWindowExceeded)
	}
	stream.bodyBytes += int64(payloadLength)
	if stream.expectedBody >= 0 && stream.bodyBytes > stream.expectedBody {
		return c.rejectData(stream, flowLength, xhttp2.ErrCodeProtocol, errRequestBodyLengthMismatch)
	}
	stream.bodyFramed = true
	if !stream.discardRequestBody && stream.maxBody > 0 && stream.bodyBytes > int64(stream.maxBody) {
		switch {
		case stream.handlerPending():
			c.refuseRequest(stream, fasthttp.ErrBodyTooLarge)
		case stream.job == jobExpect:
			// The refusal follows the Expect job.
			if err := c.discardRequestBody(stream); err != nil {
				return err
			}
			stream.jobErr = fasthttp.ErrBodyTooLarge
			stream.refusedDuringJob = true
			if stream.jobState == jobStateQueued {
				stream.job = jobError
			}
		default:
			// The handler reads the error from its body stream.
			return c.rejectData(stream, flowLength, xhttp2.ErrCodeCancel, errRequestBodyTooLarge)
		}
	}
	if stream.discardRequestBody {
		if err := c.consumeRequestBytes(stream, flowLength); err != nil {
			return err
		}
		if event.endStream {
			return c.finishRequestBody(stream)
		}
		return nil
	}
	if stream.body != nil {
		payloadBytes := int64(payloadLength)
		// A buffered request's upload staged for an Expect job is retained
		// like a buffered body and is bounded by the same budget.
		if stream.stagedBuffered && payloadBytes > int64(c.config.maxBufferedRequestBody)-c.bufferedRequestBytes {
			return c.rejectData(stream, flowLength, xhttp2.ErrCodeEnhanceYourCalm, errBufferedBodyLimit)
		}
		buffer := event.dataBuffer
		event.dataBuffer = nil
		data := event.data
		event.data = nil
		var err error
		if buffer != nil {
			err = stream.body.writeIncoming(buffer)
		} else {
			err = stream.body.writeOwned(data, nil)
		}
		if err != nil {
			// The reader closed the body: the rest of the upload is dropped
			// while the response proceeds, as HTTP/1 drains it. The chunk was
			// released and never counted, so its flow is credited whole.
			if err := c.discardRequestBody(stream); err != nil {
				return err
			}
			if err := c.consumeRequestBytes(stream, flowLength); err != nil {
				return err
			}
			if event.endStream {
				return c.finishRequestBody(stream)
			}
			return nil
		}
		if stream.stagedBuffered {
			stream.bufferedBytes += payloadBytes
			c.bufferedRequestBytes += payloadBytes
		}
		stream.unconsumedFlow += payloadBytes
		padding := flowLength - int64(payloadLength)
		if padding > 0 {
			if err := c.consumeRequestBytes(stream, padding); err != nil {
				return err
			}
		}
	} else {
		payloadBytes := int64(payloadLength)
		limit := int64(c.config.maxBufferedRequestBody)
		if payloadBytes > limit-c.bufferedRequestBytes {
			return c.rejectData(stream, flowLength, xhttp2.ErrCodeEnhanceYourCalm, errBufferedBodyLimit)
		}
		stream.request.Request.AppendBody(event.data)
		stream.bufferedBytes += payloadBytes
		c.bufferedRequestBytes += payloadBytes
		if err := c.consumeRequestBytes(stream, flowLength); err != nil {
			return err
		}
	}
	if event.endStream {
		return c.finishRequestBody(stream)
	}
	return nil
}

func (c *serverConn) finishRequestBody(stream *serverStream) error {
	c.stopRequestReadTimeout(stream)
	if stream.expectedBody >= 0 && stream.bodyBytes != stream.expectedBody {
		return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, errors.New("http2: request body length doesn't match content-length"))
	}
	stream.remoteClosed = true
	if stream.body != nil {
		stream.body.complete()
	}
	if stream.localClosed {
		c.closeOnWire(stream)
	}
	if stream.handlerPending() {
		reportUndeclaredLength(stream)
		c.startJob(stream, jobHandler)
	}
	return nil
}

// reportUndeclaredLength settles the length of a completed request without a
// declared one as HTTP/1's reader does: -1 with the Transfer-Encoding entry
// after a framed body with content, as a parsed chunked request has; 0 for an
// empty framed body, as an empty chunked body reads, and for a bodyless
// request whose method doesn't ignore the body; -2 for a GET or HEAD.
func reportUndeclaredLength(stream *serverStream) {
	if stream.expectedBody >= 0 || stream.body != nil {
		return
	}
	header := &stream.request.Request.Header
	switch {
	case stream.bodyFramed:
		// Padding and empty frames aren't content.
		if stream.bufferedBytes == 0 {
			header.SetContentLength(0)
		} else {
			header.SetContentLength(-1)
		}
	case !header.IsGet() && !header.IsHead():
		header.SetContentLength(0)
	}
}

func (c *serverConn) armRequestReadTimeout(stream *serverStream) {
	streamID := stream.id
	stream.readTimer = time.AfterFunc(stream.readTimeout, func() {
		select {
		case c.commands <- serverCommand{
			kind:     serverCommandRequestReadTimeout,
			streamID: streamID,
			err:      fasthttp.ErrTimeout,
		}:
		case <-c.ctx.Done():
		}
	})
}

func (c *serverConn) stopRequestReadTimeout(stream *serverStream) {
	if stream.readTimer != nil {
		stream.readTimer.Stop()
		stream.readTimer = nil
	}
}

// armResponseWriteTimeout starts the response deadline: the whole response
// must be queued to the connection writer within stream.writeTimeout, as
// HTTP/1's write deadline set at the handler's return has it. Progress
// neither re-arms nor stops it. Each arming has its own generation so a
// timeout that fires as the response completes cannot reset the stream.
func (c *serverConn) armResponseWriteTimeout(stream *serverStream) {
	if stream.writeTimeout <= 0 || stream.writeTimer != nil {
		return
	}
	stream.writeTimeoutGen++
	streamID, generation := stream.id, stream.writeTimeoutGen
	stream.writeTimer = time.AfterFunc(stream.writeTimeout, func() {
		select {
		case c.commands <- serverCommand{
			kind:       serverCommandResponseWriteTimeout,
			streamID:   streamID,
			generation: generation,
			err:        errWriteTimeout,
		}:
		case <-c.ctx.Done():
		}
	})
}

func (c *serverConn) stopResponseWriteTimeout(stream *serverStream) {
	if stream.writeTimer != nil {
		stream.writeTimer.Stop()
		stream.writeTimer = nil
	}
}

func (c *serverConn) consumeRequestBytes(stream *serverStream, amount int64) error {
	if amount <= 0 {
		return nil
	}
	if err := c.consumeConnectionBytes(amount); err != nil {
		return err
	}
	// RFC 9113 §5.1: no WINDOW_UPDATE on a stream the peer has closed.
	if stream.remoteClosed {
		return nil
	}
	if increment := stream.recv.consume(amount, int64(c.config.streamWindowSize)); increment != 0 {
		stream.recv.grant(increment)
		return c.framer.WriteWindowUpdate(stream.id, uint32(increment)) // #nosec G115
	}
	return nil
}

func (c *serverConn) consumeConnectionBytes(amount int64) error {
	if amount <= 0 {
		return nil
	}
	if increment := c.recv.consume(amount, int64(c.config.connectionWindowSize)); increment != 0 {
		c.recv.grant(increment)
		return c.framer.WriteWindowUpdate(0, uint32(increment)) // #nosec G115
	}
	return nil
}

func (c *serverConn) restoreConnectionWindow(amount int64) {
	if amount <= 0 {
		return
	}
	c.recv.restore(amount)
	_ = c.framer.WriteWindowUpdate(0, uint32(amount)) // #nosec G115
}

func (c *serverConn) processRST(event *incomingFrame) error {
	stream := c.streams[event.streamID]
	if stream == nil {
		if c.streamIsIdle(event.streamID) {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: reset on idle stream")}
		}
		if c.closedClientStreams[event.streamID] {
			return nil
		}
		if err := c.recordRapidReset(); err != nil {
			return err
		}
		return nil
	}
	if stream.resetSent {
		return nil
	}
	if err := c.recordRapidReset(); err != nil {
		return err
	}
	c.teardownStream(stream, peerResetError{code: event.errCode})
	return nil
}

// peerResetError is a stream's cause after the peer's RST_STREAM; it is an
// errStreamClosed.
type peerResetError struct {
	code xhttp2.ErrCode
}

func (e peerResetError) Error() string {
	return "http2: peer reset stream: " + e.code.String()
}

func (peerResetError) Unwrap() error {
	return errStreamClosed
}

// teardownStream moves stream to its terminal state and unblocks every caller
// waiting on it with the stream's terminal cause, which later reads and
// writes report too. The wire-side RST_STREAM, if any, is the caller's
// business.
func (c *serverConn) teardownStream(stream *serverStream, cause error) {
	stream.isReset = true
	stream.remoteClosed = true
	stream.localClosed = true
	c.closeOnWire(stream)
	c.dequeueJob(stream)
	stream.cancel(cause)
	terminal := stream.closedError()
	c.cancelResponsePump(stream)
	if stream.body != nil {
		stream.body.discardWithError(terminal)
	}
	c.restoreUnconsumedFlow(stream)
	if stream.pendingAck != nil {
		stream.pendingAck <- terminal
		stream.pendingAck = nil
	}
	if stream.pendingWrite != nil {
		stream.pendingWrite.complete(terminal)
		stream.pendingWrite = nil
	}
	c.stopResponseWriteTimeout(stream)
	stream.pendingData = nil
	stream.responseEOF = false
	if !stream.handlerStarted || stream.handlerDone {
		c.maybeFinalizeStream(stream)
	}
}

func (c *serverConn) processWindowUpdate(event *incomingFrame) error {
	if event.streamID == 0 {
		if !c.send.credit(event.increment) {
			return &serverError{
				code: xhttp2.ErrCodeFlowControl,
				err: fmt.Errorf(
					"http2: server connection send window overflow: current=%d increment=%d",
					c.send.window,
					event.increment,
				),
			}
		}
		return nil
	}
	stream := c.streams[event.streamID]
	if stream == nil {
		if c.streamIsIdle(event.streamID) {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: window update on idle stream")}
		}
		return nil
	}
	if !stream.resetSent && !stream.send.credit(event.increment) {
		return c.resetStream(stream.id, xhttp2.ErrCodeFlowControl, errors.New("http2: stream send window overflow"))
	}
	return nil
}

func (c *serverConn) streamIsIdle(streamID uint32) bool {
	if streamID == 0 {
		return false
	}
	if streamID&1 == 1 {
		return streamID > c.lastClientStreamID
	}
	// Server-initiated stream IDs below nextPushStreamID were promised. Once
	// they close, late RST_STREAM and WINDOW_UPDATE frames are the races RFC
	// 9113 section 5.1 explicitly requires endpoints to tolerate.
	return streamID >= c.nextPushStreamID
}

func (c *serverConn) recordRapidReset() error {
	if c.config.maxRapidResetsPerSecond == 0 {
		return nil
	}
	now := time.Now()
	if c.rapidResetWindowStart.IsZero() || now.Sub(c.rapidResetWindowStart) >= time.Second {
		c.rapidResetWindowStart = now
		c.rapidResetCount = 0
	}
	c.rapidResetCount++
	if c.rapidResetCount > c.config.maxRapidResetsPerSecond {
		return &serverError{code: xhttp2.ErrCodeEnhanceYourCalm, err: errors.New("http2: rapid reset limit exceeded")}
	}
	return nil
}

func (c *serverConn) processPriorityUpdate(event *incomingFrame) error {
	if event.streamID == 0 {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: priority update targets stream zero")}
	}
	updated, err := parsePriority(event.priority)
	if err != nil {
		// RFC 9218 §7.1: an unparsable priority signal is treated as if it were
		// absent rather than as a protocol violation.
		return nil //nolint:nilerr
	}
	if stream := c.streams[event.streamID]; stream != nil {
		stream.priority = updated
		return nil
	}
	if event.streamID&1 == 0 {
		// RFC 9218 §7.1: a closed push stream's signal is discarded; only an
		// idle one is an error.
		if c.streamIsIdle(event.streamID) {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: priority update targets an invalid future stream")}
		}
		return nil
	}
	if event.streamID > c.lastClientStreamID {
		// RFC 9218 §7.1: prioritized idle streams plus open streams may not
		// exceed SETTINGS_MAX_CONCURRENT_STREAMS; a signal that would is a
		// connection error.
		c.prunePriorityUpdates()
		added := 1
		if _, exists := c.priorityUpdates[event.streamID]; exists {
			added = 0
		}
		if c.prioritizedStreamsExceed(added) {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errTooManyPrioritizedStreams}
		}
		c.priorityUpdates[event.streamID] = updated
	}
	return nil
}

// prioritizedStreamsExceed reports whether the hints held for idle streams
// plus the open client streams, with added more, exceed the limit.
func (c *serverConn) prioritizedStreamsExceed(added int) bool {
	return len(c.priorityUpdates)+int(c.openStreams)+added > int(c.config.maxConcurrentStreams)
}

// prunePriorityUpdates drops the hints of streams that are no longer idle. A
// hint is stored only for an ID above lastClientStreamID, so there is
// nothing to drop until that moves.
func (c *serverConn) prunePriorityUpdates() {
	if c.priorityPrunedThroughID == c.lastClientStreamID {
		return
	}
	c.priorityPrunedThroughID = c.lastClientStreamID
	for streamID := range c.priorityUpdates {
		if streamID <= c.lastClientStreamID {
			delete(c.priorityUpdates, streamID)
		}
	}
}

func (c *serverConn) processCommand(command *serverCommand) error {
	if (command.kind == serverCommandHandlerDone || command.kind == serverCommandJobDone) &&
		command.generation == c.handlerGen && c.pendingHandlers > 0 {
		c.pendingHandlers--
	}
	// A write its StreamConn already cut reports the cut whatever became of
	// the stream.
	if command.kind == serverCommandResponseData && command.write != nil && command.write.isCut() {
		command.write.complete(errInterrupted)
		return nil
	}
	if command.kind == serverCommandInspect {
		command.inspect(c)
		command.result <- nil
		return nil
	}
	stream := c.streams[command.streamID]
	if stream == nil {
		command.fail(errStreamClosed)
		return nil
	}
	switch command.kind {
	case serverCommandHandlerDone:
		c.releaseStreamWorker(stream)
		c.releaseJobSlot(stream)
		if err := c.handleHandlerDone(stream, command.requestCtx); err != nil {
			return err
		}
		c.dispatchQueuedJobs()
		return nil
	case serverCommandJobDone:
		c.releaseStreamWorker(stream)
		c.releaseJobSlot(stream)
		// Queued jobs go first: a continuation joins the queue's tail.
		c.dispatchQueuedJobs()
		return c.handleJobDone(stream, command)
	case serverCommandInformational:
		if !stream.precedesFinalResponse() {
			command.result <- errStreamClosed
			return nil
		}
		block, err := c.encodeInformationalHeaders(
			command.statusCode,
			command.header,
			c.peerMaxHeaderListSize,
		)
		if err != nil {
			command.result <- err
			if errors.Is(err, errHeaderBlockEncode) {
				return err
			}
			// Nothing reached the HPACK encoder, so this is the handler's
			// problem, not the connection's.
			return nil
		}
		err = c.writeHeaderBlock(stream.id, false, block)
		command.result <- err
		return err
	case serverCommandResponseData:
		if stream.isReset || stream.localClosed {
			command.fail(stream.closedError())
			return nil
		}
		responseBusy := len(stream.pendingData) != 0 || stream.pendingAck != nil ||
			stream.pendingWrite != nil || stream.responseEOF
		if responseBusy {
			command.fail(errors.New("http2: response stream has pending data"))
			return nil
		}
		if stream.expectedResponse >= 0 && stream.responseBytes+int64(len(command.data)) > stream.expectedResponse {
			err := errors.New("http2: response body exceeds content-length")
			command.fail(err)
			return c.abortStream(stream.id, xhttp2.ErrCodeInternal, err)
		}
		stream.pendingData = command.data
		if command.write != nil {
			stream.pendingWrite = command.write
		} else {
			stream.pendingAck = command.result
		}
		stream.responseBytes += int64(len(command.data))
		c.queueFlush(stream)
		return nil
	case serverCommandResponseEOF:
		if stream.isReset || stream.localClosed {
			command.result <- stream.closedError()
			return nil
		}
		// err is the body stream's failure as the pump classified it: a read
		// error other than the bare io.EOF, or any Close error.
		if command.err != nil {
			command.result <- command.err
			return c.abortStream(stream.id, xhttp2.ErrCodeInternal, command.err)
		}
		if stream.expectedResponse >= 0 && stream.responseBytes != stream.expectedResponse {
			err := errors.New("http2: response body length doesn't match content-length")
			command.result <- err
			return c.abortStream(stream.id, xhttp2.ErrCodeInternal, err)
		}
		stream.responseEOF = true
		c.queueFlush(stream)
		if stream.pendingData == nil {
			command.result <- nil
		} else {
			stream.pendingAck = command.result
		}
		return nil
	case serverCommandResponsePumpDone:
		stream.responsePumpDone = true
		c.maybeFinalizeStream(stream)
		return nil
	case serverCommandStreamHandlerDone:
		stream.handlerDone = true
		c.releaseJobSlot(stream)
		c.dispatchQueuedJobs()
		if stream.localClosed {
			return c.finishResponse(stream)
		}
		stream.responseEOF = true
		c.queueFlush(stream)
		return nil
	case serverCommandPush:
		err := c.handlePush(stream, command.target, command.pushOpts)
		command.result <- err
		if _, fatal := errors.AsType[*serverError](err); fatal {
			return err
		}
		return nil
	case serverCommandCloseRead:
		if err := c.discardRequestBody(stream); err != nil {
			command.result <- err
			return err
		}
		if stream.body != nil {
			stream.body.fail(net.ErrClosed)
		}
		command.result <- nil
		return nil
	case serverCommandRequestReadTimeout:
		if stream.remoteClosed {
			return nil
		}
		return c.requestReadTimedOut(stream)
	case serverCommandCancelStream:
		return c.abortStream(stream.id, xhttp2.ErrCodeCancel, command.err)
	case serverCommandInterruptWrite:
		// A notification can arrive after its write ended and another began;
		// only the cut write is ended.
		if stream.pendingWrite != nil && stream.pendingWrite == command.write && command.write.isCut() {
			c.cutPendingWrite(stream)
		}
		return nil
	case serverCommandResponseWriteTimeout:
		if command.generation != stream.writeTimeoutGen || stream.writeTimer == nil {
			return nil
		}
		return c.abortStream(stream.id, xhttp2.ErrCodeInternal, command.err)
	default:
		return nil
	}
}

// requestReadTimedOut answers a request whose body didn't arrive within the
// read timeout as HTTP/1's serve loop does: through the error path, with the
// *net.OpError shape its ErrorHandler sees, when no handler owns the request
// yet; a handler that already reads the body gets the error from its reads.
func (c *serverConn) requestReadTimedOut(stream *serverStream) error {
	timeoutErr := &net.OpError{Op: "read", Err: fasthttp.ErrTimeout}
	switch {
	case stream.handlerPending():
		c.refuseRequest(stream, timeoutErr)
		return nil
	case stream.jobState == jobStateFinished:
		// No job will start again: the response in progress stands.
		return nil
	case stream.job == jobExpect:
		// The Expect job's completion answers; a refusal already recorded
		// stands.
		if stream.refusedDuringJob {
			return nil
		}
		if err := c.discardRequestBody(stream); err != nil {
			return err
		}
		stream.jobErr = timeoutErr
		stream.refusedDuringJob = true
		if stream.jobState == jobStateQueued {
			stream.job = jobError
		}
		return nil
	case stream.job == jobError:
		// The error response in progress stands.
		return nil
	default:
		// The handler owns a streamed body: its reads report the timeout and
		// it answers as it sees fit.
		if stream.body != nil {
			stream.body.fail(timeoutErr)
		}
		return nil
	}
}

func (c *serverConn) runHandler(stream *serverStream) {
	requestCtx := stream.request
	if err := c.preParseMultipartForm(requestCtx); err != nil {
		// As HTTP/1's body read: the request is reset before ErrorHandler
		// sees it, and the handler never runs.
		requestCtx.Request.Reset()
		c.writeErrorResponse(requestCtx, err)
	} else {
		c.server.Handler(requestCtx)
	}
	c.markAbandonedIfTimedOut(stream, requestCtx)
	select {
	case c.commands <- serverCommand{
		kind:       serverCommandHandlerDone,
		streamID:   stream.id,
		generation: stream.handlerGen,
		requestCtx: requestCtx,
	}:
	case <-c.ctx.Done():
	}
}

// preParseMultipartForm parses the form before the handler under HTTP/1's
// condition: not disabled, a known positive length, a multipart boundary and
// no Content-Encoding.
func (c *serverConn) preParseMultipartForm(requestCtx *fasthttp.RequestCtx) error {
	header := &requestCtx.Request.Header
	if c.server.DisablePreParseMultipartForm || header.ContentLength() <= 0 ||
		len(header.MultipartFormBoundary()) == 0 || len(header.Peek(fasthttp.HeaderContentEncoding)) != 0 {
		return nil
	}
	_, err := requestCtx.Request.MultipartForm()
	return err
}

func (c *serverConn) handleHandlerDone(
	stream *serverStream,
	requestCtx *fasthttp.RequestCtx,
) error {
	stream.handlerDone = true
	if stream.request != requestCtx {
		return errors.New("http2: handler returned an unexpected request context")
	}
	if timeoutResponse := requestCtx.LastTimeoutErrorResponse(); timeoutResponse != nil {
		// The same request: its number and connection, not a new count.
		replacement := c.protocolContext.AcquireReplacementRequestCtx(stream, requestCtx)
		switch {
		case stream.headRequest:
			replacement.Request.Header.SetMethod(fasthttp.MethodHead)
		case stream.connectRequest:
			replacement.Request.Header.SetMethod(fasthttp.MethodConnect)
		}
		timeoutResponse.CopyTo(&replacement.Response)
		// The timed-out handler keeps the original; AcceptStream stops
		// reading stream.request once runHandler marked the stream abandoned.
		stream.request = replacement
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		requestCtx = replacement
	}
	if stream.isReset {
		c.maybeFinalizeStream(stream)
		return nil
	}
	// Connection-close intent ends the connection's use for requests, as
	// it does over HTTP/1: the response goes out, then the drain starts. The
	// field itself never reaches the wire.
	closeAfterResponse := c.server.DisableKeepalive || requestCtx.Response.ConnectionClose()
	if !stream.hasAbandonedRequest && stream.hijackRejected {
		if stream.hijackNoResponse {
			// The handler gave the exchange up without answering, as the
			// adaptor's abort does; HTTP/1 closes the connection on it.
			if err := c.abortStream(stream.id, xhttp2.ErrCodeInternal, errHijackWithoutResponse); err != nil {
				return err
			}
			if closeAfterResponse {
				return c.beginDrain()
			}
			return nil
		}
		requestCtx.Response.Reset()
		requestCtx.Response.SetStatusCode(fasthttp.StatusNotImplemented)
		requestCtx.Response.SetBodyString("connection hijacking isn't supported for HTTP/2 requests")
	}
	if err := c.sendResponse(stream, requestCtx); err != nil {
		return err
	}
	if closeAfterResponse {
		return c.beginDrain()
	}
	return nil
}

// sendResponse frames requestCtx's response for stream: the header block
// now, the body through the flush queue or the pump.
func (c *serverConn) sendResponse(stream *serverStream, requestCtx *fasthttp.RequestCtx) error {
	statusCode := requestCtx.Response.StatusCode()
	if statusCode >= 100 && statusCode < 200 {
		return c.abortStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: final response cannot be informational"))
	}
	hasContentLength := len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) != 0
	// DATA on an established tunnel is tunnel bytes, not a body to measure.
	establishesTunnel := stream.connectRequest && statusCode >= 200 && statusCode < 300
	if establishesTunnel && hasContentLength {
		return c.abortStream(
			stream.id,
			xhttp2.ErrCodeInternal,
			errors.New("http2: 2xx response to CONNECT cannot have content-length"),
		)
	}

	stream.acceptMu.Lock()
	streamHandler := stream.streamHandler
	if streamHandler != nil && (stream.hasAbandonedRequest || statusCode < 200 || statusCode >= 300) {
		stream.streamHandler = nil
		streamHandler = nil
	}
	stream.acceptMu.Unlock()
	if streamHandler != nil {
		if requestCtx.Response.IsBodyStream() || len(requestCtx.Response.Body()) != 0 ||
			len(requestCtx.Response.Header.PeekTrailerKeys()) != 0 || hasContentLength {
			return c.abortStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: accepted stream response cannot have an HTTP body"))
		}
		block, err := c.encodeResponseHeaders(
			c.server,
			&requestCtx.Response,
			c.peerMaxHeaderListSize,
			c.protocolContext.ServerDate(),
		)
		if err != nil {
			return c.failEncode(stream.id, err)
		}
		if err := c.writeHeaderBlock(stream.id, false, block); err != nil {
			return err
		}
		stream.responseHeaderSent = true
		c.startStreamHandler(stream, streamHandler)
		return nil
	}

	mustNotHaveBody := responseMustNotHaveBody(stream, requestCtx)
	if requestCtx.Response.IsBodyStream() && !establishesTunnel && requestCtx.Response.Header.ContentLength() < 0 {
		// An *io.LimitedReader declares its length, as Response.writeBodyStream
		// reads it.
		if size, ok := limitedReaderSize(requestCtx.Response.BodyStream()); ok {
			requestCtx.Response.Header.SetContentLength(size)
			hasContentLength = len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) != 0
		}
	}
	if mustNotHaveBody && requestCtx.Response.IsBodyStream() {
		// Dropped as HTTP/1 drops it; its writer may still use the ctx. The
		// response isn't encoded yet, so its Close's failure is the stream's.
		if err := c.abandonUnpumpedBody(stream); err != nil {
			return c.abortStream(stream.id, xhttp2.ErrCodeInternal, err)
		}
	}
	var bufferedBody []byte
	if !requestCtx.Response.IsBodyStream() {
		bufferedBody = requestCtx.Response.Body()
		// The body's length replaces a declared one, as Response.Write does;
		// the setter is a no-op on a 204 or 304, which keep theirs.
		if !establishesTunnel && (!mustNotHaveBody || len(bufferedBody) != 0) {
			requestCtx.Response.Header.SetContentLength(len(bufferedBody))
			hasContentLength = len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) != 0
		}
	}
	expectedResponse := int64(-1)
	if hasContentLength {
		expectedResponse = int64(requestCtx.Response.Header.ContentLength())
	}
	// Every check that can abort the stream precedes the HPACK encoder: a
	// block it produced must reach the peer.
	block, err := c.encodeResponseHeaders(
		c.server,
		&requestCtx.Response,
		c.peerMaxHeaderListSize,
		c.protocolContext.ServerDate(),
	)
	if err != nil {
		return c.failEncode(stream.id, err)
	}
	stream.responseHasTrailers = len(requestCtx.Response.Header.PeekTrailerKeys()) != 0
	if mustNotHaveBody {
		if err := c.writeHeaderBlock(stream.id, true, block); err != nil {
			return err
		}
		stream.responseHeaderSent = true
		stream.localClosed = true
		return c.finishResponse(stream)
	}
	if requestCtx.Response.IsBodyStream() {
		stream.expectedResponse = expectedResponse
		if err := c.writeHeaderBlock(stream.id, false, block); err != nil {
			return err
		}
		stream.responseHeaderSent = true
		c.armResponseWriteTimeout(stream)
		c.startResponsePump(stream, requestCtx)
		return nil
	}

	body := bufferedBody
	stream.expectedResponse = expectedResponse
	stream.responseBytes = int64(len(body))
	endStream := len(body) == 0 && !stream.responseHasTrailers
	if err := c.writeHeaderBlock(stream.id, endStream, block); err != nil {
		return err
	}
	stream.responseHeaderSent = true
	if endStream {
		stream.localClosed = true
		return c.finishResponse(stream)
	}
	c.armResponseWriteTimeout(stream)
	if len(body) != 0 {
		stream.pendingData = body
	}
	stream.responseEOF = true
	c.queueFlush(stream)
	return nil
}

// startResponsePump streams request's response body to the stream. The pump
// holds its own references: the stream may be finalized under a pump the
// connection canceled. The response's declared length was settled before its
// header block was encoded, so the transfer reader applies HTTP/1's sized
// rules; pumpReader stays the raw stream, the one a cancel can close
// concurrently.
func (c *serverConn) startResponsePump(stream *serverStream, request *fasthttp.RequestCtx) {
	stream.responsePumpStarted = true
	stream.pumpReader = request.Response.BodyStream()
	reader := c.protocolContext.ResponseBodyReader(&request.Response)
	c.workers.Add(1)
	go c.pumpResponse(stream, request, reader)
}

// pumpResponse copies reader to the stream one acknowledged chunk at a time,
// then closes the body stream. Only a bare io.EOF followed by a nil Close
// completes the response; any other read error, a wrapped io.EOF and a
// recovered panic included, and any Close error, io.EOF itself included, is
// reported through the owner as the stream's failure, an earlier read error
// winning over the Close's.
func (c *serverConn) pumpResponse(stream *serverStream, request *fasthttp.RequestCtx, reader io.Reader) {
	defer func() {
		select {
		case c.commands <- serverCommand{kind: serverCommandResponsePumpDone, streamID: stream.id}:
		case <-c.ctx.Done():
		}
		c.workers.Done()
	}()
	buffer := acquireResponsePumpBuffer()
	defer releaseResponsePumpBuffer(buffer)
	// One acknowledgement is outstanding at a time, so one channel serves
	// every chunk and the EOF.
	result := make(chan error, 1)
	for {
		n, readErr := c.readBodyStream(reader, buffer.data)
		if n > 0 {
			command := serverCommand{
				kind:     serverCommandResponseData,
				streamID: stream.id,
				data:     buffer.data[:n],
				result:   result,
			}
			select {
			case c.commands <- command:
			case <-stream.Done():
				_ = stream.closePumpReader(request)
				return
			}
			select {
			case err := <-result:
				if err != nil {
					_ = stream.closePumpReader(request)
					return
				}
			case <-stream.Done():
				_ = stream.closePumpReader(request)
				return
			}
		}
		if readErr == nil {
			continue
		}
		failure := readErr
		if failure == io.EOF {
			failure = nil
		}
		if closeErr := stream.closePumpReader(request); failure == nil {
			failure = closeErr
		}
		select {
		case c.commands <- serverCommand{
			kind:     serverCommandResponseEOF,
			streamID: stream.id,
			err:      failure,
			result:   result,
		}:
		case <-stream.Done():
			return
		}
		select {
		case <-result:
		case <-stream.Done():
		}
		return
	}
}

// readBodyStream is reader.Read with HTTP/1's recovery: a panic is the body
// stream's failure, not the process's.
func (c *serverConn) readBodyStream(reader io.Reader, p []byte) (n int, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = c.bodyStreamPanicError(recovered)
		}
	}()
	return reader.Read(p)
}

// closeBodyStream closes request's response body stream with HTTP/1's
// recovery.
func (c *serverConn) closeBodyStream(request *fasthttp.RequestCtx) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = c.bodyStreamPanicError(recovered)
		}
	}()
	return request.Response.CloseBodyStream()
}

// closeBodyStreamAsync closes a body stream from its own goroutine, as a
// blocked Read requires, with HTTP/1's recovery.
func (c *serverConn) closeBodyStreamAsync(closer io.Closer) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				_ = c.bodyStreamPanicError(recovered)
			}
		}()
		_ = closer.Close()
	}()
}

// bodyStreamPanicError turns a recovered body stream panic into the
// *ErrBodyStreamWritePanic HTTP/1 reports and logs it: a stream-local
// failure never reaches the connection worker's logger.
func (c *serverConn) bodyStreamPanicError(recovered any) error {
	err := c.protocolContext.BodyStreamPanicError(recovered)
	c.logf("error when serving connection %q<->%q: %v", c.conn.LocalAddr(), c.conn.RemoteAddr(), err)
	return err
}

// logf writes through Server.Logger, or the standard logger as the Server's
// default.
func (c *serverConn) logf(format string, args ...any) {
	if c.server.Logger != nil {
		c.server.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// cancelResponsePump marks a started pump as cut short by the connection and,
// when the reader's type allows a concurrent Close, closes it from a new
// goroutine so a blocked Read returns. Its RequestCtx is abandoned at
// finalization: a body writer may still use it.
func (c *serverConn) cancelResponsePump(stream *serverStream) {
	if !stream.responsePumpStarted || stream.responsePumpDone || stream.pumpCanceled {
		return
	}
	stream.pumpCanceled = true
	closer := concurrentCloser(stream.pumpReader)
	if closer == nil {
		return
	}
	stream.pumpMu.Lock()
	closed := stream.pumpClosedReader
	if !closed {
		stream.pumpCloseOwned = true
	}
	stream.pumpMu.Unlock()
	if !closed {
		c.closeBodyStreamAsync(closer)
	}
}

// concurrentCloser returns reader as a Closer that may be called during a
// Read: net.Conn (SetBodyStreamWriter's pipe) and io.PipeReader. Other
// readers, pooled FS readers among them, are left to the pump as in HTTP/1.
func concurrentCloser(reader io.Reader) io.Closer {
	switch reader := reader.(type) {
	case net.Conn:
		return reader
	case *io.PipeReader:
		return reader
	}
	return nil
}

func acquireResponsePumpBuffer() *responsePumpBuffer {
	if value := responsePumpBufferPool.Get(); value != nil {
		buffer := value.(*responsePumpBuffer) //nolint:forcetypeassert
		if cap(buffer.data) >= defaultMaxFrameSize {
			buffer.data = buffer.data[:defaultMaxFrameSize]
			return buffer
		}
	}
	return &responsePumpBuffer{data: make([]byte, defaultMaxFrameSize)}
}

func releaseResponsePumpBuffer(buffer *responsePumpBuffer) {
	buffer.data = buffer.data[:0]
	responsePumpBufferPool.Put(buffer)
}

func (c *serverConn) startStreamHandler(
	stream *serverStream,
	handler fasthttp.StreamHandler,
) {
	stream.handlerDone = false
	stream.hasStreamConn = true
	// The stream handler counts toward the job bound like the handler did.
	c.reserveJobSlot(stream)
	c.workers.Go(func() {
		streamConn := newStreamConn(c, stream)
		handler(streamConn)
		_ = streamConn.Close()
		select {
		case c.commands <- serverCommand{kind: serverCommandStreamHandlerDone, streamID: stream.id}:
		// Watches the connection, not the stream: a reset closes stream.Done,
		// and losing that race would pin the stream slot forever.
		case <-c.ctx.Done():
		}
	})
}

// queueFlush marks a stream as owing output. Entries are stream IDs kept in
// ascending order; a connection never reuses an ID, so a stale entry resolves
// to nil and drops. IDs mostly rise, making the sorted insert an append.
func (c *serverConn) queueFlush(stream *serverStream) {
	if stream.flushQueued {
		return
	}
	stream.flushQueued = true
	if n := len(c.flushQueue); n == 0 || c.flushQueue[n-1] < stream.id {
		c.flushQueue = append(c.flushQueue, stream.id)
		return
	}
	at, _ := slices.BinarySearch(c.flushQueue, stream.id)
	c.flushQueue = slices.Insert(c.flushQueue, at, stream.id)
}

func (c *serverConn) flushResponses() error {
	if len(c.flushQueue) == 0 {
		return nil
	}
	defer c.compactFlushQueue()
	for {
		another := false
		for urgency := range c.flushBuckets {
			c.flushBuckets[urgency] = c.flushBuckets[urgency][:0]
		}
		for _, streamID := range c.flushQueue {
			stream := c.streams[streamID]
			if stream != nil {
				urgency := min(stream.priority.urgency, uint8(7))
				c.flushBuckets[urgency] = append(c.flushBuckets[urgency], streamID)
			}
		}
		for urgency := uint8(0); urgency <= 7; urgency++ {
			// The queue ascends by stream ID, so non-incremental responses
			// drain in stream order while incremental ones share the urgency
			// round-robin (RFC 9218 §10).
			for _, streamID := range c.flushBuckets[urgency] {
				stream := c.streams[streamID]
				if stream == nil || min(stream.priority.urgency, uint8(7)) != urgency {
					continue
				}
				more, err := c.flushStream(stream, !stream.priority.incremental)
				if err != nil {
					return err
				}
				if more {
					another = true
				}
			}
		}
		if !another {
			return nil
		}
	}
}

// flushStream writes what the stream may send now; drain keeps going until the
// stream or a window empties. It reports whether the stream yielded with data
// it could still send, which only an incremental stream does.
// cutPendingWrite ends the pending StreamConn write at the bytes already
// framed; the stream stays open.
func (c *serverConn) cutPendingWrite(stream *serverStream) {
	stream.responseBytes -= int64(len(stream.pendingData))
	stream.pendingData = nil
	stream.pendingWrite.complete(errInterrupted)
	stream.pendingWrite = nil
}

func (c *serverConn) flushStream(stream *serverStream, drain bool) (bool, error) {
	for {
		if stream.pendingWrite != nil && stream.pendingWrite.isCut() {
			c.cutPendingWrite(stream)
		}
		canSendData := len(stream.pendingData) != 0 && !stream.isReset && !stream.localClosed
		if !canSendData {
			break
		}
		amount := c.reserveDataChunk(&stream.streamFlowState, len(stream.pendingData))
		if amount == 0 {
			// Flow control blocks the stream; the response deadline keeps running.
			return false, nil
		}
		isLast := amount == len(stream.pendingData) && stream.responseEOF && !stream.responseHasTrailers
		if err := c.framer.WriteData(stream.id, isLast, stream.pendingData[:amount]); err != nil {
			return false, err
		}
		stream.pendingData = stream.pendingData[amount:]
		if stream.pendingWrite != nil {
			stream.pendingWrite.written += amount
		}
		if len(stream.pendingData) == 0 {
			stream.pendingData = nil
			if stream.pendingAck != nil {
				stream.pendingAck <- nil
				stream.pendingAck = nil
			}
			if stream.pendingWrite != nil {
				stream.pendingWrite.complete(nil)
				stream.pendingWrite = nil
			}
		}
		if isLast {
			stream.localClosed = true
			return false, c.finishResponse(stream)
		}
		if !drain {
			return true, nil
		}
	}

	responseComplete := len(stream.pendingData) == 0 && stream.responseEOF && !stream.localClosed
	if !responseComplete {
		return false, nil
	}
	if !stream.responseHasTrailers {
		if err := c.framer.WriteData(stream.id, true, nil); err != nil {
			return false, err
		}
		stream.localClosed = true
		return false, c.finishResponse(stream)
	}
	encoded, err := c.encodeResponseTrailers(stream)
	if err != nil {
		return false, c.failEncode(stream.id, err)
	}
	if err := c.writeHeaderBlock(stream.id, true, encoded); err != nil {
		return false, err
	}
	stream.localClosed = true
	return false, c.finishResponse(stream)
}

func (c *serverConn) compactFlushQueue() {
	remaining := c.flushQueue[:0]
	for _, streamID := range c.flushQueue {
		stream := c.streams[streamID]
		if stream == nil {
			continue
		}
		pendingEOF := len(stream.pendingData) == 0 && stream.responseEOF
		pendingData := len(stream.pendingData) != 0 && !stream.isReset
		if !stream.localClosed && (pendingData || pendingEOF) {
			remaining = append(remaining, streamID)
			continue
		}
		stream.flushQueued = false
	}
	c.flushQueue = remaining
}

func (c *serverConn) encodeResponseTrailers(stream *serverStream) ([]byte, error) {
	return c.encodeTrailerHeaders(
		&stream.request.Response.Header,
		c.peerMaxHeaderListSize,
	)
}

// failEncode answers a header encode error: a block the HPACK encoder
// produced must reach the peer, so its failure ends the connection; one
// rejected before it ends the stream.
func (c *serverConn) failEncode(streamID uint32, err error) error {
	if errors.Is(err, errHeaderBlockEncode) {
		return err
	}
	return c.abortStream(streamID, xhttp2.ErrCodeInternal, err)
}

func (c *serverConn) writeHeaderBlock(streamID uint32, endStream bool, block []byte) error {
	firstLength := min(len(block), c.peerMaxFrameSize)
	if err := c.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: block[:firstLength],
		EndStream:     endStream,
		EndHeaders:    firstLength == len(block),
	}); err != nil {
		return err
	}
	return writeContinuationFrames(c.framer, streamID, block[firstLength:], c.peerMaxFrameSize)
}

func (c *serverConn) finishResponse(stream *serverStream) error {
	stream.responseEOF = false
	c.stopResponseWriteTimeout(stream)
	if stream.pendingAck != nil {
		stream.pendingAck <- nil
		stream.pendingAck = nil
	}
	if stream.pendingWrite != nil {
		stream.pendingWrite.complete(nil)
		stream.pendingWrite = nil
	}
	if stream.streamHandler != nil && !stream.handlerDone {
		if stream.remoteClosed {
			c.closeOnWire(stream)
		}
		return nil
	}
	if !stream.remoteClosed {
		if err := c.framer.WriteRSTStream(stream.id, xhttp2.ErrCodeNo); err != nil {
			return err
		}
		stream.resetSent = true
		stream.remoteClosed = true
		if stream.body != nil {
			stream.body.discardWithError(errStreamClosed)
		}
		c.restoreUnconsumedFlow(stream)
	}
	c.closeOnWire(stream)
	c.maybeFinalizeStream(stream)
	return nil
}

// closeOnWire records that a client stream is closed in both directions on
// the wire, which frees its SETTINGS_MAX_CONCURRENT_STREAMS slot whatever
// still runs for it.
func (c *serverConn) closeOnWire(stream *serverStream) {
	if stream.wireClosed || stream.isPush {
		return
	}
	stream.wireClosed = true
	c.openStreams--
}

func (c *serverConn) restoreUnconsumedFlow(stream *serverStream) {
	stream.discardRequestBody = true
	c.restoreConnectionWindow(stream.unconsumedFlow)
	stream.unconsumedFlow = 0
}

func (c *serverConn) maybeFinalizeStream(stream *serverStream) {
	// Idempotence guard for the pool release below: releaseServerStream zeroes
	// stream.id, so a finalized stream no longer matches its map entry.
	if c.streams[stream.id] != stream {
		return
	}
	if stream.handlerStarted && !stream.handlerDone {
		return
	}
	if stream.responsePumpStarted && !stream.responsePumpDone {
		return
	}
	if !stream.isReset && (!stream.localClosed || !stream.remoteClosed) {
		return
	}
	c.stopRequestReadTimeout(stream)
	c.stopResponseWriteTimeout(stream)
	if stream.pendingAck != nil {
		stream.pendingAck <- errStreamClosed
		stream.pendingAck = nil
	}
	if stream.pendingWrite != nil {
		stream.pendingWrite.complete(errStreamClosed)
		stream.pendingWrite = nil
	}
	stream.pendingData = nil
	stream.responseEOF = false
	if stream.body != nil {
		stream.body.discardWithError(errStreamClosed)
	}
	c.restoreUnconsumedFlow(stream)
	if stream.bufferedBytes != 0 {
		c.bufferedRequestBytes -= stream.bufferedBytes
		if c.bufferedRequestBytes < 0 {
			panic("BUG: HTTP/2 buffered request body accounting underflow")
		}
		stream.bufferedBytes = 0
	}
	c.closeOnWire(stream)
	c.releaseJobSlot(stream)
	delete(c.streams, stream.id)
	if len(c.streams) == 0 {
		c.idleSince = time.Now()
		c.idleRearm = true
	}
	if stream.id&1 == 1 {
		c.trackClosedClientStream(stream.id, stream.resetSent)
	}
	if stream.isPush {
		c.activePushes--
	}
	stream.cancel(errStreamClosed)
	if stream.request != nil {
		if !stream.responsePumpStarted && stream.request.Response.IsBodyStream() {
			// The stream is over: a Close failure has nothing to fail.
			_ = c.abandonUnpumpedBody(stream)
		}
		if stream.pumpCanceled {
			c.protocolContext.AbandonRequestCtx(stream.request)
		} else {
			c.protocolContext.ReleaseRequestCtx(stream.request)
		}
		stream.request = nil
	}
	// An abandoned RequestCtx keeps referencing the stream.
	if !stream.hasAbandonedRequest && !stream.hasStreamConn && !stream.pumpCanceled {
		releaseServerStream(stream)
	}
}

// abandonUnpumpedBody treats a response body stream no pump ever read as a
// canceled pump: the stream ended before the response started, and a
// SetBodyStreamWriter goroutine may still write and use the RequestCtx. A
// reader that allows a concurrent Close is closed off the event loop; any
// other is closed here, as nothing else owns it, and that Close's error, a
// recovered panic's included, is returned. Handler completion and
// finalization both reach here; the reader is closed once.
func (c *serverConn) abandonUnpumpedBody(stream *serverStream) error {
	if stream.pumpCanceled {
		return nil
	}
	stream.pumpCanceled = true
	if closer := concurrentCloser(stream.request.Response.BodyStream()); closer != nil {
		c.closeBodyStreamAsync(closer)
		return nil
	}
	return c.closeBodyStream(stream.request)
}

// resetClosedStream answers a frame on a closed stream with STREAM_CLOSED.
// Later frames on it are then ignored, so it is reset at most once.
func (c *serverConn) resetClosedStream(streamID uint32) error {
	if err := c.recordRapidReset(); err != nil {
		return err
	}
	if _, tracked := c.closedClientStreams[streamID]; tracked {
		c.closedClientStreams[streamID] = true
	} else {
		c.trackClosedClientStream(streamID, true)
	}
	return c.framer.WriteRSTStream(streamID, xhttp2.ErrCodeStreamClosed)
}

// trackClosedClientStream records a closed client stream. Stream IDs are never
// reused, so an ID is tracked at most once.
func (c *serverConn) trackClosedClientStream(streamID uint32, resetSent bool) {
	c.closedClientStreams[streamID] = resetSent
	limit := int(c.config.maxConcurrentStreams) * 4
	if len(c.closedClientStreamOrder) < limit {
		c.closedClientStreamOrder = append(c.closedClientStreamOrder, streamID)
		return
	}
	oldest := c.closedClientStreamOrder[c.closedClientStreamCursor]
	delete(c.closedClientStreams, oldest)
	c.closedClientStreamOrder[c.closedClientStreamCursor] = streamID
	c.closedClientStreamCursor++
	if c.closedClientStreamCursor == limit {
		c.closedClientStreamCursor = 0
	}
}

func (c *serverConn) releaseAllStreams() {
	clear(c.jobQueue)
	c.jobQueue = nil
	c.jobQueueHead = 0
	for _, stream := range c.streams {
		stream.isReset = true
		stream.localClosed = true
		stream.remoteClosed = true
		stream.handlerDone = true
		stream.responsePumpDone = true
		stream.cancel(errStreamClosed)
		c.maybeFinalizeStream(stream)
	}
}

func (c *serverConn) cancelStream(streamID uint32, cause error) {
	select {
	case c.commands <- serverCommand{
		kind:     serverCommandCancelStream,
		streamID: streamID,
		err:      cause,
	}:
	case <-c.ctx.Done():
	}
}

// resetStream resets a stream over invalid peer input, which counts toward
// MaxRapidResetsPerSecond.
func (c *serverConn) resetStream(streamID uint32, code xhttp2.ErrCode, cause error) error {
	return c.writeReset(streamID, code, cause, true)
}

// abortStream resets a stream for a local reason, which doesn't count.
func (c *serverConn) abortStream(streamID uint32, code xhttp2.ErrCode, cause error) error {
	return c.writeReset(streamID, code, cause, false)
}

func (c *serverConn) writeReset(streamID uint32, code xhttp2.ErrCode, cause error, peerInduced bool) error {
	stream := c.streams[streamID]
	if stream != nil && (stream.isReset || stream.resetSent) {
		return nil
	}
	if peerInduced {
		if err := c.recordRapidReset(); err != nil {
			return err
		}
	}
	if err := c.framer.WriteRSTStream(streamID, code); err != nil {
		return err
	}
	if stream == nil {
		if _, tracked := c.closedClientStreams[streamID]; tracked {
			c.closedClientStreams[streamID] = true
		}
		return nil
	}
	stream.resetSent = true
	c.teardownStream(stream, cause)
	return nil
}

// beginDrain starts the graceful close: GOAWAY(2^31-1) and the PING whose
// ACK fixes the last stream ID; a peer that withholds the ACK gets the final
// GOAWAY at pingTimeout. Streams keep running to completion either way.
func (c *serverConn) beginDrain() error {
	if err := c.startGracefulShutdown(); err != nil {
		return err
	}
	if c.shutdownTimer == nil {
		c.shutdownTimer = time.NewTimer(c.config.pingTimeout)
	}
	return nil
}

func (c *serverConn) startGracefulShutdown() error {
	if c.isDraining || c.isGoingAway {
		return nil
	}
	c.isDraining = true
	if err := c.framer.WriteGoAway(math.MaxInt32, xhttp2.ErrCodeNo, nil); err != nil {
		return err
	}
	return c.framer.WritePing(false, shutdownPingData)
}

func (c *serverConn) finishGracefulShutdown() error {
	if c.isGoingAway {
		return nil
	}
	c.isGoingAway = true
	c.goAwaySent = true
	return c.framer.WriteGoAway(c.lastProcessedID, xhttp2.ErrCodeNo, nil)
}

func (c *serverConn) failConnection(code xhttp2.ErrCode, cause error) error {
	// A peer told to calm down gets no drain: its input is the problem.
	c.goAwaySent = code != xhttp2.ErrCodeEnhanceYourCalm
	_ = c.framer.WriteGoAway(c.lastProcessedID, code, nil)
	_ = c.writer.Flush()
	return cause
}

func (c *serverConn) handlePush(
	parent *serverStream,
	target string,
	opts *fasthttp.PushOptions,
) error {
	if c.peerGoingAway {
		return fasthttp.ErrPushNotAllowed
	}
	if !c.config.enablePush || !c.peerAllowsPush {
		return fasthttp.ErrPushDisabled
	}
	if !parent.precedesFinalResponse() {
		return fasthttp.ErrPushNotAllowed
	}
	pushExhausted := parent.isPush || c.activePushes >= maxPromisedStreams ||
		c.activePushes >= c.peerMaxConcurrentStreams
	if pushExhausted {
		return fasthttp.ErrPushLimit
	}
	if c.nextPushStreamID == 0 || c.nextPushStreamID > math.MaxInt32 {
		return fasthttp.ErrPushLimit
	}

	method := fasthttp.MethodGet
	if opts != nil && opts.Method != "" {
		method = opts.Method
	}
	if method != fasthttp.MethodGet && method != fasthttp.MethodHead {
		return errors.New("http2: pushed request method must be GET or HEAD")
	}
	if target == "" {
		return errors.New("http2: pushed request target is empty")
	}

	if opts != nil && opts.Header != nil {
		// RFC 9113 §8.4 rules, shared with the client's promise validation;
		// All yields canonical names.
		var invalid error
		opts.Header.All()(func(key, value []byte) bool {
			invalid = validatePromisedField(c.strings.name(key), string(value))
			return invalid == nil
		})
		if invalid != nil {
			return invalid
		}
	}

	parentURI := parent.request.URI()
	uri := fasthttp.AcquireURI()
	defer fasthttp.ReleaseURI(uri)
	if err := uri.Parse(parentURI.Host(), []byte(target)); err != nil {
		return fmt.Errorf("http2: invalid pushed request target: %w", err)
	}
	// An origin-form target is relative to the parent's origin.
	if target[0] == '/' {
		uri.SetSchemeBytes(parentURI.Scheme())
	}
	if !bytes.Equal(uri.Host(), parentURI.Host()) || !bytes.Equal(uri.Scheme(), parentURI.Scheme()) {
		return errors.New("http2: pushed request must have the same origin as its parent")
	}

	streamID := c.nextPushStreamID
	stream := newServerStream(c, streamID)
	stream.isPush = true
	stream.priority = parent.priority
	stream.remoteClosed = true
	stream.maxBody = parent.maxBody
	stream.writeTimeout = c.server.WriteTimeout
	requestCtx := c.protocolContext.AcquireRequestCtx(c.conn, stream)
	stream.request = requestCtx
	if opts != nil && opts.Header != nil {
		opts.Header.CopyTo(&requestCtx.Request.Header)
	}
	requestCtx.Request.SetURI(uri)
	requestCtx.Request.Header.SetMethod(method)
	requestCtx.Request.Header.SetHostBytes(uri.Host())
	requestCtx.Request.Header.SetRequestURIBytes(uri.RequestURI())
	requestCtx.Request.Header.SetProtocol("HTTP/2")
	requestCtx.Request.Header.SetConnectProtocol("")

	block, err := c.encodeRequestHeaders(
		&requestCtx.Request,
		nil,
		c.peerMaxHeaderListSize,
		false,
	)
	encoded := err == nil
	if encoded {
		err = c.writePushPromise(parent.id, streamID, block)
	}
	if err != nil {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(err)
		releaseServerStream(stream)
		// Past the HPACK encoder the block is owed to the peer.
		if encoded || errors.Is(err, errHeaderBlockEncode) {
			return &serverError{code: xhttp2.ErrCodeInternal, err: err}
		}
		return err
	}
	c.nextPushStreamID += 2
	c.activePushes++
	c.streams[streamID] = stream
	c.startJob(stream, jobHandler)
	return nil
}

func (c *serverConn) writePushPromise(parentID, promisedID uint32, block []byte) error {
	// The payload also carries the 4-byte promised stream ID.
	firstLength := min(len(block), c.peerMaxFrameSize-4)
	if err := c.framer.WritePushPromise(xhttp2.PushPromiseParam{
		StreamID:      parentID,
		PromiseID:     promisedID,
		BlockFragment: block[:firstLength],
		EndHeaders:    firstLength == len(block),
	}); err != nil {
		return err
	}
	return writeContinuationFrames(c.framer, parentID, block[firstLength:], c.peerMaxFrameSize)
}

// limitedReaderSize is the length an *io.LimitedReader body stream has left,
// when it is non-negative and fits an int.
func limitedReaderSize(reader io.Reader) (int, bool) {
	limited, ok := reader.(*io.LimitedReader)
	if !ok || limited.N < 0 || int64(int(limited.N)) != limited.N {
		return 0, false
	}
	return int(limited.N), true
}

// responseMustNotHaveBody reads the method from the stream's snapshot: the
// request may have been reset for the error path or replaced by
// TimeoutHandler's.
func responseMustNotHaveBody(stream *serverStream, requestCtx *fasthttp.RequestCtx) bool {
	// SkipBody suppresses the body as Response.Write does for HTTP/1.
	if stream.headRequest || requestCtx.Response.SkipBody {
		return true
	}
	statusCode := requestCtx.Response.StatusCode()
	return statusCode >= 100 && statusCode < 200 || statusCode == 204 || statusCode == 304
}

type priority struct {
	urgency     uint8
	incremental bool
}

var errPriorityFormat = errors.New("http2: malformed priority field")

// parsePriority parses an RFC 9218 Priority field value, an RFC 8941
// Dictionary. A syntax error anywhere fails the field; a member of the wrong
// type or out of range is ignored on its own, an unknown key too, and a
// repeated key takes its last value.
func parsePriority(value string) (priority, error) {
	parser := sfParser{s: value}
	var members priorityMembers
	if err := parser.dictionary(&members); err != nil {
		return priority{}, err
	}
	result := priority{urgency: 3}
	if members.urgency.kind == sfInteger && members.urgency.integer >= 0 && members.urgency.integer <= 7 {
		result.urgency = uint8(members.urgency.integer)
	}
	if members.incremental.kind == sfBoolean {
		result.incremental = members.incremental.boolean
	}
	return result, nil
}

// priorityMembers holds the last value of the two members RFC 9218 defines.
type priorityMembers struct {
	urgency     sfItem
	incremental sfItem
}

func (m *priorityMembers) set(key string, item sfItem) {
	switch key {
	case "u":
		m.urgency = item
	case "i":
		m.incremental = item
	}
}

type sfKind uint8

const (
	sfNone sfKind = iota
	sfInteger
	sfDecimal
	sfString
	sfToken
	sfBinary
	sfBoolean
	sfInnerList
)

// sfItem is a parsed member value: its kind, and the value of an Integer or
// Boolean, the only ones the priority semantics read.
type sfItem struct {
	kind    sfKind
	integer int64
	boolean bool
}

// sfParser parses RFC 8941 Structured Fields without allocating.
type sfParser struct {
	s   string
	pos int
}

func (p *sfParser) more() bool { return p.pos < len(p.s) }

func (p *sfParser) peek() byte { return p.s[p.pos] }

func (p *sfParser) skip(accept func(byte) bool) {
	for p.more() && accept(p.peek()) {
		p.pos++
	}
}

func isSP(b byte) bool { return b == ' ' }

func isOWS(b byte) bool { return b == ' ' || b == '\t' }

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isLCAlpha(b byte) bool { return b >= 'a' && b <= 'z' }

func isAlpha(b byte) bool { return isLCAlpha(b) || b >= 'A' && b <= 'Z' }

func isTokenChar(b byte) bool {
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~', ':', '/':
		return true
	}
	return isAlpha(b) || isDigit(b)
}

func isBase64Char(b byte) bool {
	return isAlpha(b) || isDigit(b) || b == '+' || b == '/'
}

// dictionary parses sf-dictionary (RFC 8941 §4.2.2), handing each member to
// members in order.
func (p *sfParser) dictionary(members *priorityMembers) error {
	p.skip(isSP)
	for p.more() {
		key, err := p.key()
		if err != nil {
			return err
		}
		item := sfItem{kind: sfBoolean, boolean: true}
		if p.more() && p.peek() == '=' {
			p.pos++
			if item, err = p.memberValue(); err != nil {
				return err
			}
		} else if err := p.parameters(); err != nil {
			return err
		}
		members.set(key, item)
		p.skip(isOWS)
		if !p.more() {
			break
		}
		if p.peek() != ',' {
			return errPriorityFormat
		}
		p.pos++
		p.skip(isOWS)
		if !p.more() {
			return errPriorityFormat
		}
	}
	return nil
}

func (p *sfParser) key() (string, error) {
	start := p.pos
	if !p.more() || !isLCAlpha(p.peek()) && p.peek() != '*' {
		return "", errPriorityFormat
	}
	p.pos++
	for p.more() {
		b := p.peek()
		if !isLCAlpha(b) && !isDigit(b) && b != '_' && b != '-' && b != '.' && b != '*' {
			break
		}
		p.pos++
	}
	return p.s[start:p.pos], nil
}

// memberValue parses sf-item or inner-list, parameters included.
func (p *sfParser) memberValue() (sfItem, error) {
	if p.more() && p.peek() == '(' {
		return p.innerList()
	}
	return p.item()
}

func (p *sfParser) item() (sfItem, error) {
	item, err := p.bareItem()
	if err != nil {
		return sfItem{}, err
	}
	return item, p.parameters()
}

func (p *sfParser) bareItem() (sfItem, error) {
	if !p.more() {
		return sfItem{}, errPriorityFormat
	}
	switch b := p.peek(); {
	case b == '-' || isDigit(b):
		return p.number()
	case b == '"':
		return p.quotedString()
	case isAlpha(b) || b == '*':
		return p.token()
	case b == ':':
		return p.binary()
	case b == '?':
		return p.boolean()
	}
	return sfItem{}, errPriorityFormat
}

// number parses sf-integer or sf-decimal (RFC 8941 §4.2.4).
func (p *sfParser) number() (sfItem, error) {
	negative := p.peek() == '-'
	if negative {
		p.pos++
	}
	var integer int64
	digits, fraction := 0, -1
	for p.more() {
		b := p.peek()
		switch {
		case isDigit(b):
			if fraction >= 0 {
				fraction++
				if fraction > 3 {
					return sfItem{}, errPriorityFormat
				}
			} else {
				digits++
				if digits > 15 {
					return sfItem{}, errPriorityFormat
				}
				integer = integer*10 + int64(b-'0')
			}
		case b == '.' && fraction < 0:
			if digits > 12 {
				return sfItem{}, errPriorityFormat
			}
			fraction = 0
		default:
			goto done
		}
		p.pos++
	}
done:
	if digits == 0 || fraction == 0 {
		return sfItem{}, errPriorityFormat
	}
	if fraction > 0 {
		return sfItem{kind: sfDecimal}, nil
	}
	if negative {
		integer = -integer
	}
	return sfItem{kind: sfInteger, integer: integer}, nil
}

// quotedString parses sf-string (RFC 8941 §4.2.5).
func (p *sfParser) quotedString() (sfItem, error) {
	p.pos++
	for p.more() {
		b := p.peek()
		p.pos++
		switch {
		case b == '\\':
			if !p.more() || p.peek() != '"' && p.peek() != '\\' {
				return sfItem{}, errPriorityFormat
			}
			p.pos++
		case b == '"':
			return sfItem{kind: sfString}, nil
		case b < 0x20 || b >= 0x7f:
			return sfItem{}, errPriorityFormat
		}
	}
	return sfItem{}, errPriorityFormat
}

// token parses sf-token (RFC 8941 §4.2.6); the caller checked its first byte.
func (p *sfParser) token() (sfItem, error) {
	p.pos++
	p.skip(isTokenChar)
	return sfItem{kind: sfToken}, nil
}

// binary parses sf-binary (RFC 8941 §4.2.7): base64 characters, padding
// only at the end, in a length that decodes once padding is synthesized
// (RFC 4648 §4): at most two '=', a padded length that is a multiple of
// four, and no lone sextet.
func (p *sfParser) binary() (sfItem, error) {
	p.pos++
	start := p.pos
	p.skip(isBase64Char)
	data := p.pos - start
	p.skip(func(b byte) bool { return b == '=' })
	pad := p.pos - start - data
	if !p.more() || p.peek() != ':' {
		return sfItem{}, errPriorityFormat
	}
	if pad > 2 || pad != 0 && (data+pad)%4 != 0 || data%4 == 1 {
		return sfItem{}, errPriorityFormat
	}
	p.pos++
	return sfItem{kind: sfBinary}, nil
}

// boolean parses sf-boolean (RFC 8941 §4.2.8).
func (p *sfParser) boolean() (sfItem, error) {
	p.pos++
	if !p.more() || p.peek() != '0' && p.peek() != '1' {
		return sfItem{}, errPriorityFormat
	}
	item := sfItem{kind: sfBoolean, boolean: p.peek() == '1'}
	p.pos++
	return item, nil
}

// parameters parses and drops parameters (RFC 8941 §4.2.3.2).
func (p *sfParser) parameters() error {
	for p.more() && p.peek() == ';' {
		p.pos++
		p.skip(isSP)
		if _, err := p.key(); err != nil {
			return err
		}
		if p.more() && p.peek() == '=' {
			p.pos++
			if _, err := p.bareItem(); err != nil {
				return err
			}
		}
	}
	return nil
}

// innerList parses inner-list (RFC 8941 §4.2.1.2), parameters included.
func (p *sfParser) innerList() (sfItem, error) {
	p.pos++
	for {
		p.skip(isSP)
		if !p.more() {
			return sfItem{}, errPriorityFormat
		}
		if p.peek() == ')' {
			p.pos++
			return sfItem{kind: sfInnerList}, p.parameters()
		}
		if _, err := p.item(); err != nil {
			return sfItem{}, err
		}
		if !p.more() || p.peek() != ' ' && p.peek() != ')' {
			return sfItem{}, errPriorityFormat
		}
	}
}

func errorCode(err error) xhttp2.ErrCode {
	if errors.Is(err, xhttp2.ErrFrameTooLarge) {
		return xhttp2.ErrCodeFrameSize
	}
	if connectionError, ok := errors.AsType[xhttp2.ConnectionError](err); ok {
		return xhttp2.ErrCode(connectionError)
	}
	return xhttp2.ErrCodeProtocol
}

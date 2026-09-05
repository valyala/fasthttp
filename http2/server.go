package http2

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"strconv"
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
)

type serverCommandKind uint8

const (
	serverCommandUnknown serverCommandKind = iota
	serverCommandHandlerDone
	serverCommandInformational
	serverCommandBodyConsumed
	serverCommandResponseData
	serverCommandResponseEOF
	serverCommandResponsePumpDone
	serverCommandStreamHandlerDone
	serverCommandPush
	serverCommandCloseRead
	serverCommandRequestReadTimeout
	serverCommandCancelStream
	serverCommandCancelWrite
	serverCommandResponseWriteTimeout
)

type streamWriteResult struct {
	n   int
	err error
}

// streamWrite is owned by the connection goroutine. A StreamConn.Write waits
// on result after its command is accepted, so the owner can report exactly how
// many bytes were framed before a deadline cancelled the remainder.
type streamWrite struct {
	result  chan streamWriteResult
	written int
	done    bool
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
	consumed   int
	err        error
	result     chan error
	target     string
	pushOpts   *fasthttp.PushOptions
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
	workers   sync.WaitGroup

	streams            map[uint32]*serverStream
	flushQueue         []uint32
	flushBuckets       [8][]uint32
	pendingHandlers    int
	handlerGen         uint32
	cycleTime          time.Time
	lastClientStreamID uint32
	lastProcessedID    uint32
	isGoingAway        bool
	peerGoingAway      bool
	peerGoAwayLastID   uint32
	isDraining         bool

	peerAllowsPush bool

	bufferedRequestBytes    int64
	nextPushStreamID        uint32
	activePushes            uint32
	priorityUpdates         map[uint32]priority
	priorityPrunedThroughID uint32
	// closedClientStreams records, per recently closed stream, whether its
	// STREAM_CLOSED reset was already sent. See markClosedStreamReset.
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
}

func (c *serverConn) serve(upgrade *serverUpgrade) (retErr error) {
	defer func() {
		c.cancel(retErr)
		c.failPendingStreams(retErr)
		c.stopStreamWorkers()
		c.workers.Wait()
		c.releaseAllStreams()
		if c.writer != nil {
			if err := c.writer.closeAndWait(c.config.pingTimeout); retErr == nil && err != nil {
				retErr = err
			}
		}
		_ = c.conn.Close()
	}()

	var err error
	if upgrade != nil {
		err = c.startUpgraded(upgrade)
	} else {
		err = c.startDirect()
	}
	if err != nil {
		return err
	}

	go c.readLoop()
	return c.eventLoop()
}

// failPendingStreams unblocks every goroutine waiting on a stream once the
// event loop has stopped. Closing ownerDone afterwards tells StreamConn writers
// that a command still queued will never run.
func (c *serverConn) failPendingStreams(cause error) {
	for _, stream := range c.streams {
		stream.cancel(cause)
		if stream.body != nil {
			stream.body.closeWithError(cause)
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
		return err
	}
	if err := c.readClientPreface(); err != nil {
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
// same post-preface setup.
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
	if err := c.readClientPreface(); err != nil {
		return err
	}
	c.installFramer()
	c.initQueues()
	c.bootstrapUpgradedStream(upgrade.request)
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
	c.headerDecoder = newHeaderCodec(c.config.maxDecoderTableSize, c.config.maxHeaderListSize)
	c.framer.SetMaxReadFrameSize(c.config.maxReadFrameSize)
}

func (c *serverConn) eventLoop() error {
	serverDone := c.protocolContext.Done()
	var idleTimer *time.Timer
	if c.config.idleTimeout > 0 {
		idleTimer = time.NewTimer(c.config.idleTimeout)
		defer idleTimer.Stop()
	}
	idleArmed := idleTimer != nil
	var shutdownTimer *time.Timer
	// Owned by this goroutine and never pooled: a pooled 1ms timer expires so
	// often that releasing it mid-expiry trips initTimer's sanity check.
	var coalesceTimer *time.Timer
	coalesceArmed := false
	defer func() {
		if shutdownTimer != nil {
			shutdownTimer.Stop()
		}
		if coalesceTimer != nil {
			coalesceTimer.Stop()
		}
	}()
	for {
		if c.isGoingAway && len(c.streams) == 0 {
			return nil
		}
		expectMore := false
		select {
		case event := <-c.events:
			expectMore = event.morePending
			if closeConnection, err := c.handleIncomingEvent(&event); closeConnection {
				return err
			}
		case <-timerChannel(idleTimer):
			c.reapIdleStreamWorkers()
			if len(c.streams) == 0 {
				if err := c.startGracefulShutdown(); err != nil {
					return err
				}
				if shutdownTimer == nil {
					shutdownTimer = time.NewTimer(c.config.pingTimeout)
				}
			} else {
				idleArmed = false
			}
		case <-timerChannel(shutdownTimer):
			if err := c.finishGracefulShutdown(); err != nil {
				return err
			}
		case command := <-c.commands:
			if err := c.processCommand(&command); err != nil {
				return c.failCommand(err)
			}
		case <-serverDone:
			serverDone = nil
			if err := c.startGracefulShutdown(); err != nil {
				return err
			}
			if shutdownTimer == nil {
				shutdownTimer = time.NewTimer(c.config.pingTimeout)
			}
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
		// Cycle-grained is plenty for the 10s worker idle reaper.
		c.cycleTime = time.Now()
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
		if idleTimer != nil {
			switch {
			case len(c.streams) == 0 && !idleArmed:
				idleTimer.Reset(c.config.idleTimeout)
				idleArmed = true
			case len(c.streams) != 0 && idleArmed:
				idleTimer.Stop()
				idleArmed = false
			}
		}
	}
}

func (c *serverConn) handleIncomingEvent(event *incomingFrame) (bool, error) {
	defer releaseIncomingFrame(event)
	if event.err != nil && event.kind != incomingFrameStreamError {
		if writerErr := c.writer.err(); writerErr != nil {
			return true, writerErr
		}
		if errors.Is(event.err, io.EOF) && len(c.streams) == 0 {
			return true, nil
		}
		return true, c.failConnection(errorCode(event.err), event.err)
	}
	if err := c.processFrame(event); err != nil {
		var protocolErr *serverError
		if errors.As(err, &protocolErr) {
			return true, c.failConnection(protocolErr.code, protocolErr.err)
		}
		return true, c.failConnection(xhttp2.ErrCodeInternal, err)
	}
	return false, nil
}

func (c *serverConn) failCommand(err error) error {
	var protocolErr *serverError
	if errors.As(err, &protocolErr) {
		return c.failConnection(protocolErr.code, protocolErr.err)
	}
	return c.failConnection(xhttp2.ErrCodeInternal, err)
}

func (c *serverConn) readClientPreface() error {
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
	if _, err := io.ReadFull(c.conn, preface); err != nil {
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

func (c *serverConn) writeInitialSettings() error {
	settings := []xhttp2.Setting{
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
	windowIncrement := uint32(c.config.connectionWindowSize - 65535)
	if windowIncrement != 0 {
		return c.framer.WriteWindowUpdate(0, windowIncrement)
	}
	return nil
}

func (c *serverConn) readLoop() {
	for {
		frame, err := c.frames.readFrame()
		event := incomingFrame{err: err}
		if err != nil {
			var readErr *frameReadError
			if errors.As(err, &readErr) {
				event.frameType = readErr.frameType
			}
			var streamError xhttp2.StreamError
			if errors.As(err, &streamError) {
				event.kind = incomingFrameStreamError
				event.streamID = streamError.StreamID
				event.errCode = streamError.Code
			}
		} else {
			if headers, ok := frame.(*headersFrame); ok {
				event = c.decodeIncomingHeaders(headers)
			} else {
				event = incomingFrameFromWire(frame.(xhttp2.Frame)) //nolint:forcetypeassert
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
// storage. HPACK field strings are immutable values owned by the decoder; only
// the Framer-owned field slice must be copied before the next ReadFrame call.
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
	case *xhttp2.MetaHeadersFrame:
		event.kind = incomingFrameHeaders
		event.endStream = frame.StreamEnded()
		event.truncated = frame.Truncated
		event.hasPriority = frame.HasPriority()
		if event.hasPriority {
			event.dependency = frame.Priority.StreamDep
		}
		event.fieldStorage = copyIncomingHeaderFields(frame.Fields)
		event.fields = event.fieldStorage.fields
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

func copyIncomingHeaderFields(source []hpack.HeaderField) *incomingHeaderFieldStorage {
	storage := acquireIncomingHeaderFields(len(source))
	storage.fields = append(storage.fields, source...)
	return storage
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
		if event.streamID == event.dependency {
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
	c.trackClosedClientStream(event.streamID)
	if err := c.recordRapidReset(); err != nil {
		return err
	}
	return c.framer.WriteRSTStream(event.streamID, event.errCode)
}

func (c *serverConn) processSettings(event *incomingFrame) error {
	if event.ack {
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
		hasHeaderTableSize, hasEnablePush                  bool
		hasMaxConcurrentStreams, hasInitialWindowSize      bool
		hasMaxFrameSize, hasMaxHeaderListSize              bool
	}
	var final finalSettings
	maxInitialWindowDelta := int64(math.MinInt64)
	for _, setting := range settings {
		if err := setting.Valid(); err != nil {
			return &serverError{code: errorCode(err), err: err}
		}
		switch setting.ID {
		case xhttp2.SettingHeaderTableSize:
			final.headerTableSize = setting.Val
			final.hasHeaderTableSize = true
		case xhttp2.SettingEnablePush:
			if setting.Val > 1 {
				return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid SETTINGS_ENABLE_PUSH")}
			}
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
			if setting.Val < defaultMaxFrameSize || setting.Val > 1<<24-1 {
				return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid SETTINGS_MAX_FRAME_SIZE")}
			}
			final.maxFrameSize = setting.Val
			final.hasMaxFrameSize = true
		case xhttp2.SettingMaxHeaderListSize:
			final.maxHeaderListSize = setting.Val
			final.hasMaxHeaderListSize = true
		case xhttp2.SettingEnableConnectProtocol:
			if setting.Val > 1 {
				return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid boolean setting")}
			}
		case xhttp2.SettingNoRFC7540Priorities:
			if setting.Val > 1 {
				return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid boolean setting")}
			}
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

func (c *serverConn) processHeaders(event *incomingFrame) error {
	if existing := c.streams[event.streamID]; existing != nil {
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
		if _, wasClosed := c.closedClientStreams[event.streamID]; wasClosed {
			return &serverError{code: xhttp2.ErrCodeStreamClosed, err: errors.New("http2: headers on closed stream")}
		}
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: reused or skipped client stream id")}
	}
	c.lastClientStreamID = event.streamID
	if event.hasPriority && event.streamID == event.dependency {
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeProtocol)
	}
	if event.truncated {
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeEnhanceYourCalm)
	}
	if c.isGoingAway {
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeRefusedStream)
	}
	// Pushes are server-initiated; SETTINGS_MAX_CONCURRENT_STREAMS limits
	// only the peer's.
	if uint32(len(c.streams))-c.activePushes >= c.config.maxConcurrentStreams {
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeRefusedStream)
	}

	stream := newServerStream(c, event.streamID)
	stream.priority = priority{urgency: 3}
	requestCtx := c.protocolContext.AcquireRequestCtx(c.conn, stream)
	stream.request = requestCtx
	expectedBody, priorityValue, err := populateRequest(requestCtx, event.fields, c.config.enableExtendedConnect)
	if err != nil {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(err)
		releaseServerStream(stream)
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeProtocol)
	}
	if c.server.GetOnly && !requestCtx.Request.Header.IsGet() && !requestCtx.Request.Header.IsHead() {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(fasthttp.ErrGetOnly)
		releaseServerStream(stream)
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeRefusedStream)
	}
	stream.maxBody = c.server.MaxRequestBodySize
	if stream.maxBody <= 0 {
		stream.maxBody = fasthttp.DefaultMaxRequestBodySize
	}
	if c.server.HeaderReceived != nil {
		requestConfig := c.server.HeaderReceived(&requestCtx.Request.Header)
		if requestConfig.MaxRequestBodySize > 0 {
			stream.maxBody = requestConfig.MaxRequestBodySize
		}
	}
	stream.expectedBody = expectedBody
	isExtended := len(requestCtx.Request.Header.ConnectProtocol()) != 0
	if isExtended {
		stream.maxBody = 0
	}
	if priorityValue != "" {
		if parsed, parseErr := parsePriority(priorityValue); parseErr == nil {
			stream.priority = parsed
		}
	}
	if updated, ok := c.priorityUpdates[event.streamID]; ok {
		stream.priority = updated
		delete(c.priorityUpdates, event.streamID)
	}
	if stream.maxBody > 0 && expectedBody >= 0 && expectedBody > int64(stream.maxBody) {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(errRequestBodyTooLarge)
		releaseServerStream(stream)
		return c.rejectNewStream(event.streamID, xhttp2.ErrCodeCancel)
	}
	c.streams[event.streamID] = stream
	c.lastProcessedID = event.streamID
	// Extended CONNECT streams are tunnels: they never send END_STREAM, so a
	// request read timeout would kill them mid-conversation. processHeaders
	// already clears maxBody for the same reason.
	if !event.endStream && !isExtended && c.server.ReadTimeout > 0 {
		c.armRequestReadTimeout(stream)
	}

	if !event.endStream && (c.server.StreamRequestBody || isExtended) {
		streamID := stream.id
		stream.body = newRequestBody(func(consumed int) {
			select {
			case c.commands <- serverCommand{
				kind:     serverCommandBodyConsumed,
				streamID: streamID,
				consumed: consumed,
			}:
			case <-c.ctx.Done():
			}
		})
		bodySize := -1
		if stream.expectedBody >= 0 {
			bodySize = int(stream.expectedBody)
		}
		requestCtx.Request.SetBodyStream(stream.body, bodySize)
		c.startHandler(stream)
	}
	if event.endStream {
		return c.finishRequestBody(stream)
	}
	return nil
}

func (c *serverConn) rejectNewStream(streamID uint32, code xhttp2.ErrCode) error {
	delete(c.priorityUpdates, streamID)
	c.trackClosedClientStream(streamID)
	if err := c.recordRapidReset(); err != nil {
		return err
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
	requestHeader := &stream.request.Request.Header
	if stream.body == nil {
		// A partial trailer set on error is unobservable: the stream resets and
		// the RequestCtx is recycled first.
		if err := applyRequestTrailers(requestHeader, event.fields); err != nil {
			return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
		}
		return c.finishRequestBody(stream)
	}
	// A streaming body applies trailers only when its reader drains, too late
	// to reject the frame.
	var validationHeader fasthttp.RequestHeader
	if err := applyRequestTrailers(&validationHeader, event.fields); err != nil {
		return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
	}
	fields := append([]hpack.HeaderField(nil), event.fields...)
	stream.body.setEOFCommit(func() error {
		return applyRequestTrailers(requestHeader, fields)
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
		if !c.markClosedStreamReset(event.streamID) {
			return nil
		}
		if err := c.recordRapidReset(); err != nil {
			return err
		}
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeStreamClosed)
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
	if stream.maxBody > 0 && stream.bodyBytes > int64(stream.maxBody) {
		return c.rejectData(stream, flowLength, xhttp2.ErrCodeCancel, errRequestBodyTooLarge)
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
			return c.rejectData(stream, flowLength, xhttp2.ErrCodeCancel, err)
		}
		stream.unconsumedFlow += int64(payloadLength)
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
		stream.body.closeWithError(nil)
	}
	if !stream.handlerStarted {
		c.startHandler(stream)
	}
	return nil
}

func (c *serverConn) armRequestReadTimeout(stream *serverStream) {
	streamID := stream.id
	stream.readTimer = time.AfterFunc(c.server.ReadTimeout, func() {
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

// armResponseWriteTimeout starts the stall clock once: later zero-credit
// passes must not push the deadline out. Each arming has its own generation
// so a timeout that fires while progress resumes cannot reset the stream.
func (c *serverConn) armResponseWriteTimeout(stream *serverStream) {
	if c.config.writeByteTimeout <= 0 || len(stream.pendingData) == 0 || stream.writeTimer != nil {
		return
	}
	stream.writeTimeoutGen++
	streamID, generation := stream.id, stream.writeTimeoutGen
	stream.writeTimer = time.AfterFunc(c.config.writeByteTimeout, func() {
		select {
		case c.commands <- serverCommand{
			kind:       serverCommandResponseWriteTimeout,
			streamID:   streamID,
			generation: generation,
			err:        errStreamTimeout,
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
	if increment := stream.recv.consume(amount, int64(c.config.streamWindowSize)); increment != 0 {
		return c.framer.WriteWindowUpdate(stream.id, uint32(increment))
	}
	return nil
}

func (c *serverConn) consumeConnectionBytes(amount int64) error {
	if amount <= 0 {
		return nil
	}
	if increment := c.recv.consume(amount, int64(c.config.connectionWindowSize)); increment != 0 {
		return c.framer.WriteWindowUpdate(0, uint32(increment))
	}
	return nil
}

func (c *serverConn) restoreConnectionWindow(amount int64) {
	if amount <= 0 {
		return
	}
	c.recv.restore(amount)
	_ = c.framer.WriteWindowUpdate(0, uint32(amount))
}

func (c *serverConn) processRST(event *incomingFrame) error {
	stream := c.streams[event.streamID]
	if stream == nil {
		if c.streamIsIdle(event.streamID) {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: reset on idle stream")}
		}
		if err := c.recordRapidReset(); err != nil {
			return err
		}
		return nil
	}
	if err := c.recordRapidReset(); err != nil {
		return err
	}
	c.teardownStream(stream, fmt.Errorf("http2: peer reset stream: %s", event.errCode), errStreamClosed)
	return nil
}

// teardownStream moves stream to its terminal state and unblocks every caller
// waiting on it. The wire-side RST_STREAM, if any, is the caller's business.
func (c *serverConn) teardownStream(stream *serverStream, cause, pendingErr error) {
	stream.isReset = true
	stream.remoteClosed = true
	stream.localClosed = true
	stream.cancel(cause)
	if stream.body != nil {
		stream.body.discardWithError(errStreamClosed)
	}
	c.restoreUnconsumedFlow(stream)
	if stream.pendingAck != nil {
		stream.pendingAck <- pendingErr
		stream.pendingAck = nil
	}
	if stream.pendingWrite != nil {
		stream.pendingWrite.complete(pendingErr)
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
	if !stream.send.credit(event.increment) {
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
	if event.streamID > c.lastClientStreamID {
		if event.streamID&1 == 0 {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: priority update targets an invalid future stream")}
		}
		if _, exists := c.priorityUpdates[event.streamID]; exists {
			c.priorityUpdates[event.streamID] = updated
			return nil
		}
		limit := int(c.config.maxConcurrentStreams) * 4
		if len(c.priorityUpdates) >= limit && c.priorityPrunedThroughID < c.lastClientStreamID {
			c.prunePriorityUpdates()
			c.priorityPrunedThroughID = c.lastClientStreamID
		}
		if len(c.priorityUpdates) >= limit {
			// RFC 9218 §7: the signal is advisory, so drop it once the
			// bounded hint cache is full of still-plausible future streams.
			return nil
		}
		c.priorityUpdates[event.streamID] = updated
	}
	return nil
}

func (c *serverConn) prunePriorityUpdates() {
	for streamID := range c.priorityUpdates {
		if streamID <= c.lastClientStreamID {
			delete(c.priorityUpdates, streamID)
		}
	}
}

func (c *serverConn) processCommand(command *serverCommand) error {
	if command.kind == serverCommandHandlerDone &&
		command.generation == c.handlerGen && c.pendingHandlers > 0 {
		c.pendingHandlers--
	}
	stream := c.streams[command.streamID]
	if stream == nil {
		command.fail(errStreamClosed)
		return nil
	}
	switch command.kind {
	case serverCommandHandlerDone:
		c.releaseStreamWorker(stream)
		return c.handleHandlerDone(stream, command.requestCtx)
	case serverCommandInformational:
		block, err := c.encodeInformationalHeaders(
			command.statusCode,
			command.header,
			c.peerMaxHeaderListSize,
		)
		if err != nil {
			// Nothing reached the HPACK encoder, so this is the handler's
			// problem, not the connection's.
			command.result <- err
			return nil //nolint:nilerr
		}
		err = c.writeHeaderBlock(stream.id, false, block)
		command.result <- err
		return err
	case serverCommandBodyConsumed:
		if stream.discardRequestBody || stream.isReset {
			return nil
		}
		stream.unconsumedFlow -= int64(command.consumed)
		if stream.unconsumedFlow < 0 {
			return errors.New("http2: request body flow accounting underflow")
		}
		return c.consumeRequestBytes(stream, int64(command.consumed))
	case serverCommandResponseData:
		if stream.isReset || stream.localClosed {
			command.fail(errStreamClosed)
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
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, err)
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
			command.result <- errStreamClosed
			return nil
		}
		if command.err != nil && !errors.Is(command.err, io.EOF) {
			command.result <- command.err
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, command.err)
		}
		if stream.expectedResponse >= 0 && stream.responseBytes != stream.expectedResponse {
			err := errors.New("http2: response body length doesn't match content-length")
			command.result <- err
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, err)
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
		if stream.localClosed {
			return c.finishResponse(stream)
		}
		stream.responseEOF = true
		c.queueFlush(stream)
		return nil
	case serverCommandPush:
		err := c.handlePush(stream, command.target, command.pushOpts)
		command.result <- err
		return nil
	case serverCommandCloseRead:
		stream.discardRequestBody = true
		if stream.unconsumedFlow > 0 {
			amount := stream.unconsumedFlow
			stream.unconsumedFlow = 0
			if err := c.consumeRequestBytes(stream, amount); err != nil {
				command.result <- err
				return err
			}
		}
		if stream.body != nil {
			stream.body.closeWithError(net.ErrClosed)
		}
		command.result <- nil
		return nil
	case serverCommandRequestReadTimeout:
		if stream.remoteClosed {
			return nil
		}
		return c.resetStream(stream.id, xhttp2.ErrCodeCancel, command.err)
	case serverCommandCancelStream:
		return c.resetStream(stream.id, xhttp2.ErrCodeCancel, command.err)
	case serverCommandCancelWrite:
		if command.write == nil || command.write.done {
			return nil
		}
		if stream.pendingWrite != command.write {
			command.write.complete(errStreamClosed)
			return nil
		}
		return c.resetStream(stream.id, xhttp2.ErrCodeCancel, command.err)
	case serverCommandResponseWriteTimeout:
		if command.generation != stream.writeTimeoutGen || stream.writeTimer == nil {
			return nil
		}
		return c.resetStream(stream.id, xhttp2.ErrCodeCancel, command.err)
	default:
		return nil
	}
}

func (c *serverConn) runHandler(stream *serverStream) {
	c.server.Handler(stream.request)
	select {
	case c.commands <- serverCommand{
		kind:       serverCommandHandlerDone,
		streamID:   stream.id,
		generation: stream.handlerGen,
		requestCtx: stream.request,
	}:
	case <-c.ctx.Done():
	}
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
		replacement := c.protocolContext.AcquireRequestCtx(c.conn, stream)
		replacement.Request.Header.SetMethodBytes(requestCtx.Request.Header.Method())
		timeoutResponse.CopyTo(&replacement.Response)
		stream.request = replacement
		// The timed-out handler goroutine still references the original
		// RequestCtx, so neither it nor this stream may return to a pool
		// until the connection ends; releaseAllStreams checks this flag.
		stream.hasAbandonedRequest = true
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		requestCtx = replacement
	}
	if stream.isReset {
		c.maybeFinalizeStream(stream)
		return nil
	}
	if stream.hijackRejected {
		requestCtx.Response.Reset()
		requestCtx.Response.SetStatusCode(fasthttp.StatusNotImplemented)
		requestCtx.Response.SetBodyString("connection hijacking isn't supported for HTTP/2 requests")
	}
	statusCode := requestCtx.Response.StatusCode()
	if statusCode >= 100 && statusCode < 200 {
		return c.resetStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: final response cannot be informational"))
	}
	hasContentLength := len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) != 0
	if statusCode == fasthttp.StatusNoContent &&
		(requestCtx.Response.IsBodyStream() || len(requestCtx.Response.Body()) != 0 || hasContentLength) {
		return c.resetStream(
			stream.id,
			xhttp2.ErrCodeInternal,
			errors.New("http2: 204 response cannot contain a body or content-length"),
		)
	}
	// DATA on an established tunnel is tunnel bytes, not a body to measure.
	establishesTunnel := requestCtx.Request.Header.IsConnect() && statusCode >= 200 && statusCode < 300
	if establishesTunnel && hasContentLength {
		return c.resetStream(
			stream.id,
			xhttp2.ErrCodeInternal,
			errors.New("http2: 2xx response to CONNECT cannot have content-length"),
		)
	}

	var streamHandler fasthttp.StreamHandler
	if c.config.enableExtendedConnect {
		stream.acceptMu.Lock()
		streamHandler = stream.streamHandler
		if streamHandler != nil && (statusCode < 200 || statusCode >= 300) {
			stream.streamHandler = nil
			streamHandler = nil
		}
		stream.acceptMu.Unlock()
	}
	if streamHandler != nil {
		if requestCtx.Response.IsBodyStream() || len(requestCtx.Response.Body()) != 0 ||
			len(requestCtx.Response.Header.PeekTrailerKeys()) != 0 || hasContentLength {
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: accepted stream response cannot have an HTTP body"))
		}
		block, err := c.encodeResponseHeaders(
			c.server,
			&requestCtx.Response,
			c.peerMaxHeaderListSize,
			c.protocolContext.ServerDate(),
		)
		if err != nil {
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, err)
		}
		if err := c.writeHeaderBlock(stream.id, false, block); err != nil {
			return err
		}
		stream.responseHeaderSent = true
		c.startStreamHandler(stream, streamHandler)
		return nil
	}

	mustNotHaveBody := responseMustNotHaveBody(requestCtx)
	var bufferedBody []byte
	if !requestCtx.Response.IsBodyStream() {
		bufferedBody = requestCtx.Response.Body()
		if !establishesTunnel && !hasContentLength && (!mustNotHaveBody || len(bufferedBody) != 0) {
			requestCtx.Response.Header.SetContentLength(len(bufferedBody))
			hasContentLength = true
		}
	}
	expectedResponse := int64(-1)
	if hasContentLength {
		expectedResponse = int64(requestCtx.Response.Header.ContentLength())
	}
	block, err := c.encodeResponseHeaders(
		c.server,
		&requestCtx.Response,
		c.peerMaxHeaderListSize,
		c.protocolContext.ServerDate(),
	)
	if err != nil {
		return c.resetStream(stream.id, xhttp2.ErrCodeInternal, err)
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
		c.startResponsePump(stream)
		return nil
	}

	body := bufferedBody
	stream.expectedResponse = expectedResponse
	if stream.expectedResponse >= 0 && stream.expectedResponse != int64(len(body)) {
		return c.resetStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: response body length doesn't match content-length"))
	}
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
	if len(body) != 0 {
		stream.pendingData = body
	}
	stream.responseEOF = true
	c.queueFlush(stream)
	return nil
}

func (c *serverConn) startResponsePump(stream *serverStream) {
	stream.responsePumpStarted = true
	c.workers.Add(1)
	go func() {
		defer func() {
			select {
			case c.commands <- serverCommand{kind: serverCommandResponsePumpDone, streamID: stream.id}:
			case <-c.ctx.Done():
			}
			c.workers.Done()
		}()
		reader := stream.request.Response.BodyStream()
		buffer := acquireResponsePumpBuffer()
		defer releaseResponsePumpBuffer(buffer)
		for {
			n, readErr := reader.Read(buffer.data)
			if n > 0 {
				result := make(chan error, 1)
				command := serverCommand{
					kind:     serverCommandResponseData,
					streamID: stream.id,
					data:     buffer.data[:n],
					result:   result,
				}
				select {
				case c.commands <- command:
				case <-stream.Done():
					_ = stream.request.Response.CloseBodyStream()
					return
				}
				select {
				case err := <-result:
					if err != nil {
						_ = stream.request.Response.CloseBodyStream()
						return
					}
				case <-stream.Done():
					_ = stream.request.Response.CloseBodyStream()
					return
				}
			}
			if readErr != nil {
				_ = stream.request.Response.CloseBodyStream()
				result := make(chan error, 1)
				select {
				case c.commands <- serverCommand{
					kind:     serverCommandResponseEOF,
					streamID: stream.id,
					err:      readErr,
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
	}()
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
	c.workers.Go(func() {
		var reader io.Reader = bytes.NewReader(nil)
		if stream.body != nil {
			reader = stream.body
		}
		streamConn := &streamConn{streamConnState: streamConnState{netConn: c.conn}, stream: stream, read: reader}
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
func (c *serverConn) flushStream(stream *serverStream, drain bool) (bool, error) {
	for {
		canSendData := len(stream.pendingData) != 0 && !stream.isReset && !stream.localClosed
		if !canSendData {
			break
		}
		amount := c.reserveDataChunk(&stream.streamFlowState, len(stream.pendingData))
		if amount == 0 {
			// Only a flow-control stall waits on the peer, so only it gets
			// the timeout.
			c.armResponseWriteTimeout(stream)
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
		// Progress restarts the stall clock; the next zero-credit pass re-arms it.
		c.stopResponseWriteTimeout(stream)
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
		// Nothing reached the HPACK encoder, so this is the stream's problem.
		return false, c.resetStream(stream.id, xhttp2.ErrCodeInternal, err)
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
		return nil
	}
	if !stream.remoteClosed {
		if err := c.recordRapidReset(); err != nil {
			return err
		}
		if err := c.framer.WriteRSTStream(stream.id, xhttp2.ErrCodeNo); err != nil {
			return err
		}
		stream.remoteClosed = true
		if stream.body != nil {
			stream.body.discardWithError(errStreamClosed)
		}
		c.restoreUnconsumedFlow(stream)
	}
	c.maybeFinalizeStream(stream)
	return nil
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
	delete(c.streams, stream.id)
	if stream.id&1 == 1 {
		c.trackClosedClientStream(stream.id)
	}
	if stream.isPush {
		c.activePushes--
	}
	stream.cancel(errStreamClosed)
	if stream.request != nil {
		c.protocolContext.ReleaseRequestCtx(stream.request)
		stream.request = nil
	}
	if !stream.hasAbandonedRequest {
		releaseServerStream(stream)
	}
}

// markClosedStreamReset reports whether a STREAM_CLOSED reset is still owed
// for streamID, and records that it was sent. RFC 9113 §5.4.2: at most one
// RST_STREAM per stream, however many stale DATA frames arrive.
func (c *serverConn) markClosedStreamReset(streamID uint32) bool {
	alreadyReset, tracked := c.closedClientStreams[streamID]
	if tracked {
		if alreadyReset {
			return false
		}
	} else {
		c.trackClosedClientStream(streamID)
	}
	c.closedClientStreams[streamID] = true
	return true
}

// trackClosedClientStream records a closed client stream. Stream IDs are never
// reused, so an ID is tracked at most once.
func (c *serverConn) trackClosedClientStream(streamID uint32) {
	c.closedClientStreams[streamID] = false
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

func (c *serverConn) resetStream(streamID uint32, code xhttp2.ErrCode, cause error) error {
	stream := c.streams[streamID]
	if stream != nil && stream.isReset {
		return nil
	}
	if err := c.recordRapidReset(); err != nil {
		return err
	}
	if err := c.framer.WriteRSTStream(streamID, code); err != nil {
		return err
	}
	if stream != nil {
		c.teardownStream(stream, cause, cause)
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
	return c.framer.WriteGoAway(c.lastProcessedID, xhttp2.ErrCodeNo, nil)
}

func (c *serverConn) failConnection(code xhttp2.ErrCode, cause error) error {
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
	if parent.isReset || parent.localClosed || parent.responseHeaderSent || parent.handlerDone {
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

	parentURI := parent.request.URI()
	uri := fasthttp.AcquireURI()
	defer fasthttp.ReleaseURI(uri)
	if err := uri.Parse(parentURI.Host(), []byte(target)); err != nil {
		return fmt.Errorf("http2: invalid pushed request target: %w", err)
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
		c.peerMaxHeaderListSize,
		false,
	)
	if err != nil {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(err)
		releaseServerStream(stream)
		return err
	}
	if err := c.writePushPromise(parent.id, streamID, block); err != nil {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(err)
		releaseServerStream(stream)
		return err
	}
	c.nextPushStreamID += 2
	c.activePushes++
	c.streams[streamID] = stream
	c.startHandler(stream)
	return nil
}

func (c *serverConn) writePushPromise(parentID, promisedID uint32, block []byte) error {
	firstLength := min(len(block), c.peerMaxFrameSize)
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

func responseMustNotHaveBody(ctx *fasthttp.RequestCtx) bool {
	if ctx.Request.Header.IsHead() {
		return true
	}
	statusCode := ctx.Response.StatusCode()
	return statusCode >= 100 && statusCode < 200 || statusCode == 204 || statusCode == 304
}

type priority struct {
	urgency     uint8
	incremental bool
}

func parsePriority(value string) (priority, error) {
	result := priority{urgency: 3}
	seenUrgency := false
	seenIncremental := false
	for member := range strings.SplitSeq(value, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			return priority{}, errors.New("http2: invalid empty priority member")
		}
		item, _, _ := strings.Cut(member, ";")
		item = strings.TrimSpace(item)
		switch {
		case item == "i" || item == "i=?1":
			if seenIncremental {
				return priority{}, errors.New("http2: duplicate incremental priority")
			}
			seenIncremental = true
			result.incremental = true
		case item == "i=?0":
			if seenIncremental {
				return priority{}, errors.New("http2: duplicate incremental priority")
			}
			seenIncremental = true
		case strings.HasPrefix(item, "i="):
			return priority{}, errors.New("http2: invalid incremental priority")
		case strings.HasPrefix(item, "u="):
			if seenUrgency {
				return priority{}, errors.New("http2: duplicate urgency priority")
			}
			seenUrgency = true
			urgencyText := strings.TrimPrefix(item, "u=")
			if urgencyText == "" || strings.HasPrefix(urgencyText, "+") {
				return priority{}, errors.New("http2: invalid priority urgency")
			}
			urgency, err := strconv.Atoi(urgencyText)
			if err != nil || urgency < 0 || urgency > 7 {
				return priority{}, errors.New("http2: invalid priority urgency")
			}
			result.urgency = uint8(urgency)
		}
	}
	return result, nil
}

func errorCode(err error) xhttp2.ErrCode {
	if errors.Is(err, xhttp2.ErrFrameTooLarge) {
		return xhttp2.ErrCodeFrameSize
	}
	var connectionError xhttp2.ConnectionError
	if errors.As(err, &connectionError) {
		return xhttp2.ErrCode(connectionError)
	}
	return xhttp2.ErrCodeProtocol
}

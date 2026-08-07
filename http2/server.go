package http2

import (
	"bufio"
	"bytes"
	"container/heap"
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
	incomingFrameReadIdle
	incomingFrameStreamError
)

type incomingFrame struct {
	kind            incomingFrameKind
	streamID        uint32
	endStream       bool
	ack             bool
	truncated       bool
	flowLength      int
	data            []byte
	fields          []hpack.HeaderField
	settings        []xhttp2.Setting
	increment       uint32
	errCode         xhttp2.ErrCode
	lastStreamID    uint32
	pingData        [8]byte
	priority        string
	dependency      uint32
	hasPriority     bool
	morePending     bool
	frameType       xhttp2.FrameType
	err             error
	fieldStorage    *incomingHeaderFieldStorage
	dataBuffer      *incomingDataBuffer
	settingsStorage *incomingSettingsStorage
}

type incomingHeaderFieldStorage struct {
	fields []hpack.HeaderField
}

type incomingDataBuffer struct {
	data []byte
}

type incomingSettingsStorage struct {
	settings []xhttp2.Setting
}

type responsePumpBuffer struct {
	data []byte
}

type streamIDHeap []uint32

func (h *streamIDHeap) Len() int           { return len(*h) }
func (h *streamIDHeap) Less(i, j int) bool { return (*h)[i] < (*h)[j] }
func (h *streamIDHeap) Swap(i, j int)      { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
func (h *streamIDHeap) Push(value any)     { *h = append(*h, value.(uint32)) } //nolint:forcetypeassert
func (h *streamIDHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = 0
	*h = old[:last]
	return value
}

var (
	incomingHeaderFieldsPool sync.Pool
	incomingDataPool         sync.Pool
	incomingSettingsPool     sync.Pool
	responsePumpBufferPool   sync.Pool
)

var shutdownPingData = [8]byte{'f', 'a', 's', 't', 'h', '2', 'g', 'o'}

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
	handlerGen uint32
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
	bufferedWriter  flushWriter
	writer          *asyncFrameWriter
	encoder         *hpack.Encoder
	headerBuffer    bytes.Buffer
	headerStrings   headerStringCache

	events   chan incomingFrame
	commands chan serverCommand
	workers  sync.WaitGroup

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
	receivedSettings   bool

	peerInitialStreamWindow  int64
	peerConnectionWindow     int64
	peerMaxFrameSize         int
	peerMaxHeaderListSize    uint64
	peerMaxConcurrentStreams uint32
	peerAllowsPush           bool

	receiveConnectionWindow int64
	pendingConnectionUpdate int64
	bufferedRequestBytes    int64
	nextPushStreamID        uint32
	activePushes            uint32
	priorityUpdates         map[uint32]priority
	priorityUpdateOrder     []uint32
	priorityUpdateHeap      streamIDHeap
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
		protocolContext:          protocolContext,
		server:                   protocolContext.Server(),
		config:                   *config,
		conn:                     c,
		ctx:                      ctx,
		cancel:                   cancel,
		streams:                  make(map[uint32]*serverStream),
		peerInitialStreamWindow:  65535,
		peerConnectionWindow:     65535,
		peerMaxFrameSize:         defaultMaxFrameSize,
		peerMaxHeaderListSize:    math.MaxUint32,
		peerMaxConcurrentStreams: math.MaxUint32,
		peerAllowsPush:           true,
		receiveConnectionWindow:  int64(config.connectionWindowSize),
		nextPushStreamID:         2,
		priorityUpdates:          make(map[uint32]priority),
		closedClientStreams:      make(map[uint32]bool),
	}
	conn.encoder = hpack.NewEncoder(&conn.headerBuffer)
	conn.encoder.SetMaxDynamicTableSizeLimit(config.maxEncoderTableSize)
	return conn
}

func (c *serverConn) initQueues() {
	if c.events != nil {
		return
	}
	maxCommands := min(int(c.config.maxConcurrentStreams)*2+32, c.config.maxQueuedCommands)
	c.events = make(chan incomingFrame, 64)
	c.commands = make(chan serverCommand, maxCommands)
}

func (c *serverConn) serve(upgrade *serverUpgrade) (retErr error) {
	defer func() {
		c.cancel(retErr)
		for _, stream := range c.streams {
			stream.cancel(retErr)
			if stream.body != nil {
				stream.body.closeWithError(retErr)
			}
			if stream.pendingAck != nil {
				stream.pendingAck <- retErr
				stream.pendingAck = nil
			}
			if stream.pendingWrite != nil {
				stream.pendingWrite.complete(retErr)
				stream.pendingWrite = nil
			}
		}
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

	if upgrade == nil {
		if err := validateTLSConnection(c.conn); err != nil {
			return err
		}
		if err := c.readClientPreface(); err != nil {
			_ = c.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			_, _ = io.CopyN(io.Discard, c.conn, 64<<10)
			return err
		}
	} else {
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
	}

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
	c.bufferedWriter = c.writer
	readBufferSize := c.server.ReadBufferSize
	if readBufferSize <= 0 {
		readBufferSize = defaultReadBufferSize
	}
	reader := bufio.NewReaderSize(c.conn, readBufferSize)
	c.framer = xhttp2.NewFramer(c.bufferedWriter, reader)
	c.framer.SetReuseFrames()
	c.frames = newFrameReader(c.framer, reader)
	c.headerDecoder = newHeaderCodec(c.config.maxDecoderTableSize, c.config.maxHeaderListSize)
	c.framer.SetMaxReadFrameSize(c.config.maxReadFrameSize)
	if upgrade == nil {
		if err := c.writeInitialSettings(); err != nil {
			return fmt.Errorf("http2: writing initial settings: %w", err)
		}
		if err := c.bufferedWriter.Flush(); err != nil {
			return fmt.Errorf("http2: flushing initial settings: %w", err)
		}
	}
	c.initQueues()
	if upgrade != nil {
		c.bootstrapUpgradedStream(upgrade.request)
	}

	go c.readLoop()
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
		// Drain up to 63 more events/commands before flushing, so one burst
		// becomes one write. Dry channels don't end the batch while frames are
		// still buffered or started handlers haven't reported back; the timer
		// bounds that wait in case such a handler is slow.
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
			stopTimer(coalesceTimer)
			coalesceArmed = false
		}
		if err := c.flushResponses(); err != nil {
			return err
		}
		if err := c.bufferedWriter.Flush(); err != nil {
			return err
		}
		if idleTimer != nil {
			switch {
			case len(c.streams) == 0 && !idleArmed:
				resetTimer(idleTimer, c.config.idleTimeout)
				idleArmed = true
			case len(c.streams) != 0 && idleArmed:
				stopTimer(idleTimer)
				idleArmed = false
			}
		}
	}
}

type directFrameWriter struct {
	conn    net.Conn
	timeout time.Duration
}

func (w directFrameWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) != 0 {
		if w.timeout > 0 {
			if err := w.conn.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil {
				return written, err
			}
		}
		n, err := w.conn.Write(data)
		written += n
		data = data[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
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
	waitingForPing := false
	for {
		readTimeout := c.config.readIdleTimeout
		if waitingForPing {
			readTimeout = c.config.pingTimeout
		}
		if readTimeout > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		}
		frame, err := c.frames.readFrame()
		if err != nil && isTimeout(err) && c.config.readIdleTimeout > 0 && !waitingForPing {
			select {
			case c.events <- incomingFrame{kind: incomingFrameReadIdle}:
			case <-c.ctx.Done():
				return
			}
			waitingForPing = true
			continue
		}
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
			waitingForPing = false
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
			event.settingsStorage = acquireIncomingSettings(frame.NumSettings())
			event.settings = event.settingsStorage.settings
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

func acquireIncomingSettings(length int) *incomingSettingsStorage {
	if value := incomingSettingsPool.Get(); value != nil {
		storage := value.(*incomingSettingsStorage) //nolint:forcetypeassert
		if cap(storage.settings) >= length {
			storage.settings = storage.settings[:length]
			return storage
		}
	}
	return &incomingSettingsStorage{settings: make([]xhttp2.Setting, length)}
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
	if event.settingsStorage != nil && cap(event.settingsStorage.settings) <= 64 {
		event.settingsStorage.settings = event.settingsStorage.settings[:0]
		incomingSettingsPool.Put(event.settingsStorage)
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
	case incomingFrameReadIdle:
		return c.framer.WritePing(false, [8]byte{'f', 'a', 's', 't', 'h', '2', 0, 1})
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
	c.prunePriorityUpdates(event.streamID)
	delete(c.priorityUpdates, event.streamID)
	c.rememberClosedClientStream(event.streamID)
	if c.config.countError != nil {
		c.config.countError("stream_" + strings.ToLower(event.errCode.String()))
	}
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

	// RFC 9113 requires settings to be processed in order and the last value
	// to win. For INITIAL_WINDOW_SIZE, all intermediate deltas telescope from
	// the value at frame start. Checking the largest one preserves overflow
	// detection while walking every open stream exactly once, rather than once
	// per repeated setting.
	if final.hasInitialWindowSize {
		for _, stream := range c.streams {
			if stream.sendWindow+maxInitialWindowDelta > math.MaxInt32 {
				return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: stream send window overflow")}
			}
		}
		finalDelta := int64(final.initialWindowSize) - c.peerInitialStreamWindow
		for _, stream := range c.streams {
			stream.sendWindow += finalDelta
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
	c.prunePriorityUpdates(event.streamID)
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
	expectedBody, err := populateRequest(requestCtx, c.server, event.fields, c.config.enableExtendedConnect)
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
	if value := requestCtx.Request.Header.Peek("Priority"); len(value) != 0 {
		parsed, parseErr := parsePriority(string(value))
		if parseErr == nil {
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
	c.rememberClosedClientStream(streamID)
	if c.config.countError != nil {
		c.config.countError("stream_" + strings.ToLower(code.String()))
	}
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
	var validationHeader fasthttp.RequestHeader
	if err := applyRequestTrailers(&validationHeader, event.fields); err != nil {
		return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
	}
	fields := append([]hpack.HeaderField(nil), event.fields...)
	requestHeader := &stream.request.Request.Header
	if stream.body == nil {
		if err := applyRequestTrailers(requestHeader, fields); err != nil {
			return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
		}
	} else {
		stream.body.setEOFCommit(func() error {
			return applyRequestTrailers(requestHeader, fields)
		})
	}
	return c.finishRequestBody(stream)
}

func (c *serverConn) processData(event *incomingFrame) error {
	stream := c.streams[event.streamID]
	if stream == nil && c.streamIsIdle(event.streamID) {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: data on idle stream")}
	}
	flowLength := int64(event.flowLength)
	payloadLength := len(event.data)
	c.receiveConnectionWindow -= flowLength
	if c.receiveConnectionWindow < 0 {
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
		c.restoreConnectionWindow(flowLength)
		return c.resetStream(stream.id, xhttp2.ErrCodeStreamClosed, errors.New("http2: data on half-closed remote stream"))
	}
	stream.recvWindow -= flowLength
	if stream.recvWindow < 0 {
		c.restoreConnectionWindow(flowLength)
		return c.resetStream(stream.id, xhttp2.ErrCodeFlowControl, errors.New("http2: stream receive window exceeded"))
	}
	stream.bodyBytes += int64(payloadLength)
	if stream.expectedBody >= 0 && stream.bodyBytes > stream.expectedBody {
		c.restoreConnectionWindow(flowLength)
		return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, errors.New("http2: request body exceeds content-length"))
	}
	if stream.maxBody > 0 && stream.bodyBytes > int64(stream.maxBody) {
		c.restoreConnectionWindow(flowLength)
		return c.resetStream(stream.id, xhttp2.ErrCodeCancel, errRequestBodyTooLarge)
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
			c.restoreConnectionWindow(flowLength)
			return c.resetStream(stream.id, xhttp2.ErrCodeCancel, err)
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
			c.restoreConnectionWindow(flowLength)
			return c.resetStream(
				stream.id,
				xhttp2.ErrCodeEnhanceYourCalm,
				errors.New("http2: connection buffered request body limit exceeded"),
			)
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

func (c *serverConn) armResponseWriteTimeout(stream *serverStream) {
	if c.config.writeByteTimeout <= 0 || len(stream.pendingData) == 0 {
		return
	}
	streamID := stream.id
	if stream.writeTimer == nil {
		stream.writeTimer = time.AfterFunc(c.config.writeByteTimeout, func() {
			select {
			case c.commands <- serverCommand{
				kind:     serverCommandResponseWriteTimeout,
				streamID: streamID,
				err:      timeoutError{},
			}:
			case <-c.ctx.Done():
			}
		})
		return
	}
	stream.writeTimer.Reset(c.config.writeByteTimeout)
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
	stream.pendingWindowUpdate += amount
	streamIncrement := int64(0)
	if stream.pendingWindowUpdate >= int64(c.config.streamWindowSize)/2 {
		streamIncrement = stream.pendingWindowUpdate
		stream.pendingWindowUpdate = 0
		stream.recvWindow += streamIncrement
	}
	if streamIncrement != 0 {
		return c.framer.WriteWindowUpdate(stream.id, uint32(streamIncrement))
	}
	return nil
}

func (c *serverConn) consumeConnectionBytes(amount int64) error {
	if amount <= 0 {
		return nil
	}
	c.pendingConnectionUpdate += amount
	connectionIncrement := int64(0)
	if c.pendingConnectionUpdate >= int64(c.config.connectionWindowSize)/2 {
		connectionIncrement = c.pendingConnectionUpdate
		c.pendingConnectionUpdate = 0
		c.receiveConnectionWindow += connectionIncrement
	}
	if connectionIncrement != 0 {
		return c.framer.WriteWindowUpdate(0, uint32(connectionIncrement))
	}
	return nil
}

func (c *serverConn) restoreConnectionWindow(amount int64) {
	if amount <= 0 {
		return
	}
	c.receiveConnectionWindow += amount
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
	stream.isReset = true
	stream.remoteClosed = true
	stream.localClosed = true
	stream.cancel(fmt.Errorf("http2: peer reset stream: %s", event.errCode))
	if stream.body != nil {
		stream.body.discardWithError(errStreamClosed)
	}
	c.restoreUnconsumedFlow(stream)
	if stream.pendingAck != nil {
		stream.pendingAck <- errStreamClosed
		stream.pendingAck = nil
	}
	stream.pendingData = nil
	stream.responseEOF = false
	if !stream.handlerStarted || stream.handlerDone {
		c.maybeFinalizeStream(stream)
	}
	return nil
}

func (c *serverConn) processWindowUpdate(event *incomingFrame) error {
	if event.streamID == 0 {
		if c.peerConnectionWindow+int64(event.increment) > math.MaxInt32 {
			return &serverError{
				code: xhttp2.ErrCodeFlowControl,
				err: fmt.Errorf(
					"http2: server connection send window overflow: current=%d increment=%d",
					c.peerConnectionWindow,
					event.increment,
				),
			}
		}
		c.peerConnectionWindow += int64(event.increment)
		return nil
	}
	stream := c.streams[event.streamID]
	if stream == nil {
		if c.streamIsIdle(event.streamID) {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: window update on idle stream")}
		}
		return nil
	}
	if stream.sendWindow+int64(event.increment) > math.MaxInt32 {
		return c.resetStream(stream.id, xhttp2.ErrCodeFlowControl, errors.New("http2: stream send window overflow"))
	}
	stream.sendWindow += int64(event.increment)
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
		if len(c.priorityUpdates) >= limit {
			c.evictOldestPriorityUpdate()
		}
		c.priorityUpdates[event.streamID] = updated
		c.priorityUpdateOrder = append(c.priorityUpdateOrder, event.streamID)
		heap.Push(&c.priorityUpdateHeap, event.streamID)
		if len(c.priorityUpdateOrder) > limit*2 || len(c.priorityUpdateHeap) > limit*2 {
			c.compactPriorityUpdateOrder()
		}
	}
	return nil
}

func (c *serverConn) prunePriorityUpdates(lastStreamID uint32) {
	for len(c.priorityUpdateHeap) != 0 && c.priorityUpdateHeap[0] < lastStreamID {
		streamID := heap.Pop(&c.priorityUpdateHeap).(uint32) //nolint:forcetypeassert
		delete(c.priorityUpdates, streamID)
	}
	limit := int(c.config.maxConcurrentStreams) * 4
	if len(c.priorityUpdateOrder) > limit*2 || len(c.priorityUpdateHeap) > limit*2 {
		c.compactPriorityUpdateOrder()
	}
}

func (c *serverConn) evictOldestPriorityUpdate() {
	for len(c.priorityUpdateOrder) != 0 {
		streamID := c.priorityUpdateOrder[0]
		c.priorityUpdateOrder = c.priorityUpdateOrder[1:]
		if _, exists := c.priorityUpdates[streamID]; exists {
			delete(c.priorityUpdates, streamID)
			return
		}
	}
}

func (c *serverConn) compactPriorityUpdateOrder() {
	order := c.priorityUpdateOrder[:0]
	for _, streamID := range c.priorityUpdateOrder {
		if _, exists := c.priorityUpdates[streamID]; exists {
			order = append(order, streamID)
		}
	}
	c.priorityUpdateOrder = order
	c.priorityUpdateHeap = c.priorityUpdateHeap[:0]
	for streamID := range c.priorityUpdates {
		c.priorityUpdateHeap = append(c.priorityUpdateHeap, streamID)
	}
	heap.Init(&c.priorityUpdateHeap)
}

func (c *serverConn) processCommand(command *serverCommand) error {
	if command.kind == serverCommandHandlerDone &&
		command.handlerGen == c.handlerGen && c.pendingHandlers > 0 {
		c.pendingHandlers--
	}
	stream := c.streams[command.streamID]
	if stream == nil {
		if command.write != nil {
			command.write.complete(errStreamClosed)
		}
		if command.result != nil {
			command.result <- errStreamClosed
		}
		return nil
	}
	switch command.kind {
	case serverCommandHandlerDone:
		c.releaseStreamWorker(stream)
		return c.handleHandlerDone(stream, command.requestCtx)
	case serverCommandInformational:
		block, err := encodeInformationalHeaders(
			c.encoder,
			&c.headerBuffer,
			&c.headerStrings,
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
			if command.write != nil {
				command.write.complete(errStreamClosed)
			} else {
				command.result <- errStreamClosed
			}
			return nil
		}
		if len(stream.pendingData) != 0 || stream.pendingAck != nil || stream.pendingWrite != nil || stream.responseEOF {
			err := errors.New("http2: response stream has pending data")
			if command.write != nil {
				command.write.complete(err)
			} else {
				command.result <- err
			}
			return nil
		}
		if stream.expectedResponse >= 0 && stream.responseBytes+int64(len(command.data)) > stream.expectedResponse {
			err := errors.New("http2: response body exceeds content-length")
			if command.write != nil {
				command.write.complete(err)
			} else {
				command.result <- err
			}
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
		c.armResponseWriteTimeout(stream)
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
		command.result <- command.err
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
		if len(stream.pendingData) == 0 {
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
		handlerGen: stream.handlerGen,
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
	if errors.Is(requestCtx.LastProtocolError(), fasthttp.ErrHijackNotSupported) {
		requestCtx.Response.Reset()
		requestCtx.Response.SetStatusCode(fasthttp.StatusNotImplemented)
		requestCtx.Response.SetBodyString("connection hijacking isn't supported for HTTP/2 requests")
	}
	statusCode := requestCtx.Response.StatusCode()
	if statusCode >= 100 && statusCode < 200 {
		return c.resetStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: final response cannot be informational"))
	}
	if statusCode == fasthttp.StatusNoContent &&
		(requestCtx.Response.IsBodyStream() || len(requestCtx.Response.Body()) != 0 ||
			len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) != 0) {
		return c.resetStream(
			stream.id,
			xhttp2.ErrCodeInternal,
			errors.New("http2: 204 response cannot contain a body or content-length"),
		)
	}
	// DATA on an established tunnel is tunnel bytes, not a body to measure.
	establishesTunnel := requestCtx.Request.Header.IsConnect() && statusCode >= 200 && statusCode < 300
	if establishesTunnel && len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) != 0 {
		return c.resetStream(
			stream.id,
			xhttp2.ErrCodeInternal,
			errors.New("http2: 2xx response to CONNECT cannot have content-length"),
		)
	}

	stream.acceptMu.Lock()
	streamHandler := stream.streamHandler
	if streamHandler != nil && (statusCode < 200 || statusCode >= 300) {
		stream.streamHandler = nil
		streamHandler = nil
	}
	stream.acceptMu.Unlock()
	if streamHandler != nil {
		if requestCtx.Response.IsBodyStream() || len(requestCtx.Response.Body()) != 0 ||
			len(requestCtx.Response.Header.PeekTrailerKeys()) != 0 ||
			len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) != 0 {
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: accepted stream response cannot have an HTTP body"))
		}
		block, err := encodeResponseHeaders(
			c.encoder,
			&c.headerBuffer,
			&c.headerStrings,
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

	var bufferedBody []byte
	if !requestCtx.Response.IsBodyStream() {
		bufferedBody = requestCtx.Response.Body()
		if !establishesTunnel &&
			len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) == 0 &&
			(!responseMustNotHaveBody(requestCtx) || len(bufferedBody) != 0) {
			requestCtx.Response.Header.SetContentLength(len(bufferedBody))
		}
	}
	block, err := encodeResponseHeaders(
		c.encoder,
		&c.headerBuffer,
		&c.headerStrings,
		c.server,
		&requestCtx.Response,
		c.peerMaxHeaderListSize,
		c.protocolContext.ServerDate(),
	)
	if err != nil {
		return c.resetStream(stream.id, xhttp2.ErrCodeInternal, err)
	}
	stream.responseHasTrailers = len(requestCtx.Response.Header.PeekTrailerKeys()) != 0
	if responseMustNotHaveBody(requestCtx) {
		if err := c.writeHeaderBlock(stream.id, true, block); err != nil {
			return err
		}
		stream.responseHeaderSent = true
		stream.localClosed = true
		return c.finishResponse(stream)
	}
	if requestCtx.Response.IsBodyStream() {
		stream.expectedResponse = responseContentLength(&requestCtx.Response.Header)
		if err := c.writeHeaderBlock(stream.id, false, block); err != nil {
			return err
		}
		stream.responseHeaderSent = true
		c.startResponsePump(stream)
		return nil
	}

	body := bufferedBody
	stream.expectedResponse = responseContentLength(&requestCtx.Response.Header)
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
		c.armResponseWriteTimeout(stream)
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
		streamConn := &streamConn{stream: stream, read: reader}
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
		madeProgress := false
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
			// The queue ascends by stream ID: draining non-incremental
			// responses in place serves them one at a time in stream order,
			// while incremental ones share the urgency round-robin
			// (RFC 9218 §10).
			for _, streamID := range c.flushBuckets[urgency] {
				stream := c.streams[streamID]
				if stream == nil || min(stream.priority.urgency, uint8(7)) != urgency {
					continue
				}
				progressed, err := c.flushStream(stream, !stream.priority.incremental)
				if err != nil {
					return err
				}
				if progressed {
					madeProgress = true
				}
			}
		}
		if !madeProgress {
			return nil
		}
	}
}

// flushStream writes what the stream may send now; drain keeps going until the
// stream or a window empties.
func (c *serverConn) flushStream(stream *serverStream, drain bool) (bool, error) {
	progressed := false
	for {
		if len(stream.pendingData) != 0 && !stream.isReset && !stream.localClosed &&
			c.peerConnectionWindow > 0 && stream.sendWindow > 0 {
			amount := min(
				len(stream.pendingData),
				c.peerMaxFrameSize,
				int(c.peerConnectionWindow),
				int(stream.sendWindow),
			)
			isLast := amount == len(stream.pendingData) && stream.responseEOF && !stream.responseHasTrailers
			if err := c.framer.WriteData(stream.id, isLast, stream.pendingData[:amount]); err != nil {
				return progressed, err
			}
			c.peerConnectionWindow -= int64(amount)
			stream.sendWindow -= int64(amount)
			stream.pendingData = stream.pendingData[amount:]
			if stream.pendingWrite != nil {
				stream.pendingWrite.written += amount
			}
			if len(stream.pendingData) == 0 {
				stream.pendingData = nil
			}
			progressed = true
			if len(stream.pendingData) == 0 && stream.pendingAck != nil {
				stream.pendingAck <- nil
				stream.pendingAck = nil
			}
			if len(stream.pendingData) == 0 && stream.pendingWrite != nil {
				stream.pendingWrite.complete(nil)
				stream.pendingWrite = nil
			}
			if len(stream.pendingData) == 0 {
				c.stopResponseWriteTimeout(stream)
			} else {
				c.armResponseWriteTimeout(stream)
			}
			if isLast {
				stream.localClosed = true
				return true, c.finishResponse(stream)
			}
			if drain {
				continue
			}
			return true, nil
		}
		if len(stream.pendingData) == 0 && stream.responseEOF && !stream.localClosed {
			if stream.responseHasTrailers {
				encoded, err := c.encodeResponseTrailers(stream)
				if err != nil {
					// Nothing reached the HPACK encoder, so this is the
					// stream's problem, not the connection's.
					if resetErr := c.resetStream(stream.id, xhttp2.ErrCodeInternal, err); resetErr != nil {
						return true, resetErr
					}
					return true, nil
				}
				if err := c.writeHeaderBlock(stream.id, true, encoded); err != nil {
					return true, err
				}
			} else {
				if err := c.framer.WriteData(stream.id, true, nil); err != nil {
					return true, err
				}
			}
			stream.localClosed = true
			return true, c.finishResponse(stream)
		}
		return progressed, nil
	}
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
	return encodeTrailerHeaders(
		c.encoder,
		&c.headerBuffer,
		&c.headerStrings,
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
		c.rememberClosedClientStream(stream.id)
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

func (c *serverConn) rememberClosedClientStream(streamID uint32) {
	if _, exists := c.closedClientStreams[streamID]; exists {
		return
	}
	c.trackClosedClientStream(streamID)
}

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
	result := make(chan error, 1)
	select {
	case c.commands <- serverCommand{
		kind:     serverCommandCancelStream,
		streamID: streamID,
		err:      cause,
		result:   result,
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
	if c.config.countError != nil {
		c.config.countError("stream_" + strings.ToLower(code.String()))
	}
	if err := c.framer.WriteRSTStream(streamID, code); err != nil {
		return err
	}
	if stream != nil {
		stream.isReset = true
		stream.remoteClosed = true
		stream.localClosed = true
		stream.cancel(cause)
		if stream.body != nil {
			stream.body.discardWithError(errStreamClosed)
		}
		c.restoreUnconsumedFlow(stream)
		if stream.pendingAck != nil {
			stream.pendingAck <- cause
			stream.pendingAck = nil
		}
		if stream.pendingWrite != nil {
			stream.pendingWrite.complete(cause)
			stream.pendingWrite = nil
		}
		c.stopResponseWriteTimeout(stream)
		stream.pendingData = nil
		stream.responseEOF = false
		if !stream.handlerStarted || stream.handlerDone {
			c.maybeFinalizeStream(stream)
		}
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
	if c.config.countError != nil {
		c.config.countError("connection_" + strings.ToLower(code.String()))
	}
	_ = c.framer.WriteGoAway(c.lastProcessedID, code, nil)
	_ = c.bufferedWriter.Flush()
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
	if parent.pushDepth >= c.config.maxPushDepth ||
		c.activePushes >= c.config.maxPromisedStreams ||
		c.activePushes >= c.peerMaxConcurrentStreams {
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
	stream.pushDepth = parent.pushDepth + 1
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

	block, err := encodeRequestHeaders(
		c.encoder,
		&c.headerBuffer,
		&c.headerStrings,
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

func responseContentLength(header *fasthttp.ResponseHeader) int64 {
	if len(header.Peek(fasthttp.HeaderContentLength)) == 0 {
		return -1
	}
	return int64(header.ContentLength())
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

func applyRequestTrailers(header *fasthttp.RequestHeader, fields []hpack.HeaderField) error {
	known := make(map[string]struct{}, len(header.PeekTrailerKeys())+len(fields))
	for _, key := range header.PeekTrailerKeys() {
		known[strings.ToLower(string(key))] = struct{}{}
	}
	for _, field := range fields {
		// HTTP/2 field names are lowercase. Track them once instead of
		// rescanning every previously registered trailer for every field.
		if _, exists := known[field.Name]; !exists {
			if err := header.AddTrailer(field.Name); err != nil {
				return err
			}
			known[field.Name] = struct{}{}
		}
		header.Add(field.Name, field.Value)
	}
	return nil
}

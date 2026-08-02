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
	err          error
	fieldStorage *incomingHeaderFieldStorage
	pooledData   bool
	pooledConfig bool
}

type incomingHeaderFieldStorage struct {
	fields []hpack.HeaderField
}

var incomingHeaderFieldsPool sync.Pool
var incomingDataPool sync.Pool
var incomingSettingsPool sync.Pool
var responsePumpBufferPool sync.Pool

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
	serverCommandCancelStream
)

type serverCommand struct {
	kind       serverCommandKind
	streamID   uint32
	requestCtx *fasthttp.RequestCtx
	statusCode int
	header     *fasthttp.ResponseHeader
	data       []byte
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
	ctx             context.Context
	cancel          context.CancelCauseFunc
	framer          *xhttp2.Framer
	headerDecoder   *headerCodec
	bufferedWriter  *bufio.Writer
	encoder         *hpack.Encoder
	headerBuffer    bytes.Buffer
	headerStrings   headerStringCache

	events   chan incomingFrame
	commands chan serverCommand
	workers  sync.WaitGroup

	streams            map[uint32]*serverStream
	lastClientStreamID uint32
	lastProcessedID    uint32
	isGoingAway        bool
	isDraining         bool
	receivedSettings   bool

	peerInitialStreamWindow  int64
	peerConnectionWindow     int64
	peerMaxFrameSize         int
	peerMaxHeaderListSize    uint64
	peerMaxConcurrentStreams uint32
	peerAllowsPush           bool

	receiveConnectionWindow  int64
	pendingConnectionUpdate  int64
	nextPushStreamID         uint32
	activePushes             uint32
	priorityUpdates          map[uint32]priority
	closedClientStreams      map[uint32]struct{}
	closedClientStreamOrder  []uint32
	closedClientStreamCursor int
	rapidResetWindowStart    time.Time
	rapidResetCount          uint32
}

func (h *serverHandler) ServeConn(ctx *fasthttp.ProtocolServerContext, c net.Conn) error {
	conn := newServerConn(ctx, c, h.config)
	return conn.serve()
}

func newServerConn(
	protocolContext *fasthttp.ProtocolServerContext,
	c net.Conn,
	config serverConfig,
) *serverConn {
	ctx, cancel := context.WithCancelCause(context.Background())
	maxCommands := int(config.maxConcurrentStreams)*2 + 32
	if maxCommands > config.maxQueuedControlFrames {
		maxCommands = config.maxQueuedControlFrames
	}
	if maxCommands < 32 {
		maxCommands = 32
	}
	conn := &serverConn{
		protocolContext:          protocolContext,
		server:                   protocolContext.Server(),
		config:                   config,
		conn:                     c,
		ctx:                      ctx,
		cancel:                   cancel,
		events:                   make(chan incomingFrame, 32),
		commands:                 make(chan serverCommand, maxCommands),
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
		closedClientStreams:      make(map[uint32]struct{}),
	}
	conn.encoder = hpack.NewEncoder(&conn.headerBuffer)
	conn.encoder.SetMaxDynamicTableSizeLimit(config.maxEncoderTableSize)
	return conn
}

func (c *serverConn) serve() (retErr error) {
	defer func() {
		c.cancel(retErr)
		_ = c.conn.Close()
		for _, stream := range c.streams {
			stream.cancel(retErr)
			if stream.body != nil {
				stream.body.closeWithError(retErr)
			}
			if stream.pendingAck != nil {
				stream.pendingAck <- retErr
				stream.pendingAck = nil
			}
		}
		c.workers.Wait()
		c.releaseAllStreams()
	}()

	if err := c.readClientPreface(); err != nil {
		_ = c.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, _ = io.CopyN(io.Discard, c.conn, 64<<10)
		return err
	}
	if err := validateTLSConnection(c.conn); err != nil {
		return err
	}
	writer := io.Writer(c.conn)
	if c.config.writeByteTimeout > 0 {
		writer = &deadlineWriter{conn: c.conn, timeout: c.config.writeByteTimeout}
	}
	writeBufferSize := c.server.WriteBufferSize
	if writeBufferSize <= 0 {
		writeBufferSize = defaultWriteBufferSize
	}
	c.bufferedWriter = bufio.NewWriterSize(writer, writeBufferSize)
	c.framer = xhttp2.NewFramer(c.bufferedWriter, c.conn)
	c.framer.SetReuseFrames()
	c.headerDecoder = newHeaderCodec(c.config.maxDecoderTableSize, c.config.maxHeaderListSize)
	c.framer.SetMaxReadFrameSize(c.config.maxReadFrameSize)
	if err := c.writeInitialSettings(); err != nil {
		return fmt.Errorf("http2: writing initial settings: %w", err)
	}
	if err := c.bufferedWriter.Flush(); err != nil {
		return fmt.Errorf("http2: flushing initial settings: %w", err)
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
	for {
		if c.isGoingAway && len(c.streams) == 0 {
			return nil
		}
		select {
		case event := <-c.events:
			if closeConnection, err := c.handleIncomingEvent(event); closeConnection {
				return err
			}
		case <-timerChannel(idleTimer):
			if len(c.streams) == 0 {
				if err := c.startGracefulShutdown(); err != nil {
					return err
				}
				if shutdownTimer == nil {
					shutdownTimer = time.NewTimer(c.config.pingTimeout)
					defer shutdownTimer.Stop()
				}
			} else {
				idleArmed = false
			}
		case <-timerChannel(shutdownTimer):
			if err := c.finishGracefulShutdown(); err != nil {
				return err
			}
		case command := <-c.commands:
			if err := c.processCommand(command); err != nil {
				return c.failConnection(xhttp2.ErrCodeInternal, err)
			}
		case <-serverDone:
			serverDone = nil
			if err := c.startGracefulShutdown(); err != nil {
				return err
			}
			if shutdownTimer == nil {
				shutdownTimer = time.NewTimer(c.config.pingTimeout)
				defer shutdownTimer.Stop()
			}
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
		for range 63 {
			select {
			case event := <-c.events:
				if closeConnection, err := c.handleIncomingEvent(event); closeConnection {
					return err
				}
			case command := <-c.commands:
				if err := c.processCommand(command); err != nil {
					return c.failConnection(xhttp2.ErrCodeInternal, err)
				}
			default:
				goto flush
			}
		}
	flush:
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

func (c *serverConn) handleIncomingEvent(event incomingFrame) (bool, error) {
	defer releaseIncomingFrame(&event)
	if event.err != nil && event.kind != incomingFrameStreamError {
		if errors.Is(event.err, io.EOF) && len(c.streams) == 0 {
			return true, nil
		}
		return true, c.failConnection(errorCode(event.err), event.err)
	}
	if err := c.processFrame(&event); err != nil {
		var protocolErr *serverError
		if errors.As(err, &protocolErr) {
			return true, c.failConnection(protocolErr.code, protocolErr.err)
		}
		return true, c.failConnection(xhttp2.ErrCodeInternal, err)
	}
	return false, nil
}

func (c *serverConn) readClientPreface() error {
	if c.server.ReadTimeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.server.ReadTimeout)); err != nil {
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
	if c.server.ReadTimeout > 0 {
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
		frame, err := c.framer.ReadFrame()
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
		if err == nil {
			waitingForPing = false
			if headers, ok := frame.(*xhttp2.HeadersFrame); ok {
				event = c.decodeIncomingHeaders(headers)
			} else {
				event = incomingFrameFromWire(frame)
			}
		}
		select {
		case c.events <- event:
		case <-c.ctx.Done():
			return
		}
		if err != nil || event.err != nil && event.kind != incomingFrameStreamError {
			return
		}
	}
}

func (c *serverConn) decodeIncomingHeaders(frame *xhttp2.HeadersFrame) incomingFrame {
	event := incomingFrame{
		kind:        incomingFrameHeaders,
		streamID:    frame.StreamID,
		endStream:   frame.StreamEnded(),
		hasPriority: frame.HasPriority(),
	}
	if event.hasPriority {
		event.dependency = frame.Priority.StreamDep
	}
	fieldStorage := acquireIncomingHeaderFields(8)
	decoded, truncated, invalid, err := c.headerDecoder.decode(
		c.framer,
		frame.StreamID,
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
			event.settings = acquireIncomingSettings(frame.NumSettings())
			event.pooledConfig = true
			for i := range frame.NumSettings() {
				event.settings[i] = frame.Setting(i)
			}
		}
	case *xhttp2.MetaHeadersFrame:
		event.kind = incomingFrameHeaders
		event.endStream = frame.StreamEnded()
		event.truncated = frame.Truncated
		event.hasPriority = frame.HeadersFrame.HasPriority()
		if event.hasPriority {
			event.dependency = frame.HeadersFrame.Priority.StreamDep
		}
		event.fieldStorage = copyIncomingHeaderFields(frame.Fields)
		event.fields = event.fieldStorage.fields
	case *xhttp2.DataFrame:
		event.kind = incomingFrameData
		event.endStream = frame.StreamEnded()
		event.flowLength = int(frame.Header().Length)
		event.data = copyIncomingData(frame.Data())
		event.pooledData = len(event.data) != 0
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
		event.dependency = frame.PriorityParam.StreamDep
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

func acquireIncomingSettings(length int) []xhttp2.Setting {
	if value := incomingSettingsPool.Get(); value != nil {
		settings := value.([]xhttp2.Setting) //nolint:forcetypeassert
		if cap(settings) >= length {
			return settings[:length]
		}
	}
	return make([]xhttp2.Setting, length)
}

func copyIncomingData(source []byte) []byte {
	if len(source) == 0 {
		return nil
	}
	var data []byte
	if value := incomingDataPool.Get(); value != nil {
		data = value.([]byte) //nolint:forcetypeassert
	}
	if cap(data) < len(source) {
		data = make([]byte, len(source))
	} else {
		data = data[:len(source)]
	}
	copy(data, source)
	return data
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
	if event.pooledData {
		releaseIncomingData(event.data)
	}
	if event.pooledConfig && cap(event.settings) <= 64 {
		incomingSettingsPool.Put(event.settings[:0])
	}
}

func releaseIncomingData(data []byte) {
	if cap(data) <= 1<<20 {
		incomingDataPool.Put(data[:0])
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
		return c.processSettings(*event)
	case incomingFrameHeaders:
		return c.processHeaders(*event)
	case incomingFrameData:
		return c.processData(event)
	case incomingFrameRST:
		return c.processRST(*event)
	case incomingFrameWindowUpdate:
		return c.processWindowUpdate(*event)
	case incomingFramePing:
		if event.ack && c.isDraining && event.pingData == shutdownPingData {
			return c.finishGracefulShutdown()
		}
		if !event.ack {
			return c.framer.WritePing(true, event.pingData)
		}
		return nil
	case incomingFrameGoAway:
		c.isGoingAway = true
		return nil
	case incomingFramePriorityUpdate:
		return c.processPriorityUpdate(*event)
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
		return c.processHeaderStreamError(*event)
	default:
		return nil
	}
}

func (c *serverConn) processHeaderStreamError(event incomingFrame) error {
	if stream := c.streams[event.streamID]; stream != nil {
		return c.resetStream(event.streamID, event.errCode, event.err)
	}
	if event.streamID == 0 || event.streamID&1 == 0 || event.streamID <= c.lastClientStreamID {
		return &serverError{
			code: xhttp2.ErrCodeProtocol,
			err:  errors.New("http2: malformed headers used an invalid client stream id"),
		}
	}
	c.lastClientStreamID = event.streamID
	c.rememberClosedClientStream(event.streamID)
	if c.config.countError != nil {
		c.config.countError("stream_" + strings.ToLower(event.errCode.String()))
	}
	return c.framer.WriteRSTStream(event.streamID, event.errCode)
}

func (c *serverConn) processSettings(event incomingFrame) error {
	if event.ack {
		return nil
	}
	for _, setting := range event.settings {
		if err := setting.Valid(); err != nil {
			return &serverError{code: errorCode(err), err: err}
		}
		switch setting.ID {
		case xhttp2.SettingHeaderTableSize:
			value := setting.Val
			if value > c.config.maxEncoderTableSize {
				value = c.config.maxEncoderTableSize
			}
			c.encoder.SetMaxDynamicTableSize(value)
		case xhttp2.SettingEnablePush:
			if setting.Val > 1 {
				return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid SETTINGS_ENABLE_PUSH")}
			}
			c.peerAllowsPush = setting.Val == 1
		case xhttp2.SettingMaxConcurrentStreams:
			c.peerMaxConcurrentStreams = setting.Val
		case xhttp2.SettingInitialWindowSize:
			delta := int64(setting.Val) - c.peerInitialStreamWindow
			for _, stream := range c.streams {
				stream.sendWindow += delta
				if stream.sendWindow > math.MaxInt32 {
					return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: stream send window overflow")}
				}
			}
			c.peerInitialStreamWindow = int64(setting.Val)
		case xhttp2.SettingMaxFrameSize:
			if setting.Val < defaultMaxFrameSize || setting.Val > 1<<24-1 {
				return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid SETTINGS_MAX_FRAME_SIZE")}
			}
			c.peerMaxFrameSize = int(setting.Val)
		case xhttp2.SettingMaxHeaderListSize:
			c.peerMaxHeaderListSize = uint64(setting.Val)
		case xhttp2.SettingEnableConnectProtocol, xhttp2.SettingNoRFC7540Priorities:
			if setting.Val > 1 {
				return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid boolean setting")}
			}
			// The values affect features initiated by the peer; frame validation
			// and request validation happen at their use sites.
		}
	}
	return c.framer.WriteSettingsAck()
}

func (c *serverConn) processHeaders(event incomingFrame) error {
	if event.hasPriority && event.streamID == event.dependency {
		return c.resetStream(event.streamID, xhttp2.ErrCodeProtocol, errors.New("http2: stream depends on itself"))
	}
	if event.truncated {
		return c.resetStream(event.streamID, xhttp2.ErrCodeEnhanceYourCalm, errInvalidRequestHeaders)
	}
	if existing := c.streams[event.streamID]; existing != nil {
		return c.processTrailers(existing, event)
	}
	if event.streamID != 0 && event.streamID <= c.lastClientStreamID {
		if _, wasClosed := c.closedClientStreams[event.streamID]; wasClosed {
			return &serverError{code: xhttp2.ErrCodeStreamClosed, err: errors.New("http2: headers on closed stream")}
		}
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: reused or skipped client stream id")}
	}
	if c.isGoingAway {
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeRefusedStream)
	}
	if event.streamID == 0 || event.streamID&1 == 0 || event.streamID <= c.lastClientStreamID {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: invalid client stream id")}
	}
	c.lastClientStreamID = event.streamID
	if uint32(len(c.streams)) >= c.config.maxConcurrentStreams {
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeRefusedStream)
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
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeProtocol)
	}
	if c.server.GetOnly && !requestCtx.Request.Header.IsGet() && !requestCtx.Request.Header.IsHead() {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(fasthttp.ErrGetOnly)
		releaseServerStream(stream)
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeRefusedStream)
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
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeCancel)
	}
	c.streams[event.streamID] = stream
	c.lastProcessedID = event.streamID

	if !event.endStream && (c.server.StreamRequestBody || isExtended) {
		stream.body = newRequestBody(func(consumed int) {
			select {
			case c.commands <- serverCommand{
				kind:     serverCommandBodyConsumed,
				streamID: stream.id,
				consumed: consumed,
			}:
			case <-stream.Done():
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

func (c *serverConn) processTrailers(stream *serverStream, event incomingFrame) error {
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
		if err := stream.request.Request.Header.AddTrailer(field.Name); err != nil {
			return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, err)
		}
		stream.request.Request.Header.Set(field.Name, field.Value)
	}
	return c.finishRequestBody(stream)
}

func (c *serverConn) processData(event *incomingFrame) error {
	stream := c.streams[event.streamID]
	if stream == nil && event.streamID > c.lastClientStreamID {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: data on idle stream")}
	}
	if stream == nil || stream.remoteClosed {
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeStreamClosed)
	}
	flowLength := int64(event.flowLength)
	payloadLength := len(event.data)
	c.receiveConnectionWindow -= flowLength
	stream.recvWindow -= flowLength
	if c.receiveConnectionWindow < 0 {
		return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: connection receive window exceeded")}
	}
	if stream.recvWindow < 0 {
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
		stream.unconsumedFlow += int64(payloadLength)
		var release func([]byte)
		if event.pooledData {
			release = releaseIncomingData
			event.pooledData = false
		}
		data := event.data
		event.data = nil
		if err := stream.body.writeOwned(data, release); err != nil {
			c.restoreConnectionWindow(flowLength)
			return c.resetStream(stream.id, xhttp2.ErrCodeCancel, err)
		}
		padding := flowLength - int64(payloadLength)
		if padding > 0 {
			if err := c.consumeRequestBytes(stream, padding); err != nil {
				return err
			}
		}
	} else {
		stream.request.Request.AppendBody(event.data)
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

func (c *serverConn) consumeRequestBytes(stream *serverStream, amount int64) error {
	if amount <= 0 {
		return nil
	}
	if amount > math.MaxInt32 {
		return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: window update overflow")}
	}
	c.pendingConnectionUpdate += amount
	connectionIncrement := int64(0)
	if c.pendingConnectionUpdate >= int64(c.config.connectionWindowSize)/2 {
		connectionIncrement = c.pendingConnectionUpdate
		c.pendingConnectionUpdate = 0
		c.receiveConnectionWindow += connectionIncrement
	}
	stream.pendingWindowUpdate += amount
	streamIncrement := int64(0)
	if stream.pendingWindowUpdate >= int64(c.config.streamWindowSize)/2 {
		streamIncrement = stream.pendingWindowUpdate
		stream.pendingWindowUpdate = 0
		stream.recvWindow += streamIncrement
	}
	if connectionIncrement != 0 {
		if err := c.framer.WriteWindowUpdate(0, uint32(connectionIncrement)); err != nil {
			return err
		}
	}
	if streamIncrement != 0 {
		return c.framer.WriteWindowUpdate(stream.id, uint32(streamIncrement))
	}
	return nil
}

func (c *serverConn) restoreConnectionWindow(amount int64) {
	if amount <= 0 || amount > math.MaxInt32 {
		return
	}
	c.receiveConnectionWindow += amount
	_ = c.framer.WriteWindowUpdate(0, uint32(amount))
}

func (c *serverConn) processRST(event incomingFrame) error {
	if event.streamID <= c.lastClientStreamID {
		now := time.Now()
		if c.rapidResetWindowStart.IsZero() || now.Sub(c.rapidResetWindowStart) >= time.Second {
			c.rapidResetWindowStart = now
			c.rapidResetCount = 0
		}
		c.rapidResetCount++
		if c.rapidResetCount > c.config.maxRapidResetsPerSecond {
			return &serverError{code: xhttp2.ErrCodeEnhanceYourCalm, err: errors.New("http2: rapid reset limit exceeded")}
		}
	}
	stream := c.streams[event.streamID]
	if stream == nil {
		if event.streamID > c.lastClientStreamID {
			return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: reset on idle stream")}
		}
		return nil
	}
	stream.isReset = true
	stream.remoteClosed = true
	stream.localClosed = true
	stream.cancel(fmt.Errorf("http2: peer reset stream: %s", event.errCode))
	if stream.body != nil {
		stream.body.closeWithError(errStreamClosed)
	}
	c.restoreUnconsumedFlow(stream)
	if stream.pendingAck != nil {
		stream.pendingAck <- errStreamClosed
		stream.pendingAck = nil
	}
	if !stream.handlerStarted || stream.handlerDone {
		c.releaseStream(stream)
	}
	return nil
}

func (c *serverConn) processWindowUpdate(event incomingFrame) error {
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
		if event.streamID > c.lastClientStreamID {
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

func (c *serverConn) processPriorityUpdate(event incomingFrame) error {
	if event.streamID == 0 {
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: priority update targets stream zero")}
	}
	updated, err := parsePriority(event.priority)
	if err != nil {
		return nil
	}
	if stream := c.streams[event.streamID]; stream != nil {
		stream.priority = updated
		return nil
	}
	if event.streamID > c.lastClientStreamID {
		if len(c.priorityUpdates) >= int(c.config.maxConcurrentStreams)*4 {
			return &serverError{code: xhttp2.ErrCodeEnhanceYourCalm, err: errors.New("http2: too many pending priority updates")}
		}
		c.priorityUpdates[event.streamID] = updated
	}
	return nil
}

func (c *serverConn) processCommand(command serverCommand) error {
	stream := c.streams[command.streamID]
	if stream == nil {
		if command.result != nil {
			command.result <- errStreamClosed
		}
		return nil
	}
	switch command.kind {
	case serverCommandHandlerDone:
		return c.handleHandlerDone(stream, command.requestCtx)
	case serverCommandInformational:
		block, err := encodeInformationalHeaders(
			c.encoder,
			&c.headerBuffer,
			&c.headerStrings,
			command.statusCode,
			command.header,
		)
		if err == nil {
			err = c.writeHeaderBlock(stream.id, false, block)
		}
		command.result <- err
		return err
	case serverCommandBodyConsumed:
		if stream.discardRequestBody {
			return nil
		}
		stream.unconsumedFlow -= int64(command.consumed)
		if stream.unconsumedFlow < 0 {
			return errors.New("http2: request body flow accounting underflow")
		}
		return c.consumeRequestBytes(stream, int64(command.consumed))
	case serverCommandResponseData:
		if len(stream.pendingData) != 0 || stream.pendingAck != nil || stream.responseEOF {
			command.result <- errors.New("http2: response stream has pending data")
			return nil
		}
		if stream.expectedResponse >= 0 && stream.responseBytes+int64(len(command.data)) > stream.expectedResponse {
			err := errors.New("http2: response body exceeds content-length")
			command.result <- err
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, err)
		}
		stream.pendingData = command.data
		stream.pendingAck = command.result
		stream.responseBytes += int64(len(command.data))
		return nil
	case serverCommandResponseEOF:
		if command.err != nil && !errors.Is(command.err, io.EOF) {
			if command.result != nil {
				command.result <- command.err
			}
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, command.err)
		}
		if stream.expectedResponse >= 0 && stream.responseBytes != stream.expectedResponse {
			err := errors.New("http2: response body length doesn't match content-length")
			if command.result != nil {
				command.result <- err
			}
			return c.resetStream(stream.id, xhttp2.ErrCodeInternal, err)
		}
		stream.responseEOF = true
		if command.result != nil {
			if stream.pendingData == nil {
				command.result <- nil
			} else {
				stream.pendingAck = command.result
			}
		}
		return nil
	case serverCommandResponsePumpDone:
		stream.responsePumpDone = true
		if stream.releasePending {
			c.releaseStream(stream)
		}
		return nil
	case serverCommandStreamHandlerDone:
		stream.handlerDone = true
		if !stream.localClosed {
			stream.responseEOF = true
		}
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
	case serverCommandCancelStream:
		if command.result != nil {
			command.result <- command.err
		}
		return c.resetStream(stream.id, xhttp2.ErrCodeCancel, command.err)
	default:
		return nil
	}
}

func (c *serverConn) startHandler(stream *serverStream) {
	if stream.handlerStarted {
		return
	}
	stream.handlerStarted = true
	c.workers.Add(1)
	go c.runHandler(stream)
}

func (c *serverConn) runHandler(stream *serverStream) {
	defer c.workers.Done()
	c.server.Handler(stream.request)
	select {
	case c.commands <- serverCommand{
		kind:       serverCommandHandlerDone,
		streamID:   stream.id,
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
	if stream.isReset {
		c.releaseStream(stream)
		return nil
	}
	statusCode := requestCtx.Response.StatusCode()
	if statusCode == 0 {
		statusCode = fasthttp.StatusOK
	}
	if statusCode >= 100 && statusCode < 200 {
		return c.resetStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: final response cannot be informational"))
	}
	if statusCode == fasthttp.StatusNoContent &&
		(requestCtx.Response.IsBodyStream() || len(requestCtx.Response.Body()) != 0 ||
			len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) != 0) {
		return c.resetStream(stream.id, xhttp2.ErrCodeInternal, errors.New("http2: 204 response cannot contain a body or content-length"))
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
			len(requestCtx.Response.Header.PeekTrailerKeys()) != 0 {
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
		if len(requestCtx.Response.Header.Peek(fasthttp.HeaderContentLength)) == 0 &&
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
		c.finishResponse(stream)
		return nil
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
		c.finishResponse(stream)
		return nil
	}
	if len(body) != 0 {
		stream.pendingData = body
	}
	stream.responseEOF = true
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
		defer responsePumpBufferPool.Put(buffer[:0])
		for {
			n, readErr := reader.Read(buffer)
			if n > 0 {
				result := make(chan error, 1)
				command := serverCommand{
					kind:     serverCommandResponseData,
					streamID: stream.id,
					data:     buffer[:n],
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

func acquireResponsePumpBuffer() []byte {
	if value := responsePumpBufferPool.Get(); value != nil {
		buffer := value.([]byte) //nolint:forcetypeassert
		if cap(buffer) >= defaultMaxFrameSize {
			return buffer[:defaultMaxFrameSize]
		}
	}
	return make([]byte, defaultMaxFrameSize)
}

func (c *serverConn) startStreamHandler(
	stream *serverStream,
	handler fasthttp.StreamHandler,
) {
	stream.handlerDone = false
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		var reader io.Reader = bytes.NewReader(nil)
		if stream.body != nil {
			reader = stream.body
		}
		streamConn := &streamConn{stream: stream, read: reader}
		handler(streamConn)
		_ = streamConn.Close()
		select {
		case c.commands <- serverCommand{kind: serverCommandStreamHandlerDone, streamID: stream.id}:
		case <-stream.Done():
		}
	}()
}

func (c *serverConn) flushResponses() error {
	for {
		madeProgress := false
		for urgency := uint8(0); urgency <= 7; urgency++ {
			for _, stream := range c.streams {
				if stream.priority.urgency != urgency {
					continue
				}
				if len(stream.pendingData) != 0 && c.peerConnectionWindow > 0 && stream.sendWindow > 0 {
					amount := min(
						len(stream.pendingData),
						c.peerMaxFrameSize,
						int(c.peerConnectionWindow),
						int(stream.sendWindow),
					)
					isLast := amount == len(stream.pendingData) && stream.responseEOF && !stream.responseHasTrailers
					if err := c.framer.WriteData(stream.id, isLast, stream.pendingData[:amount]); err != nil {
						return err
					}
					c.peerConnectionWindow -= int64(amount)
					stream.sendWindow -= int64(amount)
					stream.pendingData = stream.pendingData[amount:]
					if len(stream.pendingData) == 0 {
						stream.pendingData = nil
					}
					madeProgress = true
					if len(stream.pendingData) == 0 && stream.pendingAck != nil {
						stream.pendingAck <- nil
						stream.pendingAck = nil
					}
					if isLast {
						stream.localClosed = true
						c.finishResponse(stream)
					}
					continue
				}
				if len(stream.pendingData) == 0 && stream.responseEOF && !stream.localClosed {
					if stream.responseHasTrailers {
						if err := c.writeResponseTrailers(stream); err != nil {
							return err
						}
					} else {
						if err := c.framer.WriteData(stream.id, true, nil); err != nil {
							return err
						}
					}
					stream.localClosed = true
					madeProgress = true
					c.finishResponse(stream)
				}
			}
		}
		if !madeProgress {
			return nil
		}
	}
}

func (c *serverConn) writeResponseTrailers(stream *serverStream) error {
	block, err := encodeTrailerHeaders(
		c.encoder,
		&c.headerBuffer,
		&c.headerStrings,
		&stream.request.Response.Header,
	)
	if err != nil {
		return err
	}
	return c.writeHeaderBlock(stream.id, true, block)
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
	block = block[firstLength:]
	for len(block) != 0 {
		fragmentLength := min(len(block), c.peerMaxFrameSize)
		if err := c.framer.WriteContinuation(
			streamID,
			fragmentLength == len(block),
			block[:fragmentLength],
		); err != nil {
			return err
		}
		block = block[fragmentLength:]
	}
	return nil
}

func (c *serverConn) finishResponse(stream *serverStream) {
	stream.responseEOF = false
	if stream.pendingAck != nil {
		stream.pendingAck <- nil
		stream.pendingAck = nil
	}
	if stream.streamHandler != nil && !stream.handlerDone {
		return
	}
	if !stream.remoteClosed {
		_ = c.framer.WriteRSTStream(stream.id, xhttp2.ErrCodeNo)
		stream.remoteClosed = true
		c.restoreUnconsumedFlow(stream)
		if stream.body != nil {
			stream.body.closeWithError(errStreamClosed)
		}
	}
	c.releaseStream(stream)
}

func (c *serverConn) restoreUnconsumedFlow(stream *serverStream) {
	if stream.unconsumedFlow <= 0 {
		return
	}
	c.restoreConnectionWindow(stream.unconsumedFlow)
	stream.unconsumedFlow = 0
}

func (c *serverConn) releaseStream(stream *serverStream) {
	if c.streams[stream.id] != stream {
		return
	}
	if stream.responsePumpStarted && !stream.responsePumpDone {
		stream.releasePending = true
		return
	}
	delete(c.streams, stream.id)
	if stream.id&1 == 1 {
		c.rememberClosedClientStream(stream.id)
	}
	if stream.isPush && c.activePushes > 0 {
		c.activePushes--
	}
	stream.cancel(errStreamClosed)
	if stream.body != nil {
		stream.body.discardWithError(errStreamClosed)
	}
	if stream.request != nil {
		c.protocolContext.ReleaseRequestCtx(stream.request)
		stream.request = nil
	}
	releaseServerStream(stream)
}

func (c *serverConn) rememberClosedClientStream(streamID uint32) {
	if _, exists := c.closedClientStreams[streamID]; exists {
		return
	}
	c.closedClientStreams[streamID] = struct{}{}
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
		stream.cancel(errStreamClosed)
		if stream.body != nil {
			stream.body.discardWithError(errStreamClosed)
		}
		if stream.request != nil {
			c.protocolContext.ReleaseRequestCtx(stream.request)
			stream.request = nil
		}
		releaseServerStream(stream)
	}
	c.streams = make(map[uint32]*serverStream)
}

func (c *serverConn) resetStream(streamID uint32, code xhttp2.ErrCode, cause error) error {
	if c.config.countError != nil {
		c.config.countError("stream_" + strings.ToLower(code.String()))
	}
	if err := c.framer.WriteRSTStream(streamID, code); err != nil {
		return err
	}
	stream := c.streams[streamID]
	if stream != nil {
		stream.isReset = true
		stream.cancel(cause)
		if !stream.handlerStarted || stream.handlerDone {
			c.releaseStream(stream)
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

var shutdownPingData = [8]byte{'f', 'a', 's', 't', 'h', '2', 'g', 'o'}

func (c *serverConn) failConnection(code xhttp2.ErrCode, cause error) error {
	if c.config.countError != nil {
		c.config.countError("connection_" + strings.ToLower(code.String()))
	}
	_ = c.framer.WriteGoAway(c.lastProcessedID, code, nil)
	_ = c.bufferedWriter.Flush()
	if errors.Is(cause, xhttp2.ErrFrameTooLarge) {
		_ = c.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, _ = io.CopyN(io.Discard, c.conn, 1<<24)
	}
	return cause
}

func (c *serverConn) handlePush(
	parent *serverStream,
	target string,
	opts *fasthttp.PushOptions,
) error {
	if !c.config.enablePush || !c.peerAllowsPush {
		return fasthttp.ErrPushDisabled
	}
	if parent == nil || parent.isReset || parent.localClosed || parent.responseHeaderSent || parent.handlerDone {
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
	block = block[firstLength:]
	for len(block) != 0 {
		fragmentLength := min(len(block), c.peerMaxFrameSize)
		if err := c.framer.WriteContinuation(
			parentID,
			fragmentLength == len(block),
			block[:fragmentLength],
		); err != nil {
			return err
		}
		block = block[fragmentLength:]
	}
	return nil
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
	for _, member := range strings.Split(value, ",") {
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

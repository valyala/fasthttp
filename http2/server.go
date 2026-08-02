package http2

import (
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
	incomingFrameInvalidPushPromise
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
	err          error
}

type serverCommandKind uint8

const (
	serverCommandUnknown serverCommandKind = iota
	serverCommandHandlerDone
	serverCommandInformational
	serverCommandBodyConsumed
	serverCommandResponseData
	serverCommandResponseEOF
	serverCommandStreamHandlerDone
	serverCommandPush
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
	encoder         *hpack.Encoder
	headerBuffer    bytes.Buffer

	events   chan incomingFrame
	commands chan serverCommand
	workers  sync.WaitGroup

	streams            map[uint32]*serverStream
	lastClientStreamID uint32
	lastProcessedID    uint32
	isGoingAway        bool
	receivedSettings   bool

	peerInitialStreamWindow  int64
	peerConnectionWindow     int64
	peerMaxFrameSize         int
	peerMaxHeaderListSize    uint64
	peerMaxConcurrentStreams uint32
	peerAllowsPush           bool

	receiveConnectionWindow int64
	nextPushStreamID        uint32
	activePushes            uint32
	priorityUpdates         map[uint32]priority
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
		return err
	}
	decoder := hpack.NewDecoder(c.config.maxDecoderTableSize, nil)
	decoder.SetAllowedMaxDynamicTableSize(c.config.maxDecoderTableSize)
	decoder.SetMaxStringLength(int(c.config.maxHeaderListSize))
	c.framer = xhttp2.NewFramer(c.conn, c.conn)
	c.framer.ReadMetaHeaders = decoder
	c.framer.MaxHeaderListSize = c.config.maxHeaderListSize
	c.framer.SetMaxReadFrameSize(c.config.maxReadFrameSize)
	if err := c.writeInitialSettings(); err != nil {
		return fmt.Errorf("http2: writing initial settings: %w", err)
	}

	go c.readLoop()
	serverDone := c.protocolContext.Done()
	for {
		if c.isGoingAway && len(c.streams) == 0 {
			return nil
		}
		select {
		case event := <-c.events:
			if event.err != nil {
				if errors.Is(event.err, io.EOF) && len(c.streams) == 0 {
					return nil
				}
				return c.failConnection(errorCode(event.err), event.err)
			}
			if err := c.processFrame(event); err != nil {
				var protocolErr *serverError
				if errors.As(err, &protocolErr) {
					return c.failConnection(protocolErr.code, protocolErr.err)
				}
				return c.failConnection(xhttp2.ErrCodeInternal, err)
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
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
		if err := c.flushResponses(); err != nil {
			return err
		}
	}
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
	for {
		frame, err := c.framer.ReadFrame()
		event := incomingFrame{err: err}
		if err == nil {
			event = copyIncomingFrame(frame)
		}
		select {
		case c.events <- event:
		case <-c.ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func copyIncomingFrame(frame xhttp2.Frame) incomingFrame {
	header := frame.Header()
	event := incomingFrame{streamID: header.StreamID}
	switch frame := frame.(type) {
	case *xhttp2.SettingsFrame:
		event.kind = incomingFrameSettings
		event.ack = frame.IsAck()
		if !event.ack {
			event.settings = make([]xhttp2.Setting, frame.NumSettings())
			for i := range frame.NumSettings() {
				event.settings[i] = frame.Setting(i)
			}
		}
	case *xhttp2.MetaHeadersFrame:
		event.kind = incomingFrameHeaders
		event.endStream = frame.StreamEnded()
		event.truncated = frame.Truncated
		event.fields = make([]hpack.HeaderField, len(frame.Fields))
		for i, field := range frame.Fields {
			event.fields[i] = hpack.HeaderField{
				Name:      strings.Clone(field.Name),
				Value:     strings.Clone(field.Value),
				Sensitive: field.Sensitive,
			}
		}
	case *xhttp2.DataFrame:
		event.kind = incomingFrameData
		event.endStream = frame.StreamEnded()
		event.flowLength = int(frame.Header().Length)
		event.data = bytes.Clone(frame.Data())
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
	case *xhttp2.PushPromiseFrame:
		event.kind = incomingFrameInvalidPushPromise
	default:
		event.kind = incomingFrameUnknown
	}
	return event
}

func (c *serverConn) processFrame(event incomingFrame) error {
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
		if !event.ack {
			return c.framer.WritePing(true, event.pingData)
		}
		return nil
	case incomingFrameGoAway:
		c.isGoingAway = true
		return nil
	case incomingFramePriorityUpdate:
		return c.processPriorityUpdate(event)
	case incomingFrameInvalidPushPromise:
		return &serverError{code: xhttp2.ErrCodeProtocol, err: errors.New("http2: client sent push promise")}
	default:
		return nil
	}
}

func (c *serverConn) processSettings(event incomingFrame) error {
	if event.ack {
		return nil
	}
	for _, setting := range event.settings {
		switch setting.ID {
		case xhttp2.SettingHeaderTableSize:
			value := setting.Val
			if value > c.config.maxEncoderTableSize {
				value = c.config.maxEncoderTableSize
			}
			c.encoder.SetMaxDynamicTableSize(value)
		case xhttp2.SettingEnablePush:
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
			c.peerMaxFrameSize = int(setting.Val)
		case xhttp2.SettingMaxHeaderListSize:
			c.peerMaxHeaderListSize = uint64(setting.Val)
		case xhttp2.SettingEnableConnectProtocol, xhttp2.SettingNoRFC7540Priorities:
			// The values affect features initiated by the peer; frame validation
			// and request validation happen at their use sites.
		}
	}
	return c.framer.WriteSettingsAck()
}

func (c *serverConn) processHeaders(event incomingFrame) error {
	if event.truncated {
		return c.resetStream(event.streamID, xhttp2.ErrCodeEnhanceYourCalm, errInvalidRequestHeaders)
	}
	if existing := c.streams[event.streamID]; existing != nil {
		return c.processTrailers(existing, event)
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
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeProtocol)
	}
	if c.server.GetOnly && !requestCtx.Request.Header.IsGet() && !requestCtx.Request.Header.IsHead() {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(fasthttp.ErrGetOnly)
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
	if value := requestCtx.Request.Header.Peek("Priority"); len(value) != 0 {
		parsed, parseErr := parsePriority(string(value))
		if parseErr != nil {
			c.protocolContext.ReleaseRequestCtx(requestCtx)
			stream.request = nil
			stream.cancel(parseErr)
			return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeProtocol)
		}
		stream.priority = parsed
	}
	if updated, ok := c.priorityUpdates[event.streamID]; ok {
		stream.priority = updated
		delete(c.priorityUpdates, event.streamID)
	}
	if expectedBody >= 0 && expectedBody > int64(stream.maxBody) {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(errRequestBodyTooLarge)
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeCancel)
	}
	c.streams[event.streamID] = stream
	c.lastProcessedID = event.streamID

	isExtended := len(requestCtx.Request.Header.ConnectProtocol()) != 0
	if !event.endStream && (c.server.StreamRequestBody || isExtended) {
		stream.body = newRequestBody(func(consumed int) {
			select {
			case c.commands <- serverCommand{
				kind:     serverCommandBodyConsumed,
				streamID: stream.id,
				consumed: consumed,
			}:
			case <-stream.ctx.Done():
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
	if stream.remoteClosed || !event.endStream {
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

func (c *serverConn) processData(event incomingFrame) error {
	stream := c.streams[event.streamID]
	if stream == nil || stream.remoteClosed {
		return c.framer.WriteRSTStream(event.streamID, xhttp2.ErrCodeStreamClosed)
	}
	flowLength := int64(event.flowLength)
	c.receiveConnectionWindow -= flowLength
	stream.recvWindow -= flowLength
	if c.receiveConnectionWindow < 0 {
		return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: connection receive window exceeded")}
	}
	if stream.recvWindow < 0 {
		return c.resetStream(stream.id, xhttp2.ErrCodeFlowControl, errors.New("http2: stream receive window exceeded"))
	}

	stream.bodyBytes += int64(len(event.data))
	if stream.expectedBody >= 0 && stream.bodyBytes > stream.expectedBody {
		c.restoreConnectionWindow(flowLength)
		return c.resetStream(stream.id, xhttp2.ErrCodeProtocol, errors.New("http2: request body exceeds content-length"))
	}
	if stream.maxBody > 0 && stream.bodyBytes > int64(stream.maxBody) {
		c.restoreConnectionWindow(flowLength)
		return c.resetStream(stream.id, xhttp2.ErrCodeCancel, errRequestBodyTooLarge)
	}
	if stream.body != nil {
		stream.unconsumedFlow += int64(len(event.data))
		if err := stream.body.write(event.data); err != nil {
			c.restoreConnectionWindow(flowLength)
			return c.resetStream(stream.id, xhttp2.ErrCodeCancel, err)
		}
		padding := flowLength - int64(len(event.data))
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
	c.receiveConnectionWindow += amount
	stream.recvWindow += amount
	if amount > math.MaxInt32 {
		return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: window update overflow")}
	}
	increment := uint32(amount)
	if err := c.framer.WriteWindowUpdate(0, increment); err != nil {
		return err
	}
	return c.framer.WriteWindowUpdate(stream.id, increment)
}

func (c *serverConn) restoreConnectionWindow(amount int64) {
	if amount <= 0 || amount > math.MaxInt32 {
		return
	}
	c.receiveConnectionWindow += amount
	_ = c.framer.WriteWindowUpdate(0, uint32(amount))
}

func (c *serverConn) processRST(event incomingFrame) error {
	stream := c.streams[event.streamID]
	if stream == nil {
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
			return &serverError{code: xhttp2.ErrCodeFlowControl, err: errors.New("http2: connection send window overflow")}
		}
		c.peerConnectionWindow += int64(event.increment)
		return nil
	}
	stream := c.streams[event.streamID]
	if stream == nil {
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
		return err
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
		block, err := encodeInformationalHeaders(c.encoder, &c.headerBuffer, command.statusCode, command.header)
		if err == nil {
			err = c.writeHeaderBlock(stream.id, false, block)
		}
		command.result <- err
		return err
	case serverCommandBodyConsumed:
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
	go func() {
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
	}()
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

	stream.acceptMu.Lock()
	streamHandler := stream.streamHandler
	stream.acceptMu.Unlock()
	if streamHandler != nil {
		block, err := encodeResponseHeaders(
			c.encoder,
			&c.headerBuffer,
			c.server,
			&requestCtx.Response,
			c.peerMaxHeaderListSize,
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

	block, err := encodeResponseHeaders(
		c.encoder,
		&c.headerBuffer,
		c.server,
		&requestCtx.Response,
		c.peerMaxHeaderListSize,
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

	body := requestCtx.Response.Body()
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
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		reader := stream.request.Response.BodyStream()
		buffer := make([]byte, defaultMaxFrameSize)
		for {
			n, readErr := reader.Read(buffer)
			if n > 0 {
				result := make(chan error, 1)
				command := serverCommand{
					kind:     serverCommandResponseData,
					streamID: stream.id,
					data:     bytes.Clone(buffer[:n]),
					result:   result,
				}
				select {
				case c.commands <- command:
				case <-stream.ctx.Done():
					_ = stream.request.Response.CloseBodyStream()
					return
				}
				select {
				case err := <-result:
					if err != nil {
						_ = stream.request.Response.CloseBodyStream()
						return
					}
				case <-stream.ctx.Done():
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
				case <-stream.ctx.Done():
					return
				}
				select {
				case <-result:
				case <-stream.ctx.Done():
				}
				return
			}
		}
	}()
}

func (c *serverConn) startStreamHandler(
	stream *serverStream,
	handler fasthttp.StreamHandler,
) {
	stream.handlerDone = false
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		streamConn := &streamConn{stream: stream, read: stream.body}
		handler(streamConn)
		_ = streamConn.Close()
		select {
		case c.commands <- serverCommand{kind: serverCommandStreamHandlerDone, streamID: stream.id}:
		case <-stream.ctx.Done():
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
	block, err := encodeTrailerHeaders(c.encoder, &c.headerBuffer, &stream.request.Response.Header)
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
	if stream.streamHandler != nil && !stream.remoteClosed {
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
	delete(c.streams, stream.id)
	if stream.isPush && c.activePushes > 0 {
		c.activePushes--
	}
	stream.cancel(errStreamClosed)
	if stream.body != nil {
		stream.body.closeWithError(errStreamClosed)
	}
	if stream.request != nil {
		c.protocolContext.ReleaseRequestCtx(stream.request)
		stream.request = nil
	}
}

func (c *serverConn) releaseAllStreams() {
	for _, stream := range c.streams {
		if stream.request != nil {
			c.protocolContext.ReleaseRequestCtx(stream.request)
			stream.request = nil
		}
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
	_ = c.framer.WriteGoAway(c.lastProcessedID, code, []byte(cause.Error()))
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
		&requestCtx.Request,
		c.peerMaxHeaderListSize,
		false,
	)
	if err != nil {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(err)
		return err
	}
	if err := c.writePushPromise(parent.id, streamID, block); err != nil {
		c.protocolContext.ReleaseRequestCtx(requestCtx)
		stream.request = nil
		stream.cancel(err)
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
	for _, member := range strings.Split(value, ",") {
		member = strings.TrimSpace(member)
		switch {
		case member == "i" || member == "i=?1":
			result.incremental = true
		case strings.HasPrefix(member, "u="):
			urgency, err := strconv.Atoi(strings.TrimPrefix(member, "u="))
			if err != nil || urgency < 0 || urgency > 7 {
				return priority{}, errors.New("http2: invalid priority urgency")
			}
			result.urgency = uint8(urgency)
		}
	}
	return result, nil
}

func errorCode(err error) xhttp2.ErrCode {
	var connectionError xhttp2.ConnectionError
	if errors.As(err, &connectionError) {
		return xhttp2.ErrCode(connectionError)
	}
	return xhttp2.ErrCodeProtocol
}

package http2

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	errDataOnInvalidStream          = errors.New("http2: data on an invalid response stream")
	errResponseStreamWindowExceeded = errors.New("http2: response stream flow-control window exceeded")
	errResponseBodyNotPermitted     = errors.New("http2: response body isn't permitted")
	errResponseBodyLengthMismatch   = errors.New("http2: response body exceeds content-length")
	errResponseBodyTooShort         = fmt.Errorf("http2: response body shorter than content-length: %w", io.ErrUnexpectedEOF)
)

type decodedClientHeaders struct {
	streamID  uint32
	endStream bool
	truncated bool
	fields    []hpack.HeaderField
}

// healthCheckPing is the payload of the ReadIdleTimeout PING.
var healthCheckPing = [8]byte{'f', 'a', 's', 't', 'h', '2', 0, 2}

func (c *clientConn) readLoop() {
	go c.sendControl()
	healthCheck := c.config.readIdleTimeout > 0
	if healthCheck {
		c.mu.Lock()
		if !c.closed {
			c.healthTimer = time.AfterFunc(c.config.readIdleTimeout, c.checkHealth)
		}
		c.mu.Unlock()
	}
	awaitingFirstFrame := true
	for {
		frame, err := c.frames.readFrame()
		if healthCheck {
			c.lastFrameAt.Store(int64(time.Since(c.created)))
		}
		if err != nil {
			if streamErr, ok := errors.AsType[xhttp2.StreamError](err); ok && !awaitingFirstFrame {
				// The framer read the whole frame, so only its stream fails.
				if err := c.processFramerStreamError(streamErr); err != nil {
					c.fail(err)
					return
				}
				continue
			}
			// A failed writer closes the socket; its cause is the connection's.
			if writerErr := c.writer.err(); writerErr != nil {
				err = writerErr
			}
			c.fail(err)
			return
		}
		if awaitingFirstFrame {
			settings, ok := frame.(*xhttp2.SettingsFrame)
			if !ok || settings.IsAck() {
				c.fail(peerError(xhttp2.ErrCodeProtocol, "http2: server's first frame isn't settings"))
				return
			}
			awaitingFirstFrame = false
		}
		if headers, ok := frame.(*headersFrame); ok {
			if err := c.processDecodedResponseHeaders(headers); err != nil {
				c.fail(err)
				return
			}
			continue
		}
		if err := c.processFrame(frame.(xhttp2.Frame)); err != nil { //nolint:forcetypeassert
			c.fail(err)
			return
		}
	}
}

// checkHealth runs on a timer, not a read deadline, so it never cuts a frame
// short. An idle connection is PINGed and fails if no frame follows in time.
func (c *clientConn) checkHealth() {
	now := time.Since(c.created)
	lastFrame := time.Duration(c.lastFrameAt.Load())
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if c.pingOutstanding {
		waited := now - c.pingSentAt
		switch {
		case lastFrame >= c.pingSentAt:
			c.pingOutstanding = false
		case waited < c.config.pingTimeout:
			c.healthTimer.Reset(c.config.pingTimeout - waited)
			c.mu.Unlock()
			return
		default:
			c.mu.Unlock()
			c.fail(errPingTimeout)
			return
		}
	}
	if idle := now - lastFrame; idle < c.config.readIdleTimeout {
		c.healthTimer.Reset(c.config.readIdleTimeout - idle)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	// Timed from the queuing, after the wait for the write slot, and recorded
	// before the PING can be answered.
	err := c.writeControl(func() error {
		c.mu.Lock()
		c.pingOutstanding = true
		c.pingSentAt = time.Since(c.created)
		c.mu.Unlock()
		return c.framer.WritePing(false, healthCheckPing)
	})
	if err != nil {
		return
	}
	c.mu.Lock()
	if !c.closed {
		c.healthTimer.Reset(c.config.pingTimeout)
	}
	c.mu.Unlock()
}

// processFramerStreamError handles a frame the framer rejected as a stream
// error (RFC 9113 §5.4.2).
func (c *clientConn) processFramerStreamError(streamErr xhttp2.StreamError) error {
	c.mu.Lock()
	stream, idle := c.streamStateLocked(streamErr.StreamID)
	if stream == nil {
		c.mu.Unlock()
		if idle {
			return &peerConnectionError{code: xhttp2.ErrCodeProtocol, err: streamErr}
		}
		return nil
	}
	c.cancelStreamAndUnlock(stream, streamErr.Code, streamErr, failurePeer)
	return nil
}

func (c *clientConn) processFrame(frame xhttp2.Frame) error {
	switch frame := frame.(type) {
	case *xhttp2.SettingsFrame:
		return c.processSettings(frame)
	case *xhttp2.DataFrame:
		return c.processResponseData(frame)
	case *xhttp2.RSTStreamFrame:
		c.mu.Lock()
		stream, idle := c.streamStateLocked(frame.StreamID)
		if stream == nil {
			c.mu.Unlock()
			if idle {
				return peerError(xhttp2.ErrCodeProtocol, "http2: reset on idle stream")
			}
			return nil
		}
		// StreamError unwraps REFUSED_STREAM to ErrRefusedStream: the peer
		// didn't process the request (RFC 9113 §8.7).
		c.failStreamLocked(stream, &StreamError{
			StreamID: frame.StreamID,
			ErrCode:  uint32(frame.ErrCode),
		}, failurePeer)
		c.mu.Unlock()
		return nil
	case *xhttp2.WindowUpdateFrame:
		return c.processWindowUpdate(frame)
	case *xhttp2.PingFrame:
		if !frame.IsAck() {
			c.queueControl(controlFrame{kind: controlPingAck, ping: frame.Data})
		}
		return nil
	case *xhttp2.GoAwayFrame:
		return c.processGoAway(frame)
	case *xhttp2.PushPromiseFrame:
		return c.processPushPromise(frame)
	case *xhttp2.PriorityFrame:
		if frame.StreamID == frame.StreamDep {
			c.resetStream(frame.StreamID, xhttp2.ErrCodeProtocol, errors.New("http2: stream depends on itself"), failurePeer)
		}
		return nil
	case *xhttp2.PriorityUpdateFrame:
		return xhttp2.ConnectionError(xhttp2.ErrCodeProtocol)
	default:
		return nil
	}
}

func (c *clientConn) processDecodedResponseHeaders(frame *headersFrame) error {
	streamID := frame.streamID
	endStream := frame.StreamEnded()
	selfDependent := frame.hasPriority && frame.streamDep == streamID
	fieldStorage := acquireIncomingHeaderFields(8)
	decoded, truncated, invalid, err := c.headerDecoder.decode(
		c.framer,
		streamID,
		frame,
		fieldStorage.fields,
	)
	header := decodedClientHeaders{
		streamID:  streamID,
		endStream: endStream,
		truncated: truncated,
		fields:    decoded,
	}
	event := incomingFrame{fields: decoded, fieldStorage: fieldStorage}
	defer releaseIncomingFrame(&event)
	if err != nil {
		return err
	}
	if invalid != nil {
		return c.rejectHeaderBlock(streamID, xhttp2.ErrCodeProtocol, invalid)
	}
	if selfDependent {
		return c.rejectHeaderBlock(streamID, xhttp2.ErrCodeProtocol, errors.New("http2: stream depends on itself"))
	}
	return c.processResponseHeaders(&header)
}

func (c *clientConn) processSettings(frame *xhttp2.SettingsFrame) error {
	if frame.IsAck() {
		if !c.settingsAcked {
			c.settingsAcked = true
			c.applyDecoderTableSize()
		}
		return nil
	}
	var encoderTableSize, minEncoderTableSize uint32
	var updateEncoderTable, streamLimitChanged bool
	c.mu.Lock()
	for i := range frame.NumSettings() {
		setting := frame.Setting(i)
		if err := setting.Valid(); err != nil {
			c.mu.Unlock()
			return err
		}
		switch setting.ID {
		case xhttp2.SettingHeaderTableSize:
			encoderTableSize = min(setting.Val, c.config.maxEncoderTableSize)
			if !updateEncoderTable || encoderTableSize < minEncoderTableSize {
				minEncoderTableSize = encoderTableSize
			}
			updateEncoderTable = true
		case xhttp2.SettingMaxConcurrentStreams:
			c.peerMaxConcurrentStreams = setting.Val
			streamLimitChanged = true
		case xhttp2.SettingInitialWindowSize:
			delta := int64(setting.Val) - c.peerInitialStreamWindow
			for _, stream := range c.streams {
				stream.send.window += delta
				if stream.send.window > math.MaxInt32 {
					c.mu.Unlock()
					return peerError(xhttp2.ErrCodeFlowControl, "http2: stream send window overflow")
				}
			}
			c.peerInitialStreamWindow = int64(setting.Val)
		case xhttp2.SettingMaxFrameSize:
			c.peerMaxFrameSize = int(setting.Val)
		case xhttp2.SettingMaxHeaderListSize:
			c.peerMaxHeaderListSize = uint64(setting.Val)
		case xhttp2.SettingEnableConnectProtocol:
			c.peerExtendedConnect = setting.Val == 1
		case xhttp2.SettingEnablePush:
			if setting.Val != 0 {
				c.mu.Unlock()
				return peerError(xhttp2.ErrCodeProtocol, "http2: server enabled push")
			}
		case xhttp2.SettingNoRFC7540Priorities:
			if setting.Val > 1 {
				c.mu.Unlock()
				return peerError(xhttp2.ErrCodeProtocol, "http2: invalid SETTINGS_NO_RFC7540_PRIORITIES")
			}
		}
	}
	c.receivedSettings = true
	c.signalLocked()
	if streamLimitChanged {
		c.pool.capacityChanged()
	}
	// The encoder's table size changes under the write slot, right before
	// the ACK, so the next block encoded after the ACK carries the update.
	c.queueControlLocked(controlFrame{
		kind:               controlSettingsAck,
		updateEncoderTable: updateEncoderTable,
		minTableSize:       minEncoderTableSize,
		tableSize:          encoderTableSize,
	})
	c.mu.Unlock()
	return nil
}

// applyDecoderTableSize enforces the advertised HPACK table size once the
// peer acknowledges it.
func (c *clientConn) applyDecoderTableSize() {
	c.headerDecoder.setMaxTableSize(c.config.maxDecoderTableSize)
}

func (c *clientConn) waitForExtendedConnect(deadline time.Time) (bool, error) {
	for {
		c.mu.Lock()
		if c.closed {
			err := c.err
			if err == nil {
				err = errClientConnClosed
			}
			c.mu.Unlock()
			return false, err
		}
		if c.receivedSettings {
			enabled := c.peerExtendedConnect
			c.mu.Unlock()
			return enabled, nil
		}
		notify := c.notify
		c.waiters++
		c.mu.Unlock()
		err := waitForStreamEvent(notify, deadline, nil)
		c.mu.Lock()
		c.waiters--
		c.mu.Unlock()
		if err != nil {
			return false, err
		}
	}
}

func (c *clientConn) processResponseHeaders(frame *decodedClientHeaders) error {
	if frame.truncated {
		return c.rejectHeaderBlock(frame.streamID, xhttp2.ErrCodeEnhanceYourCalm, errInvalidResponseHeaders)
	}
	c.mu.Lock()
	stream := c.streams[frame.streamID]
	if stream == nil {
		_, idle := c.streamStateLocked(frame.streamID)
		c.mu.Unlock()
		if idle {
			return peerError(xhttp2.ErrCodeProtocol, "http2: response headers on idle stream")
		}
		// Closed: the block is decoded, which keeps HPACK in sync, and
		// otherwise ignored (RFC 9113 §5.1).
		return nil
	}
	// Any HEADERS, informational included, ends the reserved state.
	stream.reserved = false
	reject := func(cause error) error {
		c.mu.Unlock()
		c.resetStream(frame.streamID, xhttp2.ErrCodeProtocol, cause, failurePeer)
		return nil
	}
	if stream.responseHeader {
		if stream.remoteClosed || !frame.endStream {
			return reject(errors.New("http2: invalid response trailers"))
		}
		if stream.responseBody == nil {
			// Do reports the rejection, so a partial trailer set on resp is
			// covered by its errors-leave-resp-undefined contract.
			if err := populateResponseTrailers(c.lease, stream.resp, frame.fields); err != nil {
				return reject(err)
			}
		} else {
			// A streaming body applies trailers only when its reader drains,
			// too late to reject the frame.
			var validationResponse fasthttp.Response
			if err := populateResponseTrailers(c.lease, &validationResponse, frame.fields); err != nil {
				return reject(err)
			}
			trailers := append([]hpack.HeaderField(nil), frame.fields...)
			response := stream.resp
			// The commit may outlive the connection; ParseTrailer reads no
			// connection state.
			lease := c.lease
			stream.responseBody.setEOFCommit(func() error {
				return populateResponseTrailers(lease, response, trailers)
			})
		}
		c.endResponseLocked(stream, nil)
		c.mu.Unlock()
		return nil
	}
	status, err := responseStatus(frame.fields)
	if err != nil {
		return reject(err)
	}
	if status >= 100 && status < 200 {
		if status == 101 || frame.endStream {
			return reject(errors.New("http2: invalid informational response"))
		}
		stream.informationalResponses++
		if stream.informationalResponses > 16 {
			return reject(errors.New("http2: too many informational responses"))
		}
		c.mu.Unlock()
		return nil
	}
	status, contentLength, err := populateResponse(
		c.lease,
		stream.resp,
		frame.fields,
		c.hc.DisableHeaderNamesNormalizing,
	)
	if err != nil {
		return reject(err)
	}
	stream.statusCode = status
	// A content-length on a response defined to have no content (RFC 9113
	// §8.1.1) or on a 2xx CONNECT response (RFC 9110 §9.3.6) says nothing
	// about the DATA that follows.
	if responseHasNoBody(stream) || stream.isTunnel() {
		contentLength = -1
	}
	stream.expectedResponseBytes = contentLength
	stream.responseHeader = true
	stream.resp.ParseNetConn(c.lease.Conn())
	if !stream.discardResponseBody && stream.maxResponseBodySize > 0 &&
		contentLength > int64(stream.maxResponseBodySize) {
		c.mu.Unlock()
		c.resetStream(frame.streamID, xhttp2.ErrCodeCancel, fasthttp.ErrBodyTooLarge, failurePeer)
		return nil
	}
	// A 204 or 304 has no body to stream: the response completes at END_STREAM
	// as a buffered one does, with no body stream for the caller to close, as
	// over HTTP/1.
	if stream.isStreaming && !stream.isOpenStream && (status == 204 || status == 304) {
		stream.isStreaming = false
	}
	if stream.isStreaming {
		body := newResponseBody(c, stream.id)
		stream.responseBody = body
		if !stream.isOpenStream {
			bodySize := -1
			if contentLength >= 0 {
				bodySize = int(contentLength)
			}
			stream.resp.SetBodyStream(body, bodySize)
		}
	}
	var rejectOpenStream error
	endOpenStream := false
	if stream.isOpenStream {
		if status < 200 || status >= 300 {
			err := fmt.Errorf("http2: extended connect failed with status %d", status)
			stream.err = err
			stream.bodyDone = true
			stream.responseBody.fail(err)
			c.sendResultLocked(stream, clientResult{err: err})
			if frame.endStream {
				// The peer closed its side; END_STREAM closes this one.
				endOpenStream = true
			} else {
				rejectOpenStream = err
			}
		} else {
			if stream.timer != nil {
				stream.timer.Stop()
				stream.timer = nil
			}
			c.sendResultLocked(stream, clientResult{streamConn: newClientStreamConn(c.lease.Conn(), stream)})
		}
	} else if stream.isStreaming {
		c.sendResultLocked(stream, clientResult{})
	}
	if frame.endStream {
		c.endResponseLocked(stream, nil)
	}
	c.mu.Unlock()
	if rejectOpenStream != nil {
		c.resetStream(frame.streamID, xhttp2.ErrCodeCancel, rejectOpenStream, failurePeer)
	}
	if endOpenStream {
		c.mu.Lock()
		c.queueControlLocked(controlFrame{kind: controlEndStream, streamID: frame.streamID})
		stream.localClosed = true
		c.maybeFinalizeStreamLocked(stream)
		c.mu.Unlock()
	}
	return nil
}

func (c *clientConn) rejectHeaderBlock(streamID uint32, code xhttp2.ErrCode, cause error) error {
	c.mu.Lock()
	stream, idle := c.streamStateLocked(streamID)
	c.mu.Unlock()
	if stream != nil {
		c.resetStream(streamID, code, cause, failurePeer)
		return nil
	}
	if idle {
		return &peerConnectionError{
			code: xhttp2.ErrCodeProtocol,
			err:  fmt.Errorf("http2: invalid header block on idle stream: %w", cause),
		}
	}
	return nil
}

func responseHasNoBody(stream *clientStream) bool {
	return stream.isHead || stream.statusCode == 204 || stream.statusCode == 304
}

// isTunnel reports a successful CONNECT, whose bytes carry no length.
func (s *clientStream) isTunnel() bool {
	return s.isConnect && s.statusCode >= 200 && s.statusCode < 300
}

func (c *clientConn) processResponseData(frame *xhttp2.DataFrame) error {
	flowLength := int64(frame.Header().Length)
	data := frame.Data()
	c.mu.Lock()
	stream := c.streams[frame.StreamID]
	if stream == nil {
		_, idle := c.streamStateLocked(frame.StreamID)
		if idle {
			c.mu.Unlock()
			return peerError(xhttp2.ErrCodeProtocol, "http2: data on idle response stream")
		}
		if !c.recv.debit(flowLength) {
			c.mu.Unlock()
			return errConnectionWindowExceeded()
		}
		c.mu.Unlock()
		// Closed: the frame is ignored (RFC 9113 §5.1), but its flow still
		// counts against the connection window.
		c.restoreConnectionWindow(flowLength)
		return nil
	}
	if stream.reserved {
		c.mu.Unlock()
		return peerError(xhttp2.ErrCodeProtocol, "http2: data on reserved push stream")
	}
	if !c.recv.debit(flowLength) {
		c.mu.Unlock()
		return errConnectionWindowExceeded()
	}
	// rejectData returns the frame's flow to the connection and resets the
	// stream; it takes over the lock the caller still holds.
	rejectData := func(code xhttp2.ErrCode, cause error) error {
		c.mu.Unlock()
		c.restoreConnectionWindow(flowLength)
		c.resetStream(frame.StreamID, code, cause, failurePeer)
		return nil
	}
	if !stream.responseHeader || stream.remoteClosed {
		return rejectData(xhttp2.ErrCodeProtocol, errDataOnInvalidStream)
	}
	if !stream.recv.debit(flowLength) {
		return rejectData(xhttp2.ErrCodeFlowControl, errResponseStreamWindowExceeded)
	}
	if responseHasNoBody(stream) && len(data) != 0 {
		return rejectData(xhttp2.ErrCodeProtocol, errResponseBodyNotPermitted)
	}
	stream.responseBytes += int64(len(data))
	if stream.expectedResponseBytes >= 0 && stream.responseBytes > stream.expectedResponseBytes {
		return rejectData(xhttp2.ErrCodeProtocol, errResponseBodyLengthMismatch)
	}
	// A tunnel's bytes aren't a response body.
	if !stream.discardResponseBody && !stream.isOpenStream && stream.maxResponseBodySize > 0 &&
		stream.responseBytes > int64(stream.maxResponseBodySize) {
		return rejectData(xhttp2.ErrCodeCancel, fasthttp.ErrBodyTooLarge)
	}
	if stream.callerClosed && len(data) != 0 {
		return rejectData(xhttp2.ErrCodeCancel, errClientStreamClosed)
	}
	body := stream.responseBody
	discardBody := stream.discardResponseBody
	if body == nil && !discardBody {
		stream.resp.AppendBody(data)
	}
	ended := frame.StreamEnded()
	c.mu.Unlock()

	switch {
	case discardBody && len(data) != 0:
		c.consumeResponseBytes(frame.StreamID, len(data))
	case body != nil && len(data) != 0:
		if err := body.write(data); err != nil {
			c.closeResponseBody(frame.StreamID, len(data))
			padding := int(flowLength) - len(data)
			if padding > 0 {
				c.restoreConnectionWindow(int64(padding))
			}
			return nil
		}
	case len(data) != 0:
		c.consumeResponseBytes(frame.StreamID, len(data))
	}
	padding := int(flowLength) - len(data)
	if padding > 0 {
		c.consumeResponseBytes(frame.StreamID, padding)
	}
	if ended {
		c.mu.Lock()
		if current := c.streams[frame.StreamID]; current == stream {
			c.endResponseLocked(stream, nil)
		}
		c.mu.Unlock()
	}
	return nil
}

func errConnectionWindowExceeded() error {
	return peerError(xhttp2.ErrCodeFlowControl, "http2: response connection flow-control window exceeded")
}

func (c *clientConn) restoreConnectionWindow(amount int64) {
	if amount <= 0 {
		return
	}
	c.mu.Lock()
	c.recordCreditLocked(0, amount, 0)
	c.mu.Unlock()
}

// recordCreditLocked queues WINDOW_UPDATE increments for sendControl, which
// grants them as it commits them: until then the peer may not spend them.
func (c *clientConn) recordCreditLocked(streamID uint32, connIncrement, streamIncrement int64) {
	if connIncrement == 0 && streamIncrement == 0 {
		return
	}
	c.creditConn += connIncrement
	if streamIncrement != 0 {
		if c.creditStreams == nil {
			c.creditStreams = make(map[uint32]int64)
		}
		c.creditStreams[streamID] += streamIncrement
	}
	c.signalSenderLocked()
}

func (c *clientConn) signalSenderLocked() {
	select {
	case c.senderSignal <- struct{}{}:
	default:
	}
}

type streamCredit struct {
	id        uint32
	increment int64
}

type controlKind uint8

const (
	controlPingAck controlKind = iota
	controlSettingsAck
	controlReset
	controlEndStream
)

// controlFrame is a frame the read loop owes the peer, written by
// sendControl in the order it was queued.
type controlFrame struct {
	kind     controlKind
	streamID uint32
	code     xhttp2.ErrCode
	ping     [8]byte
	// A SETTINGS ACK carries the encoder table size change it acknowledges.
	updateEncoderTable bool
	minTableSize       uint32
	tableSize          uint32
}

// maxQueuedControlFrames bounds the control frames queued for sendControl,
// the sender's detached batch included (x/net's server constant).
var maxQueuedControlFrames = 10000

// queueControlLocked queues frame for sendControl; c.mu is held. Past the
// bound, reached when the transport is stalled and the peer keeps sending,
// the frame is dropped and the connection fails with ENHANCE_YOUR_CALM;
// nothing more is queued after that.
func (c *clientConn) queueControlLocked(frame controlFrame) {
	if c.controlOverflow {
		return
	}
	if len(c.controls)+c.controlsInFlight >= maxQueuedControlFrames {
		c.controlOverflow = true
		// Not under c.mu, and not from sendControl, which fail waits on.
		go c.fail(errTooManyControlFrames)
		return
	}
	c.controls = append(c.controls, frame)
	c.signalSenderLocked()
}

func (c *clientConn) queueControl(frame controlFrame) {
	c.mu.Lock()
	c.queueControlLocked(frame)
	c.mu.Unlock()
}

func (c *clientConn) queueReset(streamID uint32, code xhttp2.ErrCode) error {
	c.queueControl(controlFrame{kind: controlReset, streamID: streamID, code: code})
	return nil
}

func (c *clientConn) writeControlFrame(frame *controlFrame) error {
	switch frame.kind {
	case controlPingAck:
		return c.framer.WritePing(true, frame.ping)
	case controlSettingsAck:
		if frame.updateEncoderTable {
			// The encoder signals the smallest size as well as the last one
			// (RFC 7541 §4.2).
			c.encoder.SetMaxDynamicTableSize(frame.minTableSize)
			c.encoder.SetMaxDynamicTableSize(frame.tableSize)
		}
		return c.framer.WriteSettingsAck()
	case controlReset:
		return c.framer.WriteRSTStream(frame.streamID, frame.code)
	case controlEndStream:
		return c.framer.WriteData(frame.streamID, true, nil)
	}
	return nil
}

// sendControl frames the recorded credit and the queued control frames from
// its own goroutine, so the read loop never waits for the write slot. The
// credit is granted as it is committed, under the c.mu that snapshots it:
// the peer may spend it as soon as the frame reaches it, which can be before
// this goroutine runs again, and it can't spend it while it waits for the
// slot. A stream increment is dropped when its stream is gone or the peer
// ended its side (RFC 9113 §6.9: no WINDOW_UPDATE on a half-closed (remote)
// stream). Whatever is still queued when the connection fails is dropped
// with it; a write failure fails the connection. On its way out it writes
// the GOAWAY a failure recorded.
func (c *clientConn) sendControl() {
	defer close(c.senderDone)
	defer c.writeGoAwayOnExit()
	var pending []streamCredit
	var controls []controlFrame
	for {
		select {
		case <-c.senderSignal:
		case <-c.done:
			return
		}
		if err := c.lockWrite(time.Time{}, c.done); err != nil {
			return
		}
		c.mu.Lock()
		connIncrement := c.creditConn
		c.creditConn = 0
		c.recv.grant(connIncrement)
		pending = pending[:0]
		for streamID, increment := range c.creditStreams {
			if stream := c.streams[streamID]; stream != nil && !stream.remoteClosed {
				stream.recv.grant(increment)
				pending = append(pending, streamCredit{id: streamID, increment: increment})
			}
		}
		clear(c.creditStreams)
		controls, c.controls = c.controls, controls[:0]
		c.controlsInFlight = len(controls)
		c.mu.Unlock()
		var err error
		if connIncrement != 0 {
			err = c.framer.WriteWindowUpdate(0, uint32(connIncrement)) // #nosec G115
		}
		for i := range pending {
			if err != nil {
				break
			}
			err = c.framer.WriteWindowUpdate(pending[i].id, uint32(pending[i].increment)) // #nosec G115
		}
		for i := range controls {
			if err != nil {
				break
			}
			err = c.writeControlFrame(&controls[i])
		}
		if err == nil && (connIncrement != 0 || len(pending) != 0 || len(controls) != 0) {
			err = c.bufferedWriter.Flush()
		}
		if len(controls) != 0 {
			c.mu.Lock()
			c.controlsInFlight = 0
			c.mu.Unlock()
		}
		transportErr := err
		if err = c.unlockWrite(err); err != nil {
			// fail waits for this goroutine to return.
			go c.fail(transportErr)
			return
		}
	}
}

func (c *clientConn) writeGoAwayOnExit() {
	c.mu.Lock()
	goAway := c.goAwayOnExit
	c.mu.Unlock()
	if goAway != nil {
		c.writeGoAway(goAway.lastStreamID, goAway.code)
	}
}

func (c *clientConn) endResponseLocked(stream *clientStream, responseErr error) {
	if stream.expectedResponseBytes >= 0 && stream.responseBytes != stream.expectedResponseBytes {
		responseErr = errResponseBodyTooShort
	}
	stream.remoteClosed = true
	// A request body still waiting for flow control stops here.
	c.signalLocked()
	if stream.responseBody != nil {
		if responseErr != nil {
			stream.responseBody.fail(responseErr)
		} else {
			stream.responseBody.complete()
		}
	}
	switch {
	case responseErr != nil:
		stream.err = responseErr
		if !stream.isPush {
			c.sendResultLocked(stream, clientResult{retry: stream.retryable(failurePeer, responseErr), err: responseErr})
		}
	case stream.isPush:
		stream.pushComplete = true
	case !stream.isStreaming:
		c.sendResultLocked(stream, clientResult{})
	}
	c.maybeFinalizeStreamLocked(stream)
}

func (c *clientConn) consumeResponseBytes(streamID uint32, amount int) {
	c.mu.Lock()
	connectionIncrement := c.recv.consume(int64(amount), int64(c.config.connectionWindowSize))
	streamIncrement := int64(0)
	if stream := c.streams[streamID]; stream != nil && !stream.remoteClosed {
		streamIncrement = stream.recv.consume(int64(amount), int64(c.config.streamWindowSize))
	}
	c.recordCreditLocked(streamID, connectionIncrement, streamIncrement)
	c.mu.Unlock()
}

func (c *clientConn) closeResponseBody(streamID uint32, discarded int) {
	if discarded > 0 {
		c.consumeResponseBytes(streamID, discarded)
	}
	c.mu.Lock()
	stream := c.streams[streamID]
	remoteOpen := stream != nil && !stream.remoteClosed
	if stream != nil && stream.isOpenStream {
		stream.discardResponseBody = true
		remoteOpen = false
	}
	c.mu.Unlock()
	if remoteOpen {
		c.resetStream(streamID, xhttp2.ErrCodeCancel, errClientStreamClosed, failureLocal)
	}
}

func (c *clientConn) responseBodyDone(streamID uint32) {
	c.mu.Lock()
	if stream := c.streams[streamID]; stream != nil {
		stream.bodyDone = true
		c.maybeFinalizeStreamLocked(stream)
	}
	c.mu.Unlock()
}

func (c *clientConn) processWindowUpdate(frame *xhttp2.WindowUpdateFrame) error {
	c.mu.Lock()
	if frame.StreamID == 0 {
		defer c.mu.Unlock()
		if !c.send.credit(frame.Increment) {
			return &peerConnectionError{code: xhttp2.ErrCodeFlowControl, err: fmt.Errorf(
				"http2: client connection send window overflow: current=%d increment=%d",
				c.send.window,
				frame.Increment,
			)}
		}
		c.signalLocked()
		return nil
	}
	if stream := c.streams[frame.StreamID]; stream != nil {
		if stream.reserved {
			c.mu.Unlock()
			return peerError(xhttp2.ErrCodeProtocol, "http2: window update on reserved push stream")
		}
		if !stream.send.credit(frame.Increment) {
			// RFC 9113 §6.9.1: a stream error for a stream's window.
			c.cancelStreamAndUnlock(stream, xhttp2.ErrCodeFlowControl,
				errors.New("http2: stream send window overflow"), failurePeer)
			return nil
		}
		c.signalLocked()
		c.mu.Unlock()
		return nil
	}
	_, idle := c.streamStateLocked(frame.StreamID)
	c.mu.Unlock()
	if idle {
		return peerError(xhttp2.ErrCodeProtocol, "http2: window update on idle stream")
	}
	return nil
}

func (c *clientConn) streamStateLocked(streamID uint32) (*clientStream, bool) {
	if stream := c.streams[streamID]; stream != nil {
		return stream, false
	}
	if streamID&1 == 1 {
		return nil, streamID >= c.nextStreamID
	}
	return nil, streamID > c.lastPromisedStreamID
}

func (c *clientConn) processGoAway(frame *xhttp2.GoAwayFrame) error {
	c.mu.Lock()
	if c.goAway && frame.LastStreamID > c.goAwayLastStreamID {
		c.mu.Unlock()
		return xhttp2.ConnectionError(xhttp2.ErrCodeProtocol)
	}
	c.goAway = true
	c.goAwayLastStreamID = frame.LastStreamID
	goAwayErr := &GoAwayError{
		LastStreamID: frame.LastStreamID,
		ErrCode:      uint32(frame.ErrCode),
	}
	for _, stream := range c.streams {
		// LastStreamID covers client-initiated streams only, not pushes.
		if !stream.isPush && stream.id > frame.LastStreamID && !stream.responseHeader {
			c.failStreamLocked(stream, goAwayErr, failureConnection)
		}
	}
	c.signalLocked()
	closeNow := c.activeStreams == 0
	c.mu.Unlock()
	c.pool.capacityChanged()
	if closeNow {
		c.closeIfIdle()
	}
	return nil
}

func (c *clientConn) processPushPromise(frame *xhttp2.PushPromiseFrame) error {
	fieldStorage := acquireIncomingHeaderFields(8)
	allowIllegalReads := c.framer.AllowIllegalReads
	c.framer.AllowIllegalReads = true
	decoded, truncated, invalid, err := c.headerDecoder.decode(
		c.framer,
		frame.StreamID,
		frame,
		fieldStorage.fields,
	)
	c.framer.AllowIllegalReads = allowIllegalReads
	event := incomingFrame{fields: decoded, fieldStorage: fieldStorage}
	defer releaseIncomingFrame(&event)
	if err != nil {
		return err
	}

	if frame.StreamID&1 == 0 {
		// RFC 9113 §6.6: a promise rides a client-initiated stream, so a
		// pushed stream cannot promise another.
		return xhttp2.ConnectionError(xhttp2.ErrCodeProtocol)
	}
	c.mu.Lock()
	parent, idle := c.streamStateLocked(frame.StreamID)
	validID := frame.PromiseID != 0 && frame.PromiseID&1 == 0 && frame.PromiseID > c.lastPromisedStreamID
	if !validID {
		c.mu.Unlock()
		return peerError(xhttp2.ErrCodeProtocol, "http2: invalid promised stream id")
	}
	if idle {
		// RFC 9113 §6.6: the parent must be open or half-closed (remote); a
		// closed parent gets the rejection below.
		c.mu.Unlock()
		return peerError(xhttp2.ErrCodeProtocol, "http2: push promise on idle stream")
	}
	c.lastPromisedStreamID = frame.PromiseID
	pushDisabled := c.config.pushHandler == nil
	reject := parent == nil || c.activePushStreams >= c.config.maxConcurrentStreams
	c.mu.Unlock()
	if pushDisabled {
		return xhttp2.ConnectionError(xhttp2.ErrCodeProtocol)
	}
	if reject {
		return c.queueReset(frame.PromiseID, xhttp2.ErrCodeCancel)
	}
	return c.finishPushPromise(frame.StreamID, frame.PromiseID, decoded, truncated, invalid)
}

func (c *clientConn) finishPushPromise(
	parentID uint32,
	promisedID uint32,
	decoded []hpack.HeaderField,
	truncated bool,
	invalid error,
) error {
	if truncated {
		return c.queueReset(promisedID, xhttp2.ErrCodeEnhanceYourCalm)
	}
	if invalid != nil {
		return c.queueReset(promisedID, xhttp2.ErrCodeProtocol)
	}

	promised := fasthttp.AcquireRequest()
	if err := populatePromisedRequest(promised, decoded); err != nil {
		fasthttp.ReleaseRequest(promised)
		return c.queueReset(promisedID, xhttp2.ErrCodeProtocol)
	}
	c.mu.Lock()
	parent := c.streams[parentID]
	if parent == nil {
		c.mu.Unlock()
		fasthttp.ReleaseRequest(promised)
		return c.queueReset(promisedID, xhttp2.ErrCodeCancel)
	}
	if parent.parentRequest == nil || !sameOrigin(parent.parentRequest, promised) {
		// RFC 9113 §8.4: the server is only known to be authoritative for the
		// origin the request went to.
		c.mu.Unlock()
		fasthttp.ReleaseRequest(promised)
		return c.queueReset(promisedID, xhttp2.ErrCodeProtocol)
	}
	parentCopy := fasthttp.AcquireRequest()
	parent.parentRequest.CopyTo(parentCopy)
	c.mu.Unlock()
	accepted := c.config.pushHandler.Accept(parentCopy, promised)
	fasthttp.ReleaseRequest(parentCopy)
	if !accepted {
		fasthttp.ReleaseRequest(promised)
		return c.queueReset(promisedID, xhttp2.ErrCodeCancel)
	}

	response := fasthttp.AcquireResponse()
	c.mu.Lock()
	if c.closed || c.activePushStreams >= c.config.maxConcurrentStreams {
		c.mu.Unlock()
		fasthttp.ReleaseRequest(promised)
		fasthttp.ReleaseResponse(response)
		return c.queueReset(promisedID, xhttp2.ErrCodeCancel)
	}
	stream := acquireClientStream()
	isHead := promised.Header.IsHead()
	*stream = clientStream{
		id:                  promisedID,
		conn:                c,
		req:                 promised,
		resp:                response,
		requestStarted:      true,
		localClosed:         true,
		bodyDone:            true,
		isHead:              isHead,
		discardResponseBody: isHead,
		streamFlowState: streamFlowState{
			send: sendWindow{window: c.peerInitialStreamWindow},
			recv: recvWindow{window: int64(c.config.streamWindowSize)},
		},
		expectedResponseBytes: -1,
		maxResponseBodySize:   c.responseBodyLimit(false),
		isPush:                true,
		reserved:              true,
		promisedRequest:       promised,
		callerDone:            true,
		poolable:              true,
	}
	c.streams[stream.id] = stream
	c.activeStreams++
	c.activePushStreams++
	c.idleSeq = 0
	// HostClient.ReadTimeout bounds the promised response as it bounds a
	// requested one; without it the push waits until CloseIdleConnections.
	if readTimeout := c.hc.ReadTimeout; readTimeout > 0 {
		stream.readTimer = time.AfterFunc(readTimeout, func() {
			c.resetStream(promisedID, xhttp2.ErrCodeCancel, fasthttp.ErrTimeout, failureReadTimeout)
		})
	}
	c.mu.Unlock()
	return nil
}

// cancelUnansweredPushes fails pushes the peer promised but never answered
// when nothing else runs on c, so that it can be closed as idle. The close
// follows at once, so no RST_STREAM is owed and no write slot is waited for.
func (c *clientConn) cancelUnansweredPushes() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.activeStreams == 0 || c.activeStreams != c.activePushStreams {
		return
	}
	for _, stream := range c.streams {
		if stream.responseHeader {
			return
		}
	}
	for _, stream := range c.streams {
		c.failStreamLocked(stream, errClientConnClosed, failureConnection)
	}
}

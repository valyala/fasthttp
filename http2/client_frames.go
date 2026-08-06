package http2

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type decodedClientHeaders struct {
	streamID  uint32
	endStream bool
	truncated bool
	fields    []hpack.HeaderField
}

func (c *clientConn) readLoop() {
	waitingForPing := false
	awaitingFirstFrame := true
	for {
		readTimeout := c.config.readIdleTimeout
		if waitingForPing {
			readTimeout = c.config.pingTimeout
		}
		if readTimeout > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		}
		frame, err := c.frames.readFrame()
		if err != nil {
			if isTimeout(err) && c.config.readIdleTimeout > 0 && !waitingForPing {
				if pingErr := c.writeControl(func() error {
					return c.framer.WritePing(false, [8]byte{'f', 'a', 's', 't', 'h', '2', 0, 2})
				}); pingErr != nil {
					return
				}
				waitingForPing = true
				continue
			}
			c.fail(err)
			return
		}
		waitingForPing = false
		if awaitingFirstFrame {
			settings, ok := frame.(*xhttp2.SettingsFrame)
			if !ok || settings.IsAck() {
				c.fail(errors.New("http2: server's first frame isn't settings"))
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

func (c *clientConn) processFrame(frame xhttp2.Frame) error {
	switch frame := frame.(type) {
	case *xhttp2.SettingsFrame:
		return c.processSettings(frame)
	case *xhttp2.DataFrame:
		return c.processResponseData(frame)
	case *xhttp2.RSTStreamFrame:
		if c.config.countError != nil {
			// Guarded at the call site: building the tag allocates twice, and
			// resets are common enough that a disabled counter must stay free.
			c.countError("stream_" + strings.ToLower(frame.ErrCode.String()))
		}
		c.mu.Lock()
		stream, idle := c.streamStateLocked(frame.StreamID)
		c.mu.Unlock()
		if stream == nil {
			if idle {
				return errors.New("http2: reset on idle stream")
			}
			return nil
		}
		retry := frame.ErrCode == xhttp2.ErrCodeRefusedStream
		c.failStream(frame.StreamID, &StreamError{
			StreamID: frame.StreamID,
			ErrCode:  uint32(frame.ErrCode),
		}, retry)
		return nil
	case *xhttp2.WindowUpdateFrame:
		return c.processWindowUpdate(frame)
	case *xhttp2.PingFrame:
		if !frame.IsAck() {
			return c.writeControl(func() error { return c.framer.WritePing(true, frame.Data) })
		}
		return nil
	case *xhttp2.GoAwayFrame:
		return c.processGoAway(frame)
	case *xhttp2.PushPromiseFrame:
		return c.processPushPromise(frame)
	case *xhttp2.PriorityFrame:
		if frame.StreamID == frame.StreamDep {
			c.resetStream(frame.StreamID, xhttp2.ErrCodeProtocol, errors.New("http2: stream depends on itself"), false)
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
		return nil
	}
	var encoderTableSize uint32
	var updateEncoderTable bool
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
			updateEncoderTable = true
		case xhttp2.SettingMaxConcurrentStreams:
			c.peerMaxConcurrentStreams = setting.Val
		case xhttp2.SettingInitialWindowSize:
			delta := int64(setting.Val) - c.peerInitialStreamWindow
			for _, stream := range c.streams {
				stream.sendWindow += delta
				if stream.sendWindow > math.MaxInt32 {
					c.mu.Unlock()
					return errors.New("http2: stream send window overflow")
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
				return errors.New("http2: server enabled push")
			}
		case xhttp2.SettingNoRFC7540Priorities:
			if setting.Val > 1 {
				c.mu.Unlock()
				return errors.New("http2: invalid SETTINGS_NO_RFC7540_PRIORITIES")
			}
		}
	}
	c.receivedSettings = true
	c.signalLocked()
	c.mu.Unlock()
	if err := c.lockWrite(time.Time{}); err != nil {
		return err
	}
	if updateEncoderTable {
		c.encoder.SetMaxDynamicTableSize(encoderTableSize)
	}
	err := c.framer.WriteSettingsAck()
	if err == nil {
		err = c.bufferedWriter.Flush()
	}
	err = c.unlockWrite(err)
	if err != nil {
		c.fail(err)
	}
	return err
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
		err := waitForStreamEvent(notify, deadline)
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
			return errors.New("http2: response headers on idle stream")
		}
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(frame.streamID, xhttp2.ErrCodeStreamClosed)
		})
	}
	reject := func(cause error) error {
		c.mu.Unlock()
		c.resetStream(frame.streamID, xhttp2.ErrCodeProtocol, cause, false)
		return nil
	}
	if stream.responseHeader {
		if stream.remoteClosed || !frame.endStream {
			return reject(errors.New("http2: invalid response trailers"))
		}
		var validationResponse fasthttp.Response
		if err := populateResponseTrailers(&validationResponse, frame.fields); err != nil {
			return reject(err)
		}
		trailers := append([]hpack.HeaderField(nil), frame.fields...)
		if stream.responseBody == nil {
			if err := populateResponseTrailers(stream.resp, trailers); err != nil {
				return reject(err)
			}
		} else {
			response := stream.resp
			stream.responseBody.setEOFCommit(func() error {
				return populateResponseTrailers(response, trailers)
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
		stream.resp,
		frame.fields,
		c.hc.DisableHeaderNamesNormalizing,
	)
	if err != nil {
		return reject(err)
	}
	if err := validateResponseContentLength(stream, status, contentLength); err != nil {
		return reject(err)
	}
	stream.statusCode = status
	noBody := responseHasNoBody(stream)
	if noBody {
		stream.expectedResponseBytes = -1
		stream.resp.SkipBody = true
	} else {
		stream.expectedResponseBytes = contentLength
	}
	stream.responseHeader = true
	c.lease.ApplyResponseMetadata(stream.resp)
	if !noBody && !stream.discardResponseBody && stream.maxResponseBodySize > 0 &&
		contentLength > int64(stream.maxResponseBodySize) {
		c.mu.Unlock()
		c.resetStream(frame.streamID, xhttp2.ErrCodeCancel, fasthttp.ErrBodyTooLarge, false)
		return nil
	}
	if !noBody && !stream.discardResponseBody && !stream.isStreaming && contentLength > 0 {
		c.lease.PrepareResponseBody(stream.resp, int(min(contentLength, maxResponseBodyPreallocation)))
	}
	if stream.isStreaming {
		body := c.newClientResponseBody(stream.id)
		stream.responseBody = body
		if !stream.isOpenStream {
			bodySize := -1
			if contentLength >= 0 {
				bodySize = int(contentLength)
			}
			stream.resp.SetBodyStream(body, bodySize)
			stream.resp.Header.Del(fasthttp.HeaderTransferEncoding)
		}
	}
	rejectOpenStream := false
	if stream.isOpenStream {
		if status < 200 || status >= 300 {
			err := fmt.Errorf("http2: extended connect failed with status %d", status)
			stream.err = err
			stream.bodyDone = true
			stream.localClosed = true
			stream.responseBody.closeWithError(err)
			c.sendResultLocked(stream, clientResult{err: err})
			rejectOpenStream = !frame.endStream
		} else {
			if stream.timer != nil {
				stream.timer.Stop()
				stream.timer = nil
			}
			streamConn := &clientStreamConn{stream: stream, read: stream.responseBody}
			c.sendResultLocked(stream, clientResult{streamConn: streamConn})
		}
	} else if stream.isStreaming {
		c.sendResultLocked(stream, clientResult{})
	}
	if frame.endStream {
		c.endResponseLocked(stream, nil)
	}
	c.mu.Unlock()
	if rejectOpenStream {
		c.resetStream(frame.streamID, xhttp2.ErrCodeCancel, stream.err, false)
	}
	return nil
}

func (c *clientConn) newClientResponseBody(streamID uint32) *responseBody {
	return newResponseBody(
		func(consumed int) { c.consumeResponseBytes(streamID, consumed) },
		func(discarded int) { c.closeResponseBody(streamID, discarded) },
		func() { c.responseBodyDone(streamID) },
	)
}

func (c *clientConn) rejectHeaderBlock(streamID uint32, code xhttp2.ErrCode, cause error) error {
	c.mu.Lock()
	stream, idle := c.streamStateLocked(streamID)
	c.mu.Unlock()
	if stream != nil {
		c.resetStream(streamID, code, cause, false)
		return nil
	}
	if idle {
		return fmt.Errorf("http2: invalid header block on idle stream: %w", cause)
	}
	return c.writeControl(func() error {
		return c.framer.WriteRSTStream(streamID, xhttp2.ErrCodeStreamClosed)
	})
}

func responseHasNoBody(stream *clientStream) bool {
	return stream.isHead || stream.statusCode == 204 || stream.statusCode == 304
}

func validateResponseContentLength(stream *clientStream, status int, contentLength int64) error {
	if contentLength < 0 {
		return nil
	}
	if status == fasthttp.StatusNoContent ||
		stream.isConnect && status >= 200 && status < 300 {
		return errors.New("http2: response status doesn't permit content-length")
	}
	return nil
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
			return errors.New("http2: data on idle response stream")
		}
		c.receiveConnectionWindow -= flowLength
		if c.receiveConnectionWindow < 0 {
			c.mu.Unlock()
			return errors.New("http2: response connection flow-control window exceeded")
		}
		c.mu.Unlock()
		c.restoreConnectionWindow(flowLength)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(frame.StreamID, xhttp2.ErrCodeStreamClosed)
		})
	}
	c.receiveConnectionWindow -= flowLength
	if c.receiveConnectionWindow < 0 {
		c.mu.Unlock()
		return errors.New("http2: response connection flow-control window exceeded")
	}
	if !stream.responseHeader || stream.remoteClosed {
		c.mu.Unlock()
		c.restoreConnectionWindow(flowLength)
		c.resetStream(frame.StreamID, xhttp2.ErrCodeProtocol, errors.New("http2: data on an invalid response stream"), false)
		return nil
	}
	stream.recvWindow -= flowLength
	if stream.recvWindow < 0 {
		c.mu.Unlock()
		c.restoreConnectionWindow(flowLength)
		c.resetStream(
			frame.StreamID,
			xhttp2.ErrCodeFlowControl,
			errors.New("http2: response stream flow-control window exceeded"),
			false,
		)
		return nil
	}
	if responseHasNoBody(stream) && len(data) != 0 {
		c.mu.Unlock()
		c.restoreConnectionWindow(flowLength)
		c.resetStream(frame.StreamID, xhttp2.ErrCodeProtocol, errors.New("http2: response body isn't permitted"), false)
		return nil
	}
	stream.responseBytes += int64(len(data))
	if stream.expectedResponseBytes >= 0 && stream.responseBytes > stream.expectedResponseBytes {
		c.mu.Unlock()
		c.restoreConnectionWindow(flowLength)
		c.resetStream(frame.StreamID, xhttp2.ErrCodeProtocol, errors.New("http2: response body exceeds content-length"), false)
		return nil
	}
	if !stream.discardResponseBody && stream.maxResponseBodySize > 0 &&
		stream.responseBytes > int64(stream.maxResponseBodySize) {
		c.mu.Unlock()
		c.restoreConnectionWindow(flowLength)
		c.resetStream(frame.StreamID, xhttp2.ErrCodeCancel, fasthttp.ErrBodyTooLarge, false)
		return nil
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

func (c *clientConn) restoreConnectionWindow(amount int64) {
	if amount <= 0 {
		return
	}
	c.mu.Lock()
	c.receiveConnectionWindow += amount
	c.mu.Unlock()
	_ = c.writeControl(func() error {
		return c.framer.WriteWindowUpdate(0, uint32(amount))
	})
}

func (c *clientConn) endResponseLocked(stream *clientStream, responseErr error) {
	if stream.expectedResponseBytes >= 0 && stream.responseBytes != stream.expectedResponseBytes {
		responseErr = errors.New("http2: response body length doesn't match content-length")
	}
	stream.remoteClosed = true
	if stream.responseBody != nil {
		stream.responseBody.closeWithError(responseErr)
	}
	switch {
	case responseErr != nil:
		stream.err = responseErr
		if !stream.isPush {
			c.sendResultLocked(stream, clientResult{err: responseErr})
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
	stream := c.streams[streamID]
	c.pendingConnectionUpdate += int64(amount)
	connectionIncrement := int64(0)
	connectionThreshold := int64(c.config.connectionWindowSize) / 2
	if c.pendingConnectionUpdate >= connectionThreshold {
		connectionIncrement = c.pendingConnectionUpdate
		c.pendingConnectionUpdate = 0
		c.receiveConnectionWindow += connectionIncrement
	}
	streamIncrement := int64(0)
	if stream != nil {
		stream.pendingWindowUpdate += int64(amount)
		streamThreshold := int64(c.config.streamWindowSize) / 2
		if stream.pendingWindowUpdate >= streamThreshold {
			streamIncrement = stream.pendingWindowUpdate
			stream.pendingWindowUpdate = 0
			stream.recvWindow += streamIncrement
		}
	}
	c.mu.Unlock()
	if connectionIncrement == 0 && streamIncrement == 0 {
		return
	}
	_ = c.writeControl(func() error {
		if connectionIncrement != 0 {
			if err := c.framer.WriteWindowUpdate(0, uint32(connectionIncrement)); err != nil {
				return err
			}
		}
		if streamIncrement != 0 {
			return c.framer.WriteWindowUpdate(streamID, uint32(streamIncrement))
		}
		return nil
	})
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
		c.resetStream(streamID, xhttp2.ErrCodeCancel, errClientStreamClosed, false)
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
	defer c.mu.Unlock()
	if frame.StreamID == 0 {
		if c.peerConnectionWindow+int64(frame.Increment) > math.MaxInt32 {
			return fmt.Errorf(
				"http2: client connection send window overflow: current=%d increment=%d",
				c.peerConnectionWindow,
				frame.Increment,
			)
		}
		c.peerConnectionWindow += int64(frame.Increment)
		c.signalLocked()
		return nil
	}
	if stream := c.streams[frame.StreamID]; stream != nil {
		if stream.sendWindow+int64(frame.Increment) > math.MaxInt32 {
			return errors.New("http2: stream send window overflow")
		}
		stream.sendWindow += int64(frame.Increment)
		c.signalLocked()
		return nil
	}
	if _, idle := c.streamStateLocked(frame.StreamID); idle {
		return errors.New("http2: window update on idle stream")
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
		if stream.id > frame.LastStreamID && !stream.responseHeader {
			c.failStreamLocked(stream, goAwayErr, true)
		}
	}
	c.signalLocked()
	closeNow := c.activeStreams == 0
	c.mu.Unlock()
	c.pool.streamAvailable()
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

	c.mu.Lock()
	parent := c.streams[frame.StreamID]
	validID := frame.PromiseID != 0 && frame.PromiseID&1 == 0 && frame.PromiseID > c.lastPromisedStreamID
	if !validID {
		c.mu.Unlock()
		return errors.New("http2: invalid promised stream id")
	}
	c.lastPromisedStreamID = frame.PromiseID
	pushDisabled := c.config.pushHandler == nil
	reject := parent == nil || c.activePushStreams >= c.config.maxConcurrentStreams
	c.mu.Unlock()
	if pushDisabled {
		return xhttp2.ConnectionError(xhttp2.ErrCodeProtocol)
	}
	if reject {
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(frame.PromiseID, xhttp2.ErrCodeCancel)
		})
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
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(promisedID, xhttp2.ErrCodeEnhanceYourCalm)
		})
	}
	if invalid != nil {
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(promisedID, xhttp2.ErrCodeProtocol)
		})
	}

	promised := fasthttp.AcquireRequest()
	if err := populatePromisedRequest(promised, decoded); err != nil {
		fasthttp.ReleaseRequest(promised)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(promisedID, xhttp2.ErrCodeProtocol)
		})
	}
	c.mu.Lock()
	parent := c.streams[parentID]
	if parent == nil {
		c.mu.Unlock()
		fasthttp.ReleaseRequest(promised)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(promisedID, xhttp2.ErrCodeCancel)
		})
	}
	parentCopy := fasthttp.AcquireRequest()
	if parent.parentRequest != nil {
		parent.parentRequest.CopyTo(parentCopy)
	}
	c.mu.Unlock()
	accepted := c.config.pushHandler.Accept(parentCopy, promised)
	fasthttp.ReleaseRequest(parentCopy)
	if !accepted {
		fasthttp.ReleaseRequest(promised)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(promisedID, xhttp2.ErrCodeCancel)
		})
	}

	response := fasthttp.AcquireResponse()
	c.mu.Lock()
	if c.closed || c.activePushStreams >= c.config.maxConcurrentStreams {
		c.mu.Unlock()
		fasthttp.ReleaseRequest(promised)
		fasthttp.ReleaseResponse(response)
		return c.writeControl(func() error {
			return c.framer.WriteRSTStream(promisedID, xhttp2.ErrCodeCancel)
		})
	}
	stream := acquireClientStream()
	*stream = clientStream{
		id:                    promisedID,
		conn:                  c,
		req:                   promised,
		resp:                  response,
		requestStarted:        true,
		localClosed:           true,
		bodyDone:              true,
		sendWindow:            c.peerInitialStreamWindow,
		recvWindow:            int64(c.config.streamWindowSize),
		expectedResponseBytes: -1,
		maxResponseBodySize:   c.hc.MaxResponseBodySize,
		isPush:                true,
		promisedRequest:       promised,
		callerDone:            true,
		poolable:              true,
	}
	c.streams[stream.id] = stream
	c.activeStreams++
	c.activePushStreams++
	c.mu.Unlock()
	return nil
}

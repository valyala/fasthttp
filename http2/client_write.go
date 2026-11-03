package http2

import (
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
)

// requestBodyCloseError is the request body stream's Close error after the
// body went out whole.
type requestBodyCloseError struct {
	err error
}

func (e *requestBodyCloseError) Error() string { return e.err.Error() }
func (e *requestBodyCloseError) Unwrap() error { return e.err }

// writeRequest sends the request with body, the buffered body PrepareRequest
// selected, or reader, the request's body stream as RequestBodyReader bounds
// it; the body stream itself is closed once after the send.
func (c *clientConn) writeRequest(
	stream *clientStream,
	body, defaultContentType []byte,
	reader io.Reader,
	keepOpen bool,
	deadline time.Time,
) error {
	req := stream.req
	if len(req.Header.ConnectProtocol()) != 0 {
		enabled, err := c.waitForExtendedConnect(deadline)
		if err != nil {
			if !errors.Is(err, fasthttp.ErrTimeout) {
				// The connection failed before the request reached it.
				c.cancelStream(stream, xhttp2.ErrCodeCancel, err, failureConnection)
			}
			return err
		}
		if !enabled {
			return fasthttp.ErrProtocolNotSupported
		}
	}
	hasBody := reader != nil || len(body) != 0
	// Declared trailers keep the last DATA from ending the stream; the
	// trailing HEADERS ends it, also when the section turns out empty.
	hasTrailers := !keepOpen && len(req.Header.PeekTrailerKeys()) != 0
	endStream := !keepOpen && !hasBody && !hasTrailers
	if err := c.writeRequestHeaders(stream, endStream, req, defaultContentType, deadline); err != nil {
		if connectionError, ok := errors.AsType[*clientConnectionWriteError](err); ok {
			c.fail(connectionError.err)
		}
		return err
	}
	if endStream {
		c.mu.Lock()
		stream.localClosed = true
		c.maybeFinalizeStreamLocked(stream)
		c.mu.Unlock()
	}
	if keepOpen || endStream {
		return nil
	}
	var err error
	switch {
	case reader != nil:
		err = c.sendRequestStream(stream, reader, !hasTrailers, deadline)
	case hasBody:
		_, err = c.sendData(stream, body, !hasTrailers, deadline, nil)
	}
	if err == nil && hasTrailers {
		err = c.writeRequestTrailers(stream, req, deadline)
	}
	if reader != nil {
		// Closed once, after the trailers, as Request.writeBodyStream closes
		// it; a send error wins over the Close error.
		if closeErr := req.CloseBodyStream(); err == nil && closeErr != nil {
			err = &requestBodyCloseError{err: closeErr}
		}
	}
	if errors.Is(err, errResponseComplete) {
		return c.abandonRequestBody(stream)
	}
	return err
}

// writeRequestTrailers ends the request with its trailer section. An encoded
// block reaches the writer or the connection dies, to keep HPACK in sync.
func (c *clientConn) writeRequestTrailers(stream *clientStream, req *fasthttp.Request, deadline time.Time) error {
	if err := c.lockWrite(deadline, nil); err != nil {
		return err
	}
	c.mu.Lock()
	if err := stream.err; err != nil {
		c.mu.Unlock()
		_ = c.unlockWrite(nil)
		return err
	}
	if stream.remoteClosed {
		c.mu.Unlock()
		_ = c.unlockWrite(nil)
		return errResponseComplete
	}
	maxHeaderListSize := c.peerMaxHeaderListSize
	maxFrameSize := c.peerMaxFrameSize
	c.mu.Unlock()
	block, err := c.encodeRequestTrailers(&req.Header, maxHeaderListSize)
	if err != nil {
		_ = c.unlockWrite(nil)
		return err
	}
	c.mu.Lock()
	stream.localClosed = true
	c.mu.Unlock()
	streamID := stream.id
	first := min(len(block), maxFrameSize)
	err = c.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: block[:first],
		EndStream:     true,
		EndHeaders:    first == len(block),
	})
	if err == nil {
		err = writeContinuationFrames(c.framer, streamID, block[first:], maxFrameSize)
	}
	if err == nil {
		err = c.bufferedWriter.Flush()
	}
	transportErr := err
	if err = c.unlockWrite(err); err != nil {
		c.fail(transportErr)
		return err
	}
	c.mu.Lock()
	c.maybeFinalizeStreamLocked(stream)
	c.mu.Unlock()
	return nil
}

// abandonRequestBody stops a request body once its response is complete. A
// reset, unlike an early END_STREAM, cannot pass for a shorter body.
func (c *clientConn) abandonRequestBody(stream *clientStream) error {
	c.mu.Lock()
	if stream.localClosed {
		c.mu.Unlock()
		return nil
	}
	sendReset := c.resetAllowedLocked(stream)
	stream.localClosed = true
	streamID := stream.id
	c.mu.Unlock()
	// Written before the stream is finalized: an idle drain must not close
	// the connection under the reset.
	var err error
	if sendReset {
		err = c.writeControl(func() error {
			return c.framer.WriteRSTStream(streamID, xhttp2.ErrCodeCancel)
		})
	}
	c.mu.Lock()
	c.maybeFinalizeStreamLocked(stream)
	c.mu.Unlock()
	return err
}

func (c *clientConn) writeRequestHeaders(
	stream *clientStream,
	endStream bool,
	req *fasthttp.Request,
	defaultContentType []byte,
	deadline time.Time,
) error {
	if err := c.lockWrite(deadline, nil); err != nil {
		return err
	}
	c.mu.Lock()
	if err := c.refuseHeadersLocked(stream); err != nil {
		c.mu.Unlock()
		_ = c.unlockWrite(nil)
		return err
	}
	maxHeaderListSize := c.peerMaxHeaderListSize
	c.mu.Unlock()

	// Encoding fails only before it changes the encoder's state.
	block, err := c.encodeRequestHeaders(
		req,
		defaultContentType,
		maxHeaderListSize,
		c.config.enableExtendedConnect,
	)
	if err != nil {
		_ = c.unlockWrite(nil)
		c.mu.Lock()
		c.failStreamLocked(stream, err, failureLocal)
		c.mu.Unlock()
		return err
	}
	return c.commitRequestHeaders(stream, endStream, block)
}

// commitRequestHeaders writes block and releases the write slot. An encoded
// block reaches the writer or the connection dies, to keep HPACK in sync; IDs
// assigned under the write slot keep wire order equal to ID order.
func (c *clientConn) commitRequestHeaders(stream *clientStream, endStream bool, block []byte) error {
	c.mu.Lock()
	if c.closed || c.goAway {
		// No HEADERS follow on this connection, so the block can be dropped.
		err := c.refuseHeadersLocked(stream)
		c.mu.Unlock()
		_ = c.unlockWrite(nil)
		return err
	}
	streamID := c.nextStreamID
	c.nextStreamID += 2
	if c.nextStreamID > math.MaxInt32 {
		c.expired = true
	}
	// A stream canceled during encoding is already finalized without a
	// reset; its HEADERS still go out, followed by RST_STREAM.
	canceled := stream.err != nil
	if !canceled {
		stream.id = streamID
		c.streams[streamID] = stream
		// After registration, not at reservation: the SETTINGS sweep applying
		// INITIAL_WINDOW_SIZE deltas only visits registered streams.
		stream.send.window = c.peerInitialStreamWindow
		stream.requestStarted = true
		stream.localClosed = endStream
	}
	maxFrameSize := c.peerMaxFrameSize
	c.mu.Unlock()

	first := min(len(block), maxFrameSize)
	err := c.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: block[:first],
		EndStream:     endStream,
		EndHeaders:    first == len(block),
	})
	if err == nil {
		err = writeContinuationFrames(c.framer, streamID, block[first:], maxFrameSize)
	}
	if err == nil && canceled {
		err = c.framer.WriteRSTStream(streamID, xhttp2.ErrCodeCancel)
	}
	if err == nil {
		err = c.bufferedWriter.Flush()
	}
	transportErr := err
	if err = c.unlockWrite(err); err != nil {
		c.fail(transportErr)
		return &clientConnectionWriteError{err: err}
	}
	if canceled {
		c.mu.Lock()
		err = stream.err
		c.mu.Unlock()
	}
	return err
}

// refuseHeadersLocked fails stream and returns the error when its HEADERS
// can't be sent on c.
func (c *clientConn) refuseHeadersLocked(stream *clientStream) error {
	switch {
	case stream.err != nil:
		return stream.err
	case c.closed:
		c.failStreamLocked(stream, errClientConnClosed, failureConnection)
		return errClientConnClosed
	case c.goAway || c.nextStreamID > math.MaxInt32:
		// Retried on a fresh connection, like the streams a GOAWAY refuses.
		if !c.goAway {
			c.expired = true
		}
		c.failStreamLocked(stream, ErrConnectionDraining, failureConnection)
		return ErrConnectionDraining
	}
	return nil
}

// requestStreamBuffer is the pooled read scratch for streamed request bodies.
// Pooling is safe because sendData copies every chunk into the connection's
// batch writer before returning, so nothing retains the buffer between reads.
type requestStreamBuffer struct {
	data []byte
}

var requestStreamBufferPool sync.Pool

func acquireRequestStreamBuffer() *requestStreamBuffer {
	if value := requestStreamBufferPool.Get(); value != nil {
		return value.(*requestStreamBuffer) //nolint:forcetypeassert
	}
	return &requestStreamBuffer{data: make([]byte, defaultMaxFrameSize)}
}

// sendRequestStream sends reader as the request body; endStream reports
// whether its end also ends the stream. Only a bare io.EOF completes the body;
// any other error, one wrapping io.EOF included, is the reader's failure.
func (c *clientConn) sendRequestStream(
	stream *clientStream,
	reader io.Reader,
	endStream bool,
	deadline time.Time,
) error {
	bufferOwner := acquireRequestStreamBuffer()
	defer requestStreamBufferPool.Put(bufferOwner)
	buffer := bufferOwner.data
	for {
		c.mu.Lock()
		streamErr, responseComplete := stream.err, stream.remoteClosed
		c.mu.Unlock()
		if streamErr != nil {
			return streamErr
		}
		if responseComplete {
			return errResponseComplete
		}
		n, readErr := reader.Read(buffer)
		if n > 0 {
			end := readErr == io.EOF //nolint:errorlint // exact completion signal
			if _, err := c.sendData(stream, buffer[:n], end && endStream, deadline, nil); err != nil {
				return err
			}
			if end {
				return nil
			}
		}
		if readErr != nil {
			if readErr != io.EOF { //nolint:errorlint // exact completion signal
				return readErr
			}
			if !endStream {
				return nil
			}
			_, err := c.sendData(stream, nil, true, deadline, nil)
			return err
		}
	}
}

// sendData reports the bytes whose frames reached the connection, so a
// failed write can say how much of data went out.
// sendData frames data. A closed cut interrupts it between frames, which a
// StreamConn uses for its deadline and Close; the stream stays usable.
func (c *clientConn) sendData(
	stream *clientStream,
	data []byte,
	endStream bool,
	deadline time.Time,
	cut <-chan struct{},
) (int, error) {
	sent := 0
	for len(data) != 0 || endStream {
		if isClosedChan(cut) {
			return sent, errInterrupted
		}
		if err := c.waitForSendWindow(stream, data, deadline, cut); err != nil {
			return sent, err
		}

		if err := c.lockWrite(deadline, cut); err != nil {
			return sent, err
		}
		framesWritten := 0
		batched := 0
		finished := false
		var streamErr error
		var writeErr error
		for framesWritten < 16 && (len(data) != 0 || endStream) {
			if isClosedChan(cut) {
				streamErr = errInterrupted
				break
			}
			c.mu.Lock()
			if stream.err != nil {
				streamErr = stream.err
				c.mu.Unlock()
				break
			}
			if stream.localClosed {
				streamErr = errClientStreamClosed
				c.mu.Unlock()
				break
			}
			amount := c.reserveDataChunk(&stream.streamFlowState, len(data))
			if len(data) != 0 && amount == 0 {
				c.mu.Unlock()
				break
			}
			last := amount == len(data) && endStream
			if last {
				stream.localClosed = true
			}
			c.mu.Unlock()

			writeErr = c.framer.WriteData(stream.id, last, data[:amount])
			if writeErr != nil {
				break
			}
			data = data[amount:]
			batched += amount
			framesWritten++
			if last {
				finished = true
				break
			}
		}
		if framesWritten != 0 && writeErr == nil {
			writeErr = c.bufferedWriter.Flush()
		}
		transportErr := writeErr
		writeErr = c.unlockWrite(writeErr)

		if writeErr != nil {
			c.fail(transportErr)
			if stream.isOpenStream {
				// A StreamConn reports the transport's error, not the
				// request mapping.
				return sent, transportErr
			}
			return sent, writeErr
		}
		sent += batched
		if streamErr != nil {
			return sent, streamErr
		}
		if finished {
			c.mu.Lock()
			c.maybeFinalizeStreamLocked(stream)
			c.mu.Unlock()
			return sent, nil
		}
	}
	return sent, nil
}

func (c *clientConn) waitForSendWindow(stream *clientStream, data []byte, deadline time.Time, cut <-chan struct{}) error {
	for {
		c.mu.Lock()
		if stream.err != nil {
			err := stream.err
			c.mu.Unlock()
			return err
		}
		if stream.localClosed {
			c.mu.Unlock()
			return errClientStreamClosed
		}
		if stream.remoteClosed && !stream.isOpenStream {
			c.mu.Unlock()
			return errResponseComplete
		}
		if len(data) == 0 || c.send.window > 0 && stream.send.window > 0 {
			c.mu.Unlock()
			return nil
		}
		if isClosedChan(cut) {
			c.mu.Unlock()
			return errInterrupted
		}
		notify := c.notify
		c.waiters++
		c.mu.Unlock()
		err := waitForStreamEvent(notify, deadline, cut)
		c.mu.Lock()
		c.waiters--
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

func (c *clientConn) writeControl(write func() error) error {
	err := c.lockWrite(time.Time{}, nil)
	if err != nil {
		c.fail(err)
		return err
	}
	err = write()
	if err == nil {
		err = c.bufferedWriter.Flush()
	}
	transportErr := err
	err = c.unlockWrite(err)
	if err != nil {
		c.fail(transportErr)
	}
	return err
}

// acquireWrite takes the connection's single write slot. A zero deadline waits
// indefinitely; otherwise the wait is abandoned once the deadline passes so a
// stalled peer cannot make an unrelated stream's timeout ineffective. A closed
// cut abandons it too.
func (c *clientConn) acquireWrite(deadline time.Time, cut <-chan struct{}) error {
	if isClosedChan(cut) {
		return errInterrupted
	}
	var wait time.Duration
	if !deadline.IsZero() {
		if wait = time.Until(deadline); wait <= 0 {
			return fasthttp.ErrTimeout
		}
	}
	slot := c.writeSem
	select {
	case slot <- struct{}{}:
		return nil
	default:
	}
	if deadline.IsZero() {
		select {
		case slot <- struct{}{}:
			return nil
		case <-cut:
			return errInterrupted
		}
	}
	timer := fasthttp.AcquireTimer(wait)
	defer fasthttp.ReleaseTimer(timer)
	select {
	case slot <- struct{}{}:
		return nil
	case <-timer.C:
		return fasthttp.ErrTimeout
	case <-cut:
		return errInterrupted
	}
}

func (c *clientConn) releaseWrite() {
	<-c.writeSem
}

func (c *clientConn) lockWrite(requestDeadline time.Time, cut <-chan struct{}) error {
	if err := c.acquireWrite(requestDeadline, cut); err != nil {
		return err
	}
	// A producer parked on a full queue cannot see its stream die, so it
	// needs a floor; a request deadline is not one, since abandoning a
	// partly queued frame fails the whole connection.
	var producerDeadline time.Time
	if c.config.writeByteTimeout > 0 {
		producerDeadline = time.Now().Add(c.config.writeByteTimeout)
	}
	c.writer.setDeadline(producerDeadline)
	return nil
}

func (c *clientConn) unlockWrite(writeErr error) error {
	c.writer.setDeadline(time.Time{})
	c.releaseWrite()
	if writeErr != nil && isTimeout(writeErr) {
		return fasthttp.ErrTimeout
	}
	return writeErr
}

func waitForStreamEvent(notify <-chan struct{}, deadline time.Time, cut <-chan struct{}) error {
	if deadline.IsZero() {
		select {
		case <-notify:
			return nil
		case <-cut:
			return errInterrupted
		}
	}
	timer := fasthttp.AcquireTimer(time.Until(deadline))
	defer fasthttp.ReleaseTimer(timer)
	select {
	case <-notify:
		return nil
	case <-timer.C:
		return fasthttp.ErrTimeout
	case <-cut:
		return errInterrupted
	}
}

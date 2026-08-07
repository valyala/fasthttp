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

func (c *clientConn) writeRequest(stream *clientStream, keepOpen bool, deadline time.Time) error {
	req := stream.req
	if len(req.Header.ConnectProtocol()) != 0 {
		enabled, err := c.waitForExtendedConnect(deadline)
		if err != nil {
			return err
		}
		if !enabled {
			return fasthttp.ErrProtocolNotSupported
		}
	}
	var body []byte
	var reader io.Reader
	if req.IsBodyStream() {
		reader = req.BodyStream()
	} else {
		body = req.Body()
	}
	hasBody := reader != nil || len(body) != 0
	if declared := requestContentLength(&req.Header); declared >= 0 && !req.IsBodyStream() && declared != int64(len(body)) {
		return errors.New("http2: request body length doesn't match content-length")
	}
	if err := c.writeRequestHeaders(stream, !keepOpen && !hasBody, req, deadline); err != nil {
		var connectionError *clientConnectionWriteError
		if errors.As(err, &connectionError) {
			c.fail(connectionError.err)
		}
		return err
	}
	if !keepOpen && !hasBody {
		c.mu.Lock()
		stream.localClosed = true
		c.maybeFinalizeStreamLocked(stream)
		c.mu.Unlock()
	}
	if keepOpen || !hasBody {
		return nil
	}
	if reader == nil {
		return c.sendData(stream, body, true, deadline)
	}
	defer req.CloseBodyStream() //nolint:errcheck
	return c.sendRequestStream(stream, reader, requestContentLength(&req.Header), deadline)
}

func (c *clientConn) writeRequestHeaders(
	stream *clientStream,
	endStream bool,
	req *fasthttp.Request,
	deadline time.Time,
) error {
	if err := c.lockWrite(deadline); err != nil {
		return err
	}
	// Assigning the ID here, under the write slot and just before the HEADERS
	// reach the writer, is what makes wire order equal ID order.
	c.mu.Lock()
	if stream.err != nil {
		streamErr := stream.err
		c.mu.Unlock()
		if unlockErr := c.unlockWrite(nil); unlockErr != nil {
			c.fail(unlockErr)
			return &clientConnectionWriteError{err: unlockErr}
		}
		return streamErr
	}
	if c.closed {
		c.mu.Unlock()
		if unlockErr := c.unlockWrite(nil); unlockErr != nil {
			c.fail(unlockErr)
			return &clientConnectionWriteError{err: unlockErr}
		}
		return errClientConnClosed
	}
	if c.goAway || c.nextStreamID > math.MaxInt32 {
		// The peer is draining or the ID space is exhausted. Fail the stream
		// as retryable exactly like the GOAWAY sweep does for registered
		// streams, so the pool retries it on a fresh connection.
		c.failStreamLocked(stream, ErrConnectionDraining, true)
		c.mu.Unlock()
		if unlockErr := c.unlockWrite(nil); unlockErr != nil {
			c.fail(unlockErr)
			return &clientConnectionWriteError{err: unlockErr}
		}
		return ErrConnectionDraining
	}
	stream.id = c.nextStreamID
	c.nextStreamID += 2
	c.streams[stream.id] = stream
	// After registration, not at reservation: the SETTINGS sweep applying
	// INITIAL_WINDOW_SIZE deltas only visits registered streams.
	stream.send.window = c.peerInitialStreamWindow
	maxHeaderListSize := c.peerMaxHeaderListSize
	maxFrameSize := c.peerMaxFrameSize
	c.mu.Unlock()

	block, err := c.encodeRequestHeaders(
		req,
		maxHeaderListSize,
		c.config.enableExtendedConnect,
	)
	if err != nil {
		// Nothing reached the wire or the encoder, so this is the stream's
		// problem; its ID is abandoned unused, which RFC 9113 §5.1.1 permits.
		unlockErr := c.unlockWrite(nil)
		c.mu.Lock()
		c.failStreamLocked(stream, err, false)
		c.mu.Unlock()
		if unlockErr != nil {
			c.fail(unlockErr)
			return &clientConnectionWriteError{err: unlockErr}
		}
		return err
	}
	first := min(len(block), maxFrameSize)
	err = c.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      stream.id,
		BlockFragment: block[:first],
		EndStream:     endStream,
		EndHeaders:    first == len(block),
	})
	if err == nil {
		err = writeContinuationFrames(c.framer, stream.id, block[first:], maxFrameSize)
	}
	if err == nil {
		err = c.bufferedWriter.Flush()
	}
	if err != nil {
		err = c.unlockWrite(err)
		c.fail(err)
		return &clientConnectionWriteError{err: err}
	}
	c.mu.Lock()
	if c.streams[stream.id] == stream {
		stream.requestStarted = true
	}
	c.maybeFinalizeStreamLocked(stream)
	c.mu.Unlock()
	if err := c.unlockWrite(nil); err != nil {
		c.fail(err)
		return &clientConnectionWriteError{err: err}
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

func (c *clientConn) sendRequestStream(
	stream *clientStream,
	reader io.Reader,
	expected int64,
	deadline time.Time,
) error {
	bufferOwner := acquireRequestStreamBuffer()
	defer requestStreamBufferPool.Put(bufferOwner)
	buffer := bufferOwner.data
	var sent int64
	for {
		c.mu.Lock()
		responseStarted := stream.responseHeader || stream.remoteClosed
		c.mu.Unlock()
		if responseStarted {
			return c.sendData(stream, nil, true, deadline)
		}
		n, readErr := reader.Read(buffer)
		if n > 0 {
			sent += int64(n)
			if expected >= 0 && sent > expected {
				return errors.New("http2: request body exceeds content-length")
			}
			end := errors.Is(readErr, io.EOF)
			if end && expected >= 0 && sent != expected {
				return errors.New("http2: request body length doesn't match content-length")
			}
			if err := c.sendData(stream, buffer[:n], end, deadline); err != nil {
				return err
			}
			if end {
				return nil
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			if expected >= 0 && sent != expected {
				return errors.New("http2: request body length doesn't match content-length")
			}
			return c.sendData(stream, nil, true, deadline)
		}
	}
}

func (c *clientConn) sendData(stream *clientStream, data []byte, endStream bool, deadline time.Time) error {
	for len(data) != 0 || endStream {
		if err := c.waitForSendWindow(stream, data, deadline); err != nil {
			return err
		}

		if err := c.lockWrite(deadline); err != nil {
			return err
		}
		framesWritten := 0
		finished := false
		var streamErr error
		var writeErr error
		for framesWritten < 16 && (len(data) != 0 || endStream) {
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
			framesWritten++
			if last {
				finished = true
				break
			}
		}
		if framesWritten != 0 && writeErr == nil {
			writeErr = c.bufferedWriter.Flush()
		}
		writeErr = c.unlockWrite(writeErr)

		if writeErr != nil {
			c.fail(writeErr)
			return writeErr
		}
		if streamErr != nil {
			return streamErr
		}
		if finished {
			c.mu.Lock()
			c.maybeFinalizeStreamLocked(stream)
			c.mu.Unlock()
			return nil
		}
	}
	return nil
}

func (c *clientConn) waitForSendWindow(stream *clientStream, data []byte, deadline time.Time) error {
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
		if len(data) == 0 || c.send.window > 0 && stream.send.window > 0 {
			c.mu.Unlock()
			return nil
		}
		notify := c.notify
		c.waiters++
		c.mu.Unlock()
		err := waitForStreamEvent(notify, deadline)
		c.mu.Lock()
		c.waiters--
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

func (c *clientConn) writeControl(write func() error) error {
	err := c.lockWrite(time.Time{})
	if err != nil {
		c.fail(err)
		return err
	}
	err = write()
	if err == nil {
		err = c.bufferedWriter.Flush()
	}
	err = c.unlockWrite(err)
	if err != nil {
		c.fail(err)
	}
	return err
}

// acquireWrite takes the connection's single write slot. A zero deadline waits
// indefinitely; otherwise the wait is abandoned once the deadline passes so a
// stalled peer cannot make an unrelated stream's timeout ineffective.
func (c *clientConn) acquireWrite(deadline time.Time) error {
	slot := c.writeSem
	select {
	case slot <- struct{}{}:
		return nil
	default:
	}
	if deadline.IsZero() {
		slot <- struct{}{}
		return nil
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return fasthttp.ErrTimeout
	}
	timer := fasthttp.AcquireTimer(wait)
	defer fasthttp.ReleaseTimer(timer)
	select {
	case slot <- struct{}{}:
		return nil
	case <-timer.C:
		return fasthttp.ErrTimeout
	}
}

func (c *clientConn) releaseWrite() {
	<-c.writeSem
}

func (c *clientConn) lockWrite(requestDeadline time.Time) error {
	if err := c.acquireWrite(requestDeadline); err != nil {
		return err
	}
	// A producer parked on a full queue cannot see its stream die, so even a
	// deadline-less request needs a floor.
	producerDeadline := requestDeadline
	if c.config.writeByteTimeout > 0 {
		floor := time.Now().Add(c.config.writeByteTimeout)
		if producerDeadline.IsZero() || floor.Before(producerDeadline) {
			producerDeadline = floor
		}
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

func requestContentLength(header *fasthttp.RequestHeader) int64 {
	if len(header.Peek(fasthttp.HeaderContentLength)) == 0 {
		return -1
	}
	return int64(header.ContentLength())
}

func waitForStreamEvent(notify <-chan struct{}, deadline time.Time) error {
	if deadline.IsZero() {
		<-notify
		return nil
	}
	timer := fasthttp.AcquireTimer(time.Until(deadline))
	defer fasthttp.ReleaseTimer(timer)
	select {
	case <-notify:
		return nil
	case <-timer.C:
		return fasthttp.ErrTimeout
	}
}

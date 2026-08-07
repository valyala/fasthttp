package http2

import (
	"bufio"
	"fmt"
	"io"

	xhttp2 "golang.org/x/net/http2"
)

// headersFrame is a HEADERS frame parsed into reusable storage, because x/net
// allocates a *HeadersFrame per header block -- its frame cache holds DATA
// frames only.
type headersFrame struct {
	streamID    uint32
	flags       xhttp2.Flags
	streamDep   uint32
	fragment    []byte
	payload     []byte
	hasPriority bool
}

func (f *headersFrame) HeaderBlockFragment() []byte { return f.fragment }
func (f *headersFrame) HeadersEnded() bool          { return f.flags.Has(xhttp2.FlagHeadersEndHeaders) }
func (f *headersFrame) StreamEnded() bool           { return f.flags.Has(xhttp2.FlagHeadersEndStream) }

// frameReader takes HEADERS payloads itself and leaves every other type to
// x/net. conn must be the reader the Framer was built with, so that a payload
// ReadFrameHeader left behind is still waiting here.
type frameReader struct {
	framer  *xhttp2.Framer
	conn    *bufio.Reader
	headers headersFrame
}

type frameReadError struct {
	frameType xhttp2.FrameType
	err       error
}

func (e *frameReadError) Error() string {
	return fmt.Sprintf("reading %s frame: %v", e.frameType, e.err)
}
func (e *frameReadError) Unwrap() error { return e.err }

func newFrameReader(framer *xhttp2.Framer, conn *bufio.Reader) *frameReader {
	return &frameReader{framer: framer, conn: conn}
}

// completeFrameBuffered reports whether the next full frame is already
// buffered, so reading it cannot block on the socket.
func (r *frameReader) completeFrameBuffered() bool {
	buffered := r.conn.Buffered()
	if buffered < 9 {
		return false
	}
	header, err := r.conn.Peek(3)
	if err != nil {
		return false
	}
	length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
	return buffered >= 9+length
}

// readFrame returns a *headersFrame or an x/net frame, valid until the next
// call.
func (r *frameReader) readFrame() (any, error) {
	header, err := r.framer.ReadFrameHeader()
	if err != nil {
		return nil, err
	}
	if header.Type != xhttp2.FrameHeaders {
		frame, readErr := r.framer.ReadFrameForHeader(header)
		if readErr != nil {
			return nil, &frameReadError{frameType: header.Type, err: readErr}
		}
		return frame, nil
	}
	if err := r.readHeadersPayload(header); err != nil {
		return nil, err
	}
	return &r.headers, nil
}

// readHeadersPayload mirrors parseHeadersFrame, error codes included.
func (r *frameReader) readHeadersPayload(header xhttp2.FrameHeader) error {
	if header.StreamID == 0 {
		return xhttp2.ConnectionError(xhttp2.ErrCodeProtocol)
	}
	frame := &r.headers
	if cap(frame.payload) < int(header.Length) {
		frame.payload = make([]byte, header.Length)
	}
	payload := frame.payload[:header.Length]
	if _, err := io.ReadFull(r.conn, payload); err != nil {
		return err
	}

	frame.streamID = header.StreamID
	frame.flags = header.Flags
	frame.hasPriority = header.Flags.Has(xhttp2.FlagHeadersPriority)
	frame.streamDep = 0

	padLength := 0
	if header.Flags.Has(xhttp2.FlagHeadersPadded) {
		if len(payload) == 0 {
			return xhttp2.ConnectionError(xhttp2.ErrCodeFrameSize)
		}
		padLength = int(payload[0])
		payload = payload[1:]
	}
	if frame.hasPriority {
		if len(payload) < 5 {
			return xhttp2.ConnectionError(xhttp2.ErrCodeFrameSize)
		}
		frame.streamDep = (uint32(payload[0])<<24 | uint32(payload[1])<<16 |
			uint32(payload[2])<<8 | uint32(payload[3])) & 0x7fffffff
		payload = payload[5:]
	}
	if len(payload) < padLength {
		// The peer's encoder may already have mutated its dynamic table for
		// bytes hidden behind the invalid padding. Continuing without decoding
		// them would desynchronize the connection-scoped HPACK state.
		return xhttp2.ConnectionError(xhttp2.ErrCodeCompression)
	}
	frame.fragment = payload[:len(payload)-padLength]
	return nil
}

var _ headerBlockFrame = (*headersFrame)(nil)

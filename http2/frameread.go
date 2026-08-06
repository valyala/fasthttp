package http2

import (
	"io"

	xhttp2 "golang.org/x/net/http2"
)

// headersFrame is a HEADERS frame parsed into connection-owned storage.
//
// x/net's Framer allocates a *HeadersFrame for every header block it parses --
// SetReuseFrames caches DATA frames only -- which is one allocation per request
// on each side of a connection. Framer.ReadFrameHeader already validates the
// frame length and the HEADERS/CONTINUATION order, and it leaves the payload
// unread on the reader we own, so readFrame below takes the payload for this
// one frame type and leaves every other type to x/net.
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

// frameReader reads frames from conn, keeping one reusable HEADERS frame.
// The reader must be the same one the Framer was built with, so that a payload
// skipped by ReadFrameHeader is still waiting here.
type frameReader struct {
	framer  *xhttp2.Framer
	conn    io.Reader
	headers headersFrame
}

func newFrameReader(framer *xhttp2.Framer, conn io.Reader) *frameReader {
	return &frameReader{framer: framer, conn: conn}
}

// readFrame returns either a *headersFrame owned by this reader, or a frame
// owned by the Framer. Both stay valid only until the next call.
func (r *frameReader) readFrame() (any, error) {
	header, err := r.framer.ReadFrameHeader()
	if err != nil {
		return nil, err
	}
	if header.Type != xhttp2.FrameHeaders {
		return r.framer.ReadFrameForHeader(header)
	}
	if err := r.readHeadersPayload(header); err != nil {
		return nil, err
	}
	return &r.headers, nil
}

// readHeadersPayload mirrors x/net's parseHeadersFrame, including its error
// codes, over a buffer that is reused across frames.
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
		return xhttp2.StreamError{StreamID: header.StreamID, Code: xhttp2.ErrCodeProtocol}
	}
	frame.fragment = payload[:len(payload)-padLength]
	return nil
}

var _ headerBlockFrame = (*headersFrame)(nil)

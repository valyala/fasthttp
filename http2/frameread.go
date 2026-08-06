package http2

import (
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
	conn    io.Reader
	headers headersFrame
}

func newFrameReader(framer *xhttp2.Framer, conn io.Reader) *frameReader {
	return &frameReader{framer: framer, conn: conn}
}

// readFrame returns a *headersFrame or an x/net frame, valid until the next
// call.
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
		return xhttp2.StreamError{StreamID: header.StreamID, Code: xhttp2.ErrCodeProtocol}
	}
	frame.fragment = payload[:len(payload)-padLength]
	return nil
}

var _ headerBlockFrame = (*headersFrame)(nil)

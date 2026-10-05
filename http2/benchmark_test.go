package http2

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func BenchmarkHeaderCodec(b *testing.B) {
	wire := benchmarkRequestHeaderFrame(b)
	b.Run("private-codec", func(b *testing.B) {
		framer := xhttp2.NewFramer(io.Discard, &cyclingReader{data: wire})
		codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			frame, err := framer.ReadFrame()
			if err != nil {
				b.Fatal(err)
			}
			headers := frame.(*xhttp2.HeadersFrame) //nolint:forcetypeassert
			fieldStorage := acquireIncomingHeaderFields(8)
			fields, truncated, invalid, err := codec.decode(
				framer,
				headers.StreamID,
				headers,
				fieldStorage.fields,
			)
			event := incomingFrame{fields: fields, fieldStorage: fieldStorage}
			releaseIncomingFrame(&event)
			if err != nil || truncated || invalid != nil || len(fields) != 4 {
				b.Fatalf("decode = (%d fields, truncated=%v, invalid=%v, err=%v)", len(fields), truncated, invalid, err)
			}
		}
	})
	b.Run("x-net-meta-headers", func(b *testing.B) {
		framer := xhttp2.NewFramer(io.Discard, &cyclingReader{data: wire})
		decoder := hpack.NewDecoder(defaultHeaderTableSize, nil)
		framer.ReadMetaHeaders = decoder
		framer.MaxHeaderListSize = 64 << 10
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			frame, err := framer.ReadFrame()
			if err != nil {
				b.Fatal(err)
			}
			headers := frame.(*xhttp2.MetaHeadersFrame) //nolint:forcetypeassert
			if headers.Truncated || len(headers.Fields) != 4 {
				b.Fatalf("decode = (%d fields, truncated=%v)", len(headers.Fields), headers.Truncated)
			}
		}
	})
}

func benchmarkRequestHeaderFrame(b *testing.B) []byte {
	b.Helper()
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "example.com", Sensitive: true},
		{Name: ":path", Value: "/"},
	} {
		if err := encoder.WriteField(field); err != nil {
			b.Fatal(err)
		}
	}
	var wire bytes.Buffer
	framer := xhttp2.NewFramer(&wire, nil)
	if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: block.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	}); err != nil {
		b.Fatal(err)
	}
	return bytes.Clone(wire.Bytes())
}

type cyclingReader struct {
	data   []byte
	offset int
}

func (r *cyclingReader) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		amount := copy(destination[written:], r.data[r.offset:])
		written += amount
		r.offset += amount
		if r.offset == len(r.data) {
			r.offset = 0
		}
	}
	return written, nil
}

func BenchmarkRequestBodyTinyChunks(b *testing.B) {
	for _, size := range []int{128 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes-%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				body := newRequestBody(nil)
				for range size {
					if err := body.writeOwned([]byte{'x'}, nil); err != nil {
						b.Fatal(err)
					}
				}
				body.discardWithError(errStreamClosed)
			}
		})
	}
}

func BenchmarkApplyDistinctRequestTrailers(b *testing.B) {
	const fieldCount = 1724
	fields := make([]hpack.HeaderField, fieldCount)
	for i := range fields {
		fields[i] = hpack.HeaderField{Name: fmt.Sprintf("t%05x", i)}
	}
	b.ReportAllocs()
	for b.Loop() {
		var validation, request fasthttp.RequestHeader
		if err := applyRequestTrailers(&validation, fields); err != nil {
			b.Fatal(err)
		}
		if err := applyRequestTrailers(&request, fields); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRepeatedInitialWindowSettings(b *testing.B) {
	settings := make([]xhttp2.Setting, 2730)
	for i := range settings {
		settings[i] = xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 65535}
	}
	conn := &serverConn{
		config:        serverConfig{maxEncoderTableSize: defaultHeaderTableSize},
		streams:       make(map[uint32]*serverStream, 250),
		connFlowState: connFlowState{peerInitialStreamWindow: 65535},
	}
	for id := uint32(1); id <= 499; id += 2 {
		conn.streams[id] = &serverStream{id: id, streamFlowState: streamFlowState{send: sendWindow{window: 65535}}}
	}
	conn.initHeaderEncoder(defaultHeaderTableSize)
	b.ReportAllocs()
	for b.Loop() {
		if err := conn.applySettings(settings); err != nil {
			b.Fatal(err)
		}
	}
}

package http2

import (
	"bytes"
	"io"
	"testing"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func FuzzPriority(f *testing.F) {
	for _, seed := range []string{"", "u=0", "u=7, i", "i=?1, u=3", "u=8", "u=-1", "x=extension"} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, value string) {
		_, _ = parsePriority(value)
	})
}

func FuzzHTTP2ContentLength(f *testing.F) {
	for _, seed := range []string{"0", "1", "18446744073709551615", "-1", "+1", "1, 1", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, value string) {
		_, _ = parseHTTP2ContentLength(value)
	})
}

func FuzzHPACKRequestHeaders(f *testing.F) {
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "example.com"},
		{Name: ":path", Value: "/"},
	} {
		if err := encoder.WriteField(field); err != nil {
			f.Fatal(err)
		}
	}
	f.Add(encoded.Bytes())
	f.Fuzz(func(_ *testing.T, block []byte) {
		codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
		fieldStorage := acquireIncomingHeaderFields(8)
		fields, truncated, invalid, err := codec.decodeComplete(block, fieldStorage.fields)
		event := incomingFrame{fields: fields, fieldStorage: fieldStorage}
		defer releaseIncomingFrame(&event)
		if err != nil || truncated || invalid != nil {
			return
		}
		ctx := &fasthttp.RequestCtx{}
		_, _ = populateRequest(ctx, &fasthttp.Server{}, fields, true)
	})
}

func FuzzFrameSequence(f *testing.F) {
	var seed bytes.Buffer
	framer := xhttp2.NewFramer(&seed, nil)
	if err := framer.WriteSettings(); err != nil {
		f.Fatal(err)
	}
	if err := framer.WritePing(false, [8]byte{1, 2, 3}); err != nil {
		f.Fatal(err)
	}
	f.Add(seed.Bytes())
	f.Fuzz(func(_ *testing.T, data []byte) {
		framer := xhttp2.NewFramer(io.Discard, bytes.NewReader(data))
		framer.SetMaxReadFrameSize(1 << 20)
		for range 32 {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			event := incomingFrameFromWire(frame)
			releaseIncomingFrame(&event)
		}
	})
}

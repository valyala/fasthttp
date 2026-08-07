package http2

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"testing"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func FuzzPriority(f *testing.F) {
	for _, seed := range []string{"", "u=0", "u=7, i", "i=?1, u=3", "u=8", "u=-1", "x=extension"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		parsed, err := parsePriority(value)
		if err != nil {
			return
		}
		if parsed.urgency > 7 {
			t.Fatalf("parsed urgency = %d", parsed.urgency)
		}
		canonical := fmt.Sprintf("u=%d, i=?0", parsed.urgency)
		if parsed.incremental {
			canonical = fmt.Sprintf("u=%d, i=?1", parsed.urgency)
		}
		roundTrip, err := parsePriority(canonical)
		if err != nil || roundTrip != parsed {
			t.Fatalf("canonical priority %q round-trip = (%+v, %v), want %+v", canonical, roundTrip, err, parsed)
		}
	})
}

func FuzzHTTP2ContentLength(f *testing.F) {
	for _, seed := range []string{"0", "1", "18446744073709551615", "-1", "+1", "1, 1", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got, gotErr := parseHTTP2ContentLength(value)
		want, parseErr := strconv.ParseInt(value, 10, 64)
		valid := value != "" && parseErr == nil && want >= 0 && uint64(want) <= uint64(^uint(0)>>1)
		for i := range len(value) {
			valid = valid && value[i] >= '0' && value[i] <= '9'
		}
		if (gotErr == nil) != valid {
			t.Fatalf("parseHTTP2ContentLength(%q) = (%d, %v), valid=%v", value, got, gotErr, valid)
		}
		if valid && got != want {
			t.Fatalf("parseHTTP2ContentLength(%q) = %d, want %d", value, got, want)
		}
	})
}

func FuzzHPACKHeaders(f *testing.F) {
	f.Add([]byte("value"))
	f.Add([]byte{0, 1, 2, 255})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 512 {
			data = data[:512]
		}
		value := hex.EncodeToString(data)
		var encoded bytes.Buffer
		encoder := hpack.NewEncoder(&encoded)
		if err := encoder.WriteField(hpack.HeaderField{Name: "x-probe", Value: value}); err != nil {
			t.Fatal(err)
		}
		if err := encoder.WriteField(hpack.HeaderField{Name: ":path", Value: "/after-regular"}); err != nil {
			t.Fatal(err)
		}
		invalidBlock := bytes.Clone(encoded.Bytes())
		encoded.Reset()
		if err := encoder.WriteField(hpack.HeaderField{Name: "x-probe", Value: value}); err != nil {
			t.Fatal(err)
		}
		probeBlock := bytes.Clone(encoded.Bytes())

		codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
		_, truncated, invalid, err := codec.decodeComplete(invalidBlock, nil)
		if err != nil || truncated || invalid == nil {
			t.Fatalf("invalid block = (truncated=%v, invalid=%v, err=%v)", truncated, invalid, err)
		}
		fields, truncated, invalid, err := codec.decodeComplete(probeBlock, nil)
		if err != nil || truncated || invalid != nil || len(fields) != 1 || fields[0].Value != value {
			t.Fatalf("probe block = (%+v, truncated=%v, invalid=%v, err=%v)", fields, truncated, invalid, err)
		}
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
	f.Fuzz(func(t *testing.T, data []byte) {
		framer := xhttp2.NewFramer(io.Discard, bytes.NewReader(data))
		framer.SetMaxReadFrameSize(1 << 20)
		conn := &serverConn{
			server:              &fasthttp.Server{},
			config:              serverConfig{maxConcurrentStreams: 8, maxRapidResetsPerSecond: 1000},
			framer:              xhttp2.NewFramer(io.Discard, nil),
			encoder:             hpack.NewEncoder(io.Discard),
			streams:             make(map[uint32]*serverStream),
			priorityUpdates:     make(map[uint32]priority),
			closedClientStreams: make(map[uint32]bool),
			connFlowState:       connFlowState{peerConnectionWindow: 65535, peerInitialStreamWindow: 65535, receiveConnectionWindow: defaultConnectionWindowSize},
		}
		for range 32 {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			event := incomingFrameFromWire(frame)
			switch event.kind {
			case incomingFrameHeaders:
				releaseIncomingFrame(&event)
				return
			default:
				if err := conn.processFrame(&event); err != nil {
					releaseIncomingFrame(&event)
					return
				}
			}
			if conn.peerConnectionWindow < 0 || conn.peerConnectionWindow > 1<<31-1 {
				t.Fatalf("peer connection window = %d", conn.peerConnectionWindow)
			}
			if len(conn.priorityUpdates) > int(conn.config.maxConcurrentStreams)*4 {
				t.Fatalf("pending priority updates = %d", len(conn.priorityUpdates))
			}
			releaseIncomingFrame(&event)
		}
	})
}

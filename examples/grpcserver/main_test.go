package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp/http2"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// unaryResponse is what the peer saw of one Ping call.
type unaryResponse struct {
	status     string
	grpcStatus string
	body       []byte
}

// callPing sends a Ping over a fresh prior-knowledge connection: the request
// headers, then body as one DATA frame when it isn't nil.
func callPing(t *testing.T, addr string, body []byte) unaryResponse {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, xhttp2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	framer := xhttp2.NewFramer(conn, conn)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range [][2]string{
		{":method", "POST"},
		{":scheme", "http"},
		{":authority", "localhost"},
		{":path", "/echo.Echo/Ping"},
		{"content-type", "application/grpc"},
		{"te", "trailers"},
	} {
		if err := encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID: 1, BlockFragment: block.Bytes(), EndStream: body == nil, EndHeaders: true,
	}); err != nil {
		t.Fatal(err)
	}
	if body != nil {
		if err := framer.WriteData(1, true, body); err != nil {
			t.Fatal(err)
		}
	}
	var response unaryResponse
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("reading the response: %v (so far %+v)", err, response)
		}
		switch frame := frame.(type) {
		case *xhttp2.MetaHeadersFrame:
			if frame.StreamID != 1 {
				continue
			}
			if status := frame.PseudoValue("status"); status != "" {
				response.status = status
			}
			for _, field := range frame.Fields {
				if field.Name == "grpc-status" {
					response.grpcStatus = field.Value
				}
			}
			if frame.StreamEnded() {
				return response
			}
		case *xhttp2.DataFrame:
			if frame.StreamID == 1 {
				response.body = append(response.body, frame.Data()...)
				if frame.StreamEnded() {
					return response
				}
			}
		case *xhttp2.RSTStreamFrame:
			t.Fatalf("stream reset: %v (so far %+v)", frame.ErrCode, response)
		case *xhttp2.GoAwayFrame:
			t.Fatalf("GOAWAY(%v) (so far %+v)", frame.ErrCode, response)
		}
	}
}

// The unary call reads exactly one uncompressed message: no body, the
// compression flag, trailing bytes and a second message are gRPC errors
// without a body (Trailers-Only), a valid message is echoed.
func TestPingValidatesTheUnaryMessage(t *testing.T) {
	server := newServer()
	if err := http2.ConfigureServer(server, http2.ServerConfig{}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(ctx)
		if err := <-served; err != nil {
			t.Errorf("Serve() error: %v", err)
		}
	})
	message := frame([]byte("hello"))
	compressed := append([]byte(nil), message...)
	compressed[0] = 1
	for _, tc := range []struct {
		name string
		body []byte
		want unaryResponse
	}{
		{name: "empty-request", body: nil, want: unaryResponse{status: "200", grpcStatus: "13"}},
		{name: "compression-flag", body: compressed, want: unaryResponse{status: "200", grpcStatus: "12"}},
		{name: "trailing-bytes", body: append(append([]byte(nil), message...), 'x'), want: unaryResponse{status: "200", grpcStatus: "13"}},
		{name: "second-message", body: append(append([]byte(nil), message...), message...), want: unaryResponse{status: "200", grpcStatus: "13"}},
		{name: "valid", body: message, want: unaryResponse{status: "200", grpcStatus: "0", body: message}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := callPing(t, listener.Addr().String(), tc.body)
			if got.status != tc.want.status || got.grpcStatus != tc.want.grpcStatus || !bytes.Equal(got.body, tc.want.body) {
				t.Fatalf("response = %+v, want %+v", got, tc.want)
			}
		})
	}
}

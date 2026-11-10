// Command grpcserver answers gRPC calls with plain fasthttp handlers; HTTP/2
// with trailers is all gRPC needs from the transport.
//
// The unary echo works with any gRPC client, for example:
//
//	go run ./examples/grpcserver -addr 127.0.0.1:50051
//	buf curl --schema ./examples/grpcserver --protocol grpc --http2-prior-knowledge \
//	    -d '{"payload":"aGVsbG8="}' http://127.0.0.1:50051/echo.Echo/Ping
//
// Chat echoes a bidirectional stream: each request message is answered before
// the next is read.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/http2"
)

// maxMessageSize bounds the length prefix, which the peer controls.
const maxMessageSize = 4 << 20

// errCompressedMessage reports a message with the compression flag set, which
// this example doesn't implement.
var errCompressedMessage = errors.New("grpc: compressed messages aren't supported")

// frame wraps one message in gRPC's length-prefixed framing.
func frame(message []byte) []byte {
	framed := make([]byte, 5+len(message))
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(message))) // #nosec G115
	copy(framed[5:], message)
	return framed
}

func readFrame(r io.Reader) ([]byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	if header[0] != 0 {
		return nil, errCompressedMessage
	}
	size := binary.BigEndian.Uint32(header[1:5])
	if size > maxMessageSize {
		return nil, fmt.Errorf("grpc: message of %d bytes exceeds %d", size, maxMessageSize)
	}
	message := make([]byte, size)
	if _, err := io.ReadFull(r, message); err != nil {
		return nil, err
	}
	return message, nil
}

// beginGRPC declares the status trailer; bodyless errors instead set
// grpc-status as a plain header (Trailers-Only).
func beginGRPC(ctx *fasthttp.RequestCtx) bool {
	ctx.SetContentType("application/grpc")
	if err := ctx.Response.Header.AddTrailer("Grpc-Status"); err != nil {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return false
	}
	ctx.Response.Header.Set("Grpc-Status", "0")
	return true
}

// failGRPC answers without a body: grpc-status as a plain header
// (Trailers-Only).
func failGRPC(ctx *fasthttp.RequestCtx, status string) {
	ctx.SetContentType("application/grpc")
	ctx.Response.Header.Set("Grpc-Status", status)
}

// ping echoes the one message of a unary call, read from the request body
// stream; no body, a second message or trailing bytes is an error.
func ping(ctx *fasthttp.RequestCtx) {
	requests := ctx.RequestBodyStream()
	if requests == nil {
		failGRPC(ctx, "13")
		return
	}
	message, err := readFrame(requests)
	switch {
	case errors.Is(err, errCompressedMessage):
		failGRPC(ctx, "12")
		return
	case err != nil:
		failGRPC(ctx, "13")
		return
	}
	if _, err := requests.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		failGRPC(ctx, "13")
		return
	}
	if !beginGRPC(ctx) {
		return
	}
	ctx.SetBody(frame(message))
}

func chat(ctx *fasthttp.RequestCtx) {
	if !beginGRPC(ctx) {
		return
	}
	requests := ctx.RequestBodyStream()
	ctx.Response.SetBodyStreamWriter(func(w *bufio.Writer) {
		for {
			message, err := readFrame(requests)
			if err != nil {
				return
			}
			if _, err := w.Write(frame(message)); err != nil {
				return
			}
			// Trailers encode after this function returns, so the loop could
			// still change Grpc-Status here.
			if err := w.Flush(); err != nil {
				return
			}
		}
	})
}

func handle(ctx *fasthttp.RequestCtx) {
	switch string(ctx.Path()) {
	case "/echo.Echo/Ping":
		ping(ctx)
	case "/echo.Echo/Chat":
		chat(ctx)
	default:
		ctx.SetStatusCode(fasthttp.StatusNotFound)
	}
}

// newServer is the example's server: gRPC needs streamed request bodies.
func newServer() *fasthttp.Server {
	return &fasthttp.Server{StreamRequestBody: true, Handler: handle}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "listen address")
	flag.Parse()

	server := newServer()
	if err := http2.ConfigureServer(server, http2.ServerConfig{}); err != nil {
		log.Fatal(err)
	}
	log.Printf("gRPC echo on %s", *addr)
	log.Fatal(server.ListenAndServe(*addr))
}

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
	"flag"
	"io"
	"log"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/http2"
)

// frame wraps one message in gRPC's length-prefixed framing.
func frame(message []byte) []byte {
	framed := make([]byte, 5+len(message))
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(message)))
	copy(framed[5:], message)
	return framed
}

func readFrame(r io.Reader) ([]byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	message := make([]byte, binary.BigEndian.Uint32(header[1:5]))
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

func ping(ctx *fasthttp.RequestCtx) {
	body := ctx.PostBody()
	if len(body) < 5 || body[0] != 0 {
		ctx.SetContentType("application/grpc")
		ctx.Response.Header.Set("Grpc-Status", "13")
		return
	}
	if !beginGRPC(ctx) {
		return
	}
	ctx.SetBody(append([]byte(nil), body...))
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

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "listen address")
	flag.Parse()

	server := &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			switch string(ctx.Path()) {
			case "/echo.Echo/Ping":
				ping(ctx)
			case "/echo.Echo/Chat":
				chat(ctx)
			default:
				ctx.SetStatusCode(fasthttp.StatusNotFound)
			}
		},
	}
	if err := http2.ConfigureServer(server, http2.ServerConfig{}); err != nil {
		log.Fatal(err)
	}
	log.Printf("gRPC echo on %s", *addr)
	log.Fatal(server.ListenAndServe(*addr))
}

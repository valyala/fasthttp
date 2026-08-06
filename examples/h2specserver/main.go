// Command h2specserver serves a cleartext prior-knowledge endpoint for local
// h2spec conformance runs:
//
//	go run ./examples/h2specserver -addr 127.0.0.1:8080 &
//	h2spec --host 127.0.0.1 --port 8080 --strict
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/valyala/fasthttp"
	fasthttphttp2 "github.com/valyala/fasthttp/http2"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "listen address")
	maxHeaderListSize := flag.Uint("max-header-list-size", 0, "HTTP/2 decompressed header list limit; zero uses the package default")
	flag.Parse()

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("ok")
		},
	}
	config := fasthttphttp2.ServerConfig{MaxHeaderListSize: uint32(*maxHeaderListSize)}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatal(err)
	}
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stopping
		_ = listener.Close()
	}()
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		go func() {
			if serveErr := fasthttphttp2.ServeConn(server, conn, config); serveErr != nil {
				log.Printf("serve connection: %v", serveErr)
			}
		}()
	}
}

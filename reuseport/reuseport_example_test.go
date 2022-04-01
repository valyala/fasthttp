package reuseport_test

import (
	"fmt"
	"log"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/reuseport"
)

func ExampleListen() {
	ln, err := reuseport.Listen("tcp4", "localhost:12345")
	if err != nil {
		log.Fatalf("error in reuseport listener: %v", err)
	}

	if err = fasthttp.Serve(ln, requestHandler); err != nil {
		log.Fatalf("error in fasthttp Server: %v", err)
	}
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	fmt.Fprintf(ctx, "Hello, world!")
}

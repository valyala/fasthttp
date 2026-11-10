package http2_test

import (
	"io"
	"log"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/http2"
)

func ExampleConfigureServer() {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("hello over HTTP/1 or HTTP/2")
		},
	}
	if err := http2.ConfigureServer(server, http2.ServerConfig{}); err != nil {
		log.Fatal(err)
	}
	// To start serving, call server.ListenAndServeTLS with a certificate:
	// ListenAndServeTLS(":8443", "cert.pem", "key.pem").
}

func ExampleConfigureHostClient() {
	client := &fasthttp.HostClient{
		Addr:  "example.com:443",
		IsTLS: true,
	}
	if err := http2.ConfigureHostClient(client, http2.ClientConfig{}); err != nil {
		log.Fatal(err)
	}
}

func ExampleConfigureHostClient_priorKnowledge() {
	client := &fasthttp.HostClient{Addr: "127.0.0.1:8080"}
	if err := http2.ConfigureHostClient(client, http2.ClientConfig{
		Mode: http2.PriorKnowledge,
	}); err != nil {
		log.Fatal(err)
	}
}

func Example_extendedConnectByteEcho() {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Request.Header.ConnectProtocol()) != "websocket" {
				ctx.Error("extended CONNECT required", fasthttp.StatusBadRequest)
				return
			}
			ctx.Response.SetStatusCode(fasthttp.StatusOK)
			if err := ctx.AcceptStream(func(stream fasthttp.StreamConn) {
				defer stream.Close()
				_, _ = io.Copy(stream, stream)
			}); err != nil {
				ctx.Error(err.Error(), fasthttp.StatusBadRequest)
			}
		},
	}
	if err := http2.ConfigureServer(server, http2.ServerConfig{
		EnableExtendedConnect: true,
	}); err != nil {
		log.Fatal(err)
	}
	// DATA bytes are echoed on one HTTP/2 stream. A WebSocket application
	// layers its WebSocket frame codec over the returned StreamConn.
}

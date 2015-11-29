package fasthttp

import (
	"fmt"
	"log"
	"net"
)

func ExampleListenAndServe() {
	// The server will listen for incoming requests on this address.
	listenAddr := "127.0.0.1:80"

	// This function will be called by the server for each incoming request.
	//
	// RequestCtx provides a lot of functionality related to http request
	// processing. See RequestCtx docs for details.
	requestHandler := func(ctx *RequestCtx) {
		fmt.Fprintf(ctx, "Hello, world! Requested path is %q", ctx.Path())
	}

	// Start the server with default settings.
	// Create Server instance for adjusting server settings.
	//
	// ListenAndServe returns only on error, so usually it blocks forever.
	if err := ListenAndServe(listenAddr, requestHandler); err != nil {
		log.Fatalf("error in ListenAndServe: %s", err)
	}
}

func ExampleServe() {
	// Create network listener for accepting incoming requests.
	//
	// Note that you are not limited by TCP listener - arbitrary
	// net.Listener may be used by the server.
	// For example, unix socket listener or TLS listener.
	ln, err := net.Listen("tcp4", "127.0.0.1:8080")
	if err != nil {
		log.Fatalf("error in net.Listen: %s", err)
	}

	// This function will be called by the server for each incoming request.
	//
	// RequestCtx provides a lot of functionality related to http request
	// processing. See RequestCtx docs for details.
	requestHandler := func(ctx *RequestCtx) {
		fmt.Fprintf(ctx, "Hello, world! Requested path is %q", ctx.Path())
	}

	// Start the server with default settings.
	// Create Server instance for adjusting server settings.
	//
	// Serve returns on ln.Close() or error, so usually it blocks forever.
	if err := Serve(ln, requestHandler); err != nil {
		log.Fatalf("error in Serve: %s", err)
	}
}

func ExampleServer() {
	// This function will be called by the server for each incoming request.
	//
	// RequestCtx provides a lot of functionality related to http request
	// processing. See RequestCtx docs for details.
	requestHandler := func(ctx *RequestCtx) {
		fmt.Fprintf(ctx, "Hello, world! Requested path is %q", ctx.Path())
	}

	// Create custom server.
	s := &Server{
		Handler: requestHandler,

		// Every response will contain 'Server: My super server' header.
		Name: "My super server",

		// Other Server settings may be set here.
	}

	// Start the server listening for incoming requests on the given address.
	//
	// ListenAndServe returns only on error, so usually it blocks forever.
	if err := s.ListenAndServe("127.0.0.1:80"); err != nil {
		log.Fatalf("error in ListenAndServe: %s", err)
	}
}

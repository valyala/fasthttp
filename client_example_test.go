package fasthttp_test

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

func ExampleHostClient() {
	// Prepare a client, which fetches webpages via HTTP proxy listening
	// on the localhost:8080.
	c := &fasthttp.HostClient{
		Addr:                "localhost:8080",
		MaxResponseBodySize: 10 * 1024 * 1024, // Reject responses larger than 10 MiB.
	}

	// Fetch google page via local proxy.
	statusCode, body, err := c.Get(nil, "http://google.com/foo/bar")
	if err != nil {
		log.Fatalf("Error when loading google page through local proxy: %v", err)
	}
	if statusCode != fasthttp.StatusOK {
		log.Fatalf("Unexpected status code: %d. Expecting %d", statusCode, fasthttp.StatusOK)
	}
	useResponseBody(body)

	// Fetch foobar page via local proxy. Reuse body buffer.
	statusCode, body, err = c.Get(body, "http://foobar.com/google/com")
	if err != nil {
		log.Fatalf("Error when loading foobar page through local proxy: %v", err)
	}
	if statusCode != fasthttp.StatusOK {
		log.Fatalf("Unexpected status code: %d. Expecting %d", statusCode, fasthttp.StatusOK)
	}
	useResponseBody(body)
}

func useResponseBody(body []byte) {
	// Do something with body :)
}

func ExampleResponse_BodyStream() {
	listener := fasthttputil.NewInmemoryListener()
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("hello world")
		},
	}
	go func() {
		_ = server.Serve(listener)
	}()
	defer func() {
		_ = server.Shutdown()
	}()

	readBodyPrefix := func(resp *fasthttp.Response, limit int) ([]byte, error) {
		stream := resp.BodyStream()
		closer, ok := stream.(fasthttp.ReadCloserWithError)
		if !ok {
			_ = resp.CloseBodyStream()
			return nil, fmt.Errorf("unexpected body stream type %T", stream)
		}

		// Read one extra byte so an exact-size body can be distinguished from
		// a truncated body.
		body, err := io.ReadAll(io.LimitReader(stream, int64(limit)+1))
		if err != nil {
			_ = closer.CloseWithError(err)
			return body, err
		}
		if len(body) > limit {
			body = body[:limit]
			_ = closer.CloseWithError(fasthttp.ErrBodyTooLarge)
			return body, fasthttp.ErrBodyTooLarge
		}

		return body, resp.CloseBodyStream()
	}

	client := &fasthttp.Client{
		Dial: func(string) (net.Conn, error) {
			return listener.Dial()
		},
	}
	defer client.CloseIdleConnections()

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI("http://example.com/")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	resp.StreamBody = true

	if err := client.Do(req, resp); err != nil {
		fmt.Printf("request error: %v\n", err)
		return
	}
	body, err := readBodyPrefix(resp, 5)
	fmt.Printf("%s\n%t\n", body, errors.Is(err, fasthttp.ErrBodyTooLarge))

	// Output:
	// hello
	// true
}

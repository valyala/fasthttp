package fasthttp_test

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/valyala/fasthttp"
)

func ExampleResponseHeader_PeekTrailer() {
	var resp fasthttp.Response
	raw := "HTTP/1.1 200 OK\r\nX-Result: upfront\r\n" +
		"Trailer: X-Result\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"0\r\nX-Result: first\r\nX-Result: second\r\n\r\n"
	if err := resp.Read(bufio.NewReader(strings.NewReader(raw))); err != nil {
		panic(err)
	}
	fmt.Printf("header: %s\n", resp.Header.Peek("X-Result"))
	fmt.Printf("trailer: %s\n", resp.Header.PeekTrailer("X-Result"))
	fmt.Printf("trailers: %s\n", resp.Header.PeekAllTrailer("X-Result"))
	// Output:
	// header: upfront
	// trailer: first
	// trailers: [first second]
}

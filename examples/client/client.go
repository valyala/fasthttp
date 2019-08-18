// +build go1.11

package main

import (
	"fmt"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/reuseport"
	"net"
)

func main() {
	// client with SO_REUSEPORT
	// critical when many concurrent requests is running from single IP address
	// and frequency of releasing TIME_WAIT connections is low
	client := fasthttp.Client{
		Dial: func(addr string) (net.Conn, error) {
			var dialer net.Dialer
			dialer.Control = reuseport.Control
			return dialer.Dial("tcp", addr)
		},
	}
	status, body, err := client.Get(nil, "http://localhost:8080")
	if err != nil {
		panic(err)
	}
	fmt.Printf("status: %d. body: %s", status, body)
}

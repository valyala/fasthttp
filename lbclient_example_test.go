package fasthttp_test

import (
	"fmt"
	"log"

	"github.com/valyala/fasthttp"
)

func ExampleLBClient() {
	// Requests will be spread among these servers.
	servers := []string{
		"google.com:80",
		"foobar.com:8080",
		"127.0.0.1:123",
	}

	// Prepare clients for each server
	var lbc fasthttp.LBClient
	for _, addr := range servers {
		c := &fasthttp.HostClient{
			Addr: addr,
		}
		lbc.Clients = append(lbc.Clients, c)
	}

	// Send requests to load-balanced servers
	var req fasthttp.Request
	var resp fasthttp.Response
	for i := 0; i < 10; i++ {
		url := fmt.Sprintf("http://abcedfg/foo/bar/%d", i)
		req.SetRequestURI(url)
		if err := lbc.Do(&req, &resp); err != nil {
			log.Fatalf("Error when sending request: %v", err)
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			log.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), fasthttp.StatusOK)
		}

		useResponseBody(resp.Body())
	}
}

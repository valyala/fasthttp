package main

import (
	"fmt"
	"os"

	"github.com/valyala/fasthttp"
)

func main() {
	// Get URI from a pool
	url := fasthttp.AcquireURI()
	url.Parse(nil, []byte("http://localhost:8080/"))
	url.SetUsername("Aladdin")
	url.SetPassword("Open Sesame")

	hc := &fasthttp.HostClient{
		Addr: "localhost:8080", // The host address and port must be set explicitly
	}

	req := fasthttp.AcquireRequest()
	req.SetURI(url)          // copy url into request
	fasthttp.ReleaseURI(url) // now you may release the URI

	req.Header.SetMethod(fasthttp.MethodGet)
	resp := fasthttp.AcquireResponse()
	err := hc.Do(req, resp)
	fasthttp.ReleaseRequest(req)
	if err == nil {
		fmt.Printf("Response: %s\n", resp.Body())
	} else {
		fmt.Fprintf(os.Stderr, "Connection error: %v\n", err)
	}
	fasthttp.ReleaseResponse(resp)
}

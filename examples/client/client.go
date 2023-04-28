package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"time"

	"github.com/valyala/fasthttp"
)

var headerContentTypeJson = []byte("application/json")

var client *fasthttp.Client

type Entity struct {
	Id   int
	Name string
}

func main() {
	// You may read the timeouts from some config
	readTimeout, _ := time.ParseDuration("500ms")
	writeTimeout, _ := time.ParseDuration("500ms")
	maxIdleConnDuration, _ := time.ParseDuration("1h")
	client = &fasthttp.Client{
		ReadTimeout:                   readTimeout,
		WriteTimeout:                  writeTimeout,
		MaxIdleConnDuration:           maxIdleConnDuration,
		NoDefaultUserAgentHeader:      true, // Don't send: User-Agent: fasthttp
		DisableHeaderNamesNormalizing: true, // If you set the case on your headers correctly you can enable this
		DisablePathNormalizing:        true,
		// increase DNS cache time to an hour instead of default minute
		Dial: (&fasthttp.TCPDialer{
			Concurrency:      4096,
			DNSCacheDuration: time.Hour,
		}).Dial,
	}
	sendGetRequest()
	sendPostRequest()
}

func sendGetRequest() {
	req := fasthttp.AcquireRequest()
	req.SetRequestURI("http://localhost:8080/")
	req.Header.SetMethod(fasthttp.MethodGet)
	resp := fasthttp.AcquireResponse()
	err := client.Do(req, resp)
	fasthttp.ReleaseRequest(req)
	if err == nil {
		fmt.Printf("DEBUG Response: %s\n", resp.Body())
	} else {
		fmt.Fprintf(os.Stderr, "ERR Connection error: %v\n", err)
	}
	fasthttp.ReleaseResponse(resp)
}

func sendPostRequest() {
	// per-request timeout
	reqTimeout := time.Duration(100) * time.Millisecond

	reqEntity := &Entity{
		Name: "New entity",
	}
	reqEntityBytes, _ := json.Marshal(reqEntity)

	req := fasthttp.AcquireRequest()
	req.SetRequestURI("http://localhost:8080/")
	req.Header.SetMethod(fasthttp.MethodPost)
	req.Header.SetContentTypeBytes(headerContentTypeJson)
	req.SetBodyRaw(reqEntityBytes)

	resp := fasthttp.AcquireResponse()
	err := client.DoTimeout(req, resp, reqTimeout)
	fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	if err != nil {
		errName, known := httpConnError(err)
		if known {
			fmt.Fprintf(os.Stderr, "WARN conn error: %v\n", errName)
		} else {
			fmt.Fprintf(os.Stderr, "ERR conn failure: %v %v\n", errName, err)
		}

		return
	}

	statusCode := resp.StatusCode()
	respBody := resp.Body()
	fmt.Printf("DEBUG Response: %s\n", respBody)

	if statusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "ERR invalid HTTP response code: %d\n", statusCode)

		return
	}

	respEntity := &Entity{}
	err = json.Unmarshal(respBody, respEntity)
	if err == nil || errors.Is(err, io.EOF) {
		fmt.Printf("DEBUG Parsed Response: %v\n", respEntity)
	} else {
		fmt.Fprintf(os.Stderr, "ERR failed to parse response: %v\n", err)
	}
}

func httpConnError(err error) (string, bool) {
	var (
		errName string
		known   = true
	)

	switch {
	case errors.Is(err, fasthttp.ErrTimeout):
		errName = "timeout"
	case errors.Is(err, fasthttp.ErrNoFreeConns):
		errName = "conn_limit"
	case errors.Is(err, fasthttp.ErrConnectionClosed):
		errName = "conn_close"
	case reflect.TypeOf(err).String() == "*net.OpError":
		errName = "timeout"
	default:
		known = false
	}

	return errName, known
}

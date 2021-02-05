package fasthttp

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

func TestRequestStream(t *testing.T) {
	body := createFixedBody(3)
	chunkedBody := createChunkedBody(body)

	// func(ctx *RequestCtx) {
	// 	bodyBytes, err := ioutil.ReadAll(ctx.RequestBodyStream())
	// 	if err != nil {
	// 		ctx.Error("lol nope", 400)
	// 		return
	// 	}

	// 	println(bodyBytes)
	// }

	testHandler := func(ctx *RequestCtx) {
		t.Log("ioutil read started")
		bodyBytes, err := ioutil.ReadAll(ctx.RequestBodyStream())
		if err != nil {
			t.Logf("ioutil read returned err=%s", err)
			t.Error("unexpected error while reading request body stream")
		}

		if !bytes.Equal(chunkedBody, bodyBytes) {
			println("-------")
			println(string(body))
			println("-------")
			println(string(bodyBytes))
			t.Error("unexpected request body")
		}
		t.Logf("got body with len=%d", len(bodyBytes))
	}
	s := &Server{
		Handler:            testHandler,
		StreamRequestBody:  true,
		MaxRequestBodySize: 1, // easier to test with small limit
	}

	ln := fasthttputil.NewInmemoryListener()

	go func() {
		s.Serve(ln)
	}()

	req := Request{}
	req.SetHost("localhost")
	req.Header.SetMethod("POST")
	req.Header.Set("transfer-encoding", "chunked")
	// req.SetBodyString(strings.Repeat("testtesttest", 1024))

	// expectedTrailer := []byte("chunked shit")
	// chunkedBody = append(chunkedBody, expectedTrailer...)
	req.SetBodyStream(bytes.NewBuffer(chunkedBody), len(chunkedBody))
	req.Header.Set("transfer-encoding", "chunked")
	req.Header.SetContentLength(-1)

	formattedRequest := req.String()
	c, err := ln.Dial()
	if err != nil {
		t.Errorf("unexpected error while dialing: %s", err)
	}
	println("dial completed")
	if _, err = c.Write([]byte(formattedRequest)); err != nil {
		t.Errorf("unexpected error while writing request: %s", err)
	}

	println("written the request")
	println(formattedRequest)
	br := bufio.NewReader(c)
	var respH ResponseHeader
	if err = respH.Read(br); err != nil {
		t.Errorf("unexpected error: %s", err)
	}

	println(respH.String())
	time.Sleep(5 * time.Second)

}

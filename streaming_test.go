package fasthttp

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"sync"
	"testing"

	"github.com/valyala/fasthttp/fasthttputil"
)

func TestRequestStream(t *testing.T) {
	body := createFixedBody(3)
	chunkedBody := createChunkedBody(body)

	testHandler := func(ctx *RequestCtx) {
		bodyBytes, err := ioutil.ReadAll(ctx.RequestBodyStream())
		if err != nil {
			t.Logf("ioutil read returned err=%s", err)
			t.Error("unexpected error while reading request body stream")
		}

		if !bytes.Equal(chunkedBody, bodyBytes) {
			t.Errorf("unexpected request body, expected %q, got %q", chunkedBody, bodyBytes)
		}
	}
	s := &Server{
		Handler:            testHandler,
		StreamRequestBody:  true,
		MaxRequestBodySize: 1, // easier to test with small limit
	}

	ln := fasthttputil.NewInmemoryListener()

	go func() {
		err := s.Serve(ln)
		if err != nil {
			t.Errorf("could not serve listener: %s", err)
		}
	}()

	req := Request{}
	req.SetHost("localhost")
	req.Header.SetMethod("POST")
	req.SetBodyStream(bytes.NewBuffer(chunkedBody), len(chunkedBody))
	req.Header.Set("transfer-encoding", "chunked")
	req.Header.SetContentLength(-1)

	formattedRequest := req.String()
	c, err := ln.Dial()
	if err != nil {
		t.Errorf("unexpected error while dialing: %s", err)
	}
	if _, err = c.Write([]byte(formattedRequest)); err != nil {
		t.Errorf("unexpected error while writing request: %s", err)
	}

	br := bufio.NewReader(c)
	var respH ResponseHeader
	if err = respH.Read(br); err != nil {
		t.Errorf("unexpected error: %s", err)
	}
}

func BenchmarkRequestStreamE2E(b *testing.B) {
	body := createFixedBody(3)
	chunkedBody := createChunkedBody(body)

	testHandler := func(ctx *RequestCtx) {
		bodyBytes, err := ioutil.ReadAll(ctx.RequestBodyStream())
		if err != nil {
			b.Logf("ioutil read returned err=%s", err)
			b.Error("unexpected error while reading request body stream")
		}

		if !bytes.Equal(chunkedBody, bodyBytes) {
			b.Errorf("unexpected request body, expected %q, got %q", chunkedBody, bodyBytes)
		}
	}
	s := &Server{
		Handler:            testHandler,
		StreamRequestBody:  true,
		MaxRequestBodySize: 1, // easier to test with small limit
	}

	ln := fasthttputil.NewInmemoryListener()
	wg := &sync.WaitGroup{}

	go func() {
		err := s.Serve(ln)

		if err != nil {
			b.Errorf("could not serve listener: %s", err)
		}
	}()

	req := Request{}
	req.SetHost("localhost")
	req.Header.SetMethod("POST")
	req.SetBodyStream(bytes.NewBuffer(chunkedBody), len(chunkedBody))
	req.Header.Set("transfer-encoding", "chunked")
	req.Header.SetContentLength(-1)

	formattedRequest := []byte(req.String())

	wg.Add(4)
	for i := 0; i < 4; i++ {
		go func(wg *sync.WaitGroup) {
			for i := 0; i < b.N/4; i++ {
				c, err := ln.Dial()
				if err != nil {
					b.Errorf("unexpected error while dialing: %s", err)
				}
				if _, err = c.Write(formattedRequest); err != nil {
					b.Errorf("unexpected error while writing request: %s", err)
				}

				br := bufio.NewReaderSize(c, 128)
				var respH ResponseHeader
				if err = respH.Read(br); err != nil {
					b.Errorf("unexpected error: %s", err)
				}
				c.Close()
			}
			wg.Done()
		}(wg)
	}

	wg.Wait()
}

package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

func TestStreamingPipeline(t *testing.T) {
	t.Parallel()

	reqS := `POST /one HTTP/1.1
Host: example.com
Content-Length: 10

aaaaaaaaaa
POST /two HTTP/1.1
Host: example.com
Content-Length: 10

aaaaaaaaaa`

	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		StreamRequestBody: true,
		Handler: func(ctx *RequestCtx) {
			body := ""
			expected := "aaaaaaaaaa"
			if string(ctx.Path()) == "/one" {
				body = string(ctx.PostBody())
			} else {
				all, err := io.ReadAll(ctx.RequestBodyStream())
				if err != nil {
					t.Error(err)
				}
				body = string(all)
			}
			if body != expected {
				t.Errorf("expected %q got %q", expected, body)
			}
		},
	}

	ch := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		close(ch)
	}()

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err = conn.Write([]byte(reqS)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp Response
	br := bufio.NewReader(conn)
	respCh := make(chan struct{})
	go func() {
		if err := resp.Read(br); err != nil {
			t.Errorf("error when reading response: %v", err)
		}
		if resp.StatusCode() != StatusOK {
			t.Errorf("unexpected status code %d. Expecting %d", resp.StatusCode(), StatusOK)
		}

		if err := resp.Read(br); err != nil {
			t.Errorf("error when reading response: %v", err)
		}
		if resp.StatusCode() != StatusOK {
			t.Errorf("unexpected status code %d. Expecting %d", resp.StatusCode(), StatusOK)
		}
		close(respCh)
	}()

	select {
	case <-respCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("error when closing listener: %v", err)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout when waiting for the server to stop")
	}
}

func getChunkedTestEnv(t testing.TB) (*fasthttputil.InmemoryListener, []byte) {
	body := createFixedBody(128 * 1024)
	chunkedBody := createChunkedBody(body, nil, true)

	testHandler := func(ctx *RequestCtx) {
		bodyBytes, err := io.ReadAll(ctx.RequestBodyStream())
		if err != nil {
			t.Logf("io read returned err=%v", err)
			t.Error("unexpected error while reading request body stream")
		}

		if !bytes.Equal(body, bodyBytes) {
			t.Errorf("unexpected request body, expected %q, got %q", body, bodyBytes)
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
			t.Errorf("could not serve listener: %v", err)
		}
	}()

	req := Request{}
	req.SetHost("localhost")
	req.Header.SetMethod("POST")
	req.Header.Set("transfer-encoding", "chunked")
	req.Header.SetContentLength(-1)

	formattedRequest := req.Header.Header()
	formattedRequest = append(formattedRequest, chunkedBody...)

	return ln, formattedRequest
}

func TestRequestStreamChunkedWithTrailer(t *testing.T) {
	t.Parallel()

	body := createFixedBody(10)
	expectedTrailer := map[string]string{
		"Foo": "footest",
		"Bar": "bartest",
	}
	chunkedBody := createChunkedBody(body, expectedTrailer, true)
	req := fmt.Sprintf(`POST / HTTP/1.1
Host: example.com
Transfer-Encoding: chunked
Trailer: Foo, Bar

%s
`, chunkedBody)

	ln := fasthttputil.NewInmemoryListener()
	s := &Server{
		StreamRequestBody: true,
		Handler: func(ctx *RequestCtx) {
			all, err := io.ReadAll(ctx.RequestBodyStream())
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !bytes.Equal(all, body) {
				t.Errorf("unexpected body %q. Expecting %q", all, body)
			}

			for k, v := range expectedTrailer {
				r := ctx.Request.Header.Peek(k)
				if string(r) != v {
					t.Errorf("unexpected trailer %q. Expecting %q. Got %q", k, v, r)
				}
			}
		},
	}

	ch := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		close(ch)
	}()

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err = conn.Write([]byte(req)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("error when closing listener: %v", err)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout when waiting for the server to stop")
	}
}

func TestRequestStream(t *testing.T) {
	t.Parallel()

	ln, formattedRequest := getChunkedTestEnv(t)

	c, err := ln.Dial()
	if err != nil {
		t.Errorf("unexpected error while dialing: %v", err)
	}
	if _, err = c.Write(formattedRequest); err != nil {
		t.Errorf("unexpected error while writing request: %v", err)
	}

	br := bufio.NewReader(c)
	var respH ResponseHeader
	if err = respH.Read(br); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func BenchmarkRequestStreamE2E(b *testing.B) {
	ln, formattedRequest := getChunkedTestEnv(b)

	wg := &sync.WaitGroup{}
	wg.Add(4)
	for i := 0; i < 4; i++ {
		go func(wg *sync.WaitGroup) {
			for i := 0; i < b.N/4; i++ {
				c, err := ln.Dial()
				if err != nil {
					b.Errorf("unexpected error while dialing: %v", err)
				}
				if _, err = c.Write(formattedRequest); err != nil {
					b.Errorf("unexpected error while writing request: %v", err)
				}

				br := bufio.NewReaderSize(c, 128)
				var respH ResponseHeader
				if err = respH.Read(br); err != nil {
					b.Errorf("unexpected error: %v", err)
				}
				c.Close()
			}
			wg.Done()
		}(wg)
	}

	wg.Wait()
}

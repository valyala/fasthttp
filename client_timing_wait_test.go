//go:build go1.11
// +build go1.11

package fasthttp

import (
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

func newFasthttpSleepEchoHandler(sleep time.Duration) RequestHandler {
	return func(ctx *RequestCtx) {
		time.Sleep(sleep)
		ctx.Success("text/plain", ctx.RequestURI())
	}
}

func BenchmarkClientGetEndToEndWaitConn1Inmemory(b *testing.B) {
	benchmarkClientGetEndToEndWaitConnInmemory(b, 1)
}

func BenchmarkClientGetEndToEndWaitConn10Inmemory(b *testing.B) {
	benchmarkClientGetEndToEndWaitConnInmemory(b, 10)
}

func BenchmarkClientGetEndToEndWaitConn100Inmemory(b *testing.B) {
	benchmarkClientGetEndToEndWaitConnInmemory(b, 100)
}

func BenchmarkClientGetEndToEndWaitConn1000Inmemory(b *testing.B) {
	benchmarkClientGetEndToEndWaitConnInmemory(b, 1000)
}

func benchmarkClientGetEndToEndWaitConnInmemory(b *testing.B, parallelism int) {
	ln := fasthttputil.NewInmemoryListener()

	ch := make(chan struct{})
	sleepDuration := 50 * time.Millisecond
	go func() {

		if err := Serve(ln, newFasthttpSleepEchoHandler(sleepDuration)); err != nil {
			b.Errorf("error when serving requests: %s", err)
		}
		close(ch)
	}()

	c := &Client{
		MaxConnsPerHost:    1,
		Dial:               func(addr string) (net.Conn, error) { return ln.Dial() },
		MaxConnWaitTimeout: 5 * time.Second,
	}

	requestURI := "/foo/bar?baz=123&sleep=10ms"
	url := "http://unused.host" + requestURI
	b.SetParallelism(parallelism)
	b.RunParallel(func(pb *testing.PB) {
		var buf []byte
		for pb.Next() {
			statusCode, body, err := c.Get(buf, url)
			if err != nil {
				if err != ErrNoFreeConns {
					b.Fatalf("unexpected error: %s", err)
				}
			} else {
				if statusCode != StatusOK {
					b.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusOK)
				}
				if string(body) != requestURI {
					b.Fatalf("unexpected response %q. Expecting %q", body, requestURI)
				}
			}
			buf = body
		}
	})

	ln.Close()
	select {
	case <-ch:
	case <-time.After(time.Second):
		b.Fatalf("server wasn't stopped")
	}
}

func newNethttpSleepEchoHandler(sleep time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(sleep)
		w.Header().Set(HeaderContentType, "text/plain")
		w.Write([]byte(r.RequestURI)) //nolint:errcheck
	}
}

func BenchmarkNetHTTPClientGetEndToEndWaitConn1Inmemory(b *testing.B) {
	benchmarkNetHTTPClientGetEndToEndWaitConnInmemory(b, 1)
}

func BenchmarkNetHTTPClientGetEndToEndWaitConn10Inmemory(b *testing.B) {
	benchmarkNetHTTPClientGetEndToEndWaitConnInmemory(b, 10)
}

func BenchmarkNetHTTPClientGetEndToEndWaitConn100Inmemory(b *testing.B) {
	benchmarkNetHTTPClientGetEndToEndWaitConnInmemory(b, 100)
}

func BenchmarkNetHTTPClientGetEndToEndWaitConn1000Inmemory(b *testing.B) {
	benchmarkNetHTTPClientGetEndToEndWaitConnInmemory(b, 1000)
}

func benchmarkNetHTTPClientGetEndToEndWaitConnInmemory(b *testing.B, parallelism int) {
	ln := fasthttputil.NewInmemoryListener()

	ch := make(chan struct{})
	sleep := 50 * time.Millisecond
	go func() {
		if err := http.Serve(ln, newNethttpSleepEchoHandler(sleep)); err != nil && !strings.Contains(
			err.Error(), "use of closed network connection") {
			b.Errorf("error when serving requests: %s", err)
		}
		close(ch)
	}()

	c := &http.Client{
		Transport: &http.Transport{
			Dial:            func(_, _ string) (net.Conn, error) { return ln.Dial() },
			MaxConnsPerHost: 1,
		},
		Timeout: 5 * time.Second,
	}

	requestURI := "/foo/bar?baz=123"
	url := "http://unused.host" + requestURI
	b.SetParallelism(parallelism)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := c.Get(url)
			if err != nil {
				if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
					b.Fatalf("unexpected error: %s", err)
				}
			} else {
				if resp.StatusCode != http.StatusOK {
					b.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode, http.StatusOK)
				}
				body, err := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					b.Fatalf("unexpected error when reading response body: %s", err)
				}
				if string(body) != requestURI {
					b.Fatalf("unexpected response %q. Expecting %q", body, requestURI)
				}
			}
		}
	})

	ln.Close()
	select {
	case <-ch:
	case <-time.After(time.Second):
		b.Fatalf("server wasn't stopped")
	}
}

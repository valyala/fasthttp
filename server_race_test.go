//go:build race

package fasthttp

import (
	"bufio"
	"context"
	"math"
	"testing"

	"github.com/valyala/fasthttp/fasthttputil"
)

func TestServerDoneRace(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			for i := 0; i < math.MaxInt; i++ {
				ctx.Done()
			}
		},
	}

	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}()

	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer c.Close()
	if _, err = c.Write([]byte("POST / HTTP/1.1\r\nHost: go.dev\r\nContent-Length: 3\r\n\r\nABC" +
		"\r\n\r\n" + // <-- this stuff is bogus, but we'll ignore it
		"GET / HTTP/1.1\r\nHost: go.dev\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancelFunc := context.WithCancel(t.Context())
	cancelFunc()

	s.ShutdownWithContext(ctx)
}

// RequestCtx.Done() and Err() may still be used after the handler returns,
// for example by database/sql's awaitDone or context.WithCancel's propagation
// goroutine, which select on ctx.Done() and then call ctx.Err(). Waking up
// from the closed channel must not race with ShutdownWithContext clearing it.
func TestServerDoneAfterHandlerReturnsRace(t *testing.T) {
	t.Parallel()

	errCh := make(chan error, 1)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			done := ctx.Done()
			go func() {
				<-done
				errCh <- ctx.Err()
			}()
		},
	}

	ln := fasthttputil.NewInmemoryListener()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.Serve(ln)
	}()

	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: go.dev\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	var resp Response
	if err = resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c.Close()

	if err = s.Shutdown(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err = <-serveErr; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	<-errCh
}

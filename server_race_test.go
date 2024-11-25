//go:build race

package fasthttp

import (
	"context"
	"github.com/valyala/fasthttp/fasthttputil"
	"math"
	"testing"
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
	ctx, cancelFunc := context.WithCancel(context.Background())
	cancelFunc()

	s.ShutdownWithContext(ctx)
}

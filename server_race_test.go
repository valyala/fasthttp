//go:build race

package fasthttp

import (
	"context"
	"math"
	"testing"
	"time"

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

// A timeout may still be looking up its parent in RequestCtx.Value after
// it closes Done, while the handler or serve loop changes the user values.
func TestRequestCtxValueTimeoutRace(t *testing.T) {
	t.Parallel()

	ctx := &RequestCtx{}
	ctx.Init2(nil, nil, false)
	for range 10 {
		child, cancel := context.WithTimeout(ctx, time.Millisecond)
		<-child.Done()
		ctx.SetUserValue("finished", true)
		ctx.Request.Reset()
		cancel()
	}
}

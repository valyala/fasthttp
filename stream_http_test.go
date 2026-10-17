package fasthttp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
)

func TestStreamWriterHTTPFlushBeforeReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, client := net.Pipe()
		defer client.Close()
		finish := make(chan struct{})
		// The process-wide date refresher must not start inside a synctest bubble.
		s := Server{NoDefaultDate: true, Handler: func(ctx *RequestCtx) {
			ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
				if _, err := w.WriteString("event"); err != nil {
					t.Error(err)
					return
				}
				if err := w.Flush(); err != nil {
					t.Error(err)
				}
				<-finish
			})
		}}
		done := make(chan error, 1)
		go func() { done <- s.ServeConn(server) }()
		fmt.Fprint(client, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 5)
		if _, err := io.ReadFull(resp.Body, b); err != nil || string(b) != "event" {
			t.Fatal(string(b), err)
		}
		close(finish)
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestStreamWriterHTTPDisconnectStopsProducer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, client := net.Pipe()
		producerDone := make(chan struct{})
		s := Server{NoDefaultDate: true, Handler: func(ctx *RequestCtx) {
			ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
				defer close(producerDone)
				src := make([]byte, 1<<20)
				for {
					if _, err := w.Write(src); err != nil {
						return
					}
					if err := w.Flush(); err != nil {
						return
					}
				}
			})
		}}
		serverDone := make(chan error, 1)
		go func() { serverDone <- s.ServeConn(server) }()
		fmt.Fprint(client, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := make([]byte, 1)
		if _, err := io.ReadFull(resp.Body, b); err != nil {
			t.Fatal(err)
		}
		client.Close()
		if err := <-serverDone; err == nil {
			t.Fatal("missing closed connection error")
		}
		<-producerDone
	})
}

package fasthttp

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

// Real sockets stay outside synctest. Deadlines bound failures; channels gate
// future input and EOF so visibility never depends on a sleep or buffer size.
func TestStreamTCPAcceptanceFlushBeforeFutureDataAndEOF(t *testing.T) {
	for _, encoding := range []string{"plain", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			future, eof, cancel := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var cancelOnce sync.Once
			flushed, producerDone := make(chan error, 2), make(chan error, 1)
			source := NewBoundedStreamReader(func(w *bufio.Writer) {
				for i, message := range []string{"first-event\n", "last-event\n"} {
					_, err := w.WriteString(message)
					if err == nil {
						err = w.Flush()
					}
					flushed <- err
					if err != nil {
						producerDone <- err
						return
					}
					gate := future
					if i == 1 {
						gate = eof
					}
					select {
					case <-gate:
					case <-cancel:
						producerDone <- io.ErrClosedPipe
						return
					}
				}
				producerDone <- nil
			})
			codecDone := make(chan (<-chan struct{}), 1)
			s := streamTCPServer(t, source, encoding, codecDone)
			client, serverDone := streamTCPConnect(t, s, func() { cancelOnce.Do(func() { close(cancel) }); streamClose(t, source) })
			response, decoded := streamTCPResponse(t, client, encoding)
			defer streamClose(t, response.Body)
			defer streamClose(t, decoded)
			for i, want := range []string{"first-event\n", "last-event\n"} {
				if err := streamAwait(t, flushed, "producer Flush"); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(want))
				if _, err := io.ReadFull(decoded, got); err != nil || string(got) != want {
					t.Fatalf("event=%q,%v; want %q before releasing producer", got, err, want)
				}
				select {
				case err := <-producerDone:
					t.Fatalf("producer returned before its gate: %v", err)
				default:
				}
				if i == 0 {
					close(future)
				} else {
					close(eof)
				}
			}
			// EOF checks the gzip checksum/footer as well as the final HTTP chunk.
			if tail, err := io.ReadAll(decoded); err != nil || len(tail) != 0 {
				t.Fatal(string(tail), err)
			}
			if err := streamAwait(t, producerDone, "producer return"); err != nil {
				t.Fatal(err)
			}
			if err := streamAwait(t, serverDone, "ServeConn return"); err != nil {
				t.Fatal(err)
			}
			if encoding == "gzip" {
				streamAwait(t, streamAwait(t, codecDone, "bounded compressor"), "codec completion")
			}
		})
	}
}

func TestStreamTCPAcceptanceDisconnectStopsProducer(t *testing.T) {
	for _, encoding := range []string{"plain", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			proceed, cancel := make(chan struct{}), make(chan struct{})
			var cancelOnce sync.Once
			flushed, producerDone := make(chan error, 1), make(chan error, 1)
			source := NewBoundedStreamReader(func(w *bufio.Writer) {
				_, err := w.WriteString("x")
				if err == nil {
					err = w.Flush()
				}
				flushed <- err
				if err != nil {
					producerDone <- err
					return
				}
				select {
				case <-proceed:
				case <-cancel:
					producerDone <- io.ErrClosedPipe
					return
				}
				// Incompressible input prevents a tiny gzip output from hiding cancellation.
				body := streamCompatBody(32 * 1024)
				for {
					if _, err := w.Write(body); err != nil {
						producerDone <- err
						return
					}
					if err := w.Flush(); err != nil {
						producerDone <- err
						return
					}
				}
			})
			codecDone := make(chan (<-chan struct{}), 1)
			s := streamTCPServer(t, source, encoding, codecDone)
			client, serverDone := streamTCPConnect(t, s, func() { cancelOnce.Do(func() { close(cancel) }); streamClose(t, source) })
			response, decoded := streamTCPResponse(t, client, encoding)
			defer streamClose(t, response.Body)
			defer streamClose(t, decoded)
			first := make([]byte, 1)
			if _, err := io.ReadFull(decoded, first); err != nil || first[0] != 'x' {
				t.Fatal(first, err)
			}
			if err := streamAwait(t, flushed, "first Flush"); err != nil {
				t.Fatal(err)
			}
			// RST removes dependence on TCP send-buffer capacity, unlike a FIN-only peer.
			if err := client.SetLinger(0); err != nil {
				t.Fatal(err)
			}
			streamClose(t, client)
			close(proceed)
			serveErr := streamAwait(t, serverDone, "ServeConn after reset")
			var networkErr net.Error
			if !errors.As(serveErr, &networkErr) || networkErr.Timeout() {
				t.Fatalf("ServeConn=%v; want peer failure, not watchdog", serveErr)
			}
			if err := streamAwait(t, producerDone, "producer after reset"); !errors.Is(err, fasthttputil.ErrConnectionClosed) {
				t.Fatal(err)
			}
			if encoding == "gzip" {
				streamAwait(t, streamAwait(t, codecDone, "bounded compressor"), "codec completion")
			}
		})
	}
}

func streamTCPServer(t *testing.T, source io.ReadCloser, encoding string, codecDone chan<- (<-chan struct{})) *Server {
	t.Helper()
	return &Server{NoDefaultDate: true, Handler: func(ctx *RequestCtx) {
		ctx.SetContentType("text/plain")
		ctx.SetBodyStream(source, -1)
		if encoding == "gzip" {
			ctx.Response.gzipBody(CompressBestSpeed)
			// Observe the exact source without wrapping it: a wrapper would silently
			// select legacy compression and invalidate this integration test.
			compressed, ok := ctx.Response.BodyStream().(*boundedCompressedBodyStream)
			if !ok {
				t.Errorf("expected bounded compression, got %T", ctx.Response.BodyStream())
				return
			}
			codecDone <- compressed.done
		}
	}}
}

func streamTCPConnect(t *testing.T, s *Server, stopProducer func()) (*net.TCPConn, <-chan error) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		stopProducer()
		t.Fatal(err)
	}
	defer streamClose(t, listener)
	deadline := time.Now().Add(testTimeout(15 * time.Second))
	if err := listener.SetDeadline(deadline); err != nil {
		stopProducer()
		t.Fatal(err)
	}
	conn, err := (&net.Dialer{Deadline: deadline}).Dial("tcp4", listener.Addr().String())
	if err != nil {
		stopProducer()
		t.Fatal(err)
	}
	client, ok := conn.(*net.TCPConn)
	if !ok {
		streamClose(t, conn)
		stopProducer()
		t.Fatalf("dial returned %T", conn)
	}
	server, err := listener.AcceptTCP()
	if err != nil {
		streamClose(t, client)
		stopProducer()
		t.Fatal(err)
	}
	for _, c := range []*net.TCPConn{client, server} {
		if err := c.SetDeadline(deadline); err != nil {
			streamClose(t, client)
			streamClose(t, server)
			stopProducer()
			t.Fatal(err)
		}
	}
	var closed atomic.Int32
	s.ConnState = func(_ net.Conn, state ConnState) {
		if state == StateClosed {
			closed.Add(1)
		}
	}
	done, returned := make(chan error, 1), make(chan struct{})
	go func() { done <- s.ServeConn(server); close(returned) }()
	t.Cleanup(func() {
		stopProducer()
		streamClose(t, client)
		streamClose(t, server)
		streamAwait(t, returned, "ServeConn cleanup")
		if closed.Load() != 1 || s.GetCurrentConcurrency() != 0 {
			t.Errorf("retained connection: closed=%d, active=%d", closed.Load(), s.GetCurrentConcurrency())
		}
	})
	return client, done
}

func streamTCPResponse(t *testing.T, client net.Conn, encoding string) (*http.Response, io.ReadCloser) {
	t.Helper()
	if _, err := fmt.Fprint(client, "GET /stream HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != StatusOK || len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" {
		t.Fatalf("status/framing=%s/%v", response.Status, response.TransferEncoding)
	}
	want := ""
	if encoding == "gzip" {
		want = "gzip"
	}
	if got := response.Header.Get("Content-Encoding"); got != want {
		t.Fatalf("Content-Encoding=%q,want %q", got, want)
	}
	if encoding == "gzip" {
		decoded, err := gzip.NewReader(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, decoded
	}
	return response, response.Body
}

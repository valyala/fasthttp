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

// These tests intentionally use real loopback sockets. Deadlines only bound a
// broken test; channels, rather than elapsed time, establish the protocol order.
func TestStreamTCPAcceptanceFlushBeforeFutureDataAndEOF(t *testing.T) {
	for _, encoding := range []string{"plain", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			future, eof, cancel := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var cancelOnce sync.Once
			flushed, producerDone := make(chan error, 2), make(chan error, 1)
			producerReturned := make(chan struct{})
			source := streamTCPAcceptanceBoundedReader(t, func(w *bufio.Writer) {
				defer close(producerReturned)
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
			t.Cleanup(func() {
				streamTCPAcceptanceWait(t, producerReturned, "producer cleanup")
			})
			compressor := make(chan io.Reader, 1)
			s := streamTCPAcceptanceServer(source, encoding, compressor)
			client, serverDone := streamTCPAcceptanceConnect(t, s, func() {
				cancelOnce.Do(func() { close(cancel) })
				source.Close()
			})
			response, decoded := streamTCPAcceptanceResponse(t, client, encoding)
			compressed := streamTCPAcceptanceBoundedCompressor(t, source, encoding, compressor)
			for i, want := range []string{"first-event\n", "last-event\n"} {
				if err := streamTCPAcceptanceError(t, flushed, "producer Flush"); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(want))
				if _, err := io.ReadFull(decoded, got); err != nil || string(got) != want {
					t.Fatalf("event %d = %q, %v; want %q before releasing producer", i, got, err, want)
				}
				select {
				case <-producerReturned:
					t.Fatal("producer returned before its gate was released")
				default:
				}
				if i == 0 {
					close(future)
				} else {
					close(eof)
				}
			}
			// Reading through EOF also checks the gzip footer/checksum and the
			// final HTTP chunk after the last small, nonempty Flush.
			if tail, err := io.ReadAll(decoded); err != nil || len(tail) != 0 {
				t.Fatalf("tail = %q, %v", tail, err)
			}
			if err := decoded.Close(); err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if err := streamTCPAcceptanceError(t, producerDone, "producer return"); err != nil {
				t.Fatal(err)
			}
			if err := streamTCPAcceptanceError(t, serverDone, "ServeConn return"); err != nil {
				t.Fatal(err)
			}
			streamTCPAcceptanceWait(t, producerReturned, "producer callback return")
			streamTCPAcceptanceReclaimed(t, s, source, compressed)
		})
	}
}

func TestStreamTCPAcceptanceDisconnectStopsProducer(t *testing.T) {
	for _, encoding := range []string{"plain", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			continueAfterReset, cancel := make(chan struct{}), make(chan struct{})
			var cancelOnce sync.Once
			flushed, producerDone := make(chan error, 1), make(chan error, 1)
			producerReturned := make(chan struct{})
			source := streamTCPAcceptanceBoundedReader(t, func(w *bufio.Writer) {
				defer close(producerReturned)
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
				case <-continueAfterReset:
				case <-cancel:
					producerDone <- io.ErrClosedPipe
					return
				}
				// Incompressible bytes prevent gzip from hiding cancellation in
				// a tiny output. Production starts only after the peer is closed.
				body := make([]byte, 32*1024)
				state := uint32(1)
				for i := range body {
					state ^= state << 13
					state ^= state >> 17
					state ^= state << 5
					body[i] = byte(state)
				}
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
			t.Cleanup(func() {
				streamTCPAcceptanceWait(t, producerReturned, "producer cleanup")
			})
			compressor := make(chan io.Reader, 1)
			s := streamTCPAcceptanceServer(source, encoding, compressor)
			client, serverDone := streamTCPAcceptanceConnect(t, s, func() {
				cancelOnce.Do(func() { close(cancel) })
				source.Close()
			})
			response, decoded := streamTCPAcceptanceResponse(t, client, encoding)
			compressed := streamTCPAcceptanceBoundedCompressor(t, source, encoding, compressor)
			first := make([]byte, 1)
			if _, err := io.ReadFull(decoded, first); err != nil || first[0] != 'x' {
				t.Fatalf("first decoded byte = %q, %v", first, err)
			}
			if err := streamTCPAcceptanceError(t, flushed, "first producer Flush"); err != nil {
				t.Fatal(err)
			}
			// RST makes the failure independent of TCP send-buffer capacity.
			// A FIN-only peer may still legally receive additional output.
			if err := client.SetLinger(0); err != nil {
				t.Fatal(err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			close(continueAfterReset)
			serveErr := streamTCPAcceptanceError(t, serverDone, "ServeConn after reset")
			var networkErr net.Error
			if !errors.As(serveErr, &networkErr) || networkErr.Timeout() {
				t.Fatalf("ServeConn error = %v; want peer network failure, not watchdog", serveErr)
			}
			if err := streamTCPAcceptanceError(t, producerDone, "producer after reset"); !errors.Is(err, fasthttputil.ErrConnectionClosed) {
				t.Fatalf("producer error = %v; want closed stream", err)
			}
			streamTCPAcceptanceWait(t, producerReturned, "producer callback after reset")
			streamTCPAcceptanceReclaimed(t, s, source, compressed)
			// Close is idempotent after cancellation, with no remaining reader
			// racing us after the compressor and ServeConn have returned.
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, struct{ io.Reader }{source}); err != nil {
				t.Fatal(err)
			}
			decoded.Close()
			response.Body.Close()
			t.Logf("peer reset propagated: ServeConn=%v; producer=closed stream", serveErr)
		})
	}
}

func streamTCPAcceptanceServer(source *streamPipe, encoding string, compressor chan<- io.Reader) *Server {
	return &Server{NoDefaultDate: true, Handler: func(ctx *RequestCtx) {
		ctx.SetContentType("text/plain")
		ctx.SetBodyStream(source, -1)
		if encoding == "gzip" {
			ctx.Response.gzipBody(CompressBestSpeed)
			compressor <- ctx.Response.BodyStream()
		}
	}}
}

func streamTCPAcceptanceConnect(t *testing.T, s *Server, stopProducer func()) (*net.TCPConn, <-chan error) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		stopProducer()
		t.Fatal(err)
	}
	defer listener.Close()
	deadline := time.Now().Add(testTimeout(15 * time.Second))
	if err := listener.SetDeadline(deadline); err != nil {
		stopProducer()
		t.Fatal(err)
	}
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.Dial("tcp4", listener.Addr().String())
	if err != nil {
		stopProducer()
		t.Fatal(err)
	}
	client, ok := conn.(*net.TCPConn)
	if !ok {
		conn.Close()
		stopProducer()
		t.Fatalf("dial returned %T, want TCP connection", conn)
	}
	server, err := listener.AcceptTCP()
	if err != nil {
		client.Close()
		stopProducer()
		t.Fatal(err)
	}
	for _, c := range []*net.TCPConn{client, server} {
		if err := c.SetDeadline(deadline); err != nil {
			client.Close()
			server.Close()
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
	go func() {
		done <- s.ServeConn(server)
		close(returned)
	}()
	t.Cleanup(func() {
		stopProducer()
		client.Close()
		server.Close()
		streamTCPAcceptanceWait(t, returned, "ServeConn cleanup")
		if got := closed.Load(); got != 1 {
			t.Errorf("StateClosed notifications = %d, want 1", got)
		}
	})
	t.Logf("real TCP %s -> %s", client.LocalAddr(), client.RemoteAddr())
	return client, done
}

func streamTCPAcceptanceResponse(t *testing.T, client net.Conn, encoding string) (*http.Response, io.ReadCloser) {
	t.Helper()
	if _, err := fmt.Fprint(client, "GET /stream HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != StatusOK || len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" {
		t.Fatalf("response status/framing = %s / %v", response.Status, response.TransferEncoding)
	}
	wantEncoding := ""
	if encoding == "gzip" {
		wantEncoding = encoding
	}
	if got := response.Header.Get("Content-Encoding"); got != wantEncoding {
		t.Fatalf("Content-Encoding = %q, want %q", got, wantEncoding)
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

// Do not wrap the source to observe Close: only its exact *streamPipe dynamic
// type selects bounded HTTP compression. A BodyWriterTo wrapper would select
// the legacy compressedBodyStream and could produce a false coverage claim.
func streamTCPAcceptanceBoundedReader(t *testing.T, sw StreamWriter) *streamPipe {
	t.Helper()
	r := NewBoundedStreamReader(sw)
	p, ok := r.(*streamPipe)
	if !ok {
		r.Close()
		t.Fatalf("NewBoundedStreamReader returned %T, want exact *streamPipe", r)
	}
	return p
}

func streamTCPAcceptanceBoundedCompressor(t *testing.T, source *streamPipe, encoding string, compressor <-chan io.Reader) *boundedCompressedBodyStream {
	t.Helper()
	if encoding != "gzip" {
		return nil
	}
	select {
	case body := <-compressor:
		compressed, ok := body.(*boundedCompressedBodyStream)
		if !ok {
			t.Fatalf("gzip response stream = %T, want exact *boundedCompressedBodyStream", body)
		}
		if compressed.bodyStream != source {
			t.Fatal("bounded compressor did not retain the exact original *streamPipe")
		}
		return compressed
	case <-time.After(testTimeout(15 * time.Second)):
		t.Fatal("watchdog: missing compressor")
		return nil
	}
}

// Call only after ServeConn and the producer callback have returned. The
// compressor's done channel identifies this instance, rather than relying on a
// process-wide goroutine count or the long-lived stackless worker pool.
func streamTCPAcceptanceReclaimed(t *testing.T, s *Server, source *streamPipe, compressed *boundedCompressedBodyStream) {
	t.Helper()
	streamTCPAcceptanceClosed(t, source, "original bounded bridge")
	if compressed != nil {
		streamTCPAcceptanceWait(t, compressed.done, "bounded codec callback return")
		compressed.originalLock.Lock()
		originalClosed := compressed.originalClosed
		compressed.originalLock.Unlock()
		if !originalClosed {
			t.Error("bounded codec retained an open original source")
		}
		// ServeConn return synchronizes with the lazy r assignment; done
		// synchronizes with codec completion before we inspect this output.
		output, ok := compressed.r.(*streamPipe)
		if !ok {
			t.Fatalf("compressed output bridge = %T, want exact *streamPipe", compressed.r)
		}
		if output == source {
			t.Fatal("compression input and output unexpectedly share a bridge")
		}
		streamTCPAcceptanceClosed(t, output, "compressed bounded bridge")
		// A cancelled bridge intentionally preserves accepted tail bytes.
		// With its only consumer/codec finished, this test owns the drain.
		if _, err := io.Copy(io.Discard, struct{ io.Reader }{output}); err != nil {
			t.Fatal(err)
		}
	}
	// ServeConn has no listener sentinel, so inspect its exact counter rather
	// than GetOpenConnectionsCount, which subtracts the Serve listener slot.
	if open, active := s.open.Load(), s.GetCurrentConcurrency(); open != 0 || active != 0 {
		t.Errorf("server retained connections: open=%d, active=%d", open, active)
	}
	s.idleConnsMu.Lock()
	idle := len(s.idleConns)
	s.idleConnsMu.Unlock()
	if idle != 0 {
		t.Errorf("server retained %d idle-connection entries", idle)
	}
}

func streamTCPAcceptanceClosed(t *testing.T, p *streamPipe, name string) {
	t.Helper()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		t.Fatalf("%s was not closed by the completed HTTP path", name)
	}
}

func streamTCPAcceptanceError(t *testing.T, done <-chan error, event string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(testTimeout(15 * time.Second)):
		t.Fatalf("watchdog waiting for %s", event)
		return nil
	}
}

func streamTCPAcceptanceWait(t *testing.T, done <-chan struct{}, event string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testTimeout(15 * time.Second)):
		t.Fatalf("watchdog waiting for %s", event)
	}
}

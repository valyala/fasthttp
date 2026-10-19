package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

// These tests exercise the explicit bounded public stream API.
// Avoid timing assertions: channels establish ownership and
// completion, and network deadlines are only deadlock watchdogs.
func streamCompatBody(n int) []byte {
	b := make([]byte, n)
	x := uint32(1)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

func streamCompatProducer(body []byte, writeSize, flushEvery int) (StreamWriter, <-chan error) {
	done := make(chan error, 1)
	return func(w *bufio.Writer) {
		buf := make([]byte, writeSize)
		writes := 0
		for off := 0; off < len(body); {
			n := copy(buf, body[off:])
			nw, err := w.Write(buf[:n])
			// An io.Writer must no longer refer to its caller's slice on return.
			clear(buf)
			if err != nil {
				done <- err
				return
			}
			if nw != n {
				done <- io.ErrShortWrite
				return
			}
			off += nw
			writes++
			if flushEvery > 0 && writes%flushEvery == 0 {
				if err := w.Flush(); err != nil {
					done <- err
					return
				}
			}
		}
		done <- w.Flush()
	}, done
}

func TestStreamCompatRequestWriterRoundTrip(t *testing.T) {
	body := streamCompatBody(128*1024 + 17)
	for _, writeSize := range []int{3, 4095, 64 * 1024} {
		for _, flushEvery := range []int{0, 7} {
			t.Run(fmt.Sprintf("write=%d/flush=%d", writeSize, flushEvery), func(t *testing.T) {
				sw, done := streamCompatProducer(body, writeSize, flushEvery)
				var req Request
				req.Header.SetMethod(MethodPost)
				req.SetRequestURI("http://localhost/first")
				req.SetBodyStream(NewBoundedStreamReader(sw), -1)
				var wire bytes.Buffer
				bw := bufio.NewWriter(&wire)
				if err := req.Write(bw); err != nil {
					t.Fatal(err)
				}
				if _, err := bw.WriteString("GET /next HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				if err := bw.Flush(); err != nil {
					t.Fatal(err)
				}
				if err := streamAwait(t, done, "done"); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(&wire)
				decoded, err := http.ReadRequest(br)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(decoded.Body)
				if closeErr := decoded.Body.Close(); err == nil {
					err = closeErr
				}
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("request body len=%d, err=%v", len(got), err)
				}
				next, err := http.ReadRequest(br)
				if err != nil {
					t.Fatal(err)
				}
				streamClose(t, next.Body)
				if next.URL.Path != "/next" || br.Buffered() != 0 {
					t.Fatal("stream request corrupted the following request")
				}
			})
		}
	}
}

func TestStreamCompatFixedLengthFraming(t *testing.T) {
	body := streamCompatBody(192*1024 + 17)
	for _, kind := range []string{"request", "response"} {
		for _, delta := range []int{-1, 0, 1} {
			t.Run(fmt.Sprintf("%s/declaredDelta=%d", kind, delta), func(t *testing.T) {
				sw, done := streamCompatProducer(body, 64*1024, 0)
				reader := NewBoundedStreamReader(sw)
				defer streamClose(t, reader)
				size := len(body) + delta
				var wire bytes.Buffer
				bw := bufio.NewWriter(&wire)
				var err error
				if kind == "request" {
					var req Request
					req.SetRequestURI("http://localhost/fixed")
					req.Header.SetMethod(MethodPost)
					req.SetBodyStream(reader, size)
					err = req.Write(bw)
				} else {
					var resp Response
					resp.SetBodyStream(reader, size)
					err = resp.Write(bw)
				}
				switch {
				case delta < 0 && !errors.Is(err, errBodyStreamTooLong):
					t.Fatalf("oversized body error = %v", err)
				case delta > 0 && err == nil:
					t.Fatal("short stream accepted")
				case delta == 0 && err != nil:
					t.Fatal(err)
				}
				if err := bw.Flush(); err != nil {
					t.Fatal(err)
				}
				_ = streamAwait(t, done, "done")
				_, payload, found := bytes.Cut(wire.Bytes(), []byte("\r\n\r\n"))
				if !found || len(payload) != min(len(body), size) || !bytes.Equal(payload, body[:len(payload)]) {
					t.Fatalf("body framing mismatch: payload=%d, size=%d", len(payload), size)
				}
				// Closing twice must leave any accepted, unwritten tail readable.
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, struct{ io.Reader }{reader}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// Detailed exact/short/long framing is covered above. At the connection level,
// verify that a framing error prevents reuse for a pipelined request.
func TestStreamCompatFixedLengthErrorClosesConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := streamCompatBody(192*1024 + 17)
		sw, producerDone := streamCompatProducer(body, 64*1024, 0)
		calls := 0
		s := Server{NoDefaultDate: true, Handler: func(ctx *RequestCtx) {
			calls++
			ctx.SetBodyStream(NewBoundedStreamReader(sw), len(body)-1)
		}}
		client, serverDone := streamCompatServeConn(t, &s)
		if _, err := io.WriteString(client, "GET /first HTTP/1.1\r\nHost: localhost\r\n\r\nGET /next HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(client)
		first, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(first.Body)
		if closeErr := first.Body.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		// The final buffered bytes may be discarded on error; an overrun or a
		// second response must never be sent.
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
		if len(got) >= len(body) || !bytes.Equal(got, body[:len(got)]) {
			t.Fatal("fixed-length response corrupted")
		}
		if next, err := http.ReadResponse(br, nil); err == nil {
			if closeErr := next.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			t.Fatal("server reused connection after body-length error")
		}
		serveErr := <-serverDone
		<-producerDone
		if !errors.Is(serveErr, errBodyStreamTooLong) || calls != 1 {
			t.Fatalf("ServeConn=%v, handler calls=%d after framing error", serveErr, calls)
		}
	})
}

type streamCompatCodec struct {
	name   string
	encode func(*Response, int)
	decode func(*Response) ([]byte, error)
}

var streamCompatCodecs = []streamCompatCodec{
	{"gzip", (*Response).gzipBody, (*Response).BodyGunzip},
	{"deflate", (*Response).deflateBody, (*Response).BodyInflate},
	{"br", (*Response).brotliBody, (*Response).BodyUnbrotli},
	{"zstd", (*Response).zstdBody, (*Response).BodyUnzstd},
}

func TestStreamCompatHTTPCompressionTrailersAndKeepAlive(t *testing.T) {
	body := streamCompatBody(128*1024 + 17)
	codecs := append([]streamCompatCodec{{name: "identity", decode: func(r *Response) ([]byte, error) { return r.Body(), nil }}}, streamCompatCodecs...)
	for _, codec := range codecs {
		for _, writeSize := range []int{5, 64 * 1024} {
			t.Run(fmt.Sprintf("%s/write=%d", codec.name, writeSize), func(t *testing.T) {
				sw, producerDone := streamCompatProducer(body, writeSize, 7)
				s := Server{NoDefaultDate: true, Handler: CompressHandlerBrotliLevel(func(ctx *RequestCtx) {
					if string(ctx.Path()) == "/next" {
						ctx.SetBodyString("next")
						return
					}
					ctx.SetContentType("text/plain")
					if err := ctx.Response.Header.AddTrailer("X-Stream-Complete"); err != nil {
						t.Error(err)
					}
					ctx.Response.Header.Set("X-Stream-Complete", "yes")
					ctx.SetBodyStream(NewBoundedStreamReader(sw), -1)
				}, CompressBrotliBestSpeed, CompressBestSpeed)}
				client, serverDone := streamCompatServeConn(t, &s)
				if _, err := fmt.Fprintf(client, "GET /first HTTP/1.1\r\nHost: localhost\r\nAccept-Encoding: %s\r\n\r\n", codec.name); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(client)
				httpResp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatal(err)
				}
				compressed, err := io.ReadAll(httpResp.Body)
				if closeErr := httpResp.Body.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatal(err)
				}
				wantEncoding := codec.name
				if wantEncoding == "identity" {
					wantEncoding = ""
				}
				if got := httpResp.Header.Get("Content-Encoding"); got != wantEncoding {
					t.Fatalf("Content-Encoding=%q, want %q", got, wantEncoding)
				}
				if got := httpResp.Trailer.Get("X-Stream-Complete"); got != "yes" {
					t.Fatalf("trailer=%q", got)
				}
				var decoded Response
				decoded.SetBodyRaw(compressed)
				got, err := codec.decode(&decoded)
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("decoded body len=%d, err=%v", len(got), err)
				}
				if err := streamAwait(t, producerDone, "producerDone"); err != nil {
					t.Fatal(err)
				}
				streamCompatNextResponse(t, client, br)
				if err := streamAwait(t, serverDone, "serverDone"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestStreamCompatCompressionCloseCancelsProducer(t *testing.T) {
	for _, codec := range streamCompatCodecs {
		for _, started := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/started=%t", codec.name, started), func(t *testing.T) {
				done := make(chan error, 1)
				original := NewBoundedStreamReader(func(w *bufio.Writer) {
					body := streamCompatBody(64 * 1024)
					for {
						if _, err := w.Write(body); err != nil {
							done <- err
							return
						}
						if err := w.Flush(); err != nil {
							done <- err
							return
						}
					}
				})
				defer streamClose(t, original)
				var resp Response
				resp.Header.SetContentType("text/plain")
				resp.SetBodyStream(original, -1)
				codec.encode(&resp, CompressBestSpeed)
				compressed, ok := resp.BodyStream().(*boundedCompressedBodyStream)
				if !ok {
					t.Fatalf("expected compressedBodyStream, got %T", resp.BodyStream())
				}
				if started {
					if n, err := compressed.Read(make([]byte, 1)); n != 1 || err != nil {
						t.Fatal(n, err)
					}
					if _, ok := compressed.r.(*streamPipe); !ok {
						t.Fatalf("compressed output is not bounded: %T", compressed.r)
					}
				}
				for range 2 {
					if err := resp.CloseBodyStream(); err != nil {
						t.Fatal(err)
					}
				}
				if err := streamAwait(t, done, "done"); err == nil {
					t.Fatal("closing compressed stream did not stop original producer")
				}
				if started {
					streamAwait(t, compressed.done, "compressor completion")
				} else if _, err := compressed.Read(nil); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("unstarted closed stream read error=%v", err)
				}
				// Close remains idempotent after the compression goroutine exits.
				if err := compressed.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func streamCompatInfiniteProducer() (io.ReadCloser, <-chan error) {
	done := make(chan error, 1)
	r := NewBoundedStreamReader(func(w *bufio.Writer) {
		body := streamCompatBody(64 * 1024)
		for {
			if _, err := w.Write(body); err != nil {
				done <- err
				return
			}
		}
	})
	return r, done
}

func TestStreamCompatSkipBodyClosesProducer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		size, status int
		gzip         bool
	}{
		{"explicit-fixed", 192 * 1024, StatusOK, false},
		{"no-content", -1, StatusNoContent, false},
		{"not-modified", -1, StatusNotModified, false},
		{"compressed", -1, StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, done := streamCompatInfiniteProducer()
			defer streamClose(t, original)
			var resp Response
			resp.SetStatusCode(tc.status)
			resp.Header.SetContentType("text/plain")
			resp.SetBodyStream(original, tc.size)
			if tc.gzip {
				resp.gzipBody(CompressBestSpeed)
			}
			resp.SkipBody = tc.status == StatusOK
			var wire bytes.Buffer
			bw := bufio.NewWriter(&wire)
			if err := resp.Write(bw); err != nil {
				t.Fatal(err)
			}
			if err := bw.Flush(); err != nil {
				t.Fatal(err)
			}
			_, payload, found := bytes.Cut(wire.Bytes(), []byte("\r\n\r\n"))
			if !found || len(payload) != 0 {
				t.Fatalf("suppressed body wrote %d bytes", len(payload))
			}
			if err := streamAwait(t, done, "suppressed producer"); err == nil {
				t.Fatal("suppressed body left producer running")
			}
		})
	}
}

func TestStreamCompatHEADKeepAliveClosesProducer(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			original, producerDone := streamCompatInfiniteProducer()
			defer streamClose(t, original)
			s := Server{NoDefaultDate: true, Handler: CompressHandlerBrotliLevel(func(ctx *RequestCtx) {
				if ctx.IsHead() {
					ctx.SetContentType("text/plain")
					ctx.SetBodyStream(original, -1)
				} else {
					ctx.SetBodyString("next")
				}
			}, CompressBrotliBestSpeed, CompressBestSpeed)}
			client, serverDone := streamCompatServeConn(t, &s)
			if _, err := fmt.Fprintf(client, "HEAD / HTTP/1.1\r\nHost: localhost\r\nAccept-Encoding: %s\r\n\r\n", encoding); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(client)
			first, err := http.ReadResponse(br, &http.Request{Method: MethodHead})
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(first.Body)
			if closeErr := first.Body.Close(); err == nil {
				err = closeErr
			}
			if err != nil || len(got) != 0 {
				t.Fatalf("HEAD body length=%d, err=%v", len(got), err)
			}
			if err := streamAwait(t, producerDone, "producerDone"); err == nil {
				t.Fatal("HEAD did not stop producer")
			}
			streamCompatNextResponse(t, client, br)
			if err := streamAwait(t, serverDone, "serverDone"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type streamCompatFailWriter struct{ err error }

func (w streamCompatFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestStreamCompatEarlyOutputErrorClosesProducer(t *testing.T) {
	sentinel := errors.New("header destination failed")
	for _, kind := range []string{"request", "response"} {
		t.Run(kind, func(t *testing.T) {
			original, done := streamCompatInfiniteProducer()
			defer streamClose(t, original)
			bw := bufio.NewWriterSize(streamCompatFailWriter{sentinel}, 16)
			var err error
			if kind == "request" {
				var req Request
				req.Header.SetMethod(MethodPost)
				req.SetRequestURI("http://localhost/")
				req.SetBodyStream(original, -1)
				err = req.Write(bw)
			} else {
				var resp Response
				resp.SetBodyStream(original, -1)
				err = resp.Write(bw)
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("write error=%v, want %v", err, sentinel)
			}
			if err := streamAwait(t, done, "done"); err == nil {
				t.Fatal("header write failure left producer running")
			}
		})
	}
}

func TestStreamCompatCompressedHTTPDisconnectStopsProducer(t *testing.T) {
	for _, codec := range streamCompatCodecs[1:] {
		t.Run(codec.name, func(t *testing.T) {
			original, producerDone := streamCompatInfiniteProducer()
			defer streamClose(t, original)
			compressor := make(chan *boundedCompressedBodyStream, 1)
			s := Server{NoDefaultDate: true, Handler: func(ctx *RequestCtx) {
				ctx.SetContentType("text/plain")
				ctx.SetBodyStream(original, -1)
				codec.encode(&ctx.Response, CompressBestSpeed)
				compressed, ok := ctx.Response.BodyStream().(*boundedCompressedBodyStream)
				if !ok {
					t.Errorf("unexpected body type %T", ctx.Response.BodyStream())
					return
				}
				compressor <- compressed
			}}
			client, serverDone := streamCompatServeConn(t, &s)
			if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			// The client was deliberately closed before discarding its body.
			if err := resp.Body.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
			if err := streamAwait(t, serverDone, "serverDone"); err == nil {
				t.Fatal("expected connection error")
			}
			if err := streamAwait(t, producerDone, "producerDone"); err == nil {
				t.Fatal("disconnect left producer running")
			}
			compressed := streamAwait(t, compressor, "HTTP compressor")
			streamAwait(t, compressed.done, "compressor completion")
		})
	}
}

func TestStreamCompressedReadWriteToResume(t *testing.T) {
	body := streamCompatBody(128*1024 + 17)
	// Resume belongs to the shared bounded wrapper; all codecs have round-trip
	// and cancellation coverage in the tables above.
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			sw, done := streamCompatProducer(body, 64*1024, 1)
			var resp Response
			resp.Header.SetContentType("text/plain")
			resp.SetBodyStream(NewBoundedStreamReader(sw), -1)
			resp.gzipBody(CompressBestSpeed)
			defer func() {
				if err := resp.CloseBodyStream(); err != nil {
					t.Error(err)
				}
			}()
			reader := resp.BodyStream()
			wt, ok := reader.(BodyWriterTo)
			if !ok || !wt.SupportsBodyWriteTo() {
				t.Fatal("compressed bounded stream lost its explicit WriteTo capability")
			}
			prefix := make([]byte, 7)
			if _, err := io.ReadFull(reader, prefix); err != nil {
				t.Fatal(err)
			}
			// WriterTo alone cannot prove the compressed output byte bound.
			// Verify that the lazy output really uses the quota-tested bridge.
			bounded, ok := reader.(*boundedCompressedBodyStream)
			if !ok {
				t.Fatalf("compressed stream type = %T", reader)
			}
			if _, ok := bounded.r.(*streamPipe); !ok {
				t.Fatalf("compressed output is not bounded: %T", bounded.r)
			}
			var encoded bytes.Buffer
			encoded.Write(prefix)
			if partial {
				sentinel := errors.New("compressed destination failed")
				dst := streamBoundaryWriter{failAt: 1, n: 13, err: sentinel}
				n, err := wt.WriteTo(&dst)
				if !errors.Is(err, sentinel) || n != int64(dst.dst.Len()) || n != 13 {
					t.Fatalf("encoded accepted count=%d,%v, destination bytes=%d", n, err, dst.dst.Len())
				}
				encoded.Write(dst.dst.Bytes())
				// Read must resume the same encoded stream and retained output
				// prefix, without starting another codec/header after failure.
				tail, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				encoded.Write(tail)
			} else {
				before := encoded.Len()
				n, err := wt.WriteTo(&encoded)
				if err != nil || n != int64(encoded.Len()-before) {
					t.Fatalf("encoded WriteTo count=%d,%v; actual=%d", n, err, encoded.Len()-before)
				}
			}
			var extra bytes.Buffer
			if n, err := wt.WriteTo(&extra); n != 0 || err != nil || extra.Len() != 0 {
				t.Fatalf("repeated WriteTo after EOF=%d,%v, bytes=%d", n, err, extra.Len())
			}
			var decoded Response
			decoded.SetBodyRaw(encoded.Bytes())
			got, err := decoded.BodyGunzip()
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("resumed compressed body len=%d,err=%v; expected %d", len(got), err, len(body))
			}
			if err := streamAwait(t, done, "done"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStreamCompatReadyHTTPBurstFlushBeforeFutureData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready, finish := make(chan error, 1), make(chan struct{})
		r := streamGatedBurst(ready, finish)
		defer streamClose(t, r)
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		server, client := net.Pipe()
		defer streamClose(t, client)
		done := make(chan error, 1)
		go func() {
			defer streamClose(t, server)
			var resp Response
			resp.Header.noDefaultDate = true
			resp.SetBodyStream(r, -1)
			bw := bufio.NewWriterSize(server, 64*1024)
			err := resp.Write(bw)
			if err == nil {
				err = bw.Flush()
			}
			done <- err
		}()
		response, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer streamClose(t, response.Body)
		got := make([]byte, 512)
		if _, err := io.ReadFull(response.Body, got); err != nil || !bytes.Equal(got, bytes.Repeat([]byte("v"), 512)) {
			t.Fatalf("ready HTTP data=%d,%v", len(got), err)
		}
		close(finish)
		if tail, err := io.ReadAll(response.Body); err != nil || len(tail) != 0 {
			t.Fatal(len(tail), err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// Use for waits outside synctest bubbles, notably real TCP and codec workers.
// Pure bubble-local channel/Cond deadlocks are diagnosed by synctest itself.
func streamAwait[T any](t testing.TB, done <-chan T, event string) T {
	t.Helper()
	select {
	case value := <-done:
		return value
	case <-time.After(testTimeout(15 * time.Second)):
		t.Fatalf("timeout waiting for %s", event)
		var zero T
		return zero
	}
}

func streamClose(t testing.TB, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Error(err)
	}
}

// Share connection setup without hiding each test's request/response exchange.
func streamCompatServeConn(t *testing.T, s *Server) (net.Conn, <-chan error) {
	t.Helper()
	server, client := net.Pipe()
	if err := client.SetDeadline(time.Now().Add(testTimeout(5 * time.Second))); err != nil {
		t.Fatal(err)
	}
	done, returned := make(chan error, 1), make(chan struct{})
	go func() { done <- s.ServeConn(server); close(returned) }()
	t.Cleanup(func() {
		streamClose(t, client)
		streamClose(t, server)
		streamAwait(t, returned, "ServeConn cleanup")
	})
	return client, done
}

func streamCompatNextResponse(t *testing.T, client net.Conn, br *bufio.Reader) {
	t.Helper()
	if _, err := io.WriteString(client, "GET /next HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	next, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(next.Body)
	if closeErr := next.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil || string(got) != "next" {
		t.Fatal(string(got), err)
	}
}

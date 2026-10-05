package http2

import (
	"bytes"
	"io"
	"net"
	stdhttp "net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// Tests that the work a peer can ask for stays bounded by what the connection
// allows, including peers that are buggy or badly tuned.
// openRawPeer opens a connection with the preface sent, for tests that write
// frames a well-behaved client would not.
func openRawPeer(t *testing.T, server *testServer) (net.Conn, *xhttp2.Framer) {
	t.Helper()
	conn, err := net.Dial("tcp", server.listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write([]byte(xhttp2.ClientPreface)); err != nil {
		t.Fatalf("writing preface: %v", err)
	}
	framer := xhttp2.NewFramer(conn, conn)
	if err := framer.WriteSettings(); err != nil {
		t.Fatalf("writing settings: %v", err)
	}
	return conn, framer
}

func peerRequestBlock(t *testing.T, method string) []byte {
	t.Helper()
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: method},
		{Name: ":path", Value: "/"},
		{Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.com"},
	} {
		if err := encoder.WriteField(field); err != nil {
			t.Fatalf("encoding %s: %v", field.Name, err)
		}
	}
	return block.Bytes()
}

// TestServerRapidResetKeepsConcurrencyBounded checks that a peer opening a
// stream and cancelling it immediately cannot get more handlers running than
// it is allowed concurrent streams.
func TestServerRapidResetKeepsConcurrencyBounded(t *testing.T) {
	t.Parallel()

	var started atomic.Int64
	release := make(chan struct{})
	defer close(release)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			started.Add(1)
			<-release
		},
	}
	const maxStreams = 32
	testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: maxStreams})
	_, framer := openRawPeer(t, testServer)
	go drainFrames(framer)

	block := peerRequestBlock(t, stdhttp.MethodGet)
	const attempts = 4000
	for id := uint32(1); id < attempts*2; id += 2 {
		if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
			StreamID: id, BlockFragment: block, EndStream: true, EndHeaders: true,
		}); err != nil {
			break
		}
		if err := framer.WriteRSTStream(id, xhttp2.ErrCodeCancel); err != nil {
			break
		}
	}
	time.Sleep(200 * time.Millisecond)

	// Every handler is parked, so whatever started is still in flight.
	if got := started.Load(); got > maxStreams {
		t.Fatalf("%d handlers in flight after %d reset streams, expecting at most %d",
			got, attempts, maxStreams)
	}
}

// TestServerUnendingHeaderBlockIsBounded checks that a header block which
// never ends is not accumulated without limit.
func TestServerUnendingHeaderBlockIsBounded(t *testing.T) {
	t.Parallel()

	testServer := newTestServer(t, &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {},
	}, ServerConfig{})
	_, framer := openRawPeer(t, testServer)
	go drainFrames(framer)

	if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID: 1, BlockFragment: peerRequestBlock(t, stdhttp.MethodGet), EndHeaders: false,
	}); err != nil {
		t.Fatalf("writing headers: %v", err)
	}

	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	if err := encoder.WriteField(hpack.HeaderField{
		Name: "x-pad", Value: string(bytes.Repeat([]byte("v"), 1024)),
	}); err != nil {
		t.Fatalf("encoding padding: %v", err)
	}
	fragment := block.Bytes()

	const attempts = 100_000
	accepted := 0
	for range attempts {
		if err := framer.WriteContinuation(1, false, fragment); err != nil {
			break
		}
		accepted++
	}
	if accepted == attempts {
		t.Fatalf("the connection took %d CONTINUATION frames without the block ending", accepted)
	}
}

// TestServerRepeatedFramesStayBounded checks the frames that ask for an
// answer or carry no progress: none may let a peer outpace what the
// connection holds.
func TestServerRepeatedFramesStayBounded(t *testing.T) {
	t.Parallel()

	for name, write := range map[string]func(*xhttp2.Framer) error{
		"ping":     func(f *xhttp2.Framer) error { return f.WritePing(false, [8]byte{}) },
		"settings": func(f *xhttp2.Framer) error { return f.WriteSettings() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			testServer := newTestServer(t, &fasthttp.Server{
				Handler: func(ctx *fasthttp.RequestCtx) {},
			}, ServerConfig{})
			// Nothing reads the socket, so anything the server queues in
			// answer has to be held somewhere.
			conn, framer := openRawPeer(t, testServer)

			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if err := conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
					break
				}
				if err := write(framer); err != nil {
					break // back pressure reached us, which is the point
				}
			}

			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			if grown := int64(after.HeapInuse) - int64(before.HeapInuse); grown > 64<<20 {
				t.Fatalf("heap grew %d MiB answering repeated %s frames", grown>>20, name)
			}
		})
	}
}

// TestServerRequestCtxCacheIsFreedWithTheConnection locks the documented
// contract for the per-connection body cache: bounded while the connection
// lives, gone when it closes.
func TestServerRequestCtxCacheIsFreedWithTheConnection(t *testing.T) {
	testServer := newTestServer(t, &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
	}, ServerConfig{})

	body := bytes.Repeat([]byte("x"), 1<<20)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var rounds [2]int64
	for round := range rounds {
		var wait sync.WaitGroup
		for range 64 {
			wait.Go(func() {
				request, err := stdhttp.NewRequest(stdhttp.MethodPost, testServer.URL("/"), bytes.NewReader(body))
				if err != nil {
					return
				}
				response, err := testServer.client.Do(request)
				if err != nil {
					return
				}
				io.Copy(io.Discard, response.Body) //nolint:errcheck
				response.Body.Close()
			})
		}
		wait.Wait()
		runtime.GC()
		var now runtime.MemStats
		runtime.ReadMemStats(&now)
		rounds[round] = int64(now.HeapInuse) - int64(before.HeapInuse)
	}
	// The cache is reused, not grown, by a second round of the same work.
	if rounds[1] > rounds[0]+(64<<20) {
		t.Fatalf("a second round of identical requests added %d MiB", (rounds[1]-rounds[0])>>20)
	}

	testServer.transport.CloseIdleConnections()
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	var closed runtime.MemStats
	runtime.ReadMemStats(&closed)
	if retained := int64(closed.HeapInuse) - int64(before.HeapInuse); retained > 64<<20 {
		t.Fatalf("%d MiB still held after the connection closed", retained>>20)
	}
}

func drainFrames(framer *xhttp2.Framer) {
	for {
		if _, err := framer.ReadFrame(); err != nil {
			return
		}
	}
}

// TestClientSurvivesRepeatingUpstream points the transport at a peer that
// answers the preface and then repeats frames without pause: what a server can
// send is bounded by the same rules as what a client can.
func TestClientSurvivesRepeatingUpstream(t *testing.T) {
	t.Parallel()

	for name, repeat := range map[string]func(*xhttp2.Framer){
		"ping": func(framer *xhttp2.Framer) {
			for range 200_000 {
				if err := framer.WritePing(false, [8]byte{}); err != nil {
					return
				}
			}
		},
		"settings": func(framer *xhttp2.Framer) {
			for range 200_000 {
				if err := framer.WriteSettings(); err != nil {
					return
				}
			}
		},
		"continuation": func(framer *xhttp2.Framer) {
			var block bytes.Buffer
			encoder := hpack.NewEncoder(&block)
			if err := encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"}); err != nil {
				return
			}
			if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
				StreamID: 1, BlockFragment: block.Bytes(), EndHeaders: false,
			}); err != nil {
				return
			}
			block.Reset()
			if err := encoder.WriteField(hpack.HeaderField{
				Name: "x-pad", Value: string(bytes.Repeat([]byte("v"), 4096)),
			}); err != nil {
				return
			}
			for range 200_000 {
				if err := framer.WriteContinuation(1, false, block.Bytes()); err != nil {
					return
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listening: %v", err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				preface := make([]byte, len(xhttp2.ClientPreface))
				if _, err := io.ReadFull(conn, preface); err != nil {
					return
				}
				framer := xhttp2.NewFramer(conn, conn)
				if err := framer.WriteSettings(); err != nil {
					return
				}
				go drainFrames(framer)
				repeat(framer)
			}()

			hostClient := &fasthttp.HostClient{Addr: listener.Addr().String()}
			if err := ConfigureHostClient(hostClient, ClientConfig{Mode: PriorKnowledge}); err != nil {
				t.Fatalf("ConfigureHostClient() error: %v", err)
			}
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			request := fasthttp.AcquireRequest()
			defer fasthttp.ReleaseRequest(request)
			response := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(response)
			request.SetRequestURI("http://" + listener.Addr().String() + "/")

			done := make(chan error, 1)
			go func() { done <- hostClient.DoTimeout(request, response, 3*time.Second) }()
			select {
			case <-done: // any outcome is fine; hanging or growing is not
			case <-time.After(10 * time.Second):
				t.Fatal("the request never returned")
			}

			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			if grown := int64(after.HeapInuse) - int64(before.HeapInuse); grown > 64<<20 {
				t.Fatalf("repeated %s frames from the peer grew the client heap by %d MiB", name, grown>>20)
			}
		})
	}
}

package fasthttp

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

const hijackUpgradeRequest = "GET / HTTP/1.1\r\nHost: example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"

func newHijackServer(h HijackHandler) *Server {
	return &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetStatusCode(StatusSwitchingProtocols)
			ctx.Response.Header.Set("Connection", "Upgrade")
			ctx.Response.Header.Set("Upgrade", "websocket")
			ctx.Hijack(h)
		},
	}
}

func readHijackResponse(t *testing.T, c net.Conn) *bufio.Reader {
	t.Helper()

	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("cannot read hijack response: %v", err)
	}
	if resp.StatusCode() != StatusSwitchingProtocols {
		t.Fatalf("unexpected hijack response status: %d", resp.StatusCode())
	}
	return br
}

func serveHijackConn(t *testing.T, s *Server, c net.Conn) {
	t.Helper()

	go func() {
		if err := s.ServeConn(c); err != nil {
			t.Errorf("ServeConn returned an error: %v", err)
		}
	}()
}

func TestRequestCtxHijackReleasesEmptyReader(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	hijackReaderIsBuffered := make(chan bool, 1)
	serveHijackConn(t, newHijackServer(func(c net.Conn) {
		hijacked, ok := c.(*hijackConn)
		if !ok {
			t.Errorf("unexpected hijacked connection type %T", c)
			return
		}
		_, buffered := hijacked.r.(*bufio.Reader)
		hijackReaderIsBuffered <- buffered
	}), server)

	if _, err := io.WriteString(client, hijackUpgradeRequest); err != nil {
		t.Fatalf("cannot write upgrade request: %v", err)
	}
	readHijackResponse(t, client)

	select {
	case buffered := <-hijackReaderIsBuffered:
		if buffered {
			t.Fatal("empty hijack retained the server buffered reader")
		}
	case <-time.After(time.Second):
		t.Fatal("hijack handler did not run")
	}
}

func TestRequestCtxHijackReadsCoalescedFrame(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	frame := []byte("\x81\x02ok")
	serveHijackConn(t, newHijackServer(func(c net.Conn) {
		got := make([]byte, len(frame))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Errorf("cannot read coalesced frame: %v", err)
			return
		}
		if _, err := c.Write(got); err != nil {
			t.Errorf("cannot echo coalesced frame: %v", err)
		}
	}), server)

	if _, err := io.WriteString(client, hijackUpgradeRequest+string(frame)); err != nil {
		t.Fatalf("cannot write upgrade and frame: %v", err)
	}
	br := readHijackResponse(t, client)

	got := make([]byte, len(frame))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("cannot read echoed coalesced frame: %v", err)
	}
	if string(got) != string(frame) {
		t.Fatalf("unexpected echoed coalesced frame %q; want %q", got, frame)
	}
}

func TestRequestCtxHijackReadsPartialFrameAcrossReaderAndConn(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	frame := []byte("\x81\x04test")
	first := frame[:2]
	serveHijackConn(t, newHijackServer(func(c net.Conn) {
		got := make([]byte, len(frame))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Errorf("cannot read split frame: %v", err)
			return
		}
		if _, err := c.Write(got); err != nil {
			t.Errorf("cannot echo split frame: %v", err)
		}
	}), server)

	if _, err := io.WriteString(client, hijackUpgradeRequest+string(first)); err != nil {
		t.Fatalf("cannot write upgrade and first frame bytes: %v", err)
	}
	br := readHijackResponse(t, client)
	if _, err := client.Write(frame[len(first):]); err != nil {
		t.Fatalf("cannot write remaining frame bytes: %v", err)
	}

	got := make([]byte, len(frame))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("cannot read echoed split frame: %v", err)
	}
	if string(got) != string(frame) {
		t.Fatalf("unexpected echoed split frame %q; want %q", got, frame)
	}
}

func TestRequestCtxHijackTLSReadsAfterEmptyReaderRelease(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	t.Cleanup(func() { _ = clientRaw.Close() })

	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatalf("cannot generate TLS certificate: %v", err)
	}
	certificate, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatalf("cannot load TLS certificate: %v", err)
	}
	serverTLS := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{certificate}})
	clientTLS := tls.Client(clientRaw, &tls.Config{InsecureSkipVerify: true})

	frame := []byte("\x81\x02ok")
	serveHijackConn(t, newHijackServer(func(c net.Conn) {
		hijacked, ok := c.(*hijackConn)
		if !ok {
			t.Errorf("unexpected hijacked connection type %T", c)
			return
		}
		if _, buffered := hijacked.r.(*bufio.Reader); buffered {
			t.Error("TLS hijack retained an empty server buffered reader")
		}
		got := make([]byte, len(frame))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Errorf("cannot read TLS frame after hijack: %v", err)
			return
		}
		if _, err := c.Write(got); err != nil {
			t.Errorf("cannot echo TLS frame after hijack: %v", err)
		}
	}), serverTLS)

	if _, err := io.WriteString(clientTLS, hijackUpgradeRequest); err != nil {
		t.Fatalf("cannot write TLS upgrade request: %v", err)
	}
	br := readHijackResponse(t, clientTLS)
	if _, err := clientTLS.Write(frame); err != nil {
		t.Fatalf("cannot write TLS frame: %v", err)
	}

	got := make([]byte, len(frame))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("cannot read echoed TLS frame: %v", err)
	}
	if string(got) != string(frame) {
		t.Fatalf("unexpected echoed TLS frame %q; want %q", got, frame)
	}
}

func TestRequestCtxHijackFlushesSwitchingProtocolsBeforeHandlerWrite(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	payload := []byte("hijack-data")
	serveHijackConn(t, newHijackServer(func(c net.Conn) {
		if _, err := c.Write(payload); err != nil {
			t.Errorf("cannot write hijack payload: %v", err)
		}
	}), server)

	if _, err := io.WriteString(client, hijackUpgradeRequest); err != nil {
		t.Fatalf("cannot write upgrade request: %v", err)
	}
	br := readHijackResponse(t, client)
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("cannot read hijack payload: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("unexpected hijack payload %q; want %q", got, payload)
	}
}

func BenchmarkServerHijackEmptyReaderRetention(b *testing.B) {
	const connections = 1000

	var retainedPerSocket float64
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		runtime.GC()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		release := make(chan struct{})
		var started, finished sync.WaitGroup
		started.Add(connections)
		finished.Add(connections)
		s := &Server{
			KeepHijackedConns: true,
			ReadBufferSize:    32 * 1024,
			Handler: func(ctx *RequestCtx) {
				ctx.SetStatusCode(StatusSwitchingProtocols)
				ctx.Hijack(func(net.Conn) {
					started.Done()
					<-release
					finished.Done()
				})
			},
		}
		clients := make([]net.Conn, 0, connections)
		for j := 0; j < connections; j++ {
			client, server := net.Pipe()
			clients = append(clients, client)
			go func() {
				if err := s.ServeConn(server); err != nil {
					b.Errorf("ServeConn returned an error: %v", err)
				}
			}()
			if _, err := io.WriteString(client, hijackUpgradeRequest); err != nil {
				b.Fatalf("cannot write upgrade request: %v", err)
			}
			br := bufio.NewReader(client)
			var resp Response
			if err := resp.Read(br); err != nil {
				b.Fatalf("cannot read hijack response: %v", err)
			}
			if resp.StatusCode() != StatusSwitchingProtocols {
				b.Fatalf("unexpected hijack response status: %d", resp.StatusCode())
			}
		}
		started.Wait()
		runtime.GC()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		retainedPerSocket = float64(after.HeapAlloc-before.HeapAlloc) / connections

		b.StartTimer()
		close(release)
		for _, client := range clients {
			_ = client.Close()
		}
		finished.Wait()
	}
	b.ReportMetric(retainedPerSocket, "retained-B/socket")
}

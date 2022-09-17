package fasthttputil_test

import (
	"crypto/tls"
	"net"
	"testing"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// BenchmarkPlainStreaming measures end-to-end plaintext streaming performance
// for fasthttp client and server.
//
// It issues http requests over a small number of keep-alive connections.
func BenchmarkPlainStreaming(b *testing.B) {
	benchmark(b, streamingHandler, false)
}

// BenchmarkPlainHandshake measures end-to-end plaintext handshake performance
// for fasthttp client and server.
//
// It re-establishes new connection per each http request.
func BenchmarkPlainHandshake(b *testing.B) {
	benchmark(b, handshakeHandler, false)
}

// BenchmarkTLSStreaming measures end-to-end TLS streaming performance
// for fasthttp client and server.
//
// It issues http requests over a small number of TLS keep-alive connections.
func BenchmarkTLSStreaming(b *testing.B) {
	benchmark(b, streamingHandler, true)
}

func benchmark(b *testing.B, h fasthttp.RequestHandler, isTLS bool) {
	var serverTLSConfig, clientTLSConfig *tls.Config
	if isTLS {
		certFile := "rsa.pem"
		keyFile := "rsa.key"
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			b.Fatalf("cannot load TLS certificate from certFile=%q, keyFile=%q: %v", certFile, keyFile, err)
		}
		serverTLSConfig = &tls.Config{
			Certificates:             []tls.Certificate{cert},
			PreferServerCipherSuites: true,
		}
		serverTLSConfig.CurvePreferences = []tls.CurveID{}
		clientTLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
	ln := fasthttputil.NewInmemoryListener()
	serverStopCh := make(chan struct{})
	go func() {
		serverLn := net.Listener(ln)
		if serverTLSConfig != nil {
			serverLn = tls.NewListener(serverLn, serverTLSConfig)
		}
		if err := fasthttp.Serve(serverLn, h); err != nil {
			b.Errorf("unexpected error in server: %v", err)
		}
		close(serverStopCh)
	}()
	c := &fasthttp.HostClient{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		IsTLS:     isTLS,
		TLSConfig: clientTLSConfig,
	}

	b.RunParallel(func(pb *testing.PB) {
		runRequests(b, pb, c, isTLS)
	})
	ln.Close()
	<-serverStopCh
}

func streamingHandler(ctx *fasthttp.RequestCtx) {
	ctx.WriteString("foobar") //nolint:errcheck
}

func handshakeHandler(ctx *fasthttp.RequestCtx) {
	streamingHandler(ctx)

	// Explicitly close connection after each response.
	ctx.SetConnectionClose()
}

func runRequests(b *testing.B, pb *testing.PB, c *fasthttp.HostClient, isTLS bool) {
	var req fasthttp.Request
	if isTLS {
		req.SetRequestURI("https://foo.bar/baz")
	} else {
		req.SetRequestURI("http://foo.bar/baz")
	}
	var resp fasthttp.Response
	for pb.Next() {
		if err := c.Do(&req, &resp); err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			b.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), fasthttp.StatusOK)
		}
	}
}

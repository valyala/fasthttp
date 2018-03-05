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

// BenchmarkTLSHandshake measures end-to-end TLS handshake performance
// for fasthttp client and server.
//
// It re-establishes new TLS connection per each http request.
func BenchmarkTLSHandshakeRSAWithClientSessionCache(b *testing.B) {
	bc := &benchConfig{
		IsTLS: true,
		DisableClientSessionCache: false,
	}
	benchmarkExt(b, handshakeHandler, bc)
}

func BenchmarkTLSHandshakeRSAWithoutClientSessionCache(b *testing.B) {
	bc := &benchConfig{
		IsTLS: true,
		DisableClientSessionCache: true,
	}
	benchmarkExt(b, handshakeHandler, bc)
}

func BenchmarkTLSHandshakeECDSAWithClientSessionCache(b *testing.B) {
	bc := &benchConfig{
		IsTLS: true,
		DisableClientSessionCache: false,
		UseECDSA:                  true,
	}
	benchmarkExt(b, handshakeHandler, bc)
}

func BenchmarkTLSHandshakeECDSAWithoutClientSessionCache(b *testing.B) {
	bc := &benchConfig{
		IsTLS: true,
		DisableClientSessionCache: true,
		UseECDSA:                  true,
	}
	benchmarkExt(b, handshakeHandler, bc)
}

func BenchmarkTLSHandshakeECDSAWithCurvesWithClientSessionCache(b *testing.B) {
	bc := &benchConfig{
		IsTLS: true,
		DisableClientSessionCache: false,
		UseCurves:                 true,
		UseECDSA:                  true,
	}
	benchmarkExt(b, handshakeHandler, bc)
}

func BenchmarkTLSHandshakeECDSAWithCurvesWithoutClientSessionCache(b *testing.B) {
	bc := &benchConfig{
		IsTLS: true,
		DisableClientSessionCache: true,
		UseCurves:                 true,
		UseECDSA:                  true,
	}
	benchmarkExt(b, handshakeHandler, bc)
}

func benchmark(b *testing.B, h fasthttp.RequestHandler, isTLS bool) {
	bc := &benchConfig{
		IsTLS: isTLS,
	}
	benchmarkExt(b, h, bc)
}

type benchConfig struct {
	IsTLS                     bool
	DisableClientSessionCache bool
	UseCurves                 bool
	UseECDSA                  bool
}

func benchmarkExt(b *testing.B, h fasthttp.RequestHandler, bc *benchConfig) {
	var serverTLSConfig, clientTLSConfig *tls.Config
	if bc.IsTLS {
		certFile := "rsa.pem"
		keyFile := "rsa.key"
		if bc.UseECDSA {
			certFile = "ecdsa.pem"
			keyFile = "ecdsa.key"
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			b.Fatalf("cannot load TLS certificate from certFile=%q, keyFile=%q: %s", certFile, keyFile, err)
		}
		serverTLSConfig = &tls.Config{
			Certificates:             []tls.Certificate{cert},
			PreferServerCipherSuites: true,
		}
		serverTLSConfig.CurvePreferences = []tls.CurveID{}
		if bc.UseCurves {
			serverTLSConfig.CurvePreferences = []tls.CurveID{
				tls.CurveP256,
			}
		}
		clientTLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
		if bc.DisableClientSessionCache {
			clientTLSConfig.ClientSessionCache = fakeSessionCache{}
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
			b.Fatalf("unexpected error in server: %s", err)
		}
		close(serverStopCh)
	}()
	c := &fasthttp.HostClient{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		IsTLS:     clientTLSConfig != nil,
		TLSConfig: clientTLSConfig,
	}

	b.RunParallel(func(pb *testing.PB) {
		runRequests(b, pb, c)
	})
	ln.Close()
	<-serverStopCh
}

func streamingHandler(ctx *fasthttp.RequestCtx) {
	ctx.WriteString("foobar")
}

func handshakeHandler(ctx *fasthttp.RequestCtx) {
	streamingHandler(ctx)

	// Explicitly close connection after each response.
	ctx.SetConnectionClose()
}

func runRequests(b *testing.B, pb *testing.PB, c *fasthttp.HostClient) {
	var req fasthttp.Request
	req.SetRequestURI("http://foo.bar/baz")
	var resp fasthttp.Response
	for pb.Next() {
		if err := c.Do(&req, &resp); err != nil {
			b.Fatalf("unexpected error: %s", err)
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			b.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), fasthttp.StatusOK)
		}
	}
}

type fakeSessionCache struct{}

func (fakeSessionCache) Get(sessionKey string) (*tls.ClientSessionState, bool) {
	return nil, false
}

func (fakeSessionCache) Put(sessionKey string, cs *tls.ClientSessionState) {
	// no-op
}

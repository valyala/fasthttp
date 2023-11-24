package fasthttputil_test

import (
	"crypto/tls"
	"net"
	"testing"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

var (
	certblock = []byte(`-----BEGIN CERTIFICATE-----
MIICujCCAaKgAwIBAgIJAMbXnKZ/cikUMA0GCSqGSIb3DQEBCwUAMBUxEzARBgNV
BAMTCnVidW50dS5uYW4wHhcNMTUwMjA0MDgwMTM5WhcNMjUwMjAxMDgwMTM5WjAV
MRMwEQYDVQQDEwp1YnVudHUubmFuMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIB
CgKCAQEA+CELrALPDyXZxt5lEbfwF7YAvnHqizmrSePSSRNVT05DAMvqBNX9V75D
K2LB6pg3+hllc4FV68i+FMKtv5yUpuenXYTeeZyPKEjd3bcsFAfP0oXpRDe955Te
+z3g/bZejZLD8Fmiq6satBZWm0T2UkAn5oGW4Q1fEmvJnwpBVNBtJYrepCxnHgij
L5lvvQc+3m7GJlXZlTMZnyCUrRQ+OJVhU3VHOuViEihHVthC3FHn29Mzi8PtDwm1
xRiR+ceZLZLFvPgQZNh5IBnkES/6jwnHLYW0nDtFYDY98yd2WS9Dm0gwG7zQxvOY
6HjYwzauQ0/wQGdGzkmxBbIfn/QQMwIDAQABow0wCzAJBgNVHRMEAjAAMA0GCSqG
SIb3DQEBCwUAA4IBAQBQjKm/4KN/iTgXbLTL3i7zaxYXFLXsnT1tF+ay4VA8aj98
L3JwRTciZ3A5iy/W4VSCt3eASwOaPWHKqDBB5RTtL73LoAqsWmO3APOGQAbixcQ2
45GXi05OKeyiYRi1Nvq7Unv9jUkRDHUYVPZVSAjCpsXzPhFkmZoTRxmx5l0ZF7Li
K91lI5h+eFq0dwZwrmlPambyh1vQUi70VHv8DNToVU29kel7YLbxGbuqETfhrcy6
X+Mha6RYITkAn5FqsZcKMsc9eYGEF4l3XV+oS7q6xfTxktYJMFTI18J0lQ2Lv/CI
whdMnYGntDQBE/iFCrJEGNsKGc38796GBOb5j+zd
-----END CERTIFICATE-----
`)
	keyblock = []byte(`-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQD4IQusAs8PJdnG
3mURt/AXtgC+ceqLOatJ49JJE1VPTkMAy+oE1f1XvkMrYsHqmDf6GWVzgVXryL4U
wq2/nJSm56ddhN55nI8oSN3dtywUB8/ShelEN73nlN77PeD9tl6NksPwWaKrqxq0
FlabRPZSQCfmgZbhDV8Sa8mfCkFU0G0lit6kLGceCKMvmW+9Bz7ebsYmVdmVMxmf
IJStFD44lWFTdUc65WISKEdW2ELcUefb0zOLw+0PCbXFGJH5x5ktksW8+BBk2Hkg
GeQRL/qPCccthbScO0VgNj3zJ3ZZL0ObSDAbvNDG85joeNjDNq5DT/BAZ0bOSbEF
sh+f9BAzAgMBAAECggEBAJWv2cq7Jw6MVwSRxYca38xuD6TUNBopgBvjREixURW2
sNUaLuMb9Omp7fuOaE2N5rcJ+xnjPGIxh/oeN5MQctz9gwn3zf6vY+15h97pUb4D
uGvYPRDaT8YVGS+X9NMZ4ZCmqW2lpWzKnCFoGHcy8yZLbcaxBsRdvKzwOYGoPiFb
K2QuhXZ/1UPmqK9i2DFKtj40X6vBszTNboFxOVpXrPu0FJwLVSDf2hSZ4fMM0DH3
YqwKcYf5te+hxGKgrqRA3tn0NCWii0in6QIwXMC+kMw1ebg/tZKqyDLMNptAK8J+
DVw9m5X1seUHS5ehU/g2jrQrtK5WYn7MrFK4lBzlRwECgYEA/d1TeANYECDWRRDk
B0aaRZs87Rwl/J9PsvbsKvtU/bX+OfSOUjOa9iQBqn0LmU8GqusEET/QVUfocVwV
Bggf/5qDLxz100Rj0ags/yE/kNr0Bb31kkkKHFMnCT06YasR7qKllwrAlPJvQv9x
IzBKq+T/Dx08Wep9bCRSFhzRCnsCgYEA+jdeZXTDr/Vz+D2B3nAw1frqYFfGnEVY
wqmoK3VXMDkGuxsloO2rN+SyiUo3JNiQNPDub/t7175GH5pmKtZOlftePANsUjBj
wZ1D0rI5Bxu/71ibIUYIRVmXsTEQkh/ozoh3jXCZ9+bLgYiYx7789IUZZSokFQ3D
FICUT9KJ36kCgYAGoq9Y1rWJjmIrYfqj2guUQC+CfxbbGIrrwZqAsRsSmpwvhZ3m
tiSZxG0quKQB+NfSxdvQW5ulbwC7Xc3K35F+i9pb8+TVBdeaFkw+yu6vaZmxQLrX
fQM/pEjD7A7HmMIaO7QaU5SfEAsqdCTP56Y8AftMuNXn/8IRfo2KuGwaWwKBgFpU
ILzJoVdlad9E/Rw7LjYhZfkv1uBVXIyxyKcfrkEXZSmozDXDdxsvcZCEfVHM6Ipk
K/+7LuMcqp4AFEAEq8wTOdq6daFaHLkpt/FZK6M4TlruhtpFOPkoNc3e45eM83OT
6mziKINJC1CQ6m65sQHpBtjxlKMRG8rL/D6wx9s5AoGBAMRlqNPMwglT3hvDmsAt
9Lf9pdmhERUlHhD8bj8mDaBj2Aqv7f6VRJaYZqP403pKKQexuqcn80mtjkSAPFkN
Cj7BVt/RXm5uoxDTnfi26RF9F6yNDEJ7UU9+peBr99aazF/fTgW/1GcMkQnum8uV
c257YgaWmjK9uB0Y2r2VxS0G
-----END PRIVATE KEY-----`)
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
		cert, err := tls.X509KeyPair(certblock, keyblock)
		if err != nil {
			b.Fatalf("cannot load TLS certificate: %v", err)
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

// Command h2tlsserver serves HTTP/2 over TLS: ALPN negotiates h2 and
// falls back to HTTP/1.1 for clients that don't speak it.
//
//	go run ./examples/h2tlsserver -addr 127.0.0.1:8443
//	curl -vk https://127.0.0.1:8443/
//
// Without -cert and -key it signs a throwaway localhost certificate, so it
// runs out of the box; real deployments pass their own files.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/http2"
)

func selfSigned() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8443", "listen address")
	certFile := flag.String("cert", "", "TLS certificate file; empty self-signs")
	keyFile := flag.String("key", "", "TLS key file")
	flag.Parse()

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			state := ctx.TLSConnectionState()
			fmt.Fprintf(ctx, "%s over %s with %s\n",
				ctx.Request.Header.Protocol(),
				tlsVersionName(state.Version),
				state.NegotiatedProtocol)
		},
	}
	if err := http2.ConfigureServer(server, http2.ServerConfig{}); err != nil {
		log.Fatal(err)
	}

	log.Printf("https://%s", *addr)
	if *certFile != "" {
		log.Fatal(server.ListenAndServeTLS(*addr, *certFile, *keyFile))
		return
	}
	certPEM, keyPEM, err := selfSigned()
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(server.ListenAndServeTLSEmbed(*addr, certPEM, keyPEM))
}

func tlsVersionName(version uint16) string {
	switch version {
	case 0x0304:
		return "TLS 1.3"
	case 0x0303:
		return "TLS 1.2"
	default:
		return fmt.Sprintf("TLS %#04x", version)
	}
}

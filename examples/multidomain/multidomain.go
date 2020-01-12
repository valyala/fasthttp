package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/valyala/fasthttp"
)

var domains = make(map[string]fasthttp.RequestHandler)

func main() {
	server := &fasthttp.Server{
		// You can check the access using openssl command:
		// $ openssl s_client -connect localhost:8080 << EOF
		// > GET /
		// > Host: localhost
		// > EOF
		//
		// $ openssl s_client -connect localhost:8080 << EOF
		// > GET /
		// > Host: 127.0.0.1:8080
		// > EOF
		//
		Handler: func(ctx *fasthttp.RequestCtx) {
			h, ok := domains[string(ctx.Host())]
			if !ok {
				ctx.NotFound()
				return
			}
			h(ctx)
		},
	}

	// preparing first host
	cert, priv, err := GenerateCert("localhost:8080")
	if err != nil {
		panic(err)
	}
	domains["localhost:8080"] = func(ctx *fasthttp.RequestCtx) {
		ctx.Write([]byte("You are accessing to localhost:8080\n"))
	}

	err = server.AppendCertEmbed(cert, priv)
	if err != nil {
		panic(err)
	}

	// preparing second host
	cert, priv, err = GenerateCert("127.0.0.1")
	if err != nil {
		panic(err)
	}
	domains["127.0.0.1:8080"] = func(ctx *fasthttp.RequestCtx) {
		ctx.Write([]byte("You are accessing to 127.0.0.1:8080\n"))
	}

	err = server.AppendCertEmbed(cert, priv)
	if err != nil {
		panic(err)
	}

	fmt.Println(server.ListenAndServeTLS(":8080", "", ""))
}

// GenerateCert generates certificate and private key based on the given host.
func GenerateCert(host string) ([]byte, []byte, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, err
	}

	cert := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"I have your data"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		SignatureAlgorithm:    x509.SHA256WithRSA,
		DNSNames:              []string{host},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certBytes, err := x509.CreateCertificate(
		rand.Reader, cert, cert, &priv.PublicKey, priv,
	)

	p := pem.EncodeToMemory(
		&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(priv),
		},
	)

	b := pem.EncodeToMemory(
		&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: certBytes,
		},
	)

	return b, p, err
}

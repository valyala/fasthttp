package main

import (
	"crypto/tls"
	"net"

	"github.com/valyala/fasthttp"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

func requestHandler(ctx *fasthttp.RequestCtx) {
	ctx.SetBodyString("hello from https!")
}

func main() {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist("example.com"), // Replace with your domain.
		Cache:      autocert.DirCache("./certs"),
	}

	cfg := &tls.Config{
		GetCertificate: m.GetCertificate,
		NextProtos: []string{
			"http/1.1", acme.ALPNProto,
		},
	}

	// Let's Encrypt tls-alpn-01 only works on port 443.
	ln, err := net.Listen("tcp4", "0.0.0.0:443") /* #nosec G102 */
	if err != nil {
		panic(err)
	}

	lnTls := tls.NewListener(ln, cfg)

	if err := fasthttp.Serve(lnTls, requestHandler); err != nil {
		panic(err)
	}
}

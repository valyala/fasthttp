package main

import (
	"fmt"

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
	cert, priv, err := fasthttp.GenerateTestCertificate("localhost:8080")
	if err != nil {
		panic(err)
	}
	domains["localhost:8080"] = func(ctx *fasthttp.RequestCtx) {
		ctx.WriteString("You are accessing to localhost:8080\n")
	}

	err = server.AppendCertEmbed(cert, priv)
	if err != nil {
		panic(err)
	}

	// preparing second host
	cert, priv, err = fasthttp.GenerateTestCertificate("127.0.0.1")
	if err != nil {
		panic(err)
	}
	domains["127.0.0.1:8080"] = func(ctx *fasthttp.RequestCtx) {
		ctx.WriteString("You are accessing to 127.0.0.1:8080\n")
	}

	err = server.AppendCertEmbed(cert, priv)
	if err != nil {
		panic(err)
	}

	fmt.Println(server.ListenAndServeTLS(":8080", "", ""))
}

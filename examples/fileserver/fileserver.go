// Example static file server. Serves static files from the given directory.
package main

import (
	"flag"
	"log"

	"github.com/valyala/fasthttp"
)

var (
	addr = flag.String("addr", ":8080", "TCP address to listen to")
	dir  = flag.String("dir", "/usr/share/nginx/html", "Directory to serve static files from")
)

func main() {
	flag.Parse()

	h := fasthttp.FSHandler(*dir, 0)
	if err := fasthttp.ListenAndServe(*addr, h); err != nil {
		log.Fatalf("error in ListenAndServe: %s", err)
	}
}

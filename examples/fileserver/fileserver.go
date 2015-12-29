// Example static file server. Serves static files from the given directory.
package main

import (
	"flag"
	"log"

	"github.com/valyala/fasthttp"
)

var (
	addr               = flag.String("addr", ":8080", "TCP address to listen to")
	compress           = flag.Bool("compress", false, "Enables transparent response compression if set to true")
	dir                = flag.String("dir", "/usr/share/nginx/html", "Directory to serve static files from")
	generateIndexPages = flag.Bool("generateIndexPages", true, "Whether to generate directory index pages")
)

func main() {
	flag.Parse()

	fs := &fasthttp.FS{
		Root:               *dir,
		IndexNames:         []string{"index.html"},
		GenerateIndexPages: *generateIndexPages,
		Compress:           *compress,
	}
	h := fs.NewRequestHandler()

	if err := fasthttp.ListenAndServe(*addr, h); err != nil {
		log.Fatalf("error in ListenAndServe: %s", err)
	}
}

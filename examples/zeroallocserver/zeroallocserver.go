// Command zeroallocserver renders the same request info as helloworldserver,
// but with 0 allocs/op.
package main

import (
	"flag"
	"log"
	"net"
	"strconv"
	"sync"

	"github.com/valyala/fasthttp"
)

var addr = flag.String("addr", ":8080", "TCP address to listen to")

func main() {
	flag.Parse()

	if err := fasthttp.ListenAndServe(*addr, requestHandler); err != nil {
		log.Fatalf("Error in ListenAndServe: %v", err)
	}
}

// scratchPool holds reusable buffers for formatting non-[]byte values.
var scratchPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 64)
		return &b
	},
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	buf := scratchPool.Get().(*[]byte) //nolint:forcetypeassert
	defer scratchPool.Put(buf)

	resp := &ctx.Response
	resp.AppendBodyString("Hello, world!\n\n")

	resp.AppendBodyString("Request method is \"")
	resp.AppendBody(ctx.Method())
	resp.AppendBodyString("\"\nRequestURI is \"")
	resp.AppendBody(ctx.RequestURI())
	resp.AppendBodyString("\"\nRequested path is \"")
	resp.AppendBody(ctx.Path())
	resp.AppendBodyString("\"\nHost is \"")
	resp.AppendBody(ctx.Host())
	resp.AppendBodyString("\"\nQuery string is \"")
	resp.AppendBody(ctx.QueryArgs().AppendBytes((*buf)[:0]))
	resp.AppendBodyString("\"\nUser-Agent is \"")
	resp.AppendBody(ctx.UserAgent())

	resp.AppendBodyString("\"\nConnection has been established at ")
	resp.AppendBody(fasthttp.AppendHTTPDate((*buf)[:0], ctx.ConnTime()))
	resp.AppendBodyString("\nRequest has been started at ")
	resp.AppendBody(fasthttp.AppendHTTPDate((*buf)[:0], ctx.Time()))

	resp.AppendBodyString("\nSerial request number for the current connection is ")
	resp.AppendBody(strconv.AppendUint((*buf)[:0], ctx.ConnRequestNum(), 10))

	resp.AppendBodyString("\nYour ip is \"")
	resp.AppendBody(appendIP((*buf)[:0], ctx.RemoteIP()))
	resp.AppendBodyString("\"\n\n")

	ctx.SetContentType("text/plain; charset=utf8")
	resp.Header.Set("X-My-Header", "my-header-value")

	// AcquireCookie avoids the allocation a zero-value Cookie would cause.
	c := fasthttp.AcquireCookie()
	defer fasthttp.ReleaseCookie(c)
	c.SetKey("cookie-name")
	c.SetValue("cookie-value")
	resp.Header.SetCookie(c)
}

// appendIP appends ip to dst without allocating. AppendIPv4 only handles
// IPv4, so IPv6 falls back to net.IP.AppendText.
func appendIP(dst []byte, ip net.IP) []byte {
	if ip.To4() != nil {
		return fasthttp.AppendIPv4(dst, ip)
	}
	dst, _ = ip.AppendText(dst)
	return dst
}

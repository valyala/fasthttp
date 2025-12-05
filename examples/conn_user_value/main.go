package main

import (
	"fmt"
	"log"
	"sync/atomic"

	"github.com/valyala/fasthttp"
)

// Rate limiter using connection-level user value
func rateLimitHandler(ctx *fasthttp.RequestCtx) {
	// Get current request count for this connection
	currentCount := ctx.ConnUserValueUint64()
	
	// Increment counter atomically
	newCount := atomic.AddUint64(&currentCount, 1)
	ctx.SetConnUserValueUint64(newCount)
	
	// Rate limit: max 10 requests per connection
	if newCount > 10 {
		ctx.SetStatusCode(fasthttp.StatusTooManyRequests)
		ctx.SetBodyString("Rate limit exceeded: max 10 requests per connection")
		return
	}
	
	// Normal response
	ctx.SetContentType("text/plain")
	fmt.Fprintf(ctx, "Request #%d from connection %d\n", newCount, ctx.ConnID())
}

func main() {
	log.Println("Starting server on :9999")
	log.Println("Try: curl -H 'Connection: keep-alive' localhost:9999 multiple times")
	
	if err := fasthttp.ListenAndServe(":9999", rateLimitHandler); err != nil {
		log.Fatalf("Error in ListenAndServe: %s", err)
	}
}
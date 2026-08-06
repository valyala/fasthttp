package http2

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2/hpack"
)

func TestRejectedRequestHeadersKeepHPACKEncoderInSync(t *testing.T) {
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	decoder := hpack.NewDecoder(defaultHeaderTableSize, func(hpack.HeaderField) {})
	var cache headerStringCache

	bad := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(bad)
	bad.SetRequestURI("http://example.com/bad")
	bad.Header.Set("TE", "gzip")
	if _, err := encodeRequestHeaders(encoder, &encoded, &cache, bad, 1<<20, false); err == nil {
		t.Fatal("invalid TE header was accepted")
	}

	probe := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(probe)
	probe.SetRequestURI("http://example.com/probe")
	block, err := encodeRequestHeaders(encoder, &encoded, &cache, probe, 1<<20, false)
	if err != nil {
		t.Fatalf("encoding probe request: %v", err)
	}
	decodeTestHeaderBlock(t, decoder, block)
}

func TestRejectedResponseHeadersKeepHPACKEncoderInSync(t *testing.T) {
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	decoder := hpack.NewDecoder(defaultHeaderTableSize, func(hpack.HeaderField) {})
	var cache headerStringCache
	server := &fasthttp.Server{NoDefaultDate: true, NoDefaultServerHeader: true}

	warm := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(warm)
	warm.Header.Set("X-Marker", "canary-value")
	block, err := encodeResponseHeaders(encoder, &encoded, &cache, server, warm, 1<<20, nil)
	if err != nil {
		t.Fatalf("encoding warm response: %v", err)
	}
	decodeTestHeaderBlock(t, decoder, block)

	bad := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(bad)
	for i := range 4 {
		bad.Header.Set(fmt.Sprintf("X-Large-%d", i), strings.Repeat("x", 128))
	}
	if _, err := encodeResponseHeaders(encoder, &encoded, &cache, server, bad, 300, nil); err == nil {
		t.Fatal("oversized response header list was accepted")
	}

	probe := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(probe)
	probe.Header.Set("X-Marker", "canary-value")
	block, err = encodeResponseHeaders(encoder, &encoded, &cache, server, probe, 1<<20, nil)
	if err != nil {
		t.Fatalf("encoding probe response: %v", err)
	}
	decodeTestHeaderBlock(t, decoder, block)
}

func decodeTestHeaderBlock(t testing.TB, decoder *hpack.Decoder, block []byte) {
	t.Helper()
	if _, err := decoder.Write(block); err != nil {
		t.Fatalf("decoding header block: %v", err)
	}
	if err := decoder.Close(); err != nil {
		t.Fatalf("closing header block: %v", err)
	}
}

func TestResponseHeadersRejectTE(t *testing.T) {
	var response fasthttp.Response
	if _, _, err := populateResponse(&response, []hpack.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "te", Value: "trailers"},
	}, false); err == nil {
		t.Fatal("response TE header was accepted")
	}
	if err := populateResponseTrailers(&response, []hpack.HeaderField{
		{Name: "te", Value: "trailers"},
	}); err == nil {
		t.Fatal("response trailer TE field was accepted")
	}
}

func TestOutboundResponseHeadersRejectTEBeforeHPACK(t *testing.T) {
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	var cache headerStringCache
	server := &fasthttp.Server{NoDefaultDate: true, NoDefaultServerHeader: true}
	var response fasthttp.Response
	response.Header.Set("TE", "trailers")
	if _, err := encodeResponseHeaders(encoder, &encoded, &cache, server, &response, 1<<20, nil); err == nil {
		t.Fatal("outbound response TE header was accepted")
	}

	var informational fasthttp.ResponseHeader
	informational.Set("TE", "trailers")
	if _, err := encodeInformationalHeaders(encoder, &encoded, &cache, 103, &informational, 1<<20); err == nil {
		t.Fatal("outbound informational TE header was accepted")
	}
}

func TestOutboundResponseTrailersRespectHeaderListLimit(t *testing.T) {
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	var cache headerStringCache
	var header fasthttp.ResponseHeader
	if err := header.AddTrailer("X-Large-Trailer"); err != nil {
		t.Fatal(err)
	}
	header.Set("X-Large-Trailer", strings.Repeat("x", 128))
	if _, err := encodeTrailerHeaders(encoder, &encoded, &cache, &header, 64); !errors.Is(err, errResponseHeaderTooLarge) {
		t.Fatalf("encodeTrailerHeaders() error = %v, want header list limit error", err)
	}
}

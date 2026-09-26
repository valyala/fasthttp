package http2

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2/hpack"
)

func TestRejectedRequestHeadersKeepHPACKEncoderInSync(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	decoder := hpack.NewDecoder(defaultHeaderTableSize, func(hpack.HeaderField) {})

	bad := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(bad)
	bad.SetRequestURI("http://example.com/bad")
	bad.Header.Set("TE", "gzip")
	if _, err := enc.encodeRequestHeaders(bad, 1<<20, false); err == nil {
		t.Fatal("invalid TE header was accepted")
	}

	probe := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(probe)
	probe.SetRequestURI("http://example.com/probe")
	block, err := enc.encodeRequestHeaders(probe, 1<<20, false)
	if err != nil {
		t.Fatalf("encoding probe request: %v", err)
	}
	decodeTestHeaderBlock(t, decoder, block)
}

func TestResponseHeadersExcludeTrailerFields(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	server := &fasthttp.Server{NoDefaultDate: true, NoDefaultServerHeader: true}

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	resp.Header.Set("X-Early", "yes")
	if err := resp.Header.AddTrailer("Grpc-Status"); err != nil {
		t.Fatalf("AddTrailer() error: %v", err)
	}
	resp.Header.Set("Grpc-Status", "0")

	block, err := enc.encodeResponseHeaders(server, resp, 1<<20, nil)
	if err != nil {
		t.Fatalf("encoding response: %v", err)
	}
	fields := decodeTestHeaderFields(t, block)
	if _, ok := fields["grpc-status"]; ok {
		t.Fatal("trailer field leaked into the initial header block")
	}
	if fields["x-early"] != "yes" {
		t.Fatalf("headers = %v, missing x-early", fields)
	}
}

func decodeTestHeaderFields(t testing.TB, block []byte) map[string]string {
	t.Helper()
	fields := map[string]string{}
	decoder := hpack.NewDecoder(defaultHeaderTableSize, func(field hpack.HeaderField) {
		fields[field.Name] = field.Value
	})
	if _, err := decoder.Write(block); err != nil {
		t.Fatalf("decoding header block: %v", err)
	}
	if err := decoder.Close(); err != nil {
		t.Fatalf("closing header block: %v", err)
	}
	return fields
}

func TestRejectedResponseHeadersKeepHPACKEncoderInSync(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	decoder := hpack.NewDecoder(defaultHeaderTableSize, func(hpack.HeaderField) {})
	server := &fasthttp.Server{NoDefaultDate: true, NoDefaultServerHeader: true}

	warm := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(warm)
	warm.Header.Set("X-Marker", "canary-value")
	block, err := enc.encodeResponseHeaders(server, warm, 1<<20, nil)
	if err != nil {
		t.Fatalf("encoding warm response: %v", err)
	}
	decodeTestHeaderBlock(t, decoder, block)

	bad := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(bad)
	for i := range 4 {
		bad.Header.Set(fmt.Sprintf("X-Large-%d", i), strings.Repeat("x", 128))
	}
	if _, err := enc.encodeResponseHeaders(server, bad, 300, nil); err == nil {
		t.Fatal("oversized response header list was accepted")
	}

	probe := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(probe)
	probe.Header.Set("X-Marker", "canary-value")
	block, err = enc.encodeResponseHeaders(server, probe, 1<<20, nil)
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

func TestRequestTrailersKeepRepeatedFields(t *testing.T) {
	var header fasthttp.RequestHeader
	fields := []hpack.HeaderField{
		{Name: "x-checksum", Value: "one"},
		{Name: "x-checksum", Value: "two"},
	}
	if err := applyRequestTrailers(&header, fields); err != nil {
		t.Fatalf("applyRequestTrailers() error: %v", err)
	}
	values := header.PeekAll("X-Checksum")
	if len(values) != 2 || string(values[0]) != "one" || string(values[1]) != "two" {
		t.Fatalf("trailer values = %q, want [one two]", values)
	}
	if keys := header.PeekTrailerKeys(); len(keys) != 1 {
		t.Fatalf("trailer keys = %q, want one entry", keys)
	}
}

func TestResponseTrailersKeepRepeatedFields(t *testing.T) {
	var resp fasthttp.Response
	fields := []hpack.HeaderField{
		{Name: "x-checksum", Value: "one"},
		{Name: "x-checksum", Value: "two"},
	}
	if err := populateResponseTrailers(&resp, fields); err != nil {
		t.Fatalf("populateResponseTrailers() error: %v", err)
	}
	values := resp.Header.PeekAll("X-Checksum")
	if len(values) != 2 || string(values[0]) != "one" || string(values[1]) != "two" {
		t.Fatalf("trailer values = %q, want [one two]", values)
	}
	if keys := resp.Header.PeekTrailerKeys(); len(keys) != 1 {
		t.Fatalf("trailer keys = %q, want one entry", keys)
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
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	server := &fasthttp.Server{NoDefaultDate: true, NoDefaultServerHeader: true}
	var response fasthttp.Response
	response.Header.Set("TE", "trailers")
	if _, err := enc.encodeResponseHeaders(server, &response, 1<<20, nil); err == nil {
		t.Fatal("outbound response TE header was accepted")
	}

	var informational fasthttp.ResponseHeader
	informational.Set("TE", "trailers")
	if _, err := enc.encodeInformationalHeaders(103, &informational, 1<<20); err == nil {
		t.Fatal("outbound informational TE header was accepted")
	}
}

func TestOutboundResponseTrailersRespectHeaderListLimit(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	var header fasthttp.ResponseHeader
	if err := header.AddTrailer("X-Large-Trailer"); err != nil {
		t.Fatal(err)
	}
	header.Set("X-Large-Trailer", strings.Repeat("x", 128))
	if _, err := enc.encodeTrailerHeaders(&header, 64); !errors.Is(err, errResponseHeaderTooLarge) {
		t.Fatalf("encodeTrailerHeaders() error = %v, want header list limit error", err)
	}
}

func TestPopulateRequestRoutesSpecialHeaders(t *testing.T) {
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "example.com"},
		{Name: ":path", Value: "/upload"},
		{Name: "user-agent", Value: "probe/1.0"},
		{Name: "content-type", Value: "text/plain"},
		{Name: "content-length", Value: "4"},
		{Name: "priority", Value: "u=1, i"},
		{Name: "priority", Value: "u=7"},
		{Name: "x-custom", Value: "kept"},
	}
	ctx := &fasthttp.RequestCtx{}
	contentLength, priority, err := populateRequest(ctx, fields, false)
	if err != nil {
		t.Fatalf("populateRequest() error: %v", err)
	}
	if contentLength != 4 || priority != "u=1, i" {
		t.Fatalf("populateRequest() = (%d, %q), want (4, %q)", contentLength, priority, "u=1, i")
	}
	header := &ctx.Request.Header
	if got := string(header.UserAgent()); got != "probe/1.0" {
		t.Errorf("UserAgent() = %q", got)
	}
	if got := string(header.ContentType()); got != "text/plain" {
		t.Errorf("ContentType() = %q", got)
	}
	if got := header.ContentLength(); got != 4 {
		t.Errorf("ContentLength() = %d", got)
	}
	if got := header.PeekAll("Priority"); len(got) != 2 || string(got[0]) != "u=1, i" || string(got[1]) != "u=7" {
		t.Errorf("PeekAll(Priority) = %q, want both values kept", got)
	}
	if got := string(header.Peek("X-Custom")); got != "kept" {
		t.Errorf("Peek(X-Custom) = %q", got)
	}
}

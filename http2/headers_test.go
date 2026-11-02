package http2

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpguts"
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
	if _, err := enc.encodeRequestHeaders(bad, nil, 1<<20, false); err == nil {
		t.Fatal("invalid TE header was accepted")
	}

	probe := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(probe)
	probe.SetRequestURI("http://example.com/probe")
	block, err := enc.encodeRequestHeaders(probe, nil, 1<<20, false)
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
	// The declaration travels in the header block, as HTTP/1 writes it.
	if fields["trailer"] != "Grpc-Status" {
		t.Fatalf("headers = %v, missing the trailer declaration", fields)
	}
}

// An informational response carries no default Content-Type, as HTTP/1's 1xx
// has none; an explicitly set one is kept.
func TestInformationalHeadersOmitDefaultContentType(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	fields := map[string]string{}
	decoder := hpack.NewDecoder(defaultHeaderTableSize, func(field hpack.HeaderField) {
		fields[field.Name] = field.Value
	})
	var header fasthttp.ResponseHeader
	header.Add(fasthttp.HeaderLink, "</style.css>; rel=preload")

	block, err := enc.encodeInformationalHeaders(fasthttp.StatusEarlyHints, &header, 1<<20)
	if err != nil {
		t.Fatalf("encoding 103: %v", err)
	}
	decodeTestHeaderBlock(t, decoder, block)
	if value, ok := fields["content-type"]; ok {
		t.Fatalf("103 content-type = %q, want none", value)
	}
	if fields["link"] != "</style.css>; rel=preload" {
		t.Fatalf("103 fields = %v, missing link", fields)
	}

	clear(fields)
	header.SetContentType("text/plain; charset=utf-8")
	block, err = enc.encodeInformationalHeaders(fasthttp.StatusEarlyHints, &header, 1<<20)
	if err != nil {
		t.Fatalf("encoding 103: %v", err)
	}
	decodeTestHeaderBlock(t, decoder, block)
	if fields["content-type"] != "text/plain; charset=utf-8" {
		t.Fatalf("103 fields = %v, want the explicit content-type", fields)
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
	if err := applyRequestTrailers(testProtocolContext(t), &header, fields); err != nil {
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
	if err := populateResponseTrailers(&fasthttp.ProtocolClientConn{}, &resp, fields); err != nil {
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
	if _, _, err := populateResponse(&fasthttp.ProtocolClientConn{}, &response, []hpack.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "te", Value: "trailers"},
	}, false); err == nil {
		t.Fatal("response TE header was accepted")
	}
	if err := populateResponseTrailers(&fasthttp.ProtocolClientConn{}, &response, []hpack.HeaderField{
		{Name: "te", Value: "trailers"},
	}); err == nil {
		t.Fatal("response trailer TE field was accepted")
	}
}

// A TE response field is dropped with the other connection-specific fields
// (RFC 9113 §8.2.2), in final and informational responses alike.
func TestOutboundResponseHeadersDropTE(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	server := &fasthttp.Server{NoDefaultDate: true, NoDefaultServerHeader: true}
	var response fasthttp.Response
	response.Header.Set("TE", "trailers")
	response.Header.Set("X-Kept", "yes")
	block, err := enc.encodeResponseHeaders(server, &response, 1<<20, nil)
	if err != nil {
		t.Fatalf("encodeResponseHeaders() error: %v", err)
	}
	if fields := decodeTestHeaderFields(t, block); fields["x-kept"] != "yes" || fields["te"] != "" {
		t.Fatalf("response fields = %v, want x-kept without te", fields)
	}

	var informational fasthttp.ResponseHeader
	informational.Set("TE", "trailers")
	informational.Set("Link", "</style.css>; rel=preload")
	block, err = enc.encodeInformationalHeaders(103, &informational, 1<<20)
	if err != nil {
		t.Fatalf("encodeInformationalHeaders() error: %v", err)
	}
	if fields := decodeTestHeaderFields(t, block); fields["link"] == "" || fields["te"] != "" {
		t.Fatalf("103 fields = %v, want link without te", fields)
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

// Repeated priority lines combine with a comma, an empty line included: the
// combined value of an empty line and a member is a malformed list, not the
// member alone.
func TestPopulateRequestCombinesPriorityLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{name: "none", want: ""},
		{name: "one", lines: []string{"u=1, i"}, want: "u=1, i"},
		{name: "empty", lines: []string{""}, want: ""},
		{name: "two", lines: []string{"u=1, i", "u=7"}, want: "u=1, i,u=7"},
		{name: "empty-first", lines: []string{"", "u=0"}, want: ",u=0"},
		{name: "empty-second", lines: []string{"u=0", ""}, want: "u=0,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := []hpack.HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "https"},
				{Name: ":authority", Value: "example.com"},
				{Name: ":path", Value: "/"},
			}
			for _, line := range tc.lines {
				fields = append(fields, hpack.HeaderField{Name: "priority", Value: line})
			}
			ctx := &fasthttp.RequestCtx{}
			parsed, err := populateRequest(ctx, fields, testProtocolContext(t), false)
			if err != nil {
				t.Fatalf("populateRequest() error: %v", err)
			}
			if parsed.priority != tc.want {
				t.Fatalf("priority = %q, want %q", parsed.priority, tc.want)
			}
		})
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
	parsed, err := populateRequest(ctx, fields, testProtocolContext(t), false)
	if err != nil {
		t.Fatalf("populateRequest() error: %v", err)
	}
	// Repeated priority lines combine into one field (RFC 8941 §4.2).
	if parsed.contentLength != 4 || parsed.priority != "u=1, i,u=7" || parsed.refusal != nil {
		t.Fatalf("populateRequest() = %+v, want length 4, priority %q and no refusal", parsed, "u=1, i,u=7")
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

// The :status and date fields alone count against the peer's limit.
func TestOutboundStatusHeadersCountMandatoryFields(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	var header fasthttp.ResponseHeader
	header.SetNoDefaultContentType(true)
	if _, err := enc.encodeStatusHeaders("200", &header, statusHeaderOptions{
		serverDate:        []byte("Sun, 27 Sep 2026 00:00:00 GMT"),
		skipContentLength: true,
		maxHeaderListSize: 64,
	}); !errors.Is(err, errResponseHeaderTooLarge) {
		t.Fatalf("encodeStatusHeaders() error = %v, want header list limit error", err)
	}
}

func TestPopulateRequestRejectsPresentEmptyPseudoHeaders(t *testing.T) {
	protocolContext := testProtocolContext(t)
	connect := []hpack.HeaderField{{Name: ":method", Value: "CONNECT"}, {Name: ":authority", Value: "example.com:443"}}
	get := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.com"},
		{Name: ":path", Value: "/"},
	}
	extended := []hpack.HeaderField{
		{Name: ":method", Value: "CONNECT"},
		{Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.com"},
		{Name: ":path", Value: "/ws"},
	}
	for _, tc := range []struct {
		name     string
		fields   []hpack.HeaderField
		extended bool
	}{
		{"connect with empty :scheme", append(slices.Clone(connect), hpack.HeaderField{Name: ":scheme"}), false},
		{"connect with empty :path", append(slices.Clone(connect), hpack.HeaderField{Name: ":path"}), false},
		{"empty :protocol without extended connect", append(slices.Clone(get), hpack.HeaderField{Name: ":protocol"}), false},
		{"empty :protocol on GET", append(slices.Clone(get), hpack.HeaderField{Name: ":protocol"}), true},
		{"empty :protocol on CONNECT", append(slices.Clone(extended), hpack.HeaderField{Name: ":protocol"}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := populateRequest(&fasthttp.RequestCtx{}, tc.fields, protocolContext, tc.extended); !errors.Is(err, errInvalidRequestHeaders) {
				t.Fatalf("populateRequest() error = %v, want %v", err, errInvalidRequestHeaders)
			}
		})
	}
	if _, err := populateRequest(&fasthttp.RequestCtx{}, connect, protocolContext, false); err != nil {
		t.Fatalf("populateRequest(CONNECT) error: %v", err)
	}
}

// RFC 9113 §8.2.1 forbids surrounding whitespace on the wire; an HTTP/1
// recipient drops it, so it is trimmed rather than failing the message.
func TestOutboundFieldValuesAreTrimmed(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	server := &fasthttp.Server{NoDefaultDate: true, Name: "edge "}

	var response fasthttp.Response
	response.Header.Set("X-Trailing", "value ")
	response.Header.Set("X-Tabs", "\tvalue\t")
	block, err := enc.encodeResponseHeaders(server, &response, 1<<20, nil)
	if err != nil {
		t.Fatalf("encodeResponseHeaders() error: %v", err)
	}
	fields := decodeTestHeaderFields(t, block)
	if fields["x-trailing"] != "value" || fields["x-tabs"] != "value" || fields["server"] != "edge" {
		t.Fatalf("response fields = %q, want trimmed values", fields)
	}

	if err := response.Header.AddTrailer("X-Checksum"); err != nil {
		t.Fatal(err)
	}
	response.Header.Set("X-Checksum", " abc ")
	block, err = enc.encodeTrailerHeaders(&response.Header, 1<<20)
	if err != nil {
		t.Fatalf("encodeTrailerHeaders() error: %v", err)
	}
	if fields := decodeTestHeaderFields(t, block); fields["x-checksum"] != "abc" {
		t.Fatalf("trailer fields = %q, want trimmed value", fields)
	}

	var request fasthttp.Request
	request.SetRequestURI("http://example.com/")
	request.Header.Set("X-Note", "\tvalue")
	block, err = enc.encodeRequestHeaders(&request, nil, 1<<20, false)
	if err != nil {
		t.Fatalf("encodeRequestHeaders() error: %v", err)
	}
	if fields := decodeTestHeaderFields(t, block); fields["x-note"] != "value" {
		t.Fatalf("request fields = %q, want trimmed value", fields)
	}
}

// Fields a conforming peer must reject fail the message before anything
// reaches the stateful HPACK encoder.
func TestOutboundInvalidFieldsFailTheMessage(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	decoder := hpack.NewDecoder(defaultHeaderTableSize, func(hpack.HeaderField) {})
	server := &fasthttp.Server{NoDefaultDate: true, NoDefaultServerHeader: true}
	invalid := []struct{ name, value string }{
		{"X Bad", "v"},
		{"X-Bad:", "v"},
		{"X-Nul", "a\x00b"},
		{"X-Ctl", "a\x01b"},
		{"X-Del", "a\x7fb"},
	}
	for _, field := range invalid {
		var response fasthttp.Response
		response.Header.Set("X-Before", "kept")
		response.Header.Set(field.name, field.value)
		if _, err := enc.encodeResponseHeaders(server, &response, 1<<20, nil); err == nil {
			t.Errorf("response field %q: %q was encoded", field.name, field.value)
		}
		var trailers fasthttp.ResponseHeader
		if err := trailers.AddTrailer(field.name); err == nil {
			trailers.Set(field.name, field.value)
			if _, err := enc.encodeTrailerHeaders(&trailers, 1<<20); err == nil {
				t.Errorf("trailer field %q: %q was encoded", field.name, field.value)
			}
		}
		var request fasthttp.Request
		request.SetRequestURI("http://example.com/")
		request.Header.Set(field.name, field.value)
		if _, err := enc.encodeRequestHeaders(&request, nil, 1<<20, false); err == nil {
			t.Errorf("request field %q: %q was encoded", field.name, field.value)
		}
	}
	var request fasthttp.Request
	request.SetRequestURI("http://example.com/")
	request.Header.SetMethod("GET ")
	if _, err := enc.encodeRequestHeaders(&request, nil, 1<<20, false); err == nil {
		t.Error("request method with trailing whitespace was encoded")
	}

	var probe fasthttp.Response
	probe.Header.Set("X-Marker", "canary-value")
	block, err := enc.encodeResponseHeaders(server, &probe, 1<<20, nil)
	if err != nil {
		t.Fatalf("encoding probe response: %v", err)
	}
	decodeTestHeaderBlock(t, decoder, block)
}

// Names are lowered ASCII-only: a Unicode fold would turn the Kelvin sign
// U+212A into "k" and send an invalid name as a valid one.
func TestOutboundHeaderNamesLowerASCIIOnly(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	server := &fasthttp.Server{NoDefaultDate: true, NoDefaultServerHeader: true}
	const name = "X-Api-Key"

	var response fasthttp.Response
	response.Header.DisableNormalizing()
	response.Header.Set(name, "secret")
	if _, err := enc.encodeResponseHeaders(server, &response, 1<<20, nil); err == nil {
		t.Errorf("response name %q was encoded", name)
	}

	var request fasthttp.Request
	request.SetRequestURI("http://example.com/")
	request.Header.DisableNormalizing()
	request.Header.Set(name, "secret")
	if _, err := enc.encodeRequestHeaders(&request, nil, 1<<20, false); err == nil {
		t.Errorf("request name %q was encoded", name)
	}
}

func TestOutboundValidFieldsDontAllocate(t *testing.T) {
	var enc headerEncoder
	enc.initHeaderEncoder(defaultHeaderTableSize)
	server := &fasthttp.Server{}
	date := []byte("Sun, 01 Nov 2026 00:00:00 GMT")
	var response fasthttp.Response
	response.Header.Set("X-Request-Id", "abc123 ")
	response.Header.SetContentType("text/plain")
	response.SetBodyString("ok")
	encode := func() {
		if _, err := enc.encodeResponseHeaders(server, &response, 1<<20, date); err != nil {
			t.Fatal(err)
		}
	}
	encode()
	if allocs := testing.AllocsPerRun(100, encode); allocs != 0 {
		t.Fatalf("encodeResponseHeaders() allocs = %v, want 0", allocs)
	}
}

func TestValidFieldValueBytesMatchesHTTPGuts(t *testing.T) {
	base := []byte("0123456789abcdefghij")
	for at := range base {
		for b := range 256 {
			value := slices.Clone(base)
			value[at] = byte(b)
			if got, want := validFieldValueBytes(value), httpguts.ValidHeaderFieldValue(string(value)); got != want {
				t.Fatalf("validFieldValueBytes(%q) = %v, want %v", value, got, want)
			}
		}
	}
}

// RFC 8441 §4: :protocol is one protocol-name token. populateRequest rejects
// anything else, an empty present value included, and encodeRequestHeaders
// refuses to send one before the HPACK encoder sees the request (E4).
func TestExtendedConnectProtocolMustBeAToken(t *testing.T) {
	fields := func(protocol string) []hpack.HeaderField {
		return []hpack.HeaderField{
			{Name: ":method", Value: fasthttp.MethodConnect},
			{Name: ":protocol", Value: protocol},
			{Name: ":scheme", Value: "https"},
			{Name: ":authority", Value: "example.com"},
			{Name: ":path", Value: "/chat"},
		}
	}
	for _, tc := range []struct {
		protocol string
		valid    bool
	}{
		{protocol: "websocket", valid: true},
		{protocol: "web socket"},
		{protocol: ""},
		{protocol: "websocket/13"},
	} {
		t.Run("populate/"+tc.protocol, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			_, err := populateRequest(ctx, fields(tc.protocol), testProtocolContext(t), true)
			if tc.valid != (err == nil) || err != nil && !errors.Is(err, errInvalidRequestHeaders) {
				t.Fatalf("populateRequest(:protocol %q) error = %v, want valid=%v", tc.protocol, err, tc.valid)
			}
			if tc.valid && string(ctx.Request.Header.ConnectProtocol()) != tc.protocol {
				t.Fatalf("ConnectProtocol() = %q", ctx.Request.Header.ConnectProtocol())
			}
		})
		t.Run("encode/"+tc.protocol, func(t *testing.T) {
			if tc.protocol == "" {
				// Without a :protocol the request is a plain CONNECT.
				return
			}
			var encoder headerEncoder
			encoder.initHeaderEncoder(defaultHeaderTableSize)
			var req fasthttp.Request
			req.SetRequestURI("https://example.com/chat")
			req.Header.SetMethod(fasthttp.MethodConnect)
			req.Header.SetConnectProtocol(tc.protocol)
			block, err := encoder.encodeRequestHeaders(&req, nil, 0, true)
			if tc.valid != (err == nil) {
				t.Fatalf("encodeRequestHeaders(:protocol %q) = (%d bytes, %v), want valid=%v", tc.protocol, len(block), err, tc.valid)
			}
			if !tc.valid && encoder.buffer.Len() != 0 {
				t.Fatalf("rejected request reached the HPACK encoder: %d bytes", encoder.buffer.Len())
			}
		})
	}
}

// A Trailer declaration is recorded with the parser's rules: names
// accumulate over repeated lines, a header-section field of a declared name
// stays where it is, and a forbidden name is the request's refusal, reported
// apart from a malformed block, while valid names are still recorded.
func TestPopulateRequestTrailerDeclarations(t *testing.T) {
	protocolContext := testProtocolContext(t)
	get := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.com"},
		{Name: ":path", Value: "/"},
	}
	t.Run("accumulate", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		fields := append(slices.Clone(get),
			hpack.HeaderField{Name: "x-check", Value: "initial"},
			hpack.HeaderField{Name: "trailer", Value: "x-check"},
			hpack.HeaderField{Name: "trailer", Value: "x-more, x-last"},
		)
		parsed, err := populateRequest(ctx, fields, protocolContext, false)
		if err != nil || parsed.refusal != nil {
			t.Fatalf("populateRequest() = %+v, %v", parsed, err)
		}
		if got := string(ctx.Request.Header.Peek("X-Check")); got != "initial" {
			t.Fatalf("Peek(X-Check) = %q, want the header-section value", got)
		}
		if keys := ctx.Request.Header.PeekTrailerKeys(); len(keys) != 3 {
			t.Fatalf("trailer keys = %q, want three", keys)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		ctx := &fasthttp.RequestCtx{}
		fields := append(slices.Clone(get),
			hpack.HeaderField{Name: "trailer", Value: "content-length"},
			hpack.HeaderField{Name: "trailer", Value: "x-a"},
		)
		parsed, err := populateRequest(ctx, fields, protocolContext, false)
		if err != nil || !errors.Is(parsed.refusal, fasthttp.ErrBadTrailer) {
			t.Fatalf("populateRequest() = %+v, %v; want ErrBadTrailer refusal", parsed, err)
		}
		if keys := ctx.Request.Header.PeekTrailerKeys(); len(keys) != 1 || string(keys[0]) != "X-A" {
			t.Fatalf("trailer keys = %q, want [X-A]", keys)
		}
		if got := string(ctx.Request.Header.Method()); got != "GET" {
			t.Fatalf("Method() = %q, want the request populated", got)
		}
	})
	t.Run("malformed-wins", func(t *testing.T) {
		fields := append(slices.Clone(get),
			hpack.HeaderField{Name: "trailer", Value: "content-length"},
			hpack.HeaderField{Name: "connection", Value: "close"},
		)
		if _, err := populateRequest(&fasthttp.RequestCtx{}, fields, protocolContext, false); !errors.Is(err, errInvalidRequestHeaders) {
			t.Fatalf("populateRequest() error = %v, want %v", err, errInvalidRequestHeaders)
		}
	})
}

// A second content-length field is malformed whatever its value.
func TestPopulateRequestRejectsDuplicateContentLength(t *testing.T) {
	protocolContext := testProtocolContext(t)
	for _, values := range [][]string{{"4", "4"}, {"0", "0"}, {"4", "5"}} {
		fields := []hpack.HeaderField{
			{Name: ":method", Value: "POST"},
			{Name: ":scheme", Value: "http"},
			{Name: ":authority", Value: "example.com"},
			{Name: ":path", Value: "/"},
		}
		for _, value := range values {
			fields = append(fields, hpack.HeaderField{Name: "content-length", Value: value})
		}
		if _, err := populateRequest(&fasthttp.RequestCtx{}, fields, protocolContext, false); !errors.Is(err, errInvalidRequestHeaders) {
			t.Fatalf("content-length %v: error = %v, want %v", values, err, errInvalidRequestHeaders)
		}
	}
}

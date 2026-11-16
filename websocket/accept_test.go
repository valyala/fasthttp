package websocket

import (
	"bufio"
	"io"
	"iter"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func TestSubprotocol(t *testing.T) {
	got := make(chan string, 1)
	ts := startServer(t, &AcceptOptions{Subprotocols: []string{"b", "a"}}, func(c *Conn) {
		got <- c.Subprotocol()
	})
	c := ts.dial(t, &DialOptions{Subprotocols: []string{"a", "b"}})
	if c.Subprotocol() != "b" || <-got != "b" {
		t.Fatalf("client got %q", c.Subprotocol())
	}
	c = ts.dial(t, &DialOptions{Subprotocols: []string{"c"}})
	if c.Subprotocol() != "" || <-got != "" {
		t.Fatalf("client got %q", c.Subprotocol())
	}
}

// rawHandshake sends req to the server and returns the response head.
func rawHandshake(t *testing.T, addr, req string) string {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := io.WriteString(nc, req); err != nil {
		t.Fatal(err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	head, err := readHead(bufio.NewReader(nc))
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// fieldCount returns the last value of the header field and how often it
// occurs, matching the name ASCII case-insensitively.
func fieldCount(all iter.Seq2[[]byte, []byte], name string) ([]byte, int) {
	var value []byte
	n := 0
	for k, v := range all {
		if equalFoldASCII(k, name) {
			value, n = trimHTTPSpace(v), n+1
		}
	}
	return value, n
}

// Handshake fields are checked as HTTP lists and exact values under both header
// normalization settings.
func TestAcceptHeaderMatrix(t *testing.T) {
	const key = "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	const head = "GET /ws HTTP/1.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
	const noVersion = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
	cases := []struct{ name, req, status string }{
		{"connection close list", noVersion + "Sec-WebSocket-Version: 13\r\n" + strings.Replace(key, "Sec", "Connection: close, Upgrade\r\nSec", 1) + "\r\n", "400"},
		{"connection close line", head + "Host: h\r\nConnection: close\r\n" + key + "\r\n", "400"},
		{"tabs around key", head + "Host: h\r\nSec-WebSocket-Key:\tdGhlIHNhbXBsZSBub25jZQ==\t\r\n\r\n", "101"},
		{"key with inner space", head + "Host: h\r\nSec-WebSocket-Key: dGhlIHNhbXBs ZSBub25jZQ==\r\n\r\n", "400"},
		{"version 013", noVersion + "Sec-WebSocket-Version: 013\r\n" + key + "\r\n", "426"},
		{"empty version", noVersion + "Sec-WebSocket-Version:\r\n" + key + "\r\n", "426"},
		{"extensions offered", head + "Host: h\r\n" + key + "Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n", "101"},
		{"origin with default port", head + "Host: example.com\r\n" + key + "Origin: http://example.com:80\r\n\r\n", "403"},
		{"host with port, origin without", head + "Host: example.com:80\r\n" + key + "Origin: http://example.com\r\n\r\n", "403"},
		{"ipv6 origin case", head + "Host: [2001:db8::a]\r\n" + key + "Origin: HTTP://[2001:DB8::A]\r\n\r\n", "101"},
		{"ipv6 origin port", head + "Host: [::1]:8080\r\n" + key + "Origin: http://[::1]:8080\r\n\r\n", "101"},
		{"ipv6 origin wrong port", head + "Host: [::1]:8080\r\n" + key + "Origin: http://[::1]:8081\r\n\r\n", "403"},
		{"quoted subprotocol", head + "Host: h\r\n" + key + "Sec-WebSocket-Protocol: \"chat\"\r\n\r\n", "400"},
		{"subprotocol list of commas", head + "Host: h\r\n" + key + "Sec-WebSocket-Protocol: ,\r\n\r\n", "400"},
	}
	for _, normalize := range []bool{true, false} {
		ts := startServerWith(t, &AcceptOptions{Subprotocols: []string{"chat"}}, echo,
			func(s *fasthttp.Server) { s.DisableHeaderNamesNormalizing = !normalize })
		for _, tc := range cases {
			if resp := rawHandshake(t, ts.addr, tc.req); !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") {
				t.Fatalf("normalize=%v %s: %q", normalize, tc.name, resp)
			}
		}
	}
}

// A subprotocol name in AcceptOptions that is not an HTTP token is a server
// configuration error, answered with 500 before any negotiation.
func TestAcceptRejectsNonTokenSubprotocolOption(t *testing.T) {
	ts := startServer(t, &AcceptOptions{Subprotocols: []string{"bad name"}}, echo)
	req := "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if resp := rawHandshake(t, ts.addr, req); !strings.HasPrefix(resp, "HTTP/1.1 500 ") {
		t.Fatalf("response: %q", resp)
	}
}

// Handshake fields set on the response before Accept are replaced, whatever
// their spelling, while other preset headers are kept.
func TestAcceptReplacesHandshakeHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, header, value string
		supported, offered  []string
		want                string
		disableNormalizing  bool
	}{
		{name: "protocol without negotiation", header: "Sec-WebSocket-Protocol", value: "chat", offered: []string{"chat"}},
		{name: "protocol replaced by the selected one", header: "Sec-WebSocket-Protocol", value: "stale", supported: []string{"chat"}, offered: []string{"chat"}, want: "chat"},
		{name: "extension", header: "Sec-WebSocket-Extensions", value: "permessage-deflate"},
		{name: "accept spelled differently", header: "sec-websocket-accept", value: "stale", disableNormalizing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverProtocol := make(chan string, 1)
			ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
				s.DisableHeaderNamesNormalizing = tc.disableNormalizing
				s.Handler = func(ctx *fasthttp.RequestCtx) {
					ctx.Response.Header.Set(tc.header, tc.value)
					ctx.Response.Header.Set("X-App", "kept")
					_ = Accept(ctx, &AcceptOptions{Subprotocols: tc.supported}, func(c *Conn) { serverProtocol <- c.Subprotocol() })
				}
			})
			var resp fasthttp.Response
			c := ts.dial(t, &DialOptions{Response: &resp, Subprotocols: tc.offered})
			if got := <-serverProtocol; got != tc.want || c.Subprotocol() != tc.want {
				t.Fatalf("subprotocols: server %q, client %q, want %q", got, c.Subprotocol(), tc.want)
			}
			all := resp.Header.All()
			if _, n := fieldCount(all, fasthttp.HeaderSecWebSocketAccept); n != 1 {
				t.Fatalf("%d Sec-WebSocket-Accept fields", n)
			}
			if _, n := fieldCount(all, fasthttp.HeaderSecWebSocketExtensions); n != 0 {
				t.Fatal("an extension was announced")
			}
			if string(resp.Header.Peek("X-App")) != "kept" {
				t.Fatal("a preset header was dropped")
			}
		})
	}
}

// A rejected method is answered with 405 and the Allow field.
func TestAccept405CarriesAllow(t *testing.T) {
	ts := startServer(t, nil, echo)
	resp := rawHandshake(t, ts.addr, "POST /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nContent-Length: 0\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 405 ") || !strings.Contains(resp, "\r\nAllow: GET\r\n") {
		t.Fatalf("response: %q", resp)
	}
}

// A Sec-WebSocket-Protocol field with no name, or with a name twice, fails the
// handshake; empty list items between names are tolerated.
func TestAcceptRejectsEmptyOrRepeatedSubprotocols(t *testing.T) {
	const head = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	cases := []struct{ offer, status string }{
		{"Sec-WebSocket-Protocol:\r\n", "400"},
		{"Sec-WebSocket-Protocol: , ,\r\n", "400"},
		{"Sec-WebSocket-Protocol: chat, chat\r\n", "400"},
		{"Sec-WebSocket-Protocol: chat\r\nsec-websocket-protocol: chat\r\n", "400"},
		{"Sec-WebSocket-Protocol: , chat,\r\n", "101"},
		{"Sec-WebSocket-Protocol: chat\r\nSec-WebSocket-Protocol: echo\r\n", "101"},
	}
	for _, normalize := range []bool{true, false} {
		ts := startServerWith(t, &AcceptOptions{Subprotocols: []string{"chat"}}, echo,
			func(s *fasthttp.Server) { s.DisableHeaderNamesNormalizing = !normalize })
		for _, tc := range cases {
			if resp := rawHandshake(t, ts.addr, head+tc.offer+"\r\n"); !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") {
				t.Fatalf("normalize=%v %q: %q", normalize, tc.offer, resp)
			}
		}
	}
}

// Offered extensions must follow the RFC 6455 syntax, quoted values included,
// although none is selected.
func TestAcceptChecksExtensionSyntax(t *testing.T) {
	const head = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Extensions: "
	ts := startServer(t, nil, echo)
	for _, tc := range []struct{ offer, status string }{
		{"", "400"},
		{";", "400"},
		{"permessage-deflate;", "400"},
		{"permessage-deflate; client_max_window_bits=", "400"},
		{"permessage-deflate; p=\"open", "400"},
		{"permessage-deflate; =15", "400"},
		{"permessage-deflate; p=\"a\\\"", "400"},
		{"permessage-deflate; p=\"\x01\"", "400"},
		{"permessage-deflate; client_max_window_bits=15; server_no_context_takeover", "101"},
		{"permessage-deflate; client_max_window_bits = 15 , foo; bar=\"a\\b\"", "101"},
		{"foo; bar=\"a;b\"", "400"},
		{"foo; bar=\"a,b\"", "400"},
		{", permessage-deflate,", "101"},
	} {
		resp := rawHandshake(t, ts.addr, head+tc.offer+"\r\n\r\n")
		if !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") || strings.Contains(strings.ToLower(resp), "sec-websocket-extensions") {
			t.Fatalf("%q: %q", tc.offer, resp)
		}
	}
}

// A handshake field preset under a spelling Accept cannot remove is a server
// error rather than a 101 with two of them.
func TestAcceptRefusesUnreachableHandshakeHeader(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			h.DisableNormalizing()
			h.Set("sec-websocket-accept", "stale")
			h.EnableNormalizing()
			if err := Accept(ctx, nil, func(*Conn) {}); err == nil {
				t.Error("Accept succeeded")
			}
		}
	})
	resp := rawHandshake(t, ts.addr, "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 500 ") {
		t.Fatalf("response: %q", resp)
	}
}

// A handshake field that deleting the Trailer declaration brings back into
// the headers is removed as well.
func TestAcceptRemovesHeaderRestoredFromTrailer(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			_ = h.SetTrailer(fasthttp.HeaderSecWebSocketExtensions)
			h.Set(fasthttp.HeaderSecWebSocketExtensions, "permessage-deflate")
			_ = Accept(ctx, nil, func(*Conn) {})
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp})
	_ = c.CloseNow()
	if _, n := fieldCount(resp.Header.All(), fasthttp.HeaderSecWebSocketExtensions); n != 0 {
		t.Fatal("an extension was announced")
	}
}

// With header-name normalization off, preset handshake fields are removed, a
// preset Connection: close does not stop Accept, and later header changes apply.
func TestAcceptCleansPresetHeadersWithoutNormalization(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.DisableHeaderNamesNormalizing = true
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			h.Set("connection", "close, keep-alive")
			h.Set("sec-websocket-extensions", "permessage-deflate")
			h.Set("x-middleware", "before")
			if err := Accept(ctx, nil, func(*Conn) {}); err != nil {
				t.Error(err)
				return
			}
			h.Set("x-middleware", "after")
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp})
	_ = c.CloseNow()
	if v, n := fieldCount(resp.Header.All(), "X-Middleware"); n != 1 || string(v) != "after" {
		t.Fatalf("X-Middleware: %d fields, last %q", n, v)
	}
}

// A Trailer declaration naming handshake fields would divert them into the
// trailer section; Accept drops it.
func TestAcceptDropsPresetTrailer(t *testing.T) {
	serverProtocol := make(chan string, 1)
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set(fasthttp.HeaderTrailer, "Sec-WebSocket-Accept, Sec-WebSocket-Protocol")
			_ = Accept(ctx, &AcceptOptions{Subprotocols: []string{"chat"}}, func(c *Conn) { serverProtocol <- c.Subprotocol() })
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp, Subprotocols: []string{"chat"}})
	_ = c.CloseNow()
	if got := <-serverProtocol; got != "chat" || c.Subprotocol() != "chat" {
		t.Fatalf("subprotocols: server %q, client %q", got, c.Subprotocol())
	}
	if _, n := fieldCount(resp.Header.All(), fasthttp.HeaderTrailer); n != 0 {
		t.Fatal("Trailer was sent")
	}
}

// A quoted extension parameter value must unescape to a token.
func TestAcceptQuotedExtensionValuesAreTokens(t *testing.T) {
	const head = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits="
	ts := startServer(t, nil, echo)
	for _, tc := range []struct{ value, status string }{
		{`""`, "400"},
		{`"1 5"`, "400"},
		{`"1;5"`, "400"},
		{`"a\"b"`, "400"},
		{"\"\x80\"", "400"},
		{`"15"`, "101"},
		{`"\1\5"`, "101"},
	} {
		if resp := rawHandshake(t, ts.addr, head+tc.value+"\r\n\r\n"); !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") {
			t.Fatalf("%s: %q", tc.value, resp)
		}
	}
}

// Accept leaves the response header's name normalization as it found it, so
// a handler that changes headers after Accept still replaces and removes them.
func TestAcceptKeepsHeaderNormalization(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			h.Set("x-update", "before")
			h.Set("x-remove", "discard")
			if err := Accept(ctx, nil, func(*Conn) {}); err != nil {
				t.Error(err)
				return
			}
			h.Set("x-update", "after")
			h.Del("x-remove")
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp})
	_ = c.CloseNow()
	if v, n := fieldCount(resp.Header.All(), "X-Update"); n != 1 || string(v) != "after" {
		t.Fatalf("X-Update: %d fields, last %q", n, v)
	}
	if _, n := fieldCount(resp.Header.All(), "X-Remove"); n != 0 {
		t.Fatal("X-Remove was sent")
	}
}

// A 426 names the protocol it requires, as RFC 9110 demands.
func TestAccept426CarriesUpgrade(t *testing.T) {
	ts := startServer(t, nil, echo)
	resp := strings.ToLower(rawHandshake(t, ts.addr, "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 12\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	for _, want := range []string{"http/1.1 426 ", "\r\nupgrade: websocket\r\n", "\r\nconnection: upgrade\r\n", "\r\nsec-websocket-version: 13\r\n"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response lacks %q: %q", want, resp)
		}
	}
}

// A handshake request with a body is refused; a chunked body the server has
// already consumed as empty is no body.
func TestAcceptRejectsBody(t *testing.T) {
	const head = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for _, tc := range []struct {
		body, status string
		stream       bool
	}{
		{"Content-Length: 5\r\n\r\nhello", "400", false},
		{"Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "101", false},
		{"Content-Length: 5\r\n\r\nhello", "400", true},
		{"Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "400", true},
		{"\r\n", "101", true},
		{"Content-Length: 0\r\n\r\n", "101", true},
		{"Content-Length: 0\r\n\r\n", "101", false},
	} {
		ts := startServerWith(t, nil, echo, func(s *fasthttp.Server) { s.StreamRequestBody = tc.stream })
		if resp := rawHandshake(t, ts.addr, head+tc.body); !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") {
			t.Fatalf("stream=%v %q: %q", tc.stream, tc.body, resp)
		}
	}
}

// The 101 is an HTTP/1.1 response whatever protocol the response was set to
// before Accept.
func TestAcceptSetsHTTP11(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.SetProtocol([]byte("HTTP/1.0"))
			_ = Accept(ctx, nil, func(*Conn) {})
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp})
	_ = c.CloseNow()
	if p := string(resp.Header.Protocol()); p != "HTTP/1.1" {
		t.Fatalf("protocol %q", p)
	}
}

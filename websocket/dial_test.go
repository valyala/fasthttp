package websocket

import (
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

func TestDialHeaders(t *testing.T) {
	seen := make(chan string, 1)
	ts2 := startServer(t, &AcceptOptions{CheckOrigin: func(ctx *fasthttp.RequestCtx) bool {
		seen <- string(ctx.Request.Header.Peek("X-Token")) + "|" + string(ctx.Host()) +
			"|" + string(ctx.Request.Header.Peek(fasthttp.HeaderOrigin))
		return true
	}}, echo)
	var h fasthttp.RequestHeader
	h.Set("X-Token", "secret")
	h.Set(fasthttp.HeaderOrigin, "https://other.example")
	h.SetHost("override.example")
	var resp fasthttp.Response
	c := ts2.dial(t, &DialOptions{Header: &h, Response: &resp})
	if s := <-seen; s != "secret|override.example|https://other.example" {
		t.Fatalf("server saw %q", s)
	}
	if resp.StatusCode() != fasthttp.StatusSwitchingProtocols || !resp.Header.ConnectionUpgrade() {
		t.Fatalf("response: %d", resp.StatusCode())
	}
	_ = c.CloseNow()
}

func TestDialRejected(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()
	t.Cleanup(func() { ln.Close() })
	s := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/forbidden":
			ctx.Error("no", fasthttp.StatusForbidden)
		case "/badaccept":
			ctx.SetStatusCode(fasthttp.StatusSwitchingProtocols)
			ctx.Response.Header.Set(fasthttp.HeaderUpgrade, "websocket")
			ctx.Response.Header.Set(fasthttp.HeaderConnection, "Upgrade")
			ctx.Response.Header.Set(fasthttp.HeaderSecWebSocketAccept, "AAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			ctx.Hijack(func(net.Conn) {})
		case "/protocol":
			ctx.SetStatusCode(fasthttp.StatusSwitchingProtocols)
			ctx.Response.Header.Set(fasthttp.HeaderUpgrade, "websocket")
			ctx.Response.Header.Set(fasthttp.HeaderConnection, "Upgrade")
			key := ctx.Request.Header.Peek(fasthttp.HeaderSecWebSocketKey)
			ctx.Response.Header.SetBytesV(fasthttp.HeaderSecWebSocketAccept, appendAcceptKey(nil, key))
			ctx.Response.Header.Set(fasthttp.HeaderSecWebSocketProtocol, "nope")
			ctx.Hijack(func(net.Conn) {})
		}
	}}
	go func() { _ = s.Serve(ln) }()
	dial := func(string) (net.Conn, error) { return ln.Dial() }
	var resp fasthttp.Response
	for _, tc := range []struct {
		url, want string
		status    int
	}{
		{"ws://h/forbidden", "status 403", fasthttp.StatusForbidden},
		{"ws://h/badaccept", "Sec-WebSocket-Accept", fasthttp.StatusSwitchingProtocols},
		{"ws://h/protocol", "subprotocol", fasthttp.StatusSwitchingProtocols},
		{"ftp://h/", "scheme", 0},
		{"ws:///nohost", "host", 0},
	} {
		resp.Reset()
		_, err := Dial(t.Context(), tc.url, &DialOptions{Dial: dial, Response: &resp})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want %q", tc.url, err, tc.want)
		}
		if tc.status != 0 && resp.StatusCode() != tc.status {
			t.Fatalf("%s: status %d", tc.url, resp.StatusCode())
		}
	}
}

// acceptResponse answers a handshake with a plain 101 response.
func acceptResponse(accept string) string {
	return okHead + "Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
}

// Dial accepts list-valued Connection fields and mixed-case tokens, keeps frames
// behind the response, and refuses a closing or accept-repeating response.
func TestDialResponseMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, response, payload string
		ok                      bool
	}{
		{"connection close list", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: close, Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", "", false},
		{"connection keep-alive list", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", "", true},
		{"mixed case tokens", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: WebSocket\r\nConnection: upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", "", true},
		{"accept repeated", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Accept: %s\r\n\r\n", "", false},
		{"accept with tabs", okHead + "Sec-WebSocket-Accept:\t%s\t\r\n\r\n", "", true},
		{"frames behind the response", okHead + "Sec-WebSocket-Accept: %s\r\n\r\n" + string(appendFrame(nil, opText, true, false, []byte("hi"))), "hi", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := rawServer(t, func(accept string) string { return strings.ReplaceAll(tc.response, "%s", accept) })
			c, err := Dial(t.Context(), "ws://"+addr+"/", nil)
			if (err == nil) != tc.ok {
				t.Fatalf("dial: %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			if tc.payload == "" {
				return
			}
			if _, p, err := c.Read(t.Context()); err != nil || string(p) != tc.payload {
				t.Fatalf("read: %q %v", p, err)
			}
		})
	}
}

// The handshake request spells the fields as RFC 6455 does, sends one Host (the
// caller's override wins), joins the subprotocols and carries no body framing.
func TestDialRequestWireFormat(t *testing.T) {
	for _, normalized := range []bool{true, false} {
		addr, request := rawServer(t, acceptResponse)
		var h fasthttp.RequestHeader
		if !normalized {
			h.DisableNormalizing()
		}
		h.Set("host", "override.example")
		h.Set("x-token", "secret")
		c, err := Dial(t.Context(), "ws://"+addr+"/p?q=1", &DialOptions{Header: &h, Subprotocols: []string{"chat", "echo"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.CloseNow()
		head := <-request
		for _, want := range []string{
			"GET /p?q=1 HTTP/1.1\r\n", "Host: override.example\r\n", "Upgrade: websocket\r\n", "Connection: Upgrade\r\n",
			"Sec-WebSocket-Version: 13\r\n", "Sec-WebSocket-Key: ", "Sec-WebSocket-Protocol: chat, echo\r\n",
		} {
			if !strings.Contains(head, want) {
				t.Fatalf("normalized=%v: request lacks %q:\n%s", normalized, want, head)
			}
		}
		lower := strings.ToLower(head)
		if strings.Count(lower, "host:") != 1 || strings.Contains(lower, "content-length") || strings.Contains(lower, "transfer-encoding") {
			t.Fatalf("normalized=%v: request:\n%s", normalized, head)
		}
	}
}

// User information in the URL, as URI.Parse finds it, is sent decoded as Basic
// credentials, empty ones included, unless the caller's headers carry an
// Authorization.
func TestDialURLCredentials(t *testing.T) {
	basic := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct{ url, header, want string }{
		{"ws://user:pass@HOST/", "", basic("user:pass")},
		{"ws://u%40x:p%25%2Fss@HOST/", "", basic("u@x:p%/ss")},
		{"ws://user@HOST/", "", basic("user:")},
		{"ws://:pass@HOST/", "", basic(":pass")},
		{"ws://@HOST/", "", basic(":")},
		{"ws://:@HOST/", "", basic(":")},
		{"//user:pass@HOST/", "", basic("user:pass")},
		{"ws://user:pass@HOST/", "Bearer token", "Bearer token"},
		{"ws://HOST/", "", ""},
		{"ws://HOST/a@b?c=d@e", "", ""},
		{"//HOST/a://b@c", "", ""},
	} {
		addr, request := rawServer(t, acceptResponse)
		var h fasthttp.RequestHeader
		if tc.header != "" {
			h.Set(fasthttp.HeaderAuthorization, tc.header)
		}
		c, err := Dial(t.Context(), strings.Replace(tc.url, "HOST", addr, 1), &DialOptions{Header: &h})
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		_ = c.CloseNow()
		head := <-request
		if tc.want == "" {
			if strings.Contains(strings.ToLower(head), "authorization:") {
				t.Fatalf("%s: unexpected credentials:\n%s", tc.url, head)
			}
		} else if strings.Count(head, "Authorization: ") != 1 || !strings.Contains(head, "Authorization: "+tc.want+"\r\n") {
			t.Fatalf("%s: want Authorization %q:\n%s", tc.url, tc.want, head)
		}
	}
}

// An invalid escape in the URL's user information fails before the dialer runs.
func TestDialURLCredentialsBadEscape(t *testing.T) {
	for userinfo, want := range map[string]url.EscapeError{"user:p%zz": "%zz", "u%:pass": "%", "user:pass%2": "%2"} {
		dial := func(string) (net.Conn, error) {
			t.Errorf("%q: the dialer was called", userinfo)
			return nil, net.ErrClosed
		}
		if _, err := Dial(t.Context(), "ws://"+userinfo+"@example.com/", &DialOptions{Dial: dial}); !errors.Is(err, want) {
			t.Fatalf("%q: %v", userinfo, err)
		}
	}
}

// A conn from DialOptions.Dial is closed through its own Close, whatever else
// it implements, after a failed handshake as well as by CloseNow.
func TestDialClosesDialedConn(t *testing.T) {
	ts := startServer(t, nil, echo)
	var closed atomic.Int32
	opts := &DialOptions{Dial: func(string) (net.Conn, error) {
		nc, err := net.Dial("tcp", ts.addr)
		return closeCountConn{Conn: nc, closed: &closed}, err
	}}
	c, err := Dial(t.Context(), "ws://example.com/", opts)
	if err != nil {
		t.Fatal(err)
	}
	if _ = c.CloseNow(); closed.Load() != 1 {
		t.Fatalf("CloseNow closed the dialed conn %d times", closed.Load())
	}
	opts.Header = &fasthttp.RequestHeader{}
	opts.Header.Set(fasthttp.HeaderOrigin, "https://other.example")
	if _, err := Dial(t.Context(), "ws://example.com/", opts); err == nil || closed.Load() != 2 {
		t.Fatalf("refused handshake: %v, the dialed conn was closed %d times", err, closed.Load()-1)
	}
}

// The request target is sent as given: a missing path becomes "/", a query
// without a path keeps the query, and escapes and dot segments are untouched.
func TestDialRequestTargets(t *testing.T) {
	for _, tc := range []struct{ suffix, want string }{
		{"", "/"},
		{"?token=abc", "/?token=abc"},
		{"/a%2Fb//c?x=%2F", "/a%2Fb//c?x=%2F"},
		{"/a/../b//%2e?b=2&a=1", "/a/../b//%2e?b=2&a=1"},
	} {
		addr, request := rawServer(t, acceptResponse)
		c, err := Dial(t.Context(), "ws://"+addr+tc.suffix, nil)
		if err != nil {
			t.Fatalf("%q: %v", tc.suffix, err)
		}
		_ = c.CloseNow()
		if head := <-request; !strings.HasPrefix(head, "GET "+tc.want+" HTTP/1.1\r\n") {
			t.Fatalf("%q: %q", tc.suffix, head)
		}
	}
}

// Dial skips interim 1xx responses before the final one and gives up after a
// few.
func TestDialSkipsInterimResponses(t *testing.T) {
	const hints = "HTTP/1.1 103 Early Hints\r\nLink: </app.css>; rel=preload\r\n\r\n"
	for _, tc := range []struct {
		name, interim string
		ok            bool
	}{
		{"continue", "HTTP/1.1 100 Continue\r\n\r\n", true},
		{"early hints", hints, true},
		{"five hints", strings.Repeat(hints, 5), true},
		{"six hints", strings.Repeat(hints, 6), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := rawServer(t, func(accept string) string { return tc.interim + acceptResponse(accept) })
			var resp fasthttp.Response
			c, err := Dial(t.Context(), "ws://"+addr+"/", &DialOptions{Response: &resp})
			if (err == nil) != tc.ok {
				t.Fatalf("dial: %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				return
			}
			_ = c.CloseNow()
			if resp.StatusCode() != fasthttp.StatusSwitchingProtocols {
				t.Fatalf("status %d", resp.StatusCode())
			}
		})
	}
}

// The response's Upgrade field must name websocket and nothing else.
func TestDialRejectsExtraUpgradeProtocol(t *testing.T) {
	for _, tc := range []struct {
		upgrade string
		ok      bool
	}{
		{"Upgrade: websocket, h2c\r\n", false},
		{"Upgrade: websocket\r\nUpgrade: h2c\r\n", false},
		{"Upgrade: websocket\r\nUpgrade: WebSocket\r\n", true},
		{"Upgrade: , websocket,\r\n", true},
	} {
		addr, _ := rawServer(t, func(accept string) string {
			return "HTTP/1.1 101 Switching Protocols\r\n" + tc.upgrade + "Connection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"
		})
		c, err := Dial(t.Context(), "ws://"+addr+"/", nil)
		if (err == nil) != tc.ok {
			t.Fatalf("%q: %v", tc.upgrade, err)
		}
		if c != nil {
			_ = c.CloseNow()
		}
	}
}

// A path or query that cannot be sent as given fails before the connection
// is made.
func TestDialRejectsInvalidRequestTarget(t *testing.T) {
	for _, tc := range []struct {
		suffix string
		ok     bool
	}{
		{"/a b", false},
		{"/bad%zz", false},
		{"/short%4", false},
		{"/café", false},
		{"/a\"b", false},
		{"/a{b}", false},
		{"?x=\x7f", false}, // Refused by the URI parser already.
		{"/a%2Fb/~c!$&'()*+,;=:@?q[]=1&r=%41", true},
		{"", true},
	} {
		addr, _ := rawServer(t, acceptResponse)
		c, err := Dial(t.Context(), "ws://"+addr+tc.suffix, nil)
		if tc.ok && err != nil {
			t.Fatalf("%q: %v", tc.suffix, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(strings.ToLower(err.Error()), "invalid ur")) {
			t.Fatalf("%q: %v", tc.suffix, err)
		}
		if c != nil {
			_ = c.CloseNow()
		}
	}
}

package fasthttpproxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpproxy"
)

func TestDialerGetDialFunc(t *testing.T) {
	counts := make([]atomic.Int64, 4)
	proxyListenPorts := []string{"8001", "8002", "8003", "8004"}
	lns := startProxyServer(t, proxyListenPorts, counts)
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:"+proxyListenPorts[2])
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:"+proxyListenPorts[3])
	t.Setenv("NO_PROXY", "github.com")
	type fields struct {
		httpProxy  string
		httpsProxy string
		noProxy    string
	}
	type args struct {
		useEnv bool
	}
	tests := []struct {
		name               string
		fields             fields
		args               args
		expectedCounts     []int64
		dialAddr           string
		expectedErrMessage string
	}{
		{
			name: "proxy information comes from the configuration. dial https host",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			expectedCounts: []int64{0, 1, 0, 0},
			dialAddr:       "github.io:443",
		},
		{
			name: "proxy information comes from the configuration. dial http host",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			expectedCounts: []int64{1, 0, 0, 0},
			dialAddr:       "github.io:80",
		},
		{
			name: "proxy information comes from the configuration. dial http host matched with no proxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			expectedCounts: []int64{0, 0, 0, 0},
			dialAddr:       "github.com:80",
		},
		{
			name: "proxy information comes from the configuration. dial https host matched with no proxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			expectedCounts: []int64{0, 0, 0, 0},
			dialAddr:       "github.com:443",
		},
		{
			name: "proxy information comes from the env. dial http host",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: true,
			},
			expectedCounts: []int64{0, 0, 1, 0},
			dialAddr:       "github.io:80",
		},
		{
			name: "proxy information comes from the env. dial https host",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: true,
			},
			expectedCounts: []int64{0, 0, 0, 1},
			dialAddr:       "github.io:443",
		},

		{
			name: "proxy information comes from the env. dial http host matched with no proxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: true,
			},
			expectedCounts: []int64{0, 0, 0, 0},
			dialAddr:       "github.com:80",
		},
		{
			name: "proxy information comes from the env. dial https host matched with no proxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: true,
			},
			expectedCounts: []int64{0, 0, 0, 0},
			dialAddr:       "github.com:443",
		},
		{
			name: "proxy information comes from the configuration and http proxy same with https proxy. dial http host",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[0],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			expectedCounts: []int64{1, 0, 0, 0},
			dialAddr:       "github.io:80",
		},
		{
			name: "proxy information comes from the configuration and http proxy same with https proxy. dial https host",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[0],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			expectedCounts: []int64{1, 0, 0, 0},
			dialAddr:       "github.io:443",
		},
		{
			name: "proxy information comes from the configuration and http proxy same with https proxy. dial http host matched with no proxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[0],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			expectedCounts: []int64{0, 0, 0, 0},
			dialAddr:       "github.com:80",
		},
		{
			name: "proxy information comes from the configuration and http proxy same with https proxy. dial https host matched with no proxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[0],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			expectedCounts: []int64{0, 0, 0, 0},
			dialAddr:       "github.com:443",
		},
		{
			name: "return an error for unsupported proxy protocols.",
			fields: fields{
				httpProxy:  "socket6://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "socket6://127.0.0.1:" + proxyListenPorts[0],
			},
			args: args{
				useEnv: false,
			},
			expectedCounts:     []int64{0, 0, 0, 0},
			dialAddr:           "github.io:80",
			expectedErrMessage: "proxy: unknown scheme: socket6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := getDialer(tt.fields.httpProxy, tt.fields.httpsProxy, tt.fields.noProxy)
			dialFunc, err := d.GetDialFunc(tt.args.useEnv)
			if (err != nil) != (tt.expectedErrMessage != "") {
				t.Fatalf("GetDialFunc() error = %v, expectedErr %v", err, tt.expectedErrMessage)
				return
			}
			if tt.expectedErrMessage != "" {
				if err.Error() != tt.expectedErrMessage {
					t.Fatalf("expected error message: %s, got: %s", tt.expectedErrMessage, err.Error())
				}
				return
			}
			_, err = dialFunc(tt.dialAddr)
			if err != nil {
				t.Fatal(err)
			}
			if !countsEqual(getCounts(counts), tt.expectedCounts) {
				t.Errorf("GetDialFunc() counts = %v, expected %v", getCounts(counts), tt.expectedCounts)
			}
		})
		for i := range counts {
			counts[i].Store(0)
		}
	}
}

func TestHTTPProxyDialRejectsTargetAddrContainingNewlines(t *testing.T) {
	var dialed atomic.Bool

	conn, err := httpProxyDial(DialerFunc(func(network, addr string) (net.Conn, error) {
		dialed.Store(true)
		return nil, errors.New("unexpected proxy dial")
	}), "tcp4", "victim.example:443\r\nX-Injected: yes", "127.0.0.1:8080", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if conn != nil {
		t.Fatalf("expected nil conn, got %#v", conn)
	}
	if !strings.Contains(err.Error(), "cr or lf") {
		t.Fatalf("unexpected error: %v", err)
	}
	if dialed.Load() {
		t.Fatal("proxy dialer must not be invoked for invalid target addresses")
	}
}

func TestProxyDialerConstructorsReturnErroringDialFunc(t *testing.T) {
	t.Setenv("HTTP_PROXY", "socket6://127.0.0.1:8080")
	t.Setenv("HTTPS_PROXY", "socket6://127.0.0.1:8080")
	t.Setenv("NO_PROXY", "")

	tests := []struct {
		name string
		fn   func() fasthttp.DialFunc
	}{
		{
			name: "http",
			fn: func() fasthttp.DialFunc {
				return FasthttpHTTPDialer("socket6://127.0.0.1:8080")
			},
		},
		{
			name: "http dual stack",
			fn: func() fasthttp.DialFunc {
				return FasthttpHTTPDialerDualStack("socket6://127.0.0.1:8080")
			},
		},
		{
			name: "socks",
			fn: func() fasthttp.DialFunc {
				return FasthttpSocksDialer("socket6://127.0.0.1:8080")
			},
		},
		{
			name: "socks dual stack",
			fn: func() fasthttp.DialFunc {
				return FasthttpSocksDialerDualStack("socket6://127.0.0.1:8080")
			},
		},
		{
			name: "env",
			fn:   FasthttpProxyHTTPDialer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialFunc := tt.fn()
			if dialFunc == nil {
				t.Fatalf("unexpected nil dial func")
			}
			conn, err := dialFunc("example.com:80")
			if conn != nil {
				conn.Close()
			}
			if err == nil {
				t.Fatalf("expected error")
			}
			if err.Error() != "proxy: unknown scheme: socket6" {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func startProxyServer(t *testing.T, ports []string, counts []atomic.Int64) (lns []net.Listener) {
	for i, port := range ports {
		ln, err := net.Listen("tcp", ":"+port)
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
		i := i
		go func() {
			req := fasthttp.AcquireRequest()
			for {
				conn, err := ln.Accept()
				if err != nil {
					if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
						t.Error(err)
					}
					break
				}
				err = req.Read(bufio.NewReader(conn))
				if err != nil {
					t.Error(err)
				}
				if string(req.Header.Method()) == "CONNECT" {
					counts[i].Add(1)
				}
				_, err = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				if err != nil {
					t.Error(err)
				}
				req.Reset()
			}
			fasthttp.ReleaseRequest(req)
		}()
	}
	return lns
}

func getDialer(httpProxy, httpsProxy, noProxy string) *Dialer {
	return &Dialer{
		Config: httpproxy.Config{
			HTTPProxy:  httpProxy,
			HTTPSProxy: httpsProxy,
			NoProxy:    noProxy,
		},
	}
}

func getCounts(counts []atomic.Int64) (r []int64) {
	for i := range counts {
		r = append(r, counts[i].Load())
	}
	return r
}

func countsEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if b[i] != a[i] {
			return false
		}
	}
	return true
}

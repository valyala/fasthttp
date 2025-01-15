package fasthttpproxy

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpproxy"
)

func TestDialer_GetDialFunc(t *testing.T) {
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
		name           string
		fields         fields
		args           args
		wantCounts     []int64
		dialAddr       string
		wantErrMessage string
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
			wantCounts: []int64{0, 1, 0, 0},
			dialAddr:   "github.io:443",
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
			wantCounts: []int64{1, 0, 0, 0},
			dialAddr:   "github.io:80",
		},
		{
			name: "proxy information comes from the configuration. dial http host matched with noProxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			wantCounts: []int64{0, 0, 0, 0},
			dialAddr:   "github.com:80",
		},
		{
			name: "proxy information comes from the configuration. dial https host matched with noProxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			wantCounts: []int64{0, 0, 0, 0},
			dialAddr:   "github.com:443",
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
			wantCounts: []int64{0, 0, 1, 0},
			dialAddr:   "github.io:80",
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
			wantCounts: []int64{0, 0, 0, 1},
			dialAddr:   "github.io:443",
		},

		{
			name: "proxy information comes from the env. dial http host matched with noProxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: true,
			},
			wantCounts: []int64{0, 0, 0, 0},
			dialAddr:   "github.com:80",
		},
		{
			name: "proxy information comes from the env. dial https host matched with noProxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[1],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: true,
			},
			wantCounts: []int64{0, 0, 0, 0},
			dialAddr:   "github.com:443",
		},
		{
			name: "proxy information comes from the configuration and httpProxy same with httpsProxy. dial http host",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[0],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			wantCounts: []int64{1, 0, 0, 0},
			dialAddr:   "github.io:80",
		},
		{
			name: "proxy information comes from the configuration and httpProxy same with httpsProxy. dial https host",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[0],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			wantCounts: []int64{1, 0, 0, 0},
			dialAddr:   "github.io:443",
		},
		{
			name: "proxy information comes from the configuration and httpProxy same with httpsProxy. dial http host matched with noProxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[0],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			wantCounts: []int64{0, 0, 0, 0},
			dialAddr:   "github.com:80",
		},
		{
			name: "proxy information comes from the configuration and httpProxy same with httpsProxy. dial https host matched with noProxy",
			fields: fields{
				httpProxy:  "http://127.0.0.1:" + proxyListenPorts[0],
				httpsProxy: "http://127.0.0.1:" + proxyListenPorts[0],
				noProxy:    "github.com",
			},
			args: args{
				useEnv: false,
			},
			wantCounts: []int64{0, 0, 0, 0},
			dialAddr:   "github.com:443",
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
			wantCounts:     []int64{0, 0, 0, 0},
			dialAddr:       "github.io:80",
			wantErrMessage: "proxy: unknown scheme: socket6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := getDialer(tt.fields.httpProxy, tt.fields.httpsProxy, tt.fields.noProxy)
			dialFunc, err := d.GetDialFunc(tt.args.useEnv)
			if (err != nil) != (tt.wantErrMessage != "") {
				t.Fatalf("GetDialFunc() error = %v, wantErr %v", err, tt.wantErrMessage)
				return
			}
			if tt.wantErrMessage != "" {
				if err.Error() != tt.wantErrMessage {
					t.Fatalf("want error message: %s, got: %s", err.Error(), tt.wantErrMessage)
				}
				return
			}
			_, err = dialFunc(tt.dialAddr)
			if err != nil {
				t.Fatal(err)
			}
			if !countsEqual(getCounts(counts), tt.wantCounts) {
				t.Errorf("GetDialFunc() counts = %v, want %v", getCounts(counts), tt.wantCounts)
			}
		})
		for i := 0; i < len(counts); i++ {
			counts[i].Store(0)
		}
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
	return
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
	for i := 0; i < len(counts); i++ {
		r = append(r, counts[i].Load())
	}
	return
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

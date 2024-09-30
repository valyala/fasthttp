package fasthttpproxy

import (
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpproxy"
)

// FasthttpHTTPDialer returns a fasthttp.DialFunc that dials using
// the provided HTTP proxy.
//
// Example usage:
//
//	c := &fasthttp.Client{
//		Dial: fasthttpproxy.FasthttpHTTPDialer("username:password@localhost:9050"),
//	}
func FasthttpHTTPDialer(proxy string) fasthttp.DialFunc {
	return FasthttpHTTPDialerTimeout(proxy, 0)
}

// FasthttpHTTPDialerTimeout returns a fasthttp.DialFunc that dials using
// the provided HTTP proxy using the given timeout.
// The timeout parameter determines both the dial timeout and the CONNECT request timeout.
//
// Example usage:
//
//	c := &fasthttp.Client{
//		Dial: fasthttpproxy.FasthttpHTTPDialerTimeout("username:password@localhost:9050", time.Second * 2),
//	}
func FasthttpHTTPDialerTimeout(proxy string, timeout time.Duration) fasthttp.DialFunc {
	d := Dialer{Config: httpproxy.Config{HTTPProxy: proxy, HTTPSProxy: proxy}, Timeout: timeout, ConnectTimeout: timeout}
	dialFunc, _ := d.GetDialFunc(false)
	return dialFunc
}

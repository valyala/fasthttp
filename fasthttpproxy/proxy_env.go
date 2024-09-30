package fasthttpproxy

import (
	"time"

	"github.com/valyala/fasthttp"
)

const (
	httpsScheme = "https"
	httpScheme  = "http"
)

// FasthttpProxyHTTPDialer returns a fasthttp.DialFunc that dials using
// the env(HTTP_PROXY, HTTPS_PROXY and NO_PROXY) configured HTTP proxy.
//
// Example usage:
//
//	c := &fasthttp.Client{
//		Dial: fasthttpproxy.FasthttpProxyHTTPDialer(),
//	}
func FasthttpProxyHTTPDialer() fasthttp.DialFunc {
	return FasthttpProxyHTTPDialerTimeout(0)
}

// FasthttpProxyHTTPDialerTimeout returns a fasthttp.DialFunc that dials using
// the env(HTTP_PROXY, HTTPS_PROXY and NO_PROXY) configured HTTP proxy using the given timeout.
// The timeout parameter determines both the dial timeout and the CONNECT request timeout.
//
// Example usage:
//
//	c := &fasthttp.Client{
//		Dial: fasthttpproxy.FasthttpProxyHTTPDialerTimeout(time.Second * 2),
//	}
func FasthttpProxyHTTPDialerTimeout(timeout time.Duration) fasthttp.DialFunc {
	d := Dialer{Timeout: timeout, ConnectTimeout: timeout}
	dialFunc, _ := d.GetDialFunc(true)
	return dialFunc
}

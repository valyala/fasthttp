package fasthttpproxy

import (
	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpproxy"
)

// FasthttpSocksDialer returns a fasthttp.DialFunc that dials using
// the provided SOCKS5 proxy.
//
// Example usage:
//
//	c := &fasthttp.Client{
//		Dial: fasthttpproxy.FasthttpSocksDialer("socks5://localhost:9050"),
//	}
func FasthttpSocksDialer(proxyAddr string) fasthttp.DialFunc {
	d := Dialer{Config: httpproxy.Config{HTTPProxy: proxyAddr, HTTPSProxy: proxyAddr}}
	dialFunc, _ := d.GetDialFunc(false)
	return dialFunc
}

// FasthttpSocksDialerDualStack returns a fasthttp.DialFunc that dials using
// the provided SOCKS5 proxy with support for both IPv4 and IPv6.
//
// Example usage:
//
//	c := &fasthttp.Client{
//		Dial: fasthttpproxy.FasthttpSocksDialerDualStack("socks5://localhost:9050"),
//	}
func FasthttpSocksDialerDualStack(proxyAddr string) fasthttp.DialFunc {
	d := Dialer{Config: httpproxy.Config{HTTPProxy: proxyAddr, HTTPSProxy: proxyAddr}, DialDualStack: true}
	dialFunc, _ := d.GetDialFunc(false)
	return dialFunc
}
